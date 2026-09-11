// Package inventory reads provisioning/inventory.yaml — the same file
// create-nixos consumes. One schema serves both halves of the tooling
// (the fort CLI and the pulumi program) so the generators cannot drift.
package inventory

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Host is one entry under all.children.nixos.hosts. The cloud fields
// (#87 schema) are additive: hosts without `provider` are libvirt lab
// VMs and out of scope for cloud stacks.
type Host struct {
	// Node-layer fields (create-nixos schema).
	AnsibleHost  string   `yaml:"ansible_host"`
	Vcpus        int      `yaml:"vcpus"`
	Memory       int      `yaml:"memory"`
	DiskSize     string   `yaml:"disk_size"`
	WgVpnIP      string   `yaml:"wg_vpn_ip"`
	Production   bool     `yaml:"production"`
	FluxCluster  bool     `yaml:"flux_cluster"`
	LanPrefix    *int     `yaml:"lan_prefix"`
	LanGateway   string   `yaml:"lan_gateway"`
	LanDNS       []string `yaml:"lan_dns"`
	StateVersion string   `yaml:"nixos_state_version"`
	ClusterInit  *bool    `yaml:"k3s_cluster_init"`

	// #87 cloud schema: provider selects the Backend; the rest is intent
	// the backend realizes. Empty provider = libvirt (soundship lab).
	Provider     string `yaml:"provider"`
	Region       string `yaml:"region"`
	InstanceType string `yaml:"instance_type"`
	DNSZone      string `yaml:"dns_zone"`
	DNSName      string `yaml:"dns_name"`
}

type inventoryFile struct {
	All struct {
		Children struct {
			Nixos struct {
				Hosts map[string]Host `yaml:"hosts"`
			} `yaml:"nixos"`
		} `yaml:"children"`
	} `yaml:"all"`
}

// Load returns all nixos hosts keyed by hostname.
func Load(path string) (map[string]Host, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading inventory: %w", err)
	}
	var inv inventoryFile
	if err := yaml.Unmarshal(raw, &inv); err != nil {
		return nil, fmt.Errorf("parsing inventory: %w", err)
	}
	return inv.All.Children.Nixos.Hosts, nil
}

// LoadHost returns the inventory entry matching the given name. For the
// pulumi program, stack == hostname is the identity contract: an
// inventory edit alone never touches state, and destruction is only ever
// `pulumi destroy -s <host>`.
func LoadHost(path, name string) (Host, error) {
	hosts, err := Load(path)
	if err != nil {
		return Host{}, err
	}
	h, ok := hosts[name]
	if !ok {
		return Host{}, fmt.Errorf("host %q not in inventory (name must match an all.children.nixos.hosts entry)", name)
	}
	return h, nil
}
