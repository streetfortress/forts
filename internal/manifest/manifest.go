// Package manifest reads a fort's manifest: forts/<host>.toml,
// one file per fort, filename = hostname (no renames, as everywhere). One
// schema serves both halves of the tooling (the fort CLI and the pulumi
// program) so the generators cannot drift.
//
// The neutral top level is the contract every consumer reads — scaffold,
// mint, flux, the mesh: mesh_ip, [shape], [dns]. Every fort is the
// hardened host profile and a mesh member (#272); what a fort IS beyond
// that is the [cluster] table (#426) — a flux cluster: k3s, flux, the
// fleet tree — or nothing: a PLAIN fort, the baseline the framework gives
// every host (the hardened profile, the mesh, deployer with the ops verbs,
// the shell, the rebuild loop) and no more. What a plain fort does is
// plain NixOS in its host file, estate work; the manifest names no user
// and no role. A cluster-only key has no neutral level to sit at
// (k3s_cluster_init did, and the loader special-cased it; datastore lives
// in [cluster] now).
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
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Dir is the manifests' home, repo-relative.
const Dir = "forts"

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
	// StateVersion seeds system.stateVersion at scaffold time.
	StateVersion string `toml:"nixos_state_version"`

	Shape Shape `toml:"shape"`
	DNS   DNS   `toml:"dns"`
	// Cluster is the [cluster] table: this fort is a flux cluster (#426)
	// — k3s on the datastore it names, flux, flux/clusters + flux/apps,
	// the cluster age key — and what its k3s does beyond the
	// per-fort-identical standard (#626). Absent for a plain fort; the
	// table can be empty (every key has a default).
	Cluster *ClusterSpec `toml:"cluster"`
	// Links is the [links.<name>] tables (links.go, forts#55): the
	// other estates this host is on the mesh of, by the link's name.
	// Absent on most forts; a hub carries one per estate it serves.
	Links map[string]Link `toml:"links"`

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

// ClusterSpec is the [cluster] table: the shape of a flux cluster
// (#426), and the one place a key that only a cluster has can live.
//
//	[cluster]
//	datastore = "sqlite"    # sqlite (single node, the default) | etcd (embedded, cluster-init)
//	audit_logging = true    # the API server writes an audit log; a log shipper ships it
//
// datastore is a positive word, not a negated boolean: the reader asks
// "which datastore", not "was cluster-init set". scaffold renders
// `sfi.k3s.datastore = "<value>"` from it, and modules/k3s-server.nix
// maps that to services.k3s.clusterInit (etcd = true).
//
// audit_logging (#626) is a toggle the manifest carries because every
// cluster fort
// audits the same WAY, and not every fort pays the same disk for it. A
// 4 GB Linode SKU with a 25 GB disk and the lab hub with 450 GB do not
// carry the same log.
type ClusterSpec struct {
	// Datastore is k3s's datastore: "sqlite" (a single node, the
	// default — every fort today) or "etcd" (embedded, cluster-init;
	// the opt-in for an HA join, and a third of a 4 GB node, #163/#164).
	// sqlite→etcd migrates in place; the reverse is a re-provision.
	Datastore string `toml:"datastore"`
	// AuditLogging: the API server writes an audit log under
	// /var/log/kube-audit (modules/k3s-audit.nix), and the fleet's
	// a log shipper (a monitoring layer's) tails it. Default false.
	// scaffold renders `sfi.k3s.auditLog.enable = true` from it; on a
	// host that already exists the block is a hand edit, and a test in
	// cmd/fort keeps the manifest and the host file equal.
	AuditLogging bool `toml:"audit_logging"`
}

// Datastores are the accepted values of [cluster] datastore.
var Datastores = []string{"sqlite", "etcd"}

// DefaultDatastore is what a [cluster] table that names none means.
const DefaultDatastore = "sqlite"

// Validate: a typo is not a default.
func (s *ClusterSpec) Validate() error {
	if s.Datastore != "" && !slices.Contains(Datastores, s.Datastore) {
		return fmt.Errorf("[cluster] datastore %q: want one of %s", s.Datastore, strings.Join(Datastores, ", "))
	}
	return nil
}

// Datastore is the cluster's k3s datastore, the default when the table
// names none; "" for a plain fort, which has no k3s.
func (h Host) Datastore() string {
	switch {
	case h.Cluster == nil:
		return ""
	case h.Cluster.Datastore == "":
		return DefaultDatastore
	}
	return h.Cluster.Datastore
}

// AuditLogging is [cluster] audit_logging, false when unset.
func (h Host) AuditLogging() bool { return h.Cluster != nil && h.Cluster.AuditLogging }

// IsCluster says whether this fort is a flux cluster ([cluster]):
// flux/clusters + flux/apps trees, the cluster age key, the canary,
// k3s. A fort that is not a cluster is a plain fort: the host dir is its
// whole tree.
func (h Host) IsCluster() bool { return h.Cluster != nil }

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
// LAN. Lab facts (a private issuer, hostnames under the estate's mesh zone,
// the static LAN block) follow from !Cloud(); a physical machine on the
// LAN is a lab host too.
func (h Host) Cloud() bool { return h.Provider == "linode" }

// Realized says whether a backend in provisioning/pulumi raises and destroys
// this host. False for [baremetal]: the apply verbs refuse, preview
// skips, and the image is written by hand (docs/baremetal.md).
func (h Host) Realized() bool { return h.Provider != "" && h.Provider != "baremetal" }

// Linode is the decoded [linode] table, nil for any other provider.
func (h Host) Linode() *LinodeSpec { s, _ := h.Spec.(*LinodeSpec); return s }

// Libvirt is the decoded [libvirt] table, nil for any other provider.
func (h Host) Libvirt() *LibvirtSpec { s, _ := h.Spec.(*LibvirtSpec); return s }

// LibvirtURI is the hypervisor a [libvirt] fort lives on ([libvirt]
// uri, forts#27) — "" for any other provider, and for a table that
// names none. The apply verbs hand it to the libvirt provider;
// LIBVIRT_DEFAULT_URI in the environment wins over it (cmd/fort/env.go).
func (h Host) LibvirtURI() string {
	if s := h.Libvirt(); s != nil {
		return s.URI
	}
	return ""
}

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

// hostRE is a host name as every consumer of it needs it: one DNS label
// (mesh-dns renders <host>.<estate>.<mesh>; peers.json keys on it; the
// remotes table of another estate is keyed by it after a fetch, #100).
var hostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidHostName says whether name is a host name: lowercase letters,
// digits and hyphens, at most 63 characters, no hyphen at either end.
func ValidHostName(name string) bool { return hostRE.MatchString(name) }

// endpointRE is a DNS name an Endpoint can be: labels of letters,
// digits and hyphens, dotted.
var endpointRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)

// ValidEndpoint says whether s is one IP address or one DNS name — what
// an `Endpoint =` line of a WireGuard config takes, and the only shape
// a mesh_endpoint may have, since that line is rendered from it with no
// escaping and a stray newline would open a new section (#99).
func ValidEndpoint(s string) bool {
	if _, err := netip.ParseAddr(s); err == nil {
		return true
	}
	return len(s) <= 253 && endpointRE.MatchString(s)
}

// Load reads and validates one host's manifest under root.
func Load(root, host string) (Host, error) {
	if !ValidHostName(host) {
		return Host{}, fmt.Errorf("%q is not a host name — want one DNS label: lowercase letters, digits and hyphens", host)
	}
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
	// [backups] was the core's until forts#143: it is the backups layer's
	// table now, in layers/backups/forts/<host>.toml (sfi/fort-layers).
	// Named here so the refusal says where it went, not "unknown key".
	if md.IsDefined("backups") {
		return Host{}, fmt.Errorf("%s: [backups] moved to layers/backups/forts/%s.toml — the backups layer's table (sfi/fort-layers, docs/layers.md)", Path(host), host)
	}
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
	for name, l := range h.Links {
		l.Name = name
		h.Links[name] = l
	}

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
	if h.MeshEndpoint != "" && !ValidEndpoint(h.MeshEndpoint) {
		return fail("mesh_endpoint %q: want one IP address or one DNS name — it is the Endpoint line of every peer's WireGuard config", h.MeshEndpoint)
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
	if err := h.ValidateLinks(); err != nil {
		return fail("%v", err)
	}
	// The shape (#426): [cluster] makes a flux cluster, and its keys are
	// strict like a provider table's. No [cluster] table makes a plain
	// fort — the baseline host and nothing else the framework names.
	//
	// The two tables are independent (forts#142): [cluster] decides k3s
	// and flux, the provider table decides the runtime module and the
	// backend that raises the host. A plain fort goes where a cluster
	// fort goes — a lab VM, a linode, a machine on the LAN.
	if h.IsCluster() {
		if err := h.Cluster.Validate(); err != nil {
			return fail("%v", err)
		}
	}
	if err := h.Spec.Validate(host, h); err != nil {
		return fail("[%s]: %v", h.Provider, err)
	}
	return nil
}
