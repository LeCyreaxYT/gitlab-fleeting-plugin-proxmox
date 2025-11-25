package plugin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/luthermonson/go-proxmox"
	"golang.org/x/sync/errgroup"
)

const (
	proxmoxTaskWaitInterval       = 10 * time.Second
	proxmoxTaskWaitTimeout        = 5 * time.Minute
	proxmoxAgentCheckInterval     = 15 * time.Second
	proxmoxAgentStartTimeout      = 10 * time.Minute
	proxmoxAgentMaxCheckAttempts  = int(proxmoxAgentStartTimeout / proxmoxAgentCheckInterval) // 40 attempts
)

var (
	ErrCloneVMWithoutConfiguredStorage = errors.New("attempted to clone a VM without configured storage")
	ErrNoAvailableVMID                 = errors.New("no available VMID in configured range")
)

func (ig *InstanceGroup) deployInstance(ctx context.Context, template *proxmox.VirtualMachine, cloneMu *sync.Mutex) (int, error) {
	VMID, task, err := ig.cloneTemplate(ctx, template, cloneMu)
	if err == nil {
		ig.log.Info("Deploying new instance", "vmid", VMID)

		err = task.Wait(ctx, proxmoxTaskWaitInterval, proxmoxTaskWaitTimeout)
	}

	if err != nil {
		return VMID, fmt.Errorf("failed to deploy instance: %w", err)
	}

	vm, err := ig.getProxmoxVM(ctx, VMID)
	if err != nil {
		return VMID, fmt.Errorf("failed to find newly deployed instance vmid='%d': %w", VMID, err)
	}

	// Start, configure etc.
	err = func() error {
		// Start the VM
		task, err := vm.Start(ctx)
		if err == nil {
			err = task.Wait(ctx, proxmoxTaskWaitInterval, proxmoxTaskWaitTimeout)
		}

		if err != nil {
			return fmt.Errorf("failed to start newly deployed instance: %w", err)
		}

		// Wait for agent to start with polling
		err = ig.waitForAgent(ctx, vm, VMID)
		if err != nil {
			return fmt.Errorf("failed when waiting for qemu agent to start on newly deployed instance: %w", err)
		}

		return nil
	}()

	newInstanceName := ig.InstanceNameRunning

	if err != nil {
		ig.log.Error("instance deployment failed, marking for removal", "vmid", VMID, "err", err)
		newInstanceName = ig.InstanceNameRemoving
	}

	_, renameErr := vm.Config(ctx, proxmox.VirtualMachineOption{
		Name:  "name",
		Value: newInstanceName,
	})
	if renameErr != nil {
		ig.log.Error("failed to rename instance", "vmid", VMID, "err", renameErr)
	}

	if err != nil {
		return VMID, fmt.Errorf("failed to configure instance, marked for removal due to: %w", err)
	}

	return VMID, nil
}

func (ig *InstanceGroup) cloneTemplate(ctx context.Context, template *proxmox.VirtualMachine, cloneMu *sync.Mutex) (int, *proxmox.Task, error) {
	cloneMu.Lock()
	defer cloneMu.Unlock()

	cloneOptions, err := ig.getTemplateCloneOptions(ctx, template)
	if err != nil {
		return -1, nil, err
	}

	VMID, task, err := template.Clone(ctx, cloneOptions)
	if err != nil {
		return -1, nil, fmt.Errorf("failed to clone the template: %w", err)
	}

	return VMID, task, nil
}

func (ig *InstanceGroup) getTemplateCloneOptions(ctx context.Context, template *proxmox.VirtualMachine) (*proxmox.VirtualMachineCloneOptions, error) {
	vmid, err := ig.findNextAvailableVMID(ctx)
	if err != nil {
		return nil, err
	}

	cloneOptions := &proxmox.VirtualMachineCloneOptions{
		NewID:   vmid,
		Name:    ig.InstanceNameCreating,
		Pool:    ig.Pool,
		Storage: ig.Storage,
		Full:    1,
	}

	if !template.Template && ig.Storage == "" {
		return nil, ErrCloneVMWithoutConfiguredStorage
	}

	if template.Template && ig.Storage == "" {
		cloneOptions.Full = 0
	}

	return cloneOptions, nil
}

func (ig *InstanceGroup) markStaleInstancesForRemoval(ctx context.Context) error {
	pool, err := ig.getProxmoxPool(ctx)
	if err != nil {
		return err
	}

	instancesToMarkForRemoval := make([]*proxmox.ClusterResource, 0, len(pool.Members))

	for _, member := range pool.Members {
		if !ig.isProxmoxResourceAnInstance(member) {
			continue
		}

		// Always clean up instances that were being created (incomplete)
		if member.Name == ig.InstanceNameCreating {
			ig.log.Info("Found stale creating instance, marking for removal", "name", member.Name, "vmid", member.VMID, "node", member.Node)
			instancesToMarkForRemoval = append(instancesToMarkForRemoval, &member)
			continue
		}

		// Optionally clean up running instances (orphaned after crash/restart)
		if ig.CleanupRunningOnInit && member.Name == ig.InstanceNameRunning {
			ig.log.Info("Found orphaned running instance, marking for removal", "name", member.Name, "vmid", member.VMID, "node", member.Node)
			instancesToMarkForRemoval = append(instancesToMarkForRemoval, &member)
			continue
		}
	}

	if len(instancesToMarkForRemoval) < 1 {
		return nil
	}

	err = ig.markInstancesForRemoval(ctx, instancesToMarkForRemoval...)
	if err != nil {
		return fmt.Errorf("failed to mark stale instances for removal: %w", err)
	}

	return nil
}

func (ig *InstanceGroup) markInstancesForRemoval(ctx context.Context, instances ...*proxmox.ClusterResource) error {
	var errorGroup errgroup.Group

	for _, instance := range instances {
		errorGroup.Go(func() error {
			log := ig.log.With("name", instance.Name, "vmid", instance.VMID, "node", instance.Node)

			vm, err := ig.getProxmoxVMOnNode(ctx, int(instance.VMID), instance.Node)
			if err != nil {
				log.Error("Failed to mark instance for removal", "err", err)
				return fmt.Errorf("failed to mark instance for removal: %w", err)
			}

			task, err := vm.Config(ctx, proxmox.VirtualMachineOption{
				Name:  "name",
				Value: ig.InstanceNameRemoving,
			})
			if err == nil {
				err = task.Wait(ctx, proxmoxTaskWaitInterval, proxmoxTaskWaitTimeout)
			}

			if err != nil {
				log.Error("Failed to mark instance for removal", "err", err)
				return fmt.Errorf("failed to mark instance for removal: %w", err)
			}

			return nil
		})
	}

	err := errorGroup.Wait()
	if err != nil {
		ig.instanceCollectionTrigger <- struct{}{}
		return fmt.Errorf("failed to mark one or more instances for removal: %w", err)
	}

	ig.instanceCollectionTrigger <- struct{}{}

	return nil
}

func (ig *InstanceGroup) isProxmoxResourceAnInstance(member proxmox.ClusterResource) bool {
	return member.VMID != uint64(*ig.TemplateID)
}

// findNextAvailableVMID finds the next available VMID within the configured range.
// It iterates from VMIDRangeLow to VMIDRangeHigh and returns the first available ID.
func (ig *InstanceGroup) findNextAvailableVMID(ctx context.Context) (int, error) {
	cluster, err := ig.proxmox.Cluster(ctx)
	if err != nil {
		return 0, fmt.Errorf("failed to get cluster: %w", err)
	}

	for vmid := *ig.VMIDRangeLow; vmid <= *ig.VMIDRangeHigh; vmid++ {
		available, err := cluster.CheckID(ctx, vmid)
		if err != nil {
			return 0, fmt.Errorf("failed to check VMID %d availability: %w", vmid, err)
		}

		if available {
			return vmid, nil
		}
	}

	return 0, fmt.Errorf("%w: range %d-%d is full", ErrNoAvailableVMID, *ig.VMIDRangeLow, *ig.VMIDRangeHigh)
}

// waitForAgent polls the QEMU agent until it responds or timeout is reached.
// It checks every proxmoxAgentCheckInterval (15s) up to proxmoxAgentMaxCheckAttempts (40 attempts = 10min).
func (ig *InstanceGroup) waitForAgent(ctx context.Context, vm *proxmox.VirtualMachine, vmid int) error {
	ticker := time.NewTicker(proxmoxAgentCheckInterval)
	defer ticker.Stop()

	deadline := time.Now().Add(proxmoxAgentStartTimeout)
	attempt := 0

	for {
		attempt++
		ig.log.Debug("checking if qemu agent is ready", "vmid", vmid, "attempt", attempt, "max_attempts", proxmoxAgentMaxCheckAttempts)

		// Try to get agent info - if it works, the agent is ready
		_, err := vm.AgentOsInfo(ctx)
		if err == nil {
			ig.log.Info("qemu agent is ready", "vmid", vmid, "attempts", attempt)
			return nil
		}

		ig.log.Debug("qemu agent not ready yet", "vmid", vmid, "attempt", attempt, "err", err)

		// Check if we've exceeded the deadline
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout after %d attempts waiting for qemu agent", attempt)
		}

		// Check if context was cancelled
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			// Continue to next attempt
		}
	}
}
