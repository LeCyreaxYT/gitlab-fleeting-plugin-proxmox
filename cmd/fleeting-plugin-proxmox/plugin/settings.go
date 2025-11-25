package plugin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

var (
	ErrRequiredSettingMissing  = errors.New("required setting is missing")
	ErrSettingInvalidParameter = errors.New("setting has invalid parameter")
)

// Available network protocols to look for when discovering instance's IP address.
type NetworkProtocol = string

const (
	// Tries to find one internal and one external IPv4 address.
	NetworkProtocolIPv4 NetworkProtocol = "ipv4"

	// Tries to find one internal (ULA) and one global (GUA) IPv6 address.
	NetworkProtocolIPv6 NetworkProtocol = "ipv6"

	// Will prioritize IPv6 but return IPv4 if there is no IPv6.
	NetworkProtocolAny NetworkProtocol = "any"
)

// Default values for plugin settings.
const (
	DefaultInstanceNetworkInterface = "ens18"
	DefaultInstanceNetworkProtocol  = NetworkProtocolIPv4

	DefaultInstanceNameCreating = "fleeting-creating"
	DefaultInstanceNameIdle     = "fleeting-idle"
	DefaultInstanceNameRunning  = "fleeting-running"
	DefaultInstanceNameRemoving = "fleeting-removing"

	// Default prefix for matching instances during cleanup (used when %s placeholder is present).
	DefaultInstanceNamePrefix = "fleeting-"

	// Default VMID range low and high values.
	DefaultVMIDRangeLow  = 1000
	DefaultVMIDRangeHigh = 2000
)

// Plguin settings.
type Settings struct {
	// Proxmox VE URL.
	URL string `json:"url"`

	// If true then TLS certificate verification is disabled.
	InsecureSkipTLSVerify bool `json:"insecure_skip_tls_verify"`

	// Path to Proxmox VE credentials file.
	CredentialsFilePath string `json:"credentials_file_path"`

	// Name of the Proxmox VE pool to use.
	Pool string `json:"pool"`

	// Name of the Proxmox VE storage to use.
	Storage string `json:"storage"`

	// ID of the Proxmox VE VM to create instances from.
	TemplateID *int `json:"template_id,omitempty"`

	// Maximum instances than can be deployed.
	MaxInstances *int `json:"max_instances,omitempty"`

	// Network interface to read instance's IP address from.
	InstanceNetworkInterface string `json:"instance_network_interface"`

	// Network protocol to look for when discovering instance's IP address.
	InstanceNetworkProtocol NetworkProtocol `json:"instance_network_protocol"`

	// Name to set for instances during creation.
	InstanceNameCreating string `json:"instance_name_creating"`

	// Name to set for idle instances (cloned but not started).
	InstanceNameIdle string `json:"instance_name_idle"`

	// Name to set for running instances.
	InstanceNameRunning string `json:"instance_name_running"`

	// Name to set for instances during removal.
	InstanceNameRemoving string `json:"instance_name_removing"`

	// Prefix used to identify instances belonging to this plugin during cleanup.
	// Automatically derived from instance names if %s placeholder is used.
	// Can be manually set for custom cleanup behavior.
	InstanceNamePrefix string `json:"instance_name_prefix"`

	// Additional: vmid low to high range for instance VMs.
	VMIDRangeLow  *int `json:"vmid_range_low,omitempty"`
	VMIDRangeHigh *int `json:"vmid_range_high,omitempty"`

	// If true, running instances will be marked for removal on plugin init.
	// This is useful to clean up orphaned VMs after a runner crash/restart.
	// When using %s placeholder, this will match ALL instances with the same prefix.
	CleanupRunningOnInit bool `json:"cleanup_running_on_init"`

	// If true, instances will be cloned but not started until needed.
	// This reduces resource usage for idle instances.
	LazyStartInstances bool `json:"lazy_start_instances"`
}

func (s *Settings) FillWithDefaults() {
	if s.InstanceNetworkInterface == "" {
		s.InstanceNetworkInterface = DefaultInstanceNetworkInterface
	}

	if s.InstanceNetworkProtocol == "" {
		s.InstanceNetworkProtocol = DefaultInstanceNetworkProtocol
	}

	if s.InstanceNameCreating == "" {
		s.InstanceNameCreating = DefaultInstanceNameCreating
	}

	if s.InstanceNameIdle == "" {
		s.InstanceNameIdle = DefaultInstanceNameIdle
	}

	if s.InstanceNameRunning == "" {
		s.InstanceNameRunning = DefaultInstanceNameRunning
	}

	if s.InstanceNameRemoving == "" {
		s.InstanceNameRemoving = DefaultInstanceNameRemoving
	}

	if s.InstanceNetworkProtocol == "" {
		s.InstanceNetworkProtocol = DefaultInstanceNetworkProtocol
	}

	if s.VMIDRangeLow == nil {
		s.VMIDRangeLow = new(int)
		*s.VMIDRangeLow = DefaultVMIDRangeLow
	}

	if s.VMIDRangeHigh == nil {
		s.VMIDRangeHigh = new(int)
		*s.VMIDRangeHigh = DefaultVMIDRangeHigh
	}

	// Replace %s placeholder in instance names with a random identifier
	s.applyInstanceNameIdentifier()
}

// applyInstanceNameIdentifier replaces %s in instance names with a random 6-character identifier.
// This allows multiple runners to use the same pool without name conflicts.
// It also derives the InstanceNamePrefix from the name pattern for cleanup matching.
func (s *Settings) applyInstanceNameIdentifier() {
	// Derive prefix from the pattern before %s (for cleanup matching)
	// e.g., "fleeting-%s-running" -> prefix = "fleeting-"
	if s.InstanceNamePrefix == "" {
		s.InstanceNamePrefix = deriveInstanceNamePrefix(s.InstanceNameCreating)
	}

	identifier := generateRandomIdentifier(6)

	s.InstanceNameCreating = strings.ReplaceAll(s.InstanceNameCreating, "%s", identifier)
	s.InstanceNameIdle = strings.ReplaceAll(s.InstanceNameIdle, "%s", identifier)
	s.InstanceNameRunning = strings.ReplaceAll(s.InstanceNameRunning, "%s", identifier)
	s.InstanceNameRemoving = strings.ReplaceAll(s.InstanceNameRemoving, "%s", identifier)
}

// deriveInstanceNamePrefix extracts the prefix before %s placeholder.
// e.g., "fleeting-%s-running" -> "fleeting-"
// If no %s is found, returns the default prefix.
func deriveInstanceNamePrefix(namePattern string) string {
	if idx := strings.Index(namePattern, "%s"); idx > 0 {
		return namePattern[:idx]
	}
	return DefaultInstanceNamePrefix
}

// generateRandomIdentifier generates a random hex string of the specified length.
func generateRandomIdentifier(length int) string {
	bytes := make([]byte, (length+1)/2)
	_, err := rand.Read(bytes)
	if err != nil {
		// Fallback to a fixed string if random generation fails
		return "000000"[:length]
	}
	return hex.EncodeToString(bytes)[:length]
}

func (s *Settings) CheckRequiredFields() error {
	if s.URL == "" {
		return fmt.Errorf("%w: url", ErrRequiredSettingMissing)
	}

	if s.CredentialsFilePath == "" {
		return fmt.Errorf("%w: credentials_file_path", ErrRequiredSettingMissing)
	}

	if s.Pool == "" {
		return fmt.Errorf("%w: pool", ErrRequiredSettingMissing)
	}

	if s.TemplateID == nil {
		return fmt.Errorf("%w: template_id", ErrRequiredSettingMissing)
	}

	if s.MaxInstances == nil {
		return fmt.Errorf("%w: max_instances", ErrRequiredSettingMissing)
	}

	if s.InstanceNetworkProtocol != "" && s.InstanceNetworkProtocol != NetworkProtocolIPv4 && s.InstanceNetworkProtocol != NetworkProtocolIPv6 && s.InstanceNetworkProtocol != NetworkProtocolAny {
		return fmt.Errorf("%w: instance_network_protocol: must be ipv4, ipv6 or any", ErrSettingInvalidParameter)
	}

	return nil
}
