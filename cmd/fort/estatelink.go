package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gitea.zen.lofi/sfi/forts/internal/tools"
)

// fort estate <verb>: what an estate does toward another estate. One
// verbs: init (forts#197) and link (forts#57).
func estateCmd(args []string) error {
	if len(args) >= 1 && args[0] == "init" {
		return initCmd(args[1:])
	}
	if len(args) < 1 || args[0] != "link" {
		return fmt.Errorf("usage: fort estate (init [estate.toml] … | link <estate url> [--dry-run])")
	}
	return estateLink(args[1:])
}

// fort estate link <estate url> — the other half of a link (forts#55,
// #57; docs/links.md): record, in THIS estate's mesh/peers.json, the
// hosts of <estate> that link here. It fetches that estate's repo and
// reads every forts/*.toml for a [links.<name>] table whose estate is
// this one — the manifest, never that estate's Nix — and from each
// builds the remotes entry: the claimed address, the host's endpoint
// (its manifest's default), the link's port, the public key.
//
// The claim is verified before anything is written, because this file
// is the record and the other side only asked: the address lies in this
// estate's mesh; no peer, client, other remote or manifest here holds
// it; the link's interface and zone are this mesh's names. A collision
// refuses the whole run and names the holder — the fix is `fort link
// add` again on the other side, which picks another address. An entry
// already there with the same facts is current; one for the same host
// with other facts follows the manifest, and the report says what moved
// (a changed key is a hub that re-minted, said loudly). Nothing here
// removes an entry: a host that stopped linking is a hand edit, and the
// report names the candidates.
func estateLink(args []string) error {
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos = append(pos, args[0])
		args = args[1:]
	}
	fs := flag.NewFlagSet("estate", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the plan and write nothing (default: apply)")
	fs.Bool("write", true, "deprecated no-op: writing is the default")
	if err := fs.Parse(args); err != nil {
		return err
	}
	write := !*dryRun
	if len(pos) != 1 || fs.NArg() > 0 {
		return fmt.Errorf("usage: fort estate link <estate url> [--dry-run]")
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	tool := sopsCLI{root: root}
	p, err := planEstateLink(root, pos[0], gitFetch)
	if err != nil {
		return err
	}
	if write && p.errors == 0 {
		if err := p.apply(tool); err != nil {
			return err
		}
	}
	p.report(os.Stdout, write)
	if p.errors > 0 {
		return fmt.Errorf("%d claim(s) refused — nothing written", p.errors)
	}
	return nil
}

// remoteClaim is one host of the other estate that links here, judged.
type remoteClaim struct {
	host   string
	link   manifest.Link
	entry  meshPeer // the remotes entry as it will be
	status string   // "new" | "current" | "update" | "error"
	note   string
}

type estateLinkPlan struct {
	root   string
	est    estate.Estate  // this estate
	other  *fetchedEstate // the estate whose hosts link here
	local  mesh
	claims []remoteClaim
	stale  []string // remotes entries named like a fort of that estate that no longer links here
	errors int
}

func planEstateLink(root, repo string, fetch estateFetcher) (*estateLinkPlan, error) {
	est, err := estate.Load(root)
	if err != nil {
		return nil, err
	}
	if !estate.ValidRepoURL(repo) {
		return nil, fmt.Errorf("%q — want the other estate's git URL (ssh://git@<forge>:<port>/<owner>/<name>, or https://)", repo)
	}
	if estate.SameRepo(repo, est.Source.Repo) {
		return nil, fmt.Errorf("%s is this estate — its own forts are peers by fort mint; a link comes from ANOTHER estate's host", estate.Ref(repo))
	}
	local, err := loadMesh(root)
	if err != nil {
		return nil, err
	}
	hosts, _, err := manifest.LoadAll(root)
	if err != nil {
		return nil, err
	}
	other, cleanup, err := fetchEstate(fetch, repo)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	p := &estateLinkPlan{root: root, est: est, other: other, local: local}

	// What holds an address on this mesh, by holder — every table, and
	// every manifest's mesh_ip (a fort not minted yet).
	taken := map[string]string{}
	for _, n := range sortedNames(local.Peers) {
		taken[local.Peers[n].IP] = "peers." + n
	}
	for _, n := range sortedNames(local.Clients) {
		taken[local.Clients[n].IP] = "clients." + n
	}
	for _, n := range sortedNames(local.Remotes) {
		taken[local.Remotes[n].IP] = "remotes." + n
	}
	for _, n := range sortedNames(hosts) {
		if _, ok := taken[hosts[n].MeshIP]; !ok {
			taken[hosts[n].MeshIP] = manifest.Path(n) + " mesh_ip (not minted yet)"
		}
	}

	// The claims: every host of the other estate whose manifest links
	// to this estate, in name order.
	claimed := map[string]string{} // address → host, among this run's claims
	for _, host := range sortedNames(other.hosts) {
		h := other.hosts[host]
		l, ok := h.LinkTo(est.Source.Repo)
		if !ok {
			if _, there := local.Remotes[host]; there {
				p.stale = append(p.stale, host)
			}
			continue
		}
		c := remoteClaim{host: host, link: l, status: "new"}
		c.entry = meshPeer{IP: l.Address, Endpoint: h.Endpoint(), Port: l.Port, PublicKey: l.PublicKey, Estate: other.est.Source.Repo}
		refuse := func(why string) {
			c.status, c.note = "error", why
			p.errors++
		}
		have, recorded := local.Remotes[host]
		switch {
		case recorded && have.Estate != "" && !estate.SameRepo(have.Estate, c.entry.Estate):
			// The entry belongs to another estate: a second estate's
			// write would swap its address, endpoint and key (#101).
			refuse(fmt.Sprintf("remotes.%s belongs to %s — %s cannot take it over; rename the fort in one of the estates, or remove the entry by hand first", host, estate.Ref(have.Estate), other.est.Ref()))
		case c.entry.Endpoint == "":
			refuse(fmt.Sprintf("%s has no endpoint — a linked host is always-on and dialed; set mesh_endpoint, [libvirt] ip or [dns] in %s's %s", host, other.est.Ref(), manifest.Path(host)))
		case !manifest.ValidEndpoint(c.entry.Endpoint):
			// The value lands on an `Endpoint =` line of a root-level
			// WireGuard config (modules/wireguard.nix): one IP address or
			// one DNS name, nothing else, whatever that repo says (#99).
			refuse(fmt.Sprintf("%s's endpoint %q is not an IP address or a DNS name — nothing else is written to this mesh", host, c.entry.Endpoint))
		case !est.Contains(l.Address):
			refuse(fmt.Sprintf("%s is outside this estate's mesh %s", l.Address, est.Mesh.CIDR))
		case est.Mesh.Interface != "" && l.InterfaceName() != est.InterfaceName():
			refuse(fmt.Sprintf("the link's interface is %s and this mesh is %s (%s) — one name per mesh; fort link add there again reads it", l.InterfaceName(), est.InterfaceName(), estate.Path))
		case l.ZoneName() != est.ZoneName():
			refuse(fmt.Sprintf("the link's zone is %s and this mesh's is %s — one zone per mesh; fort link add there again reads it", l.ZoneName(), est.ZoneName()))
		case claimed[l.Address] != "":
			refuse(fmt.Sprintf("%s is also what %s claims in this run — two hosts of %s picked one address; fort link add again for one of them", l.Address, claimed[l.Address], other.est.Ref()))
		default:
			if holder, ok := taken[l.Address]; ok && holder != "remotes."+host {
				refuse(fmt.Sprintf("%s is %s's — the claim collided; in %s: fort link add %s %s again", l.Address, holder, other.est.Ref(), host, est.Source.Repo))
				break
			}
			claimed[l.Address] = host
			if recorded {
				if have == c.entry {
					c.status, c.note = "current", "recorded as the manifest says"
				} else {
					c.status, c.note = "update", strings.Join(peerDiff(have, c.entry), ", ")
				}
			}
		}
		p.claims = append(p.claims, c)
	}
	return p, nil
}

// peerDiff names the fields of a remotes entry that move.
func peerDiff(have, want meshPeer) []string {
	var out []string
	if have.IP != want.IP {
		out = append(out, "address "+have.IP+" → "+want.IP)
	}
	if have.Endpoint != want.Endpoint {
		out = append(out, "endpoint "+have.Endpoint+" → "+want.Endpoint)
	}
	if have.endpointPort() != want.endpointPort() {
		out = append(out, fmt.Sprintf("port %d → %d", have.endpointPort(), want.endpointPort()))
	}
	if have.PublicKey != want.PublicKey {
		out = append(out, "public key "+abbrevKey(have.PublicKey)+" → "+abbrevKey(want.PublicKey)+" (the host re-minted its link key)")
	}
	if have.Estate == "" && want.Estate != "" {
		out = append(out, "estate of origin "+estate.Ref(want.Estate)+" recorded")
	}
	return out
}

// writes is every claim apply records: new and updated entries.
func (p *estateLinkPlan) writes() []remoteClaim {
	var out []remoteClaim
	for _, c := range p.claims {
		if c.status == "new" || c.status == "update" {
			out = append(out, c)
		}
	}
	return out
}

func (p *estateLinkPlan) apply(tool secretsTool) error {
	if p.errors > 0 {
		return fmt.Errorf("%d claim(s) refused — nothing written", p.errors)
	}
	writes := p.writes()
	if len(writes) == 0 {
		return nil
	}
	full := filepath.Join(p.root, peersPath)
	raw, err := os.ReadFile(full)
	if err != nil {
		return err
	}
	text := string(raw)
	for _, c := range writes {
		if text, err = setRemote(text, c.host, c.entry); err != nil {
			return err
		}
	}
	if err := os.WriteFile(full, []byte(text), 0o644); err != nil {
		return err
	}
	return tool.GitAdd(peersPath)
}

// setRemote writes one remotes entry the way addPeer writes a peer:
// through jq, so the file keeps its formatting and key order, and the
// table is created when the file has none yet.
func setRemote(text, host string, e meshPeer) (string, error) {
	cmd, err := tools.Command("jq",
		"--arg", "n", host, "--arg", "ip", e.IP, "--arg", "ep", e.Endpoint, "--argjson", "port", fmt.Sprint(e.endpointPort()), "--arg", "pub", e.PublicKey, "--arg", "est", e.Estate,
		".remotes[$n] = {wg_vpn_ip: $ip, wg_endpoint_ip: $ep, wg_endpoint_port: $port, public_key: $pub, estate: $est}")
	if err != nil {
		return "", err
	}
	cmd.Stdin = strings.NewReader(text)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("jq: %s", exitDetail(err))
	}
	return string(out), nil
}

func (p *estateLinkPlan) report(w io.Writer, written bool) {
	o := p.other.est
	fmt.Fprintf(w, "estate link ← %s   (%d manifest(s) read; %d link(s) to %s)\n", o.Ref(), len(p.other.hosts), len(p.claims), p.est.Ref())
	for _, h := range sortedNames(p.other.problems) {
		fmt.Fprintf(w, "! %-36s does not parse there: %v\n", fmt.Sprintf("%q", manifest.Path(h)), p.other.problems[h])
	}
	if len(p.claims) == 0 {
		fmt.Fprintf(w, "  no fort of %s links to this estate — in a checkout of it: fort link add <fort> %s, then PR, merge, and this verb again\n", o.Ref(), p.est.Source.Repo)
	}
	for _, c := range p.claims {
		facts := fmt.Sprintf("%-15s endpoint %s:%d  key %s", c.entry.IP, c.entry.Endpoint, c.entry.endpointPort(), abbrevKey(c.entry.PublicKey))
		switch c.status {
		case "new":
			fmt.Fprintf(w, "  %-14s %s   + remotes.%s\n", c.host, facts, c.host)
		case "current":
			fmt.Fprintf(w, "  %-14s %s   current\n", c.host, facts)
		case "update":
			fmt.Fprintf(w, "  %-14s %s   ~ remotes.%s: %s\n", c.host, facts, c.host, c.note)
		default:
			fmt.Fprintf(w, "  %-14s %s   ✗ %s\n", c.host, facts, c.note)
		}
	}
	for _, h := range p.stale {
		fmt.Fprintf(w, "! %-36s remotes.%s: %s has a fort %s with no link to this estate — stale? removing it is a hand edit\n", peersPath, h, o.Ref(), h)
	}
	writes := p.writes()
	switch {
	case p.errors > 0:
		fmt.Fprintf(w, "✗ %-36s unchanged — %d claim(s) refused above\n", peersPath, p.errors)
	case len(writes) == 0 && len(p.claims) > 0:
		fmt.Fprintf(w, "○ %-36s current\n", peersPath)
	case len(writes) > 0:
		var names []string
		for _, c := range writes {
			names = append(names, "remotes."+c.host)
		}
		fmt.Fprintf(w, "○ %-36s %s\n", peersPath, strings.Join(names, ", "))
	}
	fmt.Fprintln(w)
	switch {
	case p.errors > 0:
		return
	case !written:
		fmt.Fprintln(w, "dry run — nothing written.")
	case len(writes) == 0:
		fmt.Fprintln(w, "nothing to write.")
	default:
		fmt.Fprintln(w, "staged — commit the rendered files.")
	}
}
