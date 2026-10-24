// Package estate reads the estate manifest: estate.toml at the root of an
// estate checkout. It is the one file that says what the checkout IS —
// its name, its git URL (flux's source and auto-rebuild's pull), its mesh,
// its operator keys (the devices that hold `deployer` on every fort of
// the estate; forts#81), and its hubs: the forts, in this estate or
// another, that serve it a role (the forge the core knows, and any role
// a layer or the estate names; forts#38, #105). Per-host facts
// are the host manifests (internal/manifest); per-estate facts are here,
// and there are few of them on purpose: an estate is plain NixOS and a
// flux tree, and this file names only what a verb reads.
//
// `fort init` writes it. `fort scaffold` reads it for the flux source URL;
// `fort check` and the link verbs read the hubs and the serves tables. A checkout with no estate.toml is not an estate, and
// every verb says so.
package estate

import (
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"golang.org/x/crypto/ssh"
)

// Path is the manifest's place: the estate root.
const Path = "estate.toml"

// Estate is estate.toml. Keys are strict: a typo is a refusal, not a
// default, like a host manifest's. The loose keys (name, operator_keys)
// come before the first table — TOML's rule, which the template keeps.
type Estate struct {
	// Name is the estate's name: the repo's, by convention.
	Name string `toml:"name"`
	// Source is the [source] table: where the estate's tree comes from
	// and how a fort reaches it (forts#161).
	Source Source `toml:"source"`
	// OperatorKeys are the SSH public keys that hold `deployer`, the
	// operator account of every fort of this estate
	// (modules/ops/deployer.nix): one line per operator device, in
	// authorized_keys form, with the device's name as the key's comment
	// (forts#81). The framework ships none, so this list is the one
	// place an operator key enters an estate: a fort of this estate
	// accepts exactly these keys, and a fort handed to another team is
	// that team's estate with that team's keys. Optional here — an
	// estate writes it before its first fort, and `fort check --all`
	// says so — because the refusal belongs to the host that would
	// carry an operator account nobody can reach (the assertion in
	// modules/ops/deployer.nix).
	OperatorKeys []string `toml:"operator_keys"`
	// Hubs is the set of roles another fort serves this estate, by role
	// ([hubs.forge], and any role the estate names; forts#38, #105). Each
	// names the estate that holds the hub and the fort that is it. The
	// hub's address on this mesh is that fort's entry in mesh/peers.json:
	// its manifest's mesh_ip when the estate is this one, its remotes
	// entry when it is another (the link that puts it on this mesh). The
	// forge is the one role the core renders for (the name pin); every
	// other role is <fort>.<zone> and the core renders nothing — a layer
	// does, for its own roles (docs/layers.md).
	Hubs map[string]Hub `toml:"hubs"`
	// Serves is the other half of the same relationship, declared by the
	// hub's estate, by role ([[serves.<role>]]): a fort of this estate,
	// the estates it serves that role, and the ports the link opens for
	// it (every role but the forge, whose pair is the core's). The key is
	// the role on both ends — [hubs.metrics] fort = "central" in the
	// served estate, [[serves.metrics]] fort = "central" here — so the
	// pair reads as one sentence from either side. Two entries under one
	// role are two hubs; an estate appears at most once per role, which
	// is the one invariant: the served estate names one fort per role.
	// What this estate consumes itself is its own [hubs.*], never here.
	Serves map[string][]ServeEntry `toml:"serves"`
	// Mesh is the [mesh] table: the estate's WireGuard mesh (forts#161).
	Mesh Mesh `toml:"mesh"`
	// ACME is the [acme] table: the estate's public ACME issuer (forts#161).
	ACME ACME `toml:"acme"`
}

// Source is the [source] table: the estate's git URL, the forge kind,
// the auth route and the forge's host key. One concern, one table —
// everything about where the tree comes from (forts#161, forts#1).
type Source struct {
	// Repo is the estate's git URL as flux and auto-rebuild reach it,
	// with or without .git: ssh://git@<forge>:<port>/<owner>/<name> on
	// the ssh route, which both clone with the deploy key fort mint
	// registers; https://<forge>/<owner>/<name> on the anonymous route
	// (forts#167), a public repo both clone with no credential at all.
	Repo string `toml:"repo"`
	// Forge is the forge's kind: "gitea" or "github". Optional — the
	// host of Repo derives it (github.com is github, every other host
	// is gitea). fort mint and fort destroy speak the kind's deploy-key
	// API (cmd/fort/forge.go, forts#1), with the kind's token
	// (`<kind>-token` in provisioning/secrets.yaml) — on the ssh route;
	// the anonymous route makes no call to the forge.
	Forge string `toml:"forge"`
	// Auth is the clone route: "ssh" (the deploy key fort mint
	// registers, the forge's host key pinned), "none" (an anonymous
	// https clone of a public repo: no deploy key, no host key, TLS
	// from the system roots; forts#167) or "token" (an https token; no
	// route yet). Optional — the scheme of Repo derives it: ssh:// is
	// "ssh", https:// is "none". The scheme and the route must agree:
	// an ssh:// repo cannot be anonymous, an https:// repo carries no
	// deploy key.
	Auth string `toml:"auth"`
	// KnownHosts is the forge's SSH host key as a known_hosts line
	// ("[gitea.example:30022] ssh-ed25519 AAAA…"): what flux's git-auth
	// and the rebuild loop pin the clone to, on the ssh route. The
	// framework ships no default (forts#139): a host of an estate on
	// the ssh route that names none does not evaluate. `fort init
	// --known-hosts` writes it; `ssh-keyscan -p <port> <forge>` prints
	// it. The anonymous route reads no host key — an https clone
	// trusts the forge's TLS certificate through the system roots (and
	// the estate's own CA under nixos/certs, where it carries one) — so
	// the key is optional there, and `fort check` says it is unread.
	KnownHosts string `toml:"known_hosts"`
}

// Mesh is the [mesh] table: the subnet, the interface name and the DNS
// zone — one mesh, three facts (forts#161).
type Mesh struct {
	// CIDR is the estate's WireGuard subnet (10.2.0.0/24). Every
	// address in mesh/peers.json and every manifest's mesh_ip lies in
	// it; fort check says so. Two estates link only when their meshes
	// do not overlap (deployments#674), and this is where each one
	// says what its mesh is. The prefix length is the mesh's: the
	// interface address (wireguard.nix, fort wg-conf) and a link's
	// prefixLength render from it. Optional: an estate with no cidr is
	// checked by nothing and renders /24, as before.
	CIDR string `toml:"cidr"`
	// Interface is the WireGuard interface every host on this mesh gives
	// it: one name per mesh, on every host that is on it — a fort's own
	// interface and a linked host's link interface alike (deployments#674).
	// At most 15 characters from [A-Za-z0-9_=+.-], the kernel's rule.
	// Optional: an estate with no interface key is on wg0, as before.
	Interface string `toml:"interface"`
	// Zone is the DNS zone of this estate's mesh (forts#45): every host
	// on the mesh — a peer or a remote in mesh/peers.json — is
	// <host>.<zone> at its mesh address, on every fort of the estate and
	// in every fort's cluster DNS, and <service>.<host>.<zone> answers
	// the same address. Optional: the default is <name>.mesh, so the
	// estate staging is the zone staging.mesh. .mesh is the fleet's one
	// private TLD; a name under it says which mesh it is on.
	Zone string `toml:"zone"`
}

// ACME is the [acme] table: the estate's public ACME account (forts#161).
type ACME struct {
	// Email is the contact of the estate's public ACME account. Set,
	// init renders the ClusterIssuer into the shared layer; absent, no
	// public issuer is rendered.
	Email string `toml:"email"`
	// Directory is the ACME directory URL the issuer speaks to. Optional:
	// the default is Let's Encrypt's production directory. A private
	// directory (a step-ca) goes here; the estate's CA file
	// (nixos/certs/ca-root.pem) is what makes the hosts trust it.
	Directory string `toml:"directory"`
}

// The forge kinds: whose deploy-key API fort mint speaks (forts#1).
const (
	ForgeGitea  = "gitea"
	ForgeGitHub = "github"
)

// The auth routes: how a fort's clone authenticates (forts#161, #1,
// #167). Two exist: ssh (a deploy key and a pinned host key) and none
// (an anonymous https clone of a public repo). token is named so the
// refusal can say it does not exist yet.
const (
	AuthSSH   = "ssh"
	AuthNone  = "none"
	AuthToken = "token"
)

// ForgeKind is the forge's kind: the forge key, else derived from the
// host of repo — github.com is github, every other host is gitea.
func (e Estate) ForgeKind() string {
	if e.Source.Forge != "" {
		return e.Source.Forge
	}
	if host, _, _, err := parseRepo(e.Source.Repo); err == nil && host == "github.com" {
		return ForgeGitHub
	}
	return ForgeGitea
}

// AuthRoute is the clone route: the auth key, else derived from the
// scheme of repo — ssh:// is ssh, https:// is none (anonymous).
func (e Estate) AuthRoute() string {
	if e.Source.Auth != "" {
		return e.Source.Auth
	}
	if strings.HasPrefix(e.Source.Repo, "https://") {
		return AuthNone
	}
	return AuthSSH
}

// Anonymous says whether the estate is on the anonymous route
// (forts#167): an https clone of a public repo with no credential.
// What that drops, in one place: fort mint mints and registers no
// deploy key and needs no forge token, fort destroy revokes none, the
// FluxInstance names no pullSecret, no host key is pinned (TLS comes
// from the system roots), and `nixos-pull --from <ip>` is refused —
// an address in the forge's place fails the certificate check.
func (e Estate) Anonymous() bool { return e.AuthRoute() == AuthNone }

// DefaultACMEDirectory is the ACME directory of an estate that names
// none: Let's Encrypt's production directory.
const DefaultACMEDirectory = "https://acme-v02.api.letsencrypt.org/directory"

// ACMEDirectory is the estate's ACME directory URL, Let's Encrypt's
// when none is named.
func (e Estate) ACMEDirectory() string {
	if e.ACME.Directory == "" {
		return DefaultACMEDirectory
	}
	return e.ACME.Directory
}

// DefaultInterface is the mesh interface of an estate that names none.
const DefaultInterface = "wg0"

var interfaceRE = regexp.MustCompile(`^[A-Za-z0-9_=+.-]{1,15}$`)

// InterfaceName is the estate's mesh interface, wg0 when none is named.
func (e Estate) InterfaceName() string {
	if e.Mesh.Interface == "" {
		return DefaultInterface
	}
	return e.Mesh.Interface
}

// DefaultMeshPrefixBits is the prefix length of an estate that declares
// no mesh: what the fleet rendered before the cidr carried it.
const DefaultMeshPrefixBits = 24

// MeshPrefixBits is the prefix length of the estate's mesh: the cidr's,
// 24 when no cidr is declared. Validate has refused a cidr that does
// not parse.
func (e Estate) MeshPrefixBits() int {
	p, err := netip.ParsePrefix(e.Mesh.CIDR)
	if err != nil {
		return DefaultMeshPrefixBits
	}
	return p.Bits()
}

// DefaultZoneSuffix is what the estate's name gains to become its zone
// when estate.toml names none: staging → staging.mesh.
const DefaultZoneSuffix = ".mesh"

// zoneRE: DNS labels of lowercase letters, digits and hyphens, joined by
// dots — one label or more. A label neither starts nor ends in a hyphen.
var zoneRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// ZoneName is the estate's mesh zone: the zone key, else <name>.mesh.
func (e Estate) ZoneName() string {
	if e.Mesh.Zone == "" {
		return e.Name + DefaultZoneSuffix
	}
	return e.Mesh.Zone
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Load reads <root>/estate.toml.
func Load(root string) (Estate, error) {
	raw, err := os.ReadFile(filepath.Join(root, Path))
	if err != nil {
		if os.IsNotExist(err) {
			return Estate{}, fmt.Errorf("no %s at %s — this checkout is not an estate; `fort init` renders one", Path, root)
		}
		return Estate{}, err
	}
	return Parse(raw)
}

// movedKeys are the loose keys of the flat schema, each with its table
// home (forts#161): no migration shim, but the refusal names the move.
var movedKeys = map[string]string{
	"repo":        "[source] repo",
	"known_hosts": "[source] known_hosts",
	"mesh":        "[mesh] cidr",
	"interface":   "[mesh] interface",
	"zone":        "[mesh] zone",
	"acme_email":  "[acme] email",
}

// Parse decodes the manifest text, strictly, and validates it.
func Parse(raw []byte) (Estate, error) {
	var e Estate
	md, err := toml.Decode(string(raw), &e)
	if err != nil {
		return Estate{}, fmt.Errorf("%s: %w", Path, err)
	}
	for _, k := range md.Undecoded() {
		if to, moved := movedKeys[k.String()]; moved {
			return Estate{}, fmt.Errorf("%s: %s moved to %s (forts#161)", Path, k.String(), to)
		}
		return Estate{}, fmt.Errorf("%s: unknown key %s", Path, k.String())
	}
	if err := e.Validate(); err != nil {
		return Estate{}, fmt.Errorf("%s: %w", Path, err)
	}
	return e, nil
}

// Validate: a name that is a repo name, a [source] table the matrix
// accepts (validateSource), the hubs and the served estates well-formed
// (validateHubs), the operator keys readable as authorized_keys lines
// (validateOperatorKeys).
func (e Estate) Validate() error {
	if !nameRE.MatchString(e.Name) {
		return fmt.Errorf("name %q — want a repo name (%s)", e.Name, nameRE)
	}
	if err := e.validateSource(); err != nil {
		return err
	}
	if err := e.validateHubs(); err != nil {
		return err
	}
	if err := e.validateOperatorKeys(); err != nil {
		return err
	}
	if e.ACME.Email != "" && !strings.Contains(e.ACME.Email, "@") {
		return fmt.Errorf("[acme] email %q — want a mailbox", e.ACME.Email)
	}
	if e.ACME.Directory != "" && !strings.HasPrefix(e.ACME.Directory, "https://") {
		return fmt.Errorf("[acme] directory %q — want the ACME directory URL, https:// (absent: Let's Encrypt, %s)", e.ACME.Directory, DefaultACMEDirectory)
	}
	if e.Mesh.Interface != "" && !interfaceRE.MatchString(e.Mesh.Interface) {
		return fmt.Errorf("[mesh] interface %q — want a WireGuard interface name: at most 15 characters from [A-Za-z0-9_=+.-] (the kernel's limit)", e.Mesh.Interface)
	}
	if e.Mesh.Zone != "" && !zoneRE.MatchString(e.Mesh.Zone) {
		return fmt.Errorf("[mesh] zone %q — want a DNS name of lowercase labels (staging.mesh); absent: <name>%s", e.Mesh.Zone, DefaultZoneSuffix)
	}
	if e.Mesh.CIDR != "" {
		p, err := netip.ParsePrefix(e.Mesh.CIDR)
		if err != nil || !p.Addr().Is4() || p != p.Masked() {
			return fmt.Errorf("[mesh] cidr %q — want the estate's WireGuard subnet as an IPv4 network address (10.2.0.0/24)", e.Mesh.CIDR)
		}
	}
	return nil
}

// validateSource is the [source] matrix (forts#161, #167): an ssh://
// repo on the ssh route with the forge's host key; an https:// repo on
// the anonymous route (auth none, derived or declared); the scheme and
// the route refused when they disagree; a token named as a route that
// does not exist; a forge kind the core knows.
func (e Estate) validateSource() error {
	repo := e.Source.Repo
	switch {
	case strings.HasPrefix(repo, "ssh://"), strings.HasPrefix(repo, "https://"):
	default:
		return fmt.Errorf("[source] repo %q — want the estate's git URL: ssh://git@<forge>:<port>/<owner>/<name> (the deploy key route), or https://<forge>/<owner>/<name> (a public repo, cloned anonymously)", repo)
	}
	https := strings.HasPrefix(repo, "https://")
	switch e.Source.Auth {
	case "":
	case AuthSSH:
		if https {
			return fmt.Errorf("[source] auth %q — an https:// repo carries no deploy key: the anonymous route (auth = %q, the default for https) clones a public repo with no credential. For the ssh route, name the repo as ssh://git@<forge>:<port>/<owner>/<name>", AuthSSH, AuthNone)
		}
	case AuthNone:
		if !https {
			return fmt.Errorf("[source] auth %q — an ssh:// repo clones with the deploy key; the anonymous route is https only: name the repo as https://<forge>/<owner>/<name>", AuthNone)
		}
	case AuthToken:
		return fmt.Errorf("[source] auth %q — no token route exists yet: the deploy key is the one credential fort mints and revokes itself. Use auth = %q with an ssh:// repo, or auth = %q with the https:// URL of a public repo", AuthToken, AuthSSH, AuthNone)
	default:
		return fmt.Errorf("[source] auth %q — want %q (an ssh:// repo, the deploy key) or %q (an https:// repo, cloned anonymously), the two routes that exist", e.Source.Auth, AuthSSH, AuthNone)
	}
	switch e.Source.Forge {
	case "", ForgeGitea, ForgeGitHub:
	default:
		return fmt.Errorf("[source] forge %q — want %q or %q (absent: the host of repo derives it)", e.Source.Forge, ForgeGitea, ForgeGitHub)
	}
	if e.Source.KnownHosts != "" && len(strings.Fields(e.Source.KnownHosts)) < 3 {
		return fmt.Errorf("[source] known_hosts %q — want one known_hosts line: <host> <key type> <key> (ssh-keyscan -p <port> <forge>)", e.Source.KnownHosts)
	}
	return nil
}

// Contains says whether an address lies in the estate's mesh. With no
// cidr declared every address does: the key is optional.
func (e Estate) Contains(ip string) bool {
	if e.Mesh.CIDR == "" {
		return true
	}
	p, err := netip.ParsePrefix(e.Mesh.CIDR)
	if err != nil {
		return false
	}
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	return p.Contains(a)
}

// RepoGit is the URL with its .git suffix, the form the FluxInstance
// carries; auto-rebuild clones the same.
func (e Estate) RepoGit() string { return WithGit(e.Source.Repo) }

// Forge is where the estate's repo lives, read off Repo: the forge host
// (no port — the ssh port in the URL is git's, the API answers on https),
// the owner and the repo name. ssh://git@forge.example:22/acme/second-estate.git
// is forge.example, acme, second-estate. The deploy key fort mint
// registers and fort destroy revokes goes on THIS repo, through this
// host's API: the estate's repo is what flux clones, so it is where the
// key must be (sfi/deployments#673's first finding — the path was a
// constant naming sfi/deployments, and a second estate's key landed on
// the first estate's repo).
func (e Estate) Forge() (host, owner, name string, err error) {
	return parseRepo(e.Source.Repo)
}

// parseRepo reads a repo URL into the forge host, the owner and the
// name, the .git suffix dropped: the shape every estate URL in
// estate.toml has — this estate's, a hub's, a served estate's — on
// either scheme.
func parseRepo(repo string) (host, owner, name string, err error) {
	u, err := url.Parse(repo)
	if err != nil {
		return "", "", "", fmt.Errorf("repo %q: %w", repo, err)
	}
	host = u.Hostname()
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("repo %q — want ssh://git@<forge>:<port>/<owner>/<name> or https://<forge>/<owner>/<name>", repo)
	}
	return host, parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

// --- the operators (forts#81)

// Operator is one entry of operator_keys, read: the key line as a fort's
// authorized_keys carries it, and the device the key is on — the key's
// comment, the one place a person says which device holds an operator
// key. Vetting (#415) trims the list by device, so a key with no
// comment is a key nobody can attribute, and `fort check --all` says so.
type Operator struct {
	Key, Name string
}

// Operators reads operator_keys into (key, device) pairs, in the order
// estate.toml lists them. Validate has refused a line that does not
// parse, so every line here is one key.
func (e Estate) Operators() []Operator {
	out := make([]Operator, 0, len(e.OperatorKeys))
	for _, line := range e.OperatorKeys {
		_, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			continue
		}
		out = append(out, Operator{Key: strings.TrimSpace(line), Name: comment})
	}
	return out
}

// validateOperatorKeys: every line is exactly one SSH public key that
// sshd can read. A typo here is a key that no fort accepts and nothing
// reports — the account is reached over the mesh and nowhere else — so
// the manifest refuses the line instead. Options (a command= prefix)
// are refused too: the operator's key carries no restriction of its
// own, the sudoers rules of ops-commands.nix are the restriction.
func (e Estate) validateOperatorKeys() error {
	seen := map[string]int{}
	for i, line := range e.OperatorKeys {
		at := fmt.Sprintf("operator_keys #%d", i+1)
		pub, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
		if err != nil {
			return fmt.Errorf("%s %q — want one SSH public key in authorized_keys form (ssh-ed25519 AAAA… <device>): %v", at, line, err)
		}
		if len(options) > 0 {
			return fmt.Errorf("%s — want the key alone, with no authorized_keys options (%s)", at, strings.Join(options, ","))
		}
		if len(strings.TrimSpace(string(rest))) > 0 {
			return fmt.Errorf("%s — want one key per entry; this entry carries more than one line", at)
		}
		fp := ssh.FingerprintSHA256(pub)
		if j, dup := seen[fp]; dup {
			return fmt.Errorf("%s is the same key as #%d (%s)", at, j+1, fp)
		}
		seen[fp] = i
	}
	return nil
}

// --- hubs and serves (forts#38)

// The roles a hub serves. A role is a small contract: what the served
// estate renders for it, what the hub opens on the link, what fort check
// says. The core knows one role by name: the forge, the gitea every fort
// clones from, whose address modules/lab-dns.nix pins (forts#26). Every
// other role is a name the estate chooses and the ports the hub opens
// on the link for it (forts#105): the core renders nothing for it, and a
// role that needs a render on the served side is a layer's
// (docs/layers.md; forts#140, #143).
const RoleForge = "forge"

// Roles is every role the core knows by name.
var Roles = []string{RoleForge}

// ForgePorts is what the forge opens on a link: gitea's ssh and https.
var ForgePorts = []int{30022, 443}

// Hub is one [hubs.<role>] table: the estate that holds the hub and the
// fort that is it. Name is the forge's: the one name it keeps on every
// mesh (forge.example), which is not <host>.<zone>; no other role has
// one.
type Hub struct {
	// Estate is the hub's estate by its git URL, as its own estate.toml
	// has it (ssh://git@forge.example:22/acme/hub): one spelling
	// of an estate everywhere, and the owner and name derive from it.
	// This estate's own URL is the in-estate case: a hub is declared the
	// same way wherever it is.
	Estate string `toml:"estate"`
	// Fort is the hub fort's name.
	Fort string `toml:"fort"`
	// Name is the forge's DNS name; forge only.
	Name string `toml:"name"`
}

// ServeEntry is one [[serves.<role>]] table: a fort of this estate, the
// estates it serves the role, by their git URLs, and the ports the link
// to each opens for it.
type ServeEntry struct {
	Fort    string   `toml:"fort"`
	Estates []string `toml:"estates"`
	// Ports is what the hub opens on the link for this role: required
	// for every role but the forge, whose ports are the core's
	// (ForgePorts), and refused there.
	Ports []int `toml:"ports"`
}

// OpenPorts is what a serves entry opens on the link: the forge's fixed
// pair, or the entry's own.
func (s ServeEntry) OpenPorts(role string) []int {
	if role == RoleForge {
		return ForgePorts
	}
	return s.Ports
}

// Serving is one (role, fort, estate) of the serves tables, flattened.
type Serving struct {
	Role, Fort, Estate string
}

// Ref is the served estate as <owner>/<name>; Name its last label, what
// the subscription file and the link are named after.
func (s Serving) Ref() string  { return Ref(s.Estate) }
func (s Serving) Name() string { return ShortName(s.Estate) }

// Ref is an estate URL reduced to <owner>/<name> (sfi/infra): how a
// served estate is named in a report, a finding, a subscription's name.
// "" for a URL that does not parse; Validate has refused it first.
func Ref(repo string) string {
	_, owner, name, err := parseRepo(repo)
	if err != nil {
		return ""
	}
	return owner + "/" + name
}

// Ref is this estate's own <owner>/<name>, read off Repo.
func (e Estate) Ref() string { return Ref(e.Source.Repo) }

// ShortName is the last label of an estate URL or <owner>/<name> ref
// (staging): what a link is named after by convention (sfi.links.<name>,
// modules/links.nix) and what the subscription file is named after.
func ShortName(ref string) string {
	ref = strings.TrimSuffix(strings.TrimRight(ref, "/"), ".git")
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// WithGit is a repo URL with its .git suffix, the form a flux
// GitRepository and a clone carry; RepoGit is the same for this estate.
func WithGit(repo string) string {
	if strings.HasSuffix(repo, ".git") {
		return repo
	}
	return repo + ".git"
}

// SameRepo says whether two URLs name one repo: the same forge host,
// owner and name, whatever the scheme, the port and the .git suffix.
func SameRepo(a, b string) bool {
	ah, ao, an, err := parseRepo(a)
	if err != nil {
		return false
	}
	bh, bo, bn, err := parseRepo(b)
	if err != nil {
		return false
	}
	return ah == bh && ao == bo && an == bn
}

// ValidRepoURL says whether s is an estate's git URL as estate.toml
// spells one: ssh:// or https://, a forge host, an owner and a name.
func ValidRepoURL(s string) bool {
	if !strings.HasPrefix(s, "ssh://") && !strings.HasPrefix(s, "https://") {
		return false
	}
	_, _, _, err := parseRepo(s)
	return err == nil
}

// ValidInterface says whether s is a WireGuard interface name the kernel
// takes: at most 15 characters from [A-Za-z0-9_=+.-].
func ValidInterface(s string) bool { return interfaceRE.MatchString(s) }

// ValidZone says whether s is a DNS name of lowercase labels (staging.mesh).
func ValidZone(s string) bool { return zoneRE.MatchString(s) }

// validRole: a role is a name the estate chooses (lowercase labels and
// dashes); the forge is the one the core knows.
func validRole(role string) bool { return nameRE.MatchString(role) }

func (e Estate) validateHubs() error {
	for _, role := range sortedKeys(e.Hubs) {
		h := e.Hubs[role]
		if !validRole(role) {
			return fmt.Errorf("[hubs.%s] — want a role name of lowercase labels and dashes (forge, or one the estate chooses; docs/hub.md)", role)
		}
		if !ValidRepoURL(h.Estate) {
			return fmt.Errorf("[hubs.%s] estate %q — want the hub's estate by its git URL (ssh://git@<forge>:<port>/<owner>/<name>, or https://); this estate is %q", role, h.Estate, e.Source.Repo)
		}
		if !nameRE.MatchString(h.Fort) {
			return fmt.Errorf("[hubs.%s] fort %q — want the hub fort's name (%s)", role, h.Fort, nameRE)
		}
		switch {
		case h.Name != "" && role != RoleForge:
			return fmt.Errorf("[hubs.%s] name — only the forge has a name of its own; every other hub is <host>.<zone>", role)
		case h.Name != "" && !zoneRE.MatchString(h.Name):
			return fmt.Errorf("[hubs.%s] name %q — want a DNS name of lowercase labels (forge.example)", role, h.Name)
		case h.Name != "" && !strings.Contains(h.Name, "."):
			// forts#172: the pin's DNS block is the name, whole; a name of
			// one label would make that block a TLD, and every fort would
			// answer NXDOMAIN for every other name under it.
			return fmt.Errorf("[hubs.%s] name %q — want the forge's name under a domain (forge.example): one label would make its DNS block a whole TLD on every fort (modules/lab-dns.nix)", role, h.Name)
		}
	}
	roles := make([]string, 0, len(e.Serves))
	for role := range e.Serves {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		if !validRole(role) {
			return fmt.Errorf("[[serves.%s]] — want a role name of lowercase labels and dashes (forge, or one the estate chooses; docs/hub.md)", role)
		}
		var seen []string
		for i, s := range e.Serves[role] {
			at := fmt.Sprintf("[[serves.%s]] #%d", role, i+1)
			if !nameRE.MatchString(s.Fort) {
				return fmt.Errorf("%s fort %q — want the name of a fort of this estate (%s)", at, s.Fort, nameRE)
			}
			if len(s.Estates) == 0 {
				return fmt.Errorf("%s estates — want the served estates by their git URLs, at least one", at)
			}
			switch {
			case role == RoleForge && len(s.Ports) > 0:
				return fmt.Errorf("%s ports — the forge's ports are the core's (30022 and 443); drop the key", at)
			case role != RoleForge && len(s.Ports) == 0:
				return fmt.Errorf("%s ports — want the ports the hub opens on the link for this role (forts#105), at least one", at)
			}
			for _, port := range s.Ports {
				if port < 1 || port > 65535 {
					return fmt.Errorf("%s ports: %d — want a TCP port", at, port)
				}
			}
			for _, url := range s.Estates {
				if !ValidRepoURL(url) {
					return fmt.Errorf("%s estates: %q — want a served estate by its git URL (ssh://git@<forge>:<port>/<owner>/<name>, or https://)", at, url)
				}
				if SameRepo(url, e.Source.Repo) {
					return fmt.Errorf("%s estates: %q is this estate — an estate serves itself through its own [hubs.*] tables, not a serves entry", at, url)
				}
				for _, prev := range seen {
					if SameRepo(prev, url) {
						return fmt.Errorf("%s estates: %q is served the role %s twice — a served estate names one fort per role", at, url, role)
					}
				}
				seen = append(seen, url)
			}
		}
	}
	return nil
}

func sortedKeys(m map[string]Hub) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Hub is the hub of one role, if the estate declares one.
func (e Estate) Hub(role string) (Hub, bool) {
	h, ok := e.Hubs[role]
	return h, ok
}

// Local says whether the hub is a fort of this estate: the in-estate
// case, where its address is its manifest's mesh_ip and the files a
// served fort renders for it are applied by this estate's own flux. The
// URLs are compared as repos (host, owner, name), so a .git suffix or a
// scheme does not make two spellings of one estate two estates.
func (h Hub) Local(e Estate) bool { return SameRepo(h.Estate, e.Source.Repo) }

// Ref is the hub's estate as <owner>/<name>.
func (h Hub) Ref() string { return Ref(h.Estate) }

// LocalHub is the host that serves a role from inside this estate, ""
// when the role is unserved or served from another estate: a write into
// the hub's tree is possible only when that tree is this one.
func (e Estate) LocalHub(role string) string {
	h, ok := e.Hubs[role]
	if !ok || !h.Local(e) {
		return ""
	}
	return h.Fort
}

// Servings flattens the serves tables: every (role, fort, estate), in
// role order, then the order written.
func (e Estate) Servings() []Serving {
	roles := make([]string, 0, len(e.Serves))
	for role := range e.Serves {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	var out []Serving
	for _, role := range roles {
		for _, s := range e.Serves[role] {
			for _, url := range s.Estates {
				out = append(out, Serving{Role: role, Fort: s.Fort, Estate: url})
			}
		}
	}
	return out
}

// ServedBy is what one fort serves, for one role: the estates it is the
// hub of. Nil when the fort serves nobody that role.
func (e Estate) ServedBy(fort, role string) []Serving {
	var out []Serving
	for _, s := range e.Servings() {
		if s.Fort == fort && s.Role == role {
			out = append(out, s)
		}
	}
	return out
}

// ServingForts is every fort the serves tables name, sorted.
func (e Estate) ServingForts() []string {
	set := map[string]bool{}
	for _, s := range e.Servings() {
		set[s.Fort] = true
	}
	out := make([]string, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
