package manifest

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// LibvirtSpec is the [libvirt] table: a lab VM on the hypervisor's LAN
// bridge. The static address is baked into the image (the host's nixos
// config), so it lives here, not in the neutral top level.
//
//	[libvirt]
//	ip = "10.0.0.41"
//	# lan_prefix = 24, lan_gateway = "10.0.0.1", lan_dns = ["10.0.0.2", "8.8.8.8"]
//	# — scaffold seeds, the lab defaults when unset
type LibvirtSpec struct {
	IP         string   `toml:"ip"`
	LanPrefix  int      `toml:"lan_prefix"`
	LanGateway string   `toml:"lan_gateway"`
	LanDNS     []string `toml:"lan_dns"`
}

func (s *LibvirtSpec) Validate(host string, h Host) error {
	if s.IP == "" {
		return fmt.Errorf("ip is required (the static LAN address baked into the image)")
	}
	if a, err := netip.ParseAddr(s.IP); err != nil || !a.Is4() {
		return fmt.Errorf("ip %q: want an IPv4 address", s.IP)
	}
	if s.LanPrefix < 0 || s.LanPrefix > 32 {
		return fmt.Errorf("lan_prefix %d: want 0-32", s.LanPrefix)
	}
	return nil
}

// The lab defaults (were create-nixos.yaml's default_vm_spec).
const (
	DefaultVcpus  = 2
	DefaultMemory = 4
	DefaultDisk   = "40G"
)

// WithLabDefaults fills what the manifest left unset with the lab
// defaults — the shape libvirt realizes.
func (s Shape) WithLabDefaults() Shape {
	if s.Vcpus == 0 {
		s.Vcpus = DefaultVcpus
	}
	if s.Memory == 0 {
		s.Memory = DefaultMemory
	}
	if s.Disk == "" {
		s.Disk = DefaultDisk
	}
	return s
}

// ParseSize reads a [shape] disk ("80G", "512M") as bytes.
func ParseSize(s string) (int, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := 1
	switch {
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	default:
		return 0, fmt.Errorf("%q: want <n>G or <n>M", s)
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q: want <n>G or <n>M", s)
	}
	return n * mult, nil
}
