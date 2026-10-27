package main

import (
	"flag"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gopkg.in/yaml.v3"
)

// fort link add <fort> <estate url> — the local half of a link
// (forts#55, #56; docs/links.md): declare that <fort> is on <estate>'s
// mesh, and mint the key. An author verb in mint's shape: it reads the
// other estate's repo, plans, prints the marked tree; --write lays it
// down and stages it.
//
// What it writes, all in THIS estate: the private half into
// nixos/secrets/<fort>.yaml as wireguard-private-key-<name> (sops set);
// the [links.<name>] table into forts/<fort>.toml — estate, address,
// port, interface, zone, public_key; and the sfi.links.<name> block
// into nixos/hosts/<fort>/default.nix, the same block fort scaffold
// renders, with the links module imported.
//
// The address is this side's CLAIM. The other estate's mesh/peers.json
// is the record, and its `fort estate link <this estate>` verifies the
// claim before it writes the remotes entry — so this verb reads that
// estate's estate.toml (the mesh, the interface, the zone) and its
// peers.json and manifests (the addresses taken) and picks the highest
// free address, from .254 down: forts choose mesh_ip by hand and those
// numbers run low, so the two allocators do not meet. --address and
// --port take the choice away; both are still checked.
//
// Re-runnable: the key is restored from the secrets file when it is
// there (rotation is a deliberate act, as in mint), the table is
// rewritten only where it differs, and an address that is still free
// is kept. An address another entry has taken since — the collision
// `fort estate link` refuses on — is picked again, and the report says
// so. Nothing is written in the other estate.
func linkCmd(args []string) error {
	if len(args) < 1 || args[0] != "add" {
		return fmt.Errorf("usage: fort link add <fort> <estate url> [--address <ip>] [--port <udp>] [--write]")
	}
	return linkAdd(args[1:])
}

func linkAdd(args []string) error {
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos = append(pos, args[0])
		args = args[1:]
	}
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	var o linkAddOpts
	fs.StringVar(&o.address, "address", "", "this host's address on the other mesh (default: the highest free one)")
	fs.IntVar(&o.port, "port", 0, "the link's UDP listen port (default: the lowest free one above 51820)")
	write := fs.Bool("write", false, "mint, encrypt, write and stage (default: print the plan)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if len(pos) != 2 || fs.NArg() > 0 {
		return fmt.Errorf("usage: fort link add <fort> <estate url> [--address <ip>] [--port <udp>] [--write]")
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	tool := sopsCLI{root: root}
	p, err := planLinkAdd(root, pos[0], pos[1], o, tool, gitFetch)
	if err != nil {
		return err
	}
	if *write {
		if err := p.apply(tool); err != nil {
			return err
		}
	}
	p.report(os.Stdout, *write)
	return nil
}

type linkAddOpts struct {
	address string
	port    int
}

// linkSecretKey is the sops key of a link's private half, as
// modules/links.nix names it by default.
func linkSecretKey(name string) string { return "wireguard-private-key-" + name }

type linkPlan struct {
	root, host, name string
	est              estate.Estate // this estate
	entry            manifest.Host
	remote           *fetchedEstate // the estate being linked to
	link             manifest.Link  // the table as it will be
	had              *manifest.Link // the table as it is, nil when new
	wg               wgKey
	keyMissing       bool // apply sops-sets the private half
	secretsPath      string
	addressWhy       string // how the address was chosen, for the report
	portWhy          string
	recorded         string // what the other estate's remotes entry says of this host, "" when none
	roles            []string
	edits            []dEdit // the manifest and the host file, "exists" ones filtered out
}

func planLinkAdd(root, host, repo string, o linkAddOpts, tool secretsTool, fetch estateFetcher) (*linkPlan, error) {
	entry, err := manifest.Load(root, host)
	if err != nil {
		return nil, err
	}
	est, err := estate.Load(root)
	if err != nil {
		return nil, err
	}
	if !estate.ValidRepoURL(repo) {
		return nil, fmt.Errorf("%q — want the other estate's git URL (ssh://git@<forge>:<port>/<owner>/<name>, or https://)", repo)
	}
	if estate.SameRepo(repo, est.Source.Repo) {
		return nil, fmt.Errorf("%s is this estate — a fort is on its own mesh by its manifest's mesh_ip; a link reaches ANOTHER estate's mesh", estate.Ref(repo))
	}
	name := estate.ShortName(repo)
	p := &linkPlan{root: root, host: host, name: name, est: est, entry: entry}
	// Gate: the host is scaffolded and minted — the block has a file to
	// land in, and the key a secrets file.
	hostNix := filepath.Join("nixos/hosts", host, "default.nix")
	if !exists(root, hostNix) {
		return nil, fmt.Errorf("missing %s — run `fort scaffold %s --write` first", hostNix, host)
	}
	p.secretsPath = filepath.Join("nixos/secrets", host+".yaml")
	if !exists(root, p.secretsPath) {
		return nil, fmt.Errorf("missing %s — run `fort mint %s --write` first; the link's key lands beside the host's own", p.secretsPath, host)
	}

	// 1. the other estate: its mesh, interface and zone, and what is
	// taken on it.
	remote, cleanup, err := fetchEstate(fetch, repo)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	p.remote = remote
	if remote.est.Mesh.CIDR == "" && o.address == "" {
		return nil, fmt.Errorf("%s's %s declares no mesh — add a `[mesh]` table with `cidr = \"10.x.0.0/24\"` there (fort init --mesh), or pass --address", remote.est.Ref(), estate.Path)
	}
	taken := p.takenOn(remote)
	for h, l := range p.otherClaims(root) {
		taken[l.Address] = h
	}
	if r, ok := remote.mesh.Remotes[host]; ok {
		p.recorded = fmt.Sprintf("%s, endpoint %s:%d, key %s", r.IP, r.Endpoint, r.endpointPort(), abbrevKey(r.PublicKey))
	}

	// 2. the key: restored from the secrets file, minted when absent.
	raw, err := tool.Decrypt(filepath.Join(root, p.secretsPath))
	if err != nil {
		return nil, err
	}
	have := map[string]string{}
	if err := yaml.Unmarshal(raw, &have); err != nil {
		return nil, fmt.Errorf("%s: %w", p.secretsPath, err)
	}
	if v, ok := have[linkSecretKey(name)]; ok {
		p.wg, err = wgKeyFrom(v)
	} else {
		p.wg, err = newWGKey()
		p.keyMissing = true
	}
	if err != nil {
		return nil, err
	}
	if l, ok := entry.Links[name]; ok {
		had := l
		p.had = &had
		if had.PublicKey != p.wg.public {
			if p.keyMissing {
				return nil, fmt.Errorf("%s names [links.%s] public_key %s, and %s holds no %s — the link's real key is gone from the secrets file, or the table is stale; a re-key is a deliberate act", manifest.Path(host), name, abbrevKey(had.PublicKey), p.secretsPath, linkSecretKey(name))
			}
			return nil, fmt.Errorf("%s names [links.%s] public_key %s, and %s in %s derives %s — one of the two is stale; a re-key is a deliberate act", manifest.Path(host), name, abbrevKey(had.PublicKey), linkSecretKey(name), p.secretsPath, abbrevKey(p.wg.public))
		}
	} else if _, ok := entry.LinkTo(repo); ok {
		return nil, fmt.Errorf("%s already links to %s under another name — one link per estate", manifest.Path(host), remote.est.Ref())
	}

	// 3. the table: interface and zone are the other estate's names;
	// address and port are chosen here.
	iface := remote.est.InterfaceName()
	if iface == estate.DefaultInterface {
		// That estate is on wg0, and a link cannot be wg0 (this host's
		// own mesh): the module's default name, which the reconciler
		// does not check against an estate that names none.
		iface = "wg-" + name
	}
	p.link = manifest.Link{Name: name, Estate: repo, Interface: iface, Zone: remote.est.ZoneName(), PublicKey: p.wg.public}
	if bits := remote.est.MeshPrefixBits(); bits != estate.DefaultMeshPrefixBits {
		// The other mesh is not a /24: the table carries its prefix, and
		// links.nix renders it (the option's default stays 24).
		p.link.Prefix = bits
	}
	if p.had != nil {
		p.link.Estate = p.had.Estate
		if !estate.SameRepo(p.had.Estate, repo) {
			p.link.Estate = repo
		}
	}
	p.link.Address, p.addressWhy, err = p.chooseAddress(o.address, taken)
	if err != nil {
		return nil, err
	}
	p.link.Port, p.portWhy, err = p.choosePort(o.port)
	if err != nil {
		return nil, err
	}
	// The new table against the manifest's own rules — the host's other
	// links included, so a port or interface they hold is refused here.
	withNew := entry
	withNew.Links = map[string]manifest.Link{}
	for n, l := range entry.Links {
		withNew.Links[n] = l
	}
	withNew.Links[name] = p.link
	if err := withNew.ValidateLinks(); err != nil {
		return nil, fmt.Errorf("%s: %w", manifest.Path(host), err)
	}
	for _, s := range est.Servings() {
		if s.Fort == host && estate.SameRepo(s.Estate, repo) {
			p.roles = append(p.roles, s.Role)
		}
	}

	// 4. the edits, as text: the table, then the block.
	manifestPath := manifest.Path(host)
	raw, err = os.ReadFile(filepath.Join(root, manifestPath))
	if err != nil {
		return nil, err
	}
	next, err := p.manifestEdit(string(raw))
	if err != nil {
		return nil, err
	}
	if next != string(raw) {
		note := "+ [links." + name + "]"
		if p.had != nil {
			note = "~ [links." + name + "]: " + strings.Join(p.changed(), ", ")
		}
		p.edits = append(p.edits, dEdit{manifestPath, note, p.manifestEdit})
	}
	nix, err := os.ReadFile(filepath.Join(root, hostNix))
	if err != nil {
		return nil, err
	}
	withLink := entry
	withLink.Links = map[string]manifest.Link{name: p.link}
	next, err = ensureLinks(string(nix), withLink)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", hostNix, err)
	}
	attr := "sfi.links." + name
	hadBlock := nixSetsAttr(string(nix), attr)
	if next != string(nix) {
		note := "+ " + attr + " (the block fort scaffold renders; the links module imported)"
		if hadBlock {
			note = "+ the links module imported (" + attr + " is there by hand; kept)"
		}
		p.edits = append(p.edits, dEdit{hostNix, note, func(s string) (string, error) { return ensureLinks(s, withLink) }})
	}
	if hadBlock {
		// The block is there and is not the one the table renders — a
		// hand edit, or a manifest that changed under it. The block does
		// not follow by itself (scaffold never rewrites), so the report
		// says what to compare.
		block, err := renderLinksBlock(withLink)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(string(nix), strings.TrimRight(block, "\n")) {
			why := "it is not the block the table renders"
			if c := p.changed(); len(c) > 0 {
				why = strings.Join(c, ", ")
			}
			p.edits = append(p.edits, dEdit{hostNix, "! " + attr + " is there and does not follow the table — edit the block by hand: " + why, nil})
		}
	}
	return p, nil
}

// manifestEdit is the manifest edit as apply runs it: the text with the
// table as planned, parsed again before it is handed back — a text
// edit that lands a manifest the parser refuses is refused here, not
// found by the next verb.
func (p *linkPlan) manifestEdit(text string) (string, error) {
	next := p.manifestText(text)
	if _, err := manifest.Parse(p.host, []byte(next)); err != nil {
		return "", fmt.Errorf("the edit of %s does not parse — the file has a shape this verb cannot edit; write [links.%s] by hand: %w", manifest.Path(p.host), p.name, err)
	}
	return next, nil
}

// takenOn is every address in use on the other estate's mesh, by
// holder: its peers, clients and remotes (this host's own entry
// excepted — it is the claim being made), and the mesh_ip of every
// manifest there, minted or not.
func (p *linkPlan) takenOn(remote *fetchedEstate) map[string]string {
	taken := map[string]string{}
	for _, n := range sortedNames(remote.mesh.Peers) {
		taken[remote.mesh.Peers[n].IP] = "peers." + n
	}
	for _, n := range sortedNames(remote.mesh.Clients) {
		taken[remote.mesh.Clients[n].IP] = "clients." + n
	}
	for _, n := range sortedNames(remote.mesh.Remotes) {
		if n == p.host {
			continue
		}
		taken[remote.mesh.Remotes[n].IP] = "remotes." + n
	}
	for _, n := range sortedNames(remote.hosts) {
		if _, ok := taken[remote.hosts[n].MeshIP]; !ok {
			taken[remote.hosts[n].MeshIP] = manifest.Path(n) + " mesh_ip (not minted yet)"
		}
	}
	return taken
}

// otherClaims is what the other hosts of THIS estate claim on the same
// mesh: a second hub linking to the same estate, not yet recorded there.
func (p *linkPlan) otherClaims(root string) map[string]manifest.Link {
	out := map[string]manifest.Link{}
	hosts, _, err := manifest.LoadAll(root)
	if err != nil {
		return out
	}
	for h, e := range hosts {
		if h == p.host {
			continue
		}
		if l, ok := e.LinkTo(p.remote.est.Source.Repo); ok {
			out[manifest.Path(h)+" [links."+l.Name+"]"] = l
		}
	}
	return out
}

// chooseAddress: --address when given (checked: inside the mesh, not
// taken); else the table's own when it is still free; else the highest
// free address of the mesh.
func (p *linkPlan) chooseAddress(flag string, taken map[string]string) (string, string, error) {
	mesh := p.remote.est
	if flag != "" {
		a, err := netip.ParseAddr(flag)
		if err != nil || !a.Is4() {
			return "", "", fmt.Errorf("--address %q: want an IPv4 address on %s's mesh", flag, mesh.Ref())
		}
		if !mesh.Contains(flag) {
			return "", "", fmt.Errorf("--address %s is outside %s's mesh %s", flag, mesh.Ref(), mesh.Mesh.CIDR)
		}
		if holder, ok := taken[flag]; ok {
			return "", "", fmt.Errorf("--address %s is %s's on %s's mesh", flag, holder, mesh.Ref())
		}
		return flag, "--address", nil
	}
	if p.had != nil {
		if holder, ok := taken[p.had.Address]; !ok {
			return p.had.Address, "kept: still free on " + mesh.Ref() + "'s mesh", nil
		} else {
			next, err := pickAddress(mesh, taken)
			if err != nil {
				return "", "", err
			}
			return next, fmt.Sprintf("picked again: %s is %s's now", p.had.Address, holder), nil
		}
	}
	next, err := pickAddress(mesh, taken)
	if err != nil {
		return "", "", err
	}
	return next, fmt.Sprintf("picked: the highest free address of %s (%d taken)", mesh.Mesh.CIDR, len(taken)), nil
}

// pickAddress is the highest host address of the mesh that nothing
// holds, from the top down (the broadcast address excepted).
func pickAddress(mesh estate.Estate, taken map[string]string) (string, error) {
	prefix, err := netip.ParsePrefix(mesh.Mesh.CIDR)
	if err != nil {
		return "", fmt.Errorf("%s's mesh %q: %w", mesh.Ref(), mesh.Mesh.CIDR, err)
	}
	// The last address of the prefix: the network address with the host
	// bits set, which is the broadcast address; the candidates start one
	// below it and end one above the network address.
	bits := prefix.Bits()
	a := prefix.Addr().As4()
	hostBits := 32 - bits
	if hostBits < 2 {
		return "", fmt.Errorf("%s's mesh %s has no host addresses to pick from — a mesh is a /30 or wider", mesh.Ref(), mesh.Mesh.CIDR)
	}
	base := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	size := uint32(1) << hostBits
	for off := size - 2; off >= 1; off-- {
		v := base + off
		ip := netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}).String()
		if _, ok := taken[ip]; !ok {
			return ip, nil
		}
	}
	return "", fmt.Errorf("%s's mesh %s has no free address", mesh.Ref(), mesh.Mesh.CIDR)
}

// choosePort: --port when given (never 51820, not another link's);
// else the table's own; else the lowest port above 51820 no link of
// this host holds.
func (p *linkPlan) choosePort(flag int) (int, string, error) {
	used := map[int]string{}
	for _, n := range p.entry.LinkNames() {
		if n != p.name {
			used[p.entry.Links[n].Port] = "[links." + n + "]"
		}
	}
	switch {
	case flag == manifest.MeshPort:
		return 0, "", fmt.Errorf("--port %d is this host's own mesh port — a link listens beside it", flag)
	case flag < 0 || flag > 65535:
		return 0, "", fmt.Errorf("--port %d: want a UDP port", flag)
	case flag != 0:
		if other, ok := used[flag]; ok {
			return 0, "", fmt.Errorf("--port %d is %s's", flag, other)
		}
		return flag, "--port", nil
	case p.had != nil:
		return p.had.Port, "kept", nil
	}
	for port := manifest.MeshPort + 1; port < 65536; port++ {
		if _, ok := used[port]; !ok {
			return port, fmt.Sprintf("picked: the lowest free port beside %d", manifest.MeshPort), nil
		}
	}
	return 0, "", fmt.Errorf("no free port")
}

// changed lists the table's keys that differ from what the file has.
func (p *linkPlan) changed() []string {
	if p.had == nil {
		return nil
	}
	var out []string
	if p.had.Estate != p.link.Estate {
		out = append(out, "estate")
	}
	if p.had.Address != p.link.Address {
		out = append(out, "address "+p.had.Address+" → "+p.link.Address)
	}
	if p.had.Port != p.link.Port {
		out = append(out, fmt.Sprintf("port %d → %d", p.had.Port, p.link.Port))
	}
	if p.had.InterfaceName() != p.link.Interface {
		out = append(out, "interface "+p.had.InterfaceName()+" → "+p.link.Interface)
	}
	if p.had.ZoneName() != p.link.Zone {
		out = append(out, "zone "+p.had.ZoneName()+" → "+p.link.Zone)
	}
	if p.had.PrefixBits() != p.link.PrefixBits() {
		out = append(out, fmt.Sprintf("prefix %d → %d", p.had.PrefixBits(), p.link.PrefixBits()))
	}
	if p.had.PublicKey != p.link.PublicKey {
		out = append(out, "public_key")
	}
	return out
}

// manifestText is the manifest with the table as planned: appended
// whole when new (under a comment that says what it is); when the
// table exists, each key whose value moved is set in place, and a key
// whose value stands — spelled by hand, or left to its default — is
// not touched, so a re-run changes nothing.
func (p *linkPlan) manifestText(text string) string {
	table := "links." + p.name
	l := p.link
	if !tomlHasTable(text, table) {
		var b strings.Builder
		fmt.Fprintf(&b, "\n# This host on %s's mesh (fort link add; docs/links.md). The address is this\n", p.remote.est.Ref())
		fmt.Fprintf(&b, "# estate's claim until `fort estate link` there records it in that mesh/peers.json.\n")
		fmt.Fprintf(&b, "[%s]\n", table)
		fmt.Fprintf(&b, "estate = %s\n", tomlString(l.Estate))
		fmt.Fprintf(&b, "address = %s\n", tomlString(l.Address))
		fmt.Fprintf(&b, "port = %d\n", l.Port)
		fmt.Fprintf(&b, "interface = %s\n", tomlString(l.Interface))
		fmt.Fprintf(&b, "zone = %s\n", tomlString(l.Zone))
		if l.Prefix != 0 {
			fmt.Fprintf(&b, "prefix = %d\n", l.Prefix)
		}
		fmt.Fprintf(&b, "public_key = %s   # the public half of %s\n", tomlString(l.PublicKey), linkSecretKey(p.name))
		return appendBlock(text, b.String())
	}
	had := p.had
	if had == nil {
		had = &manifest.Link{}
	}
	for _, kv := range []struct {
		k, v  string
		stays bool
	}{
		{"estate", tomlString(l.Estate), had.Estate == l.Estate},
		{"address", tomlString(l.Address), had.Address == l.Address},
		{"port", fmt.Sprint(l.Port), had.Port == l.Port},
		{"interface", tomlString(l.Interface), had.Name != "" && had.InterfaceName() == l.Interface},
		{"zone", tomlString(l.Zone), had.Name != "" && had.ZoneName() == l.Zone},
		{"prefix", fmt.Sprint(l.PrefixBits()), had.PrefixBits() == l.PrefixBits()},
		{"public_key", tomlString(l.PublicKey), had.PublicKey == l.PublicKey},
	} {
		if !kv.stays {
			text = tomlSetInTable(text, table, kv.k, kv.v)
		}
	}
	return text
}

func (p *linkPlan) apply(tool secretsTool) error {
	// The key first: a write that fails after it leaves a secret with
	// no table, which a re-run restores; the other order leaves a
	// public key in the manifest with no private half anywhere.
	if p.keyMissing {
		if err := tool.SetWith(filepath.Join(p.root, "nixos/.sops.yaml"), filepath.Join(p.root, p.secretsPath), `["`+linkSecretKey(p.name)+`"]`, p.wg.private); err != nil {
			return err
		}
		if err := tool.GitAdd(p.secretsPath); err != nil {
			return err
		}
	}
	for _, e := range p.edits {
		if e.fn == nil {
			continue // a note, not an edit
		}
		full := filepath.Join(p.root, e.path)
		raw, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		out, err := e.fn(string(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", e.path, err)
		}
		if err := os.WriteFile(full, []byte(out), 0o644); err != nil {
			return err
		}
		if err := tool.GitAdd(e.path); err != nil {
			return err
		}
	}
	return nil
}

func (p *linkPlan) report(w io.Writer, written bool) {
	r := p.remote.est
	fmt.Fprintf(w, "link add ⛫ %s → %s   (mesh %s · %s · %s)\n", p.host, r.Ref(), orNone(r.Mesh.CIDR), p.link.Interface, p.link.Zone)
	fmt.Fprintf(w, "  address    %-16s %s\n", p.link.Address, p.addressWhy)
	fmt.Fprintf(w, "  port       %-16d %s\n", p.link.Port, p.portWhy)
	fmt.Fprintf(w, "  public key %s\n", p.wg.public)
	switch {
	case p.keyMissing && !written:
		fmt.Fprintf(w, "○ %-36s + %s (sops set)   (dry run: minted in memory and discarded — --write mints the real one)\n", p.secretsPath, linkSecretKey(p.name))
	case p.keyMissing:
		fmt.Fprintf(w, "○ %-36s + %s (sops set)\n", p.secretsPath, linkSecretKey(p.name))
	default:
		fmt.Fprintf(w, "○ %-36s %s — key restored from it\n", p.secretsPath, linkSecretKey(p.name))
	}
	for _, e := range p.edits {
		mark := '○'
		if e.fn == nil {
			mark = '!'
		}
		fmt.Fprintf(w, "%c %-36s %s\n", mark, e.path, e.note)
	}
	if len(p.edits) == 0 {
		fmt.Fprintf(w, "○ %-36s current\n", manifest.Path(p.host))
	}
	if len(p.roles) > 0 {
		fmt.Fprintf(w, "· roles %s — from [[serves.*]] in %s; the link opens their ports (docs/hub.md)\n", strings.Join(p.roles, ", "), estate.Path)
	} else {
		fmt.Fprintf(w, "! no [[serves.*]] entry in %s names %s for %s — the link opens no port; declare the role this host serves that estate (docs/hub.md)\n", estate.Path, p.host, r.Ref())
	}
	switch {
	case p.recorded == "":
		fmt.Fprintf(w, "· remotes.%s in %s: not recorded yet\n", p.host, r.Ref())
	case strings.HasPrefix(p.recorded, p.link.Address+","):
		fmt.Fprintf(w, "· remotes.%s in %s: recorded (%s)\n", p.host, r.Ref(), p.recorded)
	default:
		fmt.Fprintf(w, "! remotes.%s in %s: recorded as %s — differs; `fort estate link` there follows this table after the merge\n", p.host, r.Ref(), p.recorded)
	}
	fmt.Fprintln(w)
	if !written {
		fmt.Fprintln(w, "dry run — nothing written (link add --write).")
		return
	}
	fmt.Fprintf(w, "staged — commit the rendered files. next, in a checkout of %s: fort estate link %s --write\n", r.Ref(), p.est.Source.Repo)
}

func orNone(s string) string {
	if s == "" {
		return "no mesh declared"
	}
	return s
}

// abbrevKey is the first eight characters of a key, for a report that
// names it without quoting it whole.
func abbrevKey(k string) string {
	if len(k) <= 8 {
		return k
	}
	return k[:8] + "…"
}
