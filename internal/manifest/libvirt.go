package manifest

import (
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// LibvirtSpec is the [libvirt] table: a lab VM on a hypervisor's LAN
// bridge. The static address is baked into the image (the host's nixos
// config), so it lives here, not in the neutral top level. The
// hypervisor lives here too (forts#27): which machine raises this fort
// is the estate's fact, as much as a cloud host's region is.
//
//	[libvirt]
//	uri = "qemu+ssh://soundship/system"
//	ip = "10.0.0.41"
//	# pool = "images" — the storage pool, the default when unset
//	# lan_prefix = 24, lan_gateway = "10.0.0.1", lan_dns = ["10.0.0.2", "8.8.8.8"]
//	# — scaffold seeds, the lab defaults when unset
type LibvirtSpec struct {
	// URI is the hypervisor this fort lives on, as the libvirt provider
	// dials it (forts#27). The CLI passes it to the provider as it
	// stands and adds nothing to it: the manifest is one file that every
	// operator machine reads, so it records the hypervisor's NAME and no
	// credential — a key path is one machine's (/Users against /home,
	// sfi/estate-hub), and `~/.ssh/config` on each machine supplies the
	// user, the address and the identity. This is the same split as the
	// estate's operator_keys. The escape hatch is
	// LIBVIRT_DEFAULT_URI, which wins over this value on the machine
	// that sets it. Optional: a fort that names none needs that variable
	// in the environment of every apply, and `fort check` says so.
	URI string `toml:"uri"`
	IP  string `toml:"ip"`
	// Pool is the libvirt storage pool that holds this host's volumes —
	// the root qcow2 and the seed ISO (cmd/fort-pulumi/libvirt.go).
	// Optional: DefaultLibvirtPool when unset.
	Pool       string   `toml:"pool"`
	LanPrefix  int      `toml:"lan_prefix"`
	LanGateway string   `toml:"lan_gateway"`
	LanDNS     []string `toml:"lan_dns"`
}

// DefaultLibvirtPool is the storage pool a [libvirt] table that names
// none means — the pool every lab hypervisor of the fleet carries.
const DefaultLibvirtPool = "images"

// PoolName is [libvirt] pool, the default when the table names none.
func (s *LibvirtSpec) PoolName() string {
	if s.Pool == "" {
		return DefaultLibvirtPool
	}
	return s.Pool
}

func (s *LibvirtSpec) Validate(host string, h Host) error {
	if s.URI != "" {
		if err := ValidateLibvirtURI(s.URI); err != nil {
			return err
		}
	}
	if s.Pool != "" && !poolRE.MatchString(s.Pool) {
		return fmt.Errorf("pool %q: want a libvirt pool name (letters, digits, dot, dash, underscore)", s.Pool)
	}
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

// poolRE is a libvirt storage pool name as the volumes name it.
var poolRE = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// libvirtSchemeRE is a libvirt URI's scheme: the driver, and the
// transport after a plus (qemu, qemu+ssh).
var libvirtSchemeRE = regexp.MustCompile(`^[a-z0-9]+(\+[a-z0-9]+)?$`)

// remoteTransports dial another machine, so a URI that names one must
// name a host. The rest are local (a unix socket, the driver's default).
var remoteTransports = []string{"ssh", "libssh", "libssh2", "tcp", "tls"}

// ValidateLibvirtURI checks the shape of a libvirt connection URI —
// driver[+transport]://[user@]host[:port]/path[?params] — so `fort
// check` refuses a typo before an apply verb dials it (forts#27). It
// judges the shape alone: whether the hypervisor answers is the apply's
// answer, and a URI that the environment supplies is never read here.
func ValidateLibvirtURI(uri string) error {
	u, err := url.Parse(uri)
	if err != nil {
		return fmt.Errorf("uri %q: %w", uri, err)
	}
	if !libvirtSchemeRE.MatchString(u.Scheme) {
		return fmt.Errorf("uri %q: want the scheme <driver>[+<transport>] (qemu+ssh, qemu), got %q", uri, u.Scheme)
	}
	if strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("uri %q: no connection path — libvirt takes the driver's path (/system for the system instance)", uri)
	}
	_, transport, _ := strings.Cut(u.Scheme, "+")
	if slices.Contains(remoteTransports, transport) && u.Hostname() == "" {
		return fmt.Errorf("uri %q: the %s transport dials a hypervisor, and this URI names no host", uri, transport)
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
