// Package estate reads the estate manifest: estate.toml at the root of an
// estate checkout. It is the one file that says what the checkout IS —
// its name, its git URL (flux's source and auto-rebuild's pull) and, when
// it has one, the fort that is its monitoring hub. Per-host facts are the
// host manifests (internal/manifest); per-estate facts are here, and
// there are few of them on purpose: an estate is plain NixOS and a flux
// tree, and this file names only what a verb reads.
//
// `fort init` writes it. `fort scaffold` reads it for the flux source URL
// and the hub; `fort add` reads the zone; `fort check`, `fort dismantle`
// read the hub. A checkout with no estate.toml is not an estate, and
// every verb says so.
package estate

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Path is the manifest's place: the estate root.
const Path = "estate.toml"

// Estate is estate.toml. Keys are strict: a typo is a refusal, not a
// default, like a host manifest's.
type Estate struct {
	// Name is the estate's name: the gitea repo's, by convention.
	Name string `toml:"name"`
	// Repo is the estate's git URL as flux and auto-rebuild reach it
	// (ssh://git@<gitea>:<port>/<owner>/<name>, with or without .git).
	// Both clone it with the deploy key fort mint registers.
	Repo string `toml:"repo"`
	// Hub is the fort of THIS estate whose monitoring tree carries every
	// other fort's file (the dead man's switch, the edge Alertmanager
	// datasource, a plain fort's scrape), and whose rustfs-central the
	// forts with a backups profile register with. Optional: an estate
	// with no hub has no one to notice a silent fort, and says so by
	// leaving this out.
	Hub string `toml:"hub"`
	// HubPush is a hub in ANOTHER estate, by mesh address: the edges push
	// metrics and logs to it, and that estate decides what it scrapes.
	// Exclusive with Hub. No files render for it here.
	HubPush string `toml:"hub_push"`
	// Mesh is the estate's WireGuard subnet, CIDR (10.2.0.0/24). Every
	// address in mesh/peers.json and every manifest's mesh_ip lies in
	// it; fort check says so. Two estates link only when their meshes
	// do not overlap (deployments#674), and this is where each one
	// says what its mesh is. Optional: an estate with no mesh key is
	// checked by nothing, as before.
	Mesh string `toml:"mesh"`
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

// DefaultInterface is the mesh interface of an estate that names none.
const DefaultInterface = "wg0"

var interfaceRE = regexp.MustCompile(`^[A-Za-z0-9_=+.-]{1,15}$`)

// InterfaceName is the estate's mesh interface, wg0 when none is named.
func (e Estate) InterfaceName() string {
	if e.Interface == "" {
		return DefaultInterface
	}
	return e.Interface
}

// DefaultZoneSuffix is what the estate's name gains to become its zone
// when estate.toml names none: staging → staging.mesh.
const DefaultZoneSuffix = ".mesh"

// zoneRE: DNS labels of lowercase letters, digits and hyphens, joined by
// dots — one label or more. A label neither starts nor ends in a hyphen.
var zoneRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$`)

// ZoneName is the estate's mesh zone: the zone key, else <name>.mesh.
func (e Estate) ZoneName() string {
	if e.Zone == "" {
		return e.Name + DefaultZoneSuffix
	}
	return e.Zone
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

// Parse decodes the manifest text, strictly, and validates it.
func Parse(raw []byte) (Estate, error) {
	var e Estate
	md, err := toml.Decode(string(raw), &e)
	if err != nil {
		return Estate{}, fmt.Errorf("%s: %w", Path, err)
	}
	for _, k := range md.Undecoded() {
		return Estate{}, fmt.Errorf("%s: unknown key %s", Path, k.String())
	}
	if err := e.Validate(); err != nil {
		return Estate{}, fmt.Errorf("%s: %w", Path, err)
	}
	return e, nil
}

// Validate: a name that is a repo name, a repo URL a clone understands,
// a hub that is a host name if present.
func (e Estate) Validate() error {
	if !nameRE.MatchString(e.Name) {
		return fmt.Errorf("name %q — want a repo name (%s)", e.Name, nameRE)
	}
	if !strings.HasPrefix(e.Repo, "ssh://") && !strings.HasPrefix(e.Repo, "https://") {
		return fmt.Errorf("repo %q — want the estate's git URL (ssh://git@<gitea>:<port>/<owner>/<name>, or https://)", e.Repo)
	}
	if e.Hub != "" && !nameRE.MatchString(e.Hub) {
		return fmt.Errorf("hub %q — want the hub fort's host name (%s), or no hub", e.Hub, nameRE)
	}
	if e.HubPush != "" && net.ParseIP(e.HubPush) == nil {
		return fmt.Errorf("hub_push %q — want the mesh address of a hub in another estate", e.HubPush)
	}
	if e.Hub != "" && e.HubPush != "" {
		return fmt.Errorf("hub and hub_push together — a hub is either a fort of this estate or an address in another, not both")
	}
	if e.Interface != "" && !interfaceRE.MatchString(e.Interface) {
		return fmt.Errorf("interface %q — want a WireGuard interface name: at most 15 characters from [A-Za-z0-9_=+.-] (the kernel's limit)", e.Interface)
	}
	if e.Zone != "" && !zoneRE.MatchString(e.Zone) {
		return fmt.Errorf("zone %q — want a DNS name of lowercase labels (staging.mesh); absent: <name>%s", e.Zone, DefaultZoneSuffix)
	}
	if e.Mesh != "" {
		p, err := netip.ParsePrefix(e.Mesh)
		if err != nil || !p.Addr().Is4() || p != p.Masked() {
			return fmt.Errorf("mesh %q — want the estate's WireGuard subnet as an IPv4 network address (10.2.0.0/24)", e.Mesh)
		}
	}
	return nil
}

// Contains says whether an address lies in the estate's mesh. With no
// mesh declared every address does: the key is optional.
func (e Estate) Contains(ip string) bool {
	if e.Mesh == "" {
		return true
	}
	p, err := netip.ParsePrefix(e.Mesh)
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
func (e Estate) RepoGit() string {
	if strings.HasSuffix(e.Repo, ".git") {
		return e.Repo
	}
	return e.Repo + ".git"
}

// HasHub says whether a fort of this estate is the hub: files render
// into its tree.
func (e Estate) HasHub() bool { return e.Hub != "" }

// Pushes says whether the edges have somewhere to push: a hub here or an
// address elsewhere. The shared layer carries remote_write and the log
// shipper only then.
func (e Estate) Pushes() bool { return e.Hub != "" || e.HubPush != "" }

// Forge is where the estate's repo lives, read off Repo: the forge host
// (no port — the ssh port in the URL is git's, the API answers on https),
// the owner and the repo name. ssh://git@gitea.zen.lofi:30022/sfi/second-estate.git
// is gitea.zen.lofi, sfi, second-estate. The deploy key fort mint
// registers and fort destroy revokes goes on THIS repo, through this
// host's API: the estate's repo is what flux clones, so it is where the
// key must be (sfi/deployments#673's first finding — the path was a
// constant naming sfi/deployments, and a second estate's key landed on
// the first estate's repo).
func (e Estate) Forge() (host, owner, name string, err error) {
	u, err := url.Parse(e.Repo)
	if err != nil {
		return "", "", "", fmt.Errorf("repo %q: %w", e.Repo, err)
	}
	host = u.Hostname()
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if host == "" || len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("repo %q — want ssh://git@<forge>:<port>/<owner>/<name>", e.Repo)
	}
	return host, parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

// APIBase is the forge's API root, https://<host>/api/v1 — gitea's.
func (e Estate) APIBase() (string, error) {
	host, _, _, err := e.Forge()
	if err != nil {
		return "", err
	}
	return "https://" + host + "/api/v1", nil
}

// RepoAPI is the estate repo's API route, <APIBase>/repos/<owner>/<name>:
// the deploy keys hang off it.
func (e Estate) RepoAPI() (string, error) {
	host, owner, name, err := e.Forge()
	if err != nil {
		return "", err
	}
	return "https://" + host + "/api/v1/repos/" + owner + "/" + name, nil
}
