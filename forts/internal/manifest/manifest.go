// Package manifest reads a fort's manifest: provisioning/forts/<host>.toml,
// one file per fort, filename = hostname (no renames, as everywhere). One
// schema serves both halves of the tooling (the fort CLI and the pulumi
// program) so the generators cannot drift.
//
// The neutral top level is the contract every consumer reads — scaffold,
// mint, flux, the mesh: mesh_ip, [shape], [dns]. What a fort IS is not a
// key: every fort is the hardened host profile, flux-managed, a mesh
// member (#272) — a different shape will be a positive declaration.
// Exactly one provider table ([linode], [libvirt], [baremetal]) names the
// backend that raises the host; it is decoded by that provider's own Spec
// type and is opaque to everything else. Adding a provider = one file in
// this package (its Spec + Validate) and one entry in providers; the
// shared struct never grows a column. [baremetal] is the one table with
// no backend behind it: the machine exists, a human writes the image
// (#287), and the verbs that would apply refuse by name.
//
// The playbook-era inventory.yaml this replaces (#252) was one flat map
// serving three vocabularies; the Ansible nesting and field names
// (ansible_host, wg_vpn_ip) went with it.
package manifest

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Dir is the manifests' home, repo-relative.
const Dir = "provisioning/forts"

// Path is the host's manifest, repo-relative.
func Path(host string) string { return filepath.Join(Dir, host+".toml") }

// Host is one fort's manifest: the neutral intent plus the one provider
// table, decoded.
type Host struct {
	// MeshIP is the host's WireGuard mesh address; mint keys the wg key
	// and the peer entry off it. Mandatory (#99): a hardened host without
	// one has no login path at all.
	MeshIP string `toml:"mesh_ip"`
	// MeshEndpoint is where other peers dial this host (peers.json's
	// wg_endpoint_ip). Optional: see Endpoint for the defaults.
	MeshEndpoint string `toml:"mesh_endpoint"`
	// ClusterInit: k3s embedded etcd (true, the default) or sqlite (false).
	// A scaffold seed only — the host file is hand-maintained after.
	ClusterInit *bool `toml:"k3s_cluster_init"`
	// StateVersion seeds system.stateVersion at scaffold time.
	StateVersion string `toml:"nixos_state_version"`

	Shape   Shape   `toml:"shape"`
	DNS     DNS     `toml:"dns"`
	Backups Backups `toml:"backups"`

	// Provider is the one provider table present; Spec its decoded form.
	Provider string `toml:"-"`
	Spec     Spec   `toml:"-"`
}

// Shape is the VM's size. libvirt realizes it literally (the lab
// contract, with WithLabDefaults for what is unset); a cloud host's shape
// is its plan, and nothing reads [shape] there.
type Shape struct {
	Vcpus  int    `toml:"vcpus"`
	Memory int    `toml:"memory"` // GiB
	Disk   string `toml:"disk"`   // "80G"
}

// Backups is the host's PV-backup intent (docs/infra/backups.md). The
// first real second variant the manifest has met (#272 kept toggles out
// until one existed): every other standard component is per-fort
// identical, but where a fort's volumes land is not — a lab fort is
// reproducible and keeps one local copy, a production fort's offsite
// copy must not depend on the lab being reachable.
//
// `profile` names a DESTINATION SET, never individual targets: there are
// exactly two object stores at fleet level, and enumerating endpoints
// per fort is how they drift. scaffold renders `sfi.pv-backup` from it.
type Backups struct {
	// Profile is "lab", "production", or "none". Required, with no
	// default: a production fort silently inheriting lab-only backups is
	// the failure worth one line of typing, and it cannot be inferred
	// from the provider table (staging-k3s is a lab VM that may want
	// covering; a Linode drill fort should not pay for B2).
	//
	// "none" is a positive declaration, not an omission — luxor is the
	// hub of copies and an origin of nothing, so backing its volumes up
	// to its own rustfs would be circular.
	Profile string `toml:"profile"`
}

// Profiles are the accepted values of [backups] profile.
var Profiles = []string{"lab", "production", "none"}

// Backed reports whether this host's volumes are backed up at all.
func (b Backups) Backed() bool { return b.Profile != "" && b.Profile != "none" }

// DNS is the host's public name: <name>.<zone>, realized by the cloud
// backend (A + wildcard) and the mesh peer's default endpoint.
type DNS struct {
	Zone string `toml:"zone"`
	Name string `toml:"name"`
}

// FQDN is name.zone, or "" when the host has no public name.
func (d DNS) FQDN() string {
	if d.Zone == "" || d.Name == "" {
		return ""
	}
	return d.Name + "." + d.Zone
}

// Spec is a provider table, decoded by its backend's own type.
type Spec interface {
	// Validate checks the table against the host's neutral fields, so
	// `fort check --all` refuses a bad manifest without running pulumi.
	Validate(host string, h Host) error
}

// providers: table name → a fresh Spec to decode it into.
var providers = map[string]func() Spec{
	"linode":    func() Spec { return &LinodeSpec{} },
	"libvirt":   func() Spec { return &LibvirtSpec{} },
	"baremetal": func() Spec { return &BaremetalSpec{} },
}

// ProviderTables lists the accepted tables, for error text.
const ProviderTables = "[libvirt] (lab VM), [baremetal] (lab hardware) or [linode]"

// Cloud says whether a host is realized by a cloud backend — off the lab
// LAN. Lab facts (step-ca issuer, .lofi hostnames, the static LAN block)
// follow from !Cloud(); a physical machine on the LAN is a lab host too.
func (h Host) Cloud() bool { return h.Provider == "linode" }

// Realized says whether a backend in forts/pulumi raises and destroys
// this host. False for [baremetal]: the apply verbs refuse, preview
// skips, and the image is written by hand (docs/forts/baremetal.md).
func (h Host) Realized() bool { return h.Provider != "" && h.Provider != "baremetal" }

// Linode is the decoded [linode] table, nil for any other provider.
func (h Host) Linode() *LinodeSpec { s, _ := h.Spec.(*LinodeSpec); return s }

// Libvirt is the decoded [libvirt] table, nil for any other provider.
func (h Host) Libvirt() *LibvirtSpec { s, _ := h.Spec.(*LibvirtSpec); return s }

// Baremetal is the decoded [baremetal] table, nil for any other provider.
func (h Host) Baremetal() *BaremetalSpec { s, _ := h.Spec.(*BaremetalSpec); return s }

// LanIP is the static LAN address a lab host bakes into its image —
// [libvirt] ip or [baremetal] ip; "" for a cloud host.
func (h Host) LanIP() string {
	switch {
	case h.Libvirt() != nil:
		return h.Libvirt().IP
	case h.Baremetal() != nil:
		return h.Baremetal().IP
	}
	return ""
}

// Endpoint is where the other mesh peers dial this host: mesh_endpoint
// when set, else a lab host's static LAN address, else the public DNS
// name — "" for a host reachable only by its own dial-out.
func (h Host) Endpoint() string {
	switch {
	case h.MeshEndpoint != "":
		return h.MeshEndpoint
	case h.LanIP() != "":
		return h.LanIP()
	default:
		return h.DNS.FQDN()
	}
}

// Load reads and validates one host's manifest under root.
func Load(root, host string) (Host, error) {
	raw, err := os.ReadFile(filepath.Join(root, Path(host)))
	if err != nil {
		if os.IsNotExist(err) {
			return Host{}, fmt.Errorf("host %q has no manifest (%s)", host, Path(host))
		}
		return Host{}, err
	}
	return Parse(host, raw)
}

// LoadAll reads every manifest under root, keyed by hostname. A file
// that fails to parse or validate is reported in problems (keyed the
// same way) rather than aborting the walk, so `fort check --all` can
// list every bad manifest and the other verbs can refuse on any.
func LoadAll(root string) (hosts map[string]Host, problems map[string]error, err error) {
	entries, err := os.ReadDir(filepath.Join(root, Dir))
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", Dir, err)
	}
	hosts, problems = map[string]Host{}, map[string]error{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		host := strings.TrimSuffix(e.Name(), ".toml")
		h, err := Load(root, host)
		if err != nil {
			problems[host] = err
			continue
		}
		hosts[host] = h
	}
	return hosts, problems, nil
}

// Names lists the manifests under root, sorted — every host the repo
// declares, parsed or not.
func Names(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, Dir))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", Dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
			names = append(names, strings.TrimSuffix(e.Name(), ".toml"))
		}
	}
	sort.Strings(names)
	return names, nil
}

// Parse decodes manifest text for host and validates it. Strict: an
// unknown key anywhere is an error (a typo'd flag must not silently
// mean its default), and exactly one provider table must be present.
func Parse(host string, raw []byte) (Host, error) {
	var h Host
	md, err := toml.Decode(string(raw), &h)
	if err != nil {
		return Host{}, fmt.Errorf("%s: %w", Path(host), err)
	}
	var tables []string
	for _, k := range md.Undecoded() {
		if _, ok := providers[k[0]]; ok {
			if len(k) == 1 {
				tables = append(tables, k[0])
			}
			continue // the table's own keys are decoded below
		}
		return Host{}, fmt.Errorf("%s: unknown key %s", Path(host), k)
	}
	switch len(tables) {
	case 1:
	case 0:
		return Host{}, fmt.Errorf("%s: no provider table — add %s; nothing raises a provider-less host", Path(host), ProviderTables)
	default:
		sort.Strings(tables)
		return Host{}, fmt.Errorf("%s: %d provider tables (%s) — a fort has exactly one", Path(host), len(tables), strings.Join(tables, ", "))
	}
	h.Provider = tables[0]

	// The provider table, decoded by its own type; strict the same way.
	var prims map[string]toml.Primitive
	pmd, err := toml.Decode(string(raw), &prims)
	if err != nil {
		return Host{}, fmt.Errorf("%s: %w", Path(host), err)
	}
	spec := providers[h.Provider]()
	if err := pmd.PrimitiveDecode(prims[h.Provider], spec); err != nil {
		return Host{}, fmt.Errorf("%s: [%s]: %w", Path(host), h.Provider, err)
	}
	for _, k := range pmd.Undecoded() {
		if k[0] == h.Provider && len(k) > 1 {
			return Host{}, fmt.Errorf("%s: [%s]: unknown key %s", Path(host), h.Provider, strings.Join(k[1:], "."))
		}
	}
	h.Spec = spec

	if err := h.Validate(host); err != nil {
		return Host{}, err
	}
	return h, nil
}

// Validate checks the neutral fields and hands the provider table to
// its Spec. Load and Parse run it; it is exported for hosts built in
// memory.
func (h Host) Validate(host string) error {
	fail := func(format string, a ...any) error {
		return fmt.Errorf("%s: %s", Path(host), fmt.Sprintf(format, a...))
	}
	if h.Spec == nil || h.Provider == "" {
		return fail("no provider table — add %s; nothing raises a provider-less host", ProviderTables)
	}
	if h.MeshIP == "" {
		// cluster-vars.yaml's MESH_IP is mandatory (#99): every fort is a
		// mesh member — a hardened host without one has no login path.
		return fail("mesh_ip is required — every fort is a mesh member")
	}
	if a, err := netip.ParseAddr(h.MeshIP); err != nil || !a.Is4() {
		return fail("mesh_ip %q: want an IPv4 address", h.MeshIP)
	}
	if (h.DNS.Zone == "") != (h.DNS.Name == "") {
		return fail("[dns] needs both zone and name — an apex record must be asked for explicitly")
	}
	if h.Shape.Disk != "" {
		if _, err := ParseSize(h.Shape.Disk); err != nil {
			return fail("[shape] disk: %v", err)
		}
	}
	if h.Shape.Vcpus < 0 || h.Shape.Memory < 0 {
		return fail("[shape] vcpus/memory: want positive")
	}
	switch h.Backups.Profile {
	case "lab", "production", "none":
	case "":
		return fail("[backups] profile is required — one of %s. A fort that does not say silently gets nothing backed up; \"none\" says so out loud (docs/infra/backups.md)", strings.Join(Profiles, ", "))
	default:
		return fail("[backups] profile %q: want one of %s", h.Backups.Profile, strings.Join(Profiles, ", "))
	}
	if err := h.Spec.Validate(host, h); err != nil {
		return fail("[%s]: %v", h.Provider, err)
	}
	return nil
}
