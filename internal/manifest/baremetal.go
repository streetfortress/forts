package manifest

import (
	"fmt"
	"net"
	"net/netip"
	"strings"
)

// BaremetalSpec is the [baremetal] table: a physical machine on the lab
// LAN (#287). Like [libvirt] the static address is baked into the image;
// unlike it there is no provider to raise the host — the image is written
// to the machine's disk by a human at the keyboard (docs/baremetal.md),
// and nothing in provisioning/pulumi realizes this table. It exists so the fort
// verbs (scaffold, mint, seed, image, check) see a fort like any other.
//
//	[baremetal]
//	ip = "10.0.0.6"
//	mac = "f8:ce:72:02:05:07"   # the NIC the address goes on — a laptop on a
//	                            # dock is the "several en*" case
//	disk = "/dev/nvme0n1"       # the writer's target; informational
//	# lan_prefix = 24, lan_gateway = "10.0.0.1", lan_dns = ["10.0.0.2", "8.8.8.8"]
//	# — scaffold seeds, the lab defaults when unset
//
// mac is optional: unset, the host matches `en*` as a lab VM does, which
// is right for a box with one wired NIC. Set it where the name is not a
// fact — a USB/Thunderbolt-attached NIC is named for the port it sits on
// (enp116s0f4u1u1) and renamed when the dock moves, and a second adapter
// would take the address too.
type BaremetalSpec struct {
	IP         string   `toml:"ip"`
	MAC        string   `toml:"mac"`
	Disk       string   `toml:"disk"`
	LanPrefix  int      `toml:"lan_prefix"`
	LanGateway string   `toml:"lan_gateway"`
	LanDNS     []string `toml:"lan_dns"`
}

func (s *BaremetalSpec) Validate(host string, h Host) error {
	if s.IP == "" {
		return fmt.Errorf("ip is required (the static LAN address baked into the image)")
	}
	if a, err := netip.ParseAddr(s.IP); err != nil || !a.Is4() {
		return fmt.Errorf("ip %q: want an IPv4 address", s.IP)
	}
	if s.MAC != "" {
		hw, err := net.ParseMAC(s.MAC)
		if err != nil || len(hw) != 6 {
			return fmt.Errorf("mac %q: want aa:bb:cc:dd:ee:ff", s.MAC)
		}
		if s.MAC != strings.ToLower(s.MAC) {
			// networkd matches case-insensitively, but the rendered file
			// is diffed against the manifest by humans: one spelling.
			return fmt.Errorf("mac %q: lowercase, as `ip link` prints it", s.MAC)
		}
	}
	if s.Disk != "" && !strings.HasPrefix(s.Disk, "/dev/") {
		return fmt.Errorf("disk %q: want a device path (/dev/nvme0n1, /dev/disk/by-id/…)", s.Disk)
	}
	if s.LanPrefix < 0 || s.LanPrefix > 32 {
		return fmt.Errorf("lan_prefix %d: want 0-32", s.LanPrefix)
	}
	if h.Shape != (Shape{}) {
		return fmt.Errorf("[shape] is meaningless here — the machine is the shape")
	}
	return nil
}
