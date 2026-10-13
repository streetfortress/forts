package manifest

import (
	"encoding/base64"
	"fmt"
	"net/netip"
	"sort"

	"gitea.zen.lofi/sfi/forts/internal/estate"
)

// Link is one [links.<name>] table (forts#55): this fort on the mesh of
// another estate, as a host that estate's forts peer with. Every fact of
// the link lives here, the public key included — the private half is
// the sops secret wireguard-private-key-<name> in the host's secrets
// file, and the public half is plaintext in the manifest, the split a
// fort's own key already has (sops + mesh/peers.json).
//
//	[links.staging]                  # the name: the last label of the estate URL, and sfi.links.<name>
//	estate = "ssh://git@forge.example:22/acme/staging"
//	address = "10.2.0.254"           # this host's address on that mesh — a claim, until that estate records it
//	port = 51821                     # the UDP port the link listens on; never 51820 (this host's own mesh)
//	interface = "wg-staging"         # what that estate's estate.toml names its mesh
//	zone = "staging.mesh"            # its zone
//	public_key = "…"                 # the public half of wireguard-private-key-staging
//
// `fort link add <fort> <estate url>` writes the table and mints the
// key; `fort scaffold` renders sfi.links.<name> from it (modules/
// links.nix) — nobody writes that block by hand. The other estate's
// `fort estate link <this estate's url>` reads the table from a fetch
// of this repo and writes the remotes entry: the manifest is the
// cross-repo file every verb reads, and the other estate never
// evaluates this estate's Nix. The address is this side's claim and
// that estate's mesh/peers.json is the record; its verb verifies the
// claim before it writes (docs/links.md).
type Link struct {
	// Estate is the other estate's git URL, as its own estate.toml
	// spells it. Always written, also when a [[serves.*]] entry of
	// estate.toml could supply it: the other estate's verb matches on
	// it, and reads nothing else of this estate.
	Estate string `toml:"estate"`
	// Address is this host's address on the other estate's mesh.
	Address string `toml:"address"`
	// Port is the link interface's UDP listen port, beside 51820.
	Port int `toml:"port"`
	// Interface is the link's WireGuard interface: one name per mesh,
	// the one the other estate's estate.toml gives it. Default wg-<name>.
	Interface string `toml:"interface"`
	// Zone is the other mesh's DNS zone (forts#45). Default <name>.mesh.
	Zone string `toml:"zone"`
	// PublicKey is the public half of the link's WireGuard key.
	PublicKey string `toml:"public_key"`

	// Name is the table's key, set by Parse.
	Name string `toml:"-"`
}

// MeshPort is the UDP port every fort's own mesh interface listens on
// (modules/wireguard.nix); a link listens beside it, never on it.
const MeshPort = 51820

// InterfaceName is the link's interface, wg-<name> when the table names
// none — the default modules/links.nix gives it.
func (l Link) InterfaceName() string {
	if l.Interface == "" {
		return "wg-" + l.Name
	}
	return l.Interface
}

// ZoneName is the link's zone, <name>.mesh when the table names none.
func (l Link) ZoneName() string {
	if l.Zone == "" {
		return l.Name + estate.DefaultZoneSuffix
	}
	return l.Zone
}

// Ref is the other estate as <owner>/<name>.
func (l Link) Ref() string { return estate.Ref(l.Estate) }

// ValidWGPublicKey says whether s is a WireGuard public key as `wg
// pubkey` prints one: 32 bytes, base64.
func ValidWGPublicKey(s string) bool {
	b, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(b) == 32
}

// ValidateLinks: every table well-formed, named by its estate's last
// label, and no two sharing what the kernel or the firewall cannot
// share. The estate-level facts — the link's interface and zone are
// not this host's own mesh's, the estate is not this one — are fort
// check's: the manifest alone does not know them. Validate runs it;
// fort link add runs it on the one table it is about to write.
func (h Host) ValidateLinks() error {
	seenPort, seenIface, seenZone := map[int]string{}, map[string]string{}, map[string]string{}
	for _, name := range h.LinkNames() {
		l := h.Links[name]
		at := fmt.Sprintf("[links.%s]", name)
		if !estate.ValidRepoURL(l.Estate) {
			return fmt.Errorf("%s estate %q — want the other estate's git URL (ssh://git@<gitea>:<port>/<owner>/<name>, or https://)", at, l.Estate)
		}
		if want := estate.ShortName(l.Estate); want != name {
			return fmt.Errorf("%s names an estate whose last label is %s — a link is named after the estate it reaches (sfi.links.%s, modules/links.nix)", at, want, want)
		}
		if a, err := netip.ParseAddr(l.Address); err != nil || !a.Is4() {
			return fmt.Errorf("%s address %q: want this host's IPv4 address on that mesh", at, l.Address)
		}
		switch {
		case l.Port == MeshPort:
			return fmt.Errorf("%s port %d is this host's own mesh port — a link listens beside it", at, MeshPort)
		case l.Port < 1 || l.Port > 65535:
			return fmt.Errorf("%s port %d: want a UDP port", at, l.Port)
		}
		if !estate.ValidInterface(l.InterfaceName()) {
			return fmt.Errorf("%s interface %q — want a WireGuard interface name: at most 15 characters from [A-Za-z0-9_=+.-]", at, l.InterfaceName())
		}
		if !estate.ValidZone(l.ZoneName()) {
			return fmt.Errorf("%s zone %q — want a DNS name of lowercase labels (staging.mesh)", at, l.ZoneName())
		}
		if !ValidWGPublicKey(l.PublicKey) {
			return fmt.Errorf("%s public_key: want a WireGuard public key (32 bytes, base64) — fort link add writes it", at)
		}
		if other, ok := seenPort[l.Port]; ok {
			return fmt.Errorf("%s port %d is also [links.%s]'s — one UDP port per link", at, l.Port, other)
		}
		if other, ok := seenIface[l.InterfaceName()]; ok {
			return fmt.Errorf("%s interface %s is also [links.%s]'s — one interface per link", at, l.InterfaceName(), other)
		}
		if other, ok := seenZone[l.ZoneName()]; ok {
			return fmt.Errorf("%s zone %s is also [links.%s]'s — one zone per link", at, l.ZoneName(), other)
		}
		seenPort[l.Port], seenIface[l.InterfaceName()], seenZone[l.ZoneName()] = name, name, name
	}
	return nil
}

// LinkNames lists the host's links, sorted.
func (h Host) LinkNames() []string {
	out := make([]string, 0, len(h.Links))
	for n := range h.Links {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// LinkTo is the host's link to an estate, by its repo URL in any
// spelling; false when the host does not link there.
func (h Host) LinkTo(repo string) (Link, bool) {
	for _, n := range h.LinkNames() {
		if estate.SameRepo(h.Links[n].Estate, repo) {
			return h.Links[n], true
		}
	}
	return Link{}, false
}
