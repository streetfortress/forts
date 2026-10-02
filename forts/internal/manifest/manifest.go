// Package manifest reads a fort's manifest: provisioning/forts/<host>.toml,
// one file per fort, filename = hostname (no renames, as everywhere). One
// schema serves both halves of the tooling (the fort CLI and the pulumi
// program) so the generators cannot drift.
//
// The neutral top level is the contract every consumer reads — scaffold,
// mint, flux, the mesh: mesh_ip, [shape], [dns]. What a fort IS is not a
// key: every fort is the hardened host profile, a mesh member (#272), and
// — unless it says otherwise — a flux cluster. The one thing it can say
// is [dev] (#413): a transient lab VM hosting the agent accounts (#532),
// no cluster at all. A positive declaration, never the absence of flux.
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
	// Cluster is the [cluster] table when present: what this fort's
	// k3s does beyond the per-fort-identical standard (#626). Absent on
	// a fort that opts into nothing, so a fort inherits nothing by
	// silence; refused with [dev], which has no cluster.
	Cluster *ClusterSpec `toml:"cluster"`
	// Dev is the [dev] table when present: this fort is a dev fort
	// (#413). It names the fort's accounts (#532).
	Dev *DevSpec `toml:"dev"`

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

// ClusterSpec is the [cluster] table (#626): the second toggle the
// manifest carries, for the same reason as the first ([backups]
// profile): every cluster fort audits the same WAY, and not every fort
// pays the same disk for it. A 4 GB Linode SKU with a 25 GB disk and
// the lab hub with 450 GB do not carry the same log.
//
//	[cluster]
//	audit_logging = true    # the API server writes an audit log; fluent-bit ships it to Loki
//
// k3s_cluster_init stays a top-level key for now; a later change can
// move it in here.
type ClusterSpec struct {
	// AuditLogging: the API server writes an audit log under
	// /var/log/kube-audit (modules/k3s-audit.nix), and the fleet's
	// fluent-bit tails it into Loki as job="kube-audit". Default false.
	// scaffold renders `sfi.k3s.auditLog.enable = true` from it; on a
	// host that already exists the block is a hand edit, and a test in
	// cmd/fort keeps the manifest and the host file equal.
	AuditLogging bool `toml:"audit_logging"`
}

// AuditLogging is [cluster] audit_logging, false when the table is absent.
func (h Host) AuditLogging() bool { return h.Cluster != nil && h.Cluster.AuditLogging }

// DevSpec is the [dev] table (#413): a dev fort — a transient lab VM
// with unprivileged agent accounts, the dev toolchains and the
// dotfiles, and NO cluster: no k3s, no flux, no fleet tree, no cluster
// age key, no backups. scaffold renders the dev host shape, mint skips
// the cluster identity, check expects no fleet dirs.
//
// The table names the fort's accounts (#532). One Unix account per role
// holds one gitea identity: the kernel is the boundary, so a review
// session cannot read the dispatch account's token, and the verb a
// caller dials selects the author. Every name lives here and nowhere in
// the Go code, because the mint is the half that leaves this repo later
// (a CLI with a fort's names compiled in cannot serve a second fleet).
//
//	[dev]
//
//	[[dev.accounts]]
//	user = "agent"
//	verb = "pickup"
//	gitea_user = "sfi-agent"
//	email = "support@streetfortress.com"
//	token_scopes = ["write:repository", "write:issue", "read:organization", "read:user", "write:misc"]
//
//	[[dev.accounts]]
//	user = "scout"
//	verb = "scout"
//	gitea_user = "sfi-scout"
//	email = "scout@streetfortress.com"
//	ssh_alias = "sfi-scout"
//	token_scopes = ["write:repository", "write:issue", "read:organization", "read:user", "write:misc"]
type DevSpec struct {
	Accounts []DevAccount `toml:"accounts"`
}

// DevAccount is one [[dev.accounts]] entry: a Unix account on the fort,
// the gitea user it acts as, and the one verb it serves.
//
// The same word runs through every layer — the Unix account `scout`, the
// gitea user `sfi-scout`, the ssh alias `sfi-scout`, the verb `scout` —
// so a reader of a comment on gitea knows which session wrote it.
type DevAccount struct {
	// User is the Unix account. It names the account's secrets in
	// nixos/secrets/<host>.yaml — <user>-ssh-key, <user>-gpg-key,
	// <user>-gitea-token — and its home on the fort.
	User string `toml:"user"`
	// Verb is the one dispatch verb this account installs (`pickup`,
	// `scout`, `review`). An account serves one verb and no other: `ssh
	// sfi-scout pickup …` must fail as a missing command, so the
	// identity is a property of the host and not a rule the caller
	// remembers. nixos/home/agent.nix realizes the set.
	Verb string `toml:"verb"`
	// GiteaUser is the gitea user the bundle registers on. A human
	// creates it and adds it to the sfi-agentic-dev team; mint refuses
	// to invent one.
	GiteaUser string `toml:"gitea_user"`
	// Email is the address on that gitea user. The GPG uid carries it,
	// and gitea marks a uid active only for an address the user holds —
	// so a gitea user per role needs an address per role (gitea keeps
	// every address unique across users).
	Email string `toml:"email"`
	// SSHAlias is the mesh name the verb is dialed by (`ssh sfi-scout
	// review …`). The FIRST account answers on the fort's own name and
	// on its role alias (`sfi-dev`), so it names none; every other
	// account must name one, or nothing can reach it.
	SSHAlias string `toml:"ssh_alias"`
	// TokenScopes are the gitea scopes of this account's token. Per
	// account, because the narrow token is the point: an analyst that
	// only comments holds no write:repository, and Claude runs with
	// permissions bypassed on a fort — a token that cannot push is a
	// stronger boundary than a rule in a prompt.
	TokenScopes []string `toml:"token_scopes"`
}

// DevVerbs are the dispatch verbs an account can serve. The set is
// realized in nixos/home/agent.nix (one writeShellApplication per verb)
// and in the gotea verbs that dial them; a typo here is an account that
// installs nothing, so the manifest refuses one.
var DevVerbs = []string{"pickup", "scout", "review"}

// devNameRE is a Unix account name and an ssh alias: both are typed by
// hand and both must survive a shell word with no quoting.
var devNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Validate: every field is required, because a dev fort with a
// half-named account mints a bundle nobody can use.
func (s *DevSpec) Validate() error {
	if len(s.Accounts) == 0 {
		return fmt.Errorf("[dev] names no accounts — add one [[dev.accounts]] per role (user, verb, gitea_user, email, token_scopes); the accounts are the fort's identities (#532)")
	}
	seen := map[string]map[string]bool{"user": {}, "verb": {}, "gitea_user": {}, "ssh_alias": {}}
	unique := func(field, value string) error {
		if seen[field][value] {
			return fmt.Errorf("[[dev.accounts]] %s %q twice — one account per %s", field, value, field)
		}
		seen[field][value] = true
		return nil
	}
	for i, a := range s.Accounts {
		where := fmt.Sprintf("[[dev.accounts]] %d", i+1)
		if !devNameRE.MatchString(a.User) {
			return fmt.Errorf("%s: user %q — want a Unix account name (%s)", where, a.User, devNameRE)
		}
		if a.GiteaUser == "" {
			return fmt.Errorf("%s (%s): gitea_user is required — a human creates the user and adds it to sfi-agentic-dev; mint never invents one", where, a.User)
		}
		if !strings.Contains(a.Email, "@") {
			return fmt.Errorf("%s (%s): email %q — want the address on gitea user %s (the GPG uid carries it)", where, a.User, a.Email, a.GiteaUser)
		}
		if !slices.Contains(DevVerbs, a.Verb) {
			return fmt.Errorf("%s (%s): verb %q — want one of %s", where, a.User, a.Verb, strings.Join(DevVerbs, ", "))
		}
		if len(a.TokenScopes) == 0 {
			return fmt.Errorf("%s (%s): token_scopes is required — the scopes are the account's boundary, so no account inherits another's", where, a.User)
		}
		switch {
		case i == 0 && a.SSHAlias != "":
			return fmt.Errorf("%s (%s): the first account answers on the fort's name and its role alias, so it must not name ssh_alias %q", where, a.User, a.SSHAlias)
		case i > 0 && !devNameRE.MatchString(a.SSHAlias):
			return fmt.Errorf("%s (%s): ssh_alias %q — want the mesh name that dials this account (%s); without one nothing reaches it", where, a.User, a.SSHAlias, devNameRE)
		}
		// In field order, so the message a duplicate produces is the same
		// on every run (a map's order is not).
		dup := [][2]string{{"user", a.User}, {"verb", a.Verb}, {"gitea_user", a.GiteaUser}}
		if a.SSHAlias != "" {
			dup = append(dup, [2]string{"ssh_alias", a.SSHAlias})
		}
		for _, d := range dup {
			if err := unique(d[0], d[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

// Accounts is the fort's dev accounts, in manifest order; nil for a
// cluster fort.
func (h Host) Accounts() []DevAccount {
	if h.Dev == nil {
		return nil
	}
	return h.Dev.Accounts
}

// DefaultAccount is the account the fort's own name answers as — the
// first one the manifest lists. nil for a cluster fort.
func (h Host) DefaultAccount() *DevAccount {
	if as := h.Accounts(); len(as) > 0 {
		return &as[0]
	}
	return nil
}

// IsDev says whether this is a dev fort — the one shape that is not a
// flux cluster.
func (h Host) IsDev() bool { return h.Dev != nil }

// IsCluster says whether this fort is a flux cluster: fleet/clusters +
// fleet/apps trees, the cluster age key, the canary, k3s. Every fort
// but a dev one.
func (h Host) IsCluster() bool { return !h.IsDev() }

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
	if h.IsDev() {
		// A dev fort has nothing to back up and nothing to cluster; the
		// manifest must not pretend otherwise. Lab VM only for now: the
		// shape is a libvirt domain on the hypervisor (forts#12), and a
		// cloud or hardware dev fort would be its own declaration.
		if h.Backups.Profile != "none" {
			return fail("[dev] requires [backups] profile = \"none\" — a dev fort is transient and owns nothing worth a copy")
		}
		if h.ClusterInit != nil {
			return fail("[dev] and k3s_cluster_init together — a dev fort has no cluster")
		}
		if h.Cluster != nil {
			return fail("[dev] and [cluster] together — a dev fort has no cluster")
		}
		if h.Libvirt() == nil {
			return fail("[dev] needs the [libvirt] table — a dev fort is a lab VM on the hypervisor (forts#12)")
		}
		if err := h.Dev.Validate(); err != nil {
			return fail("%v", err)
		}
	}
	if err := h.Spec.Validate(host, h); err != nil {
		return fail("[%s]: %v", h.Provider, err)
	}
	return nil
}
