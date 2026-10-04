package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

// The client side of the mesh (#434): what a machine needs in order to
// REACH the peers, rendered from the same source of truth the peers
// themselves are built from. mesh/peers.json says who is on the
// mesh and where; the manifests say what each fort is, and so which
// account answers on 22. Three renders come out of that:
//
//   - mesh/ssh_config   one Host block per peer — the fort name, a
//                            stable role alias where one fort holds the
//                            role (sfi-dev, sfi-prod, sfi-stage), the mesh
//                            address, the account (deployer). Committed;
//                            consumers Include it. mint and dismantle
//                            re-render it, fort check --all fails when it
//                            is stale.
//   - mesh/known_hosts  one line per fort — its ssh host key, which
//                            mint has in hand (ssh-host-key.pub, otherwise
//                            sops-encrypted in nixos/secrets/<host>.yaml).
//                            First login is prompt-free; a replaced fort
//                            never trips "HOST IDENTIFICATION HAS CHANGED"
//                            because the key is looked up by fort name
//                            (HostKeyAlias), and the name carries the sha.
//   - `fort wg-conf <member>` a member's own wg0 as the WireGuard app wants
//                            it pasted — every other peer, the endpoints,
//                            the keepalives — with the PrivateKey line left
//                            to the device: the key never leaves it, and
//                            this CLI never handles one.
//
// The account comes from the manifest, never from peers.json: a fort
// answers as deployer, the operator account the framework gives every
// host (ops/deployer.nix), and a peer with no manifest is a hand-managed
// interactive host — steven's (the ubuntu holdouts; no NixOS interactive
// profile exists). peers.json stays three fields per peer, plus the
// endpoint port where it is not the default; nothing is added to it
// here. An account the estate adds to a fort is reached from
// the client's own config, above the Include. A role alias two forts
// claim is suspended: a replacement raised before the old fort is
// dismantled must not send a verb to either one.

const (
	sshConfigPath  = "mesh/ssh_config"
	knownHostsPath = "mesh/known_hosts"
	peersPath      = "mesh/peers.json"

	userDeployer    = "deployer"
	userInteractive = "steven"

	wgPort = 51820
)

// meshPeer is one peers.json entry (any table). The field set is the
// file's and nothing more — see docs/mesh.md. Port is the endpoint's
// UDP port when it is not the mesh default: a host linked from another
// estate listens for THIS mesh on a port of its own, beside its own
// mesh's (deployments#674).
type meshPeer struct {
	IP        string `json:"wg_vpn_ip"`
	Endpoint  string `json:"wg_endpoint_ip,omitempty"`
	Port      int    `json:"wg_endpoint_port,omitempty"`
	PublicKey string `json:"public_key"`
}

// endpointPort is the port the others dial: the entry's own, else the
// mesh default every fort listens on.
func (p meshPeer) endpointPort() int {
	if p.Port != 0 {
		return p.Port
	}
	return wgPort
}

// mesh is peers.json decoded: peers are mesh members (keepalive both
// ways, an Endpoint where one is stable), clients roaming devices the
// members accept but never dial, remotes the hosts of OTHER estates that
// link to this mesh (deployments#674) — dialed like a peer, with the
// endpoint and port the link gave them, but not a fort of this estate:
// no ssh_config block, no manifest, no known_hosts line. The linked
// host's own peer set comes from its reconciler reading this file.
type mesh struct {
	Peers   map[string]meshPeer `json:"peers"`
	Clients map[string]meshPeer `json:"clients"`
	Remotes map[string]meshPeer `json:"remotes,omitempty"`
}

func parseMesh(text string) (mesh, error) {
	var m mesh
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		return m, fmt.Errorf("%s: %w", peersPath, err)
	}
	if m.Peers == nil {
		m.Peers = map[string]meshPeer{}
	}
	if m.Clients == nil {
		m.Clients = map[string]meshPeer{}
	}
	if m.Remotes == nil {
		m.Remotes = map[string]meshPeer{}
	}
	for name, r := range m.Remotes {
		if r.Endpoint == "" {
			return m, fmt.Errorf("%s: remotes.%s has no wg_endpoint_ip — a linked host is always-on and dialed; nothing learns its endpoint otherwise", peersPath, name)
		}
	}
	return m, nil
}

func loadMesh(root string) (mesh, error) {
	raw, err := os.ReadFile(filepath.Join(root, peersPath))
	if err != nil {
		return mesh{}, err
	}
	return parseMesh(string(raw))
}

func sortedNames[T any](m map[string]T) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// --- ssh_config

// sshUser is the account that answers on a peer's 22 under the peer's own
// name (the header of this file has the why): deployer on every fort, the
// operator account the framework gives each host; an interactive user on
// a hand-managed peer. An estate that adds accounts to a fort reaches them
// by its own ssh config, above the Include.
func sshUser(entry *manifest.Host) string {
	if entry == nil {
		return userInteractive
	}
	return userDeployer
}

// roleStemRE: a fort named by the convention <role>-<7-hex sha> (2026-09-12:
// sfi-prod-439bf72, sfi-stage-d95cc36, sfi-dev-831c637). The stem is the
// role; sfi-fort-zenbook has none and gets no alias.
var roleStemRE = regexp.MustCompile(`^(.+)-[0-9a-f]{7}$`)

func roleStem(host string) string {
	if m := roleStemRE.FindStringSubmatch(host); m != nil {
		return m[1]
	}
	return ""
}

// roleAliases maps fort → its role alias, for every role exactly one
// fort holds. `ssh sfi-dev` survives the dev fort being replaced — which
// it is, by design — because the alias follows whichever fort carries
// the stem, and a second fort with the same stem (a replacement raised
// before the old one is dismantled) suspends the alias rather than
// pointing it at either.
func roleAliases(hosts map[string]manifest.Host) map[string]string {
	byStem := map[string][]string{}
	for h := range hosts {
		if s := roleStem(h); s != "" {
			byStem[s] = append(byStem[s], h)
		}
	}
	out := map[string]string{}
	for s, hs := range byStem {
		if len(hs) == 1 {
			out[hs[0]] = s
		}
	}
	return out
}

const sshConfigHeader = `# SSH over the SFI mesh — rendered by ` + "`fort ssh-config`" + ` from
# mesh/peers.json + forts/*.toml. Do not edit: fort mint
# and fort dismantle re-render it, and fort check --all fails when it is
# stale. docs/ssh-config.md.
#
# Use it from ~/.ssh/config — anything above the Include wins, which is
# where a client's own overrides go (an off-mesh machine's ProxyJump, a
# LAN address for a host it reaches without the tunnel):
#
#   Include ~/deployments/mesh/ssh_config
#   UserKnownHostsFile ~/.ssh/known_hosts ~/deployments/mesh/known_hosts
#
# Every fort answers as deployer, the operator account the framework
# gives each host; a peer with no manifest is a hand-managed interactive
# host (steven). A role alias (sfi-dev, sfi-prod, …) exists while exactly
# one fort claims the stem. Other accounts on a fort are the estate's:
# reach them from the client's own config, above the Include.
`

// renderSSHConfig is the whole file: every mesh peer in name order (the
// clients table is roaming devices, not ssh targets). Comments sit on
// their own lines — ssh_config has no trailing comments.
func renderSSHConfig(m mesh, hosts map[string]manifest.Host) string {
	aliases := roleAliases(hosts)
	var sb strings.Builder
	sb.WriteString(sshConfigHeader)
	for _, name := range sortedNames(m.Peers) {
		p := m.Peers[name]
		entry, isFort := hosts[name]
		sb.WriteString("\n")
		patterns := name
		if isFort {
			kind := "cluster fort"
			if !entry.IsCluster() {
				kind = "plain fort"
			}
			if entry.Provider != "" {
				kind += ", " + entry.Provider
			}
			if a := aliases[name]; a != "" {
				patterns = a + " " + name
				kind += " — the " + a + " role"
			}
			fmt.Fprintf(&sb, "# %s\n", kind)
			fmt.Fprintf(&sb, "Host %s\n", patterns)
			fmt.Fprintf(&sb, "  Hostname %s\n", p.IP)
			fmt.Fprintf(&sb, "  User %s\n", sshUser(&entry))
			// The host key is looked up by the fort's name whichever
			// alias or address was typed: the name carries the sha, so a
			// successor at a reused mesh address is a new key under a new
			// name, not a changed key under an old one.
			fmt.Fprintf(&sb, "  HostKeyAlias %s\n", name)
			continue
		}
		sb.WriteString("# hand-managed peer (no manifest)\n")
		fmt.Fprintf(&sb, "Host %s\n", patterns)
		fmt.Fprintf(&sb, "  Hostname %s\n", p.IP)
		fmt.Fprintf(&sb, "  User %s\n", sshUser(nil))
	}
	return sb.String()
}

// sshConfigLine is the one-line summary a report prints for a fort's
// block ("Host sfi-dev sfi-dev-831c637 → deployer@10.1.0.61").
func sshConfigLine(host string, hosts map[string]manifest.Host, ip string) string {
	entry := hosts[host]
	patterns := host
	if a := roleAliases(hosts)[host]; a != "" {
		patterns = a + " " + host
	}
	return fmt.Sprintf("Host %s → %s@%s", patterns, sshUser(&entry), ip)
}

// renderSSHConfigFrom renders from the tree: peers.json as given (a
// plan's post-edit text) and the manifests on disk, minus `without` (a
// host whose manifest this plan removes). A manifest that fails to parse
// is left out here and is fort check's finding.
func renderSSHConfigFrom(root, peersText, without string) (string, map[string]manifest.Host, error) {
	m, err := parseMesh(peersText)
	if err != nil {
		return "", nil, err
	}
	hosts, _, err := manifest.LoadAll(root)
	if err != nil {
		return "", nil, err
	}
	delete(hosts, without)
	return renderSSHConfig(m, hosts), hosts, nil
}

// --- known_hosts

const knownHostsHeader = `# SSH host keys of the forts — rendered by fort mint (the key it minted or
# restored) and fort ssh-config (the key sops-decrypted from
# nixos/secrets/<host>.yaml); fort dismantle removes a line. Do not edit.
# Keyed by fort name (ssh_config sets HostKeyAlias) and mesh address.
`

type knownHost struct {
	host, ip string
	key      string // "<type> <base64>"
}

// parseKnownHosts reads the file's lines by fort name: the first field
// is "<host>,<ip>", the rest the key. Comments and blank lines pass.
func parseKnownHosts(text string) map[string]knownHost {
	out := map[string]knownHost{}
	for _, l := range strings.Split(text, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		names := strings.SplitN(f[0], ",", 2)
		kh := knownHost{host: names[0], key: f[1] + " " + f[2]}
		if len(names) == 2 {
			kh.ip = names[1]
		}
		out[kh.host] = kh
	}
	return out
}

// hostKeyLine reduces an authorized_keys-style public line ("ssh-ed25519
// AAAA… comment\n") to the two fields known_hosts wants.
func hostKeyLine(pub string) string {
	f := strings.Fields(pub)
	if len(f) < 2 {
		return strings.TrimSpace(pub)
	}
	return f[0] + " " + f[1]
}

// renderKnownHosts writes the entries in name order under the header —
// the file is regenerated whole, never appended to, so its order and
// header never drift.
func renderKnownHosts(entries map[string]knownHost) string {
	var sb strings.Builder
	sb.WriteString(knownHostsHeader)
	for _, h := range sortedNames(entries) {
		e := entries[h]
		names := e.host
		if e.ip != "" {
			names += "," + e.ip
		}
		fmt.Fprintf(&sb, "%s %s\n", names, e.key)
	}
	return sb.String()
}

func knownHostsSet(text, host, ip, pub string) string {
	entries := parseKnownHosts(text)
	entries[host] = knownHost{host: host, ip: ip, key: hostKeyLine(pub)}
	return renderKnownHosts(entries)
}

func knownHostsRemove(text, host string) string {
	entries := parseKnownHosts(text)
	delete(entries, host)
	return renderKnownHosts(entries)
}

// readOrEmpty is a file's text, "" when it does not exist yet — the
// rendered files are created by the first mint that needs them.
func readOrEmpty(root, rel string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, rel))
	if os.IsNotExist(err) {
		return "", nil
	}
	return string(raw), err
}

// --- wg-conf

// renderWGConf is a member's own wg0 the way wireguard.nix builds a
// fort's: every other peer with a keepalive and, where it has one, an
// Endpoint; every client with neither. The [Interface] carries no
// PrivateKey — the WireGuard app generated that key on the device and
// registered its public half in peers.json; the line the app wrote is
// the one to keep. A client (roaming) member has no ListenPort — nobody
// dials it.
func renderWGConf(m mesh, member string) (string, error) {
	self, isPeer := m.Peers[member]
	if !isPeer {
		var ok bool
		if self, ok = m.Clients[member]; !ok {
			return "", fmt.Errorf("%s is not in %s — register it first (docs/mesh.md, \"Joining\"): the address and public key come from there", member, peersPath)
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "# wg0 for %s — rendered by `fort wg-conf %s` from %s.\n", member, member, peersPath)
	sb.WriteString("# Paste below the PrivateKey line the WireGuard app generated (Add Empty\n")
	sb.WriteString("# Tunnel): that key never leaves the device, and its public half is the one\n")
	fmt.Fprintf(&sb, "# peers.json carries for %s:\n#   %s\n", member, self.PublicKey)
	sb.WriteString("# Re-render after any peers.json change; the app has no other way to learn it.\n")
	sb.WriteString("\n[Interface]\n")
	// The one place the subnet is written on a fort is its Address /24;
	// same here.
	fmt.Fprintf(&sb, "Address = %s/24\n", self.IP)
	if isPeer {
		// A mesh peer is dialed by the peers that know its endpoint and
		// by the ones that learn it from its keepalives; either way the
		// port must hold still.
		fmt.Fprintf(&sb, "ListenPort = %d\n", wgPort)
	}
	sb.WriteString("# split DNS for .lofi over the mesh (soundship's dnsmasq); a machine on the\n")
	sb.WriteString("# lab LAN already resolves it there and can leave this out\n")
	sb.WriteString("# DNS = 10.1.0.1, lofi\n")

	for _, name := range sortedNames(m.Peers) {
		if name == member {
			continue
		}
		p := m.Peers[name]
		fmt.Fprintf(&sb, "\n[Peer]\n# %s\n", name)
		fmt.Fprintf(&sb, "PublicKey = %s\n", p.PublicKey)
		fmt.Fprintf(&sb, "AllowedIPs = %s/32\n", p.IP)
		switch {
		case p.Endpoint != "":
			fmt.Fprintf(&sb, "Endpoint = %s:%d\n", p.Endpoint, p.endpointPort())
		case isPeer && self.Endpoint != "":
			sb.WriteString("# no fixed endpoint: it dials this host (learned from our keepalives)\n")
		case isPeer:
			sb.WriteString("# no fixed endpoint on either side: no direct path until one of the two\n")
			sb.WriteString("# gets a wg_endpoint_ip in peers.json (a LAN address for an always-on host)\n")
		default:
			// A roaming client and a NAT'd peer have no path to each
			// other unless one is given by hand — the router's DDNS →
			// soundship forward is that path, and it is not in peers.json.
			sb.WriteString("# no fixed endpoint in peers.json: a roaming client reaches it only through\n")
			sb.WriteString("# a hand-set Endpoint (the router's DDNS forward, for soundship)\n")
		}
		// Both shapes keep the NAT mapping pinned: a peer so the others
		// can reach it, a roaming client because nothing else will.
		sb.WriteString("PersistentKeepalive = 25\n")
	}
	if isPeer {
		for _, name := range sortedNames(m.Clients) {
			c := m.Clients[name]
			fmt.Fprintf(&sb, "\n[Peer]\n# %s (roaming client: no endpoint, no keepalive)\n", name)
			fmt.Fprintf(&sb, "PublicKey = %s\n", c.PublicKey)
			fmt.Fprintf(&sb, "AllowedIPs = %s/32\n", c.IP)
		}
	}
	// A linked host of another estate: dialed at the endpoint and port
	// the link gave it, kept alive like a peer. Every member dials it,
	// a roaming client too — it is the gitea or the hub this mesh reaches.
	for _, name := range sortedNames(m.Remotes) {
		r := m.Remotes[name]
		fmt.Fprintf(&sb, "\n[Peer]\n# %s (linked from another estate)\n", name)
		fmt.Fprintf(&sb, "PublicKey = %s\n", r.PublicKey)
		fmt.Fprintf(&sb, "AllowedIPs = %s/32\n", r.IP)
		fmt.Fprintf(&sb, "Endpoint = %s:%d\n", r.Endpoint, r.endpointPort())
		sb.WriteString("PersistentKeepalive = 25\n")
	}
	return sb.String(), nil
}

// meshSubnetFindings: when estate.toml declares the mesh, every address
// in peers.json — a fort's, a client's, a linked host's — is inside it,
// and so is every manifest's mesh_ip. Two estates whose meshes overlap
// cannot link (deployments#674); the subnet is where that is caught,
// before a host is born into it.
func meshSubnetFindings(root string, est estate.Estate, hosts map[string]manifest.Host) []finding {
	if est.Mesh == "" {
		return nil
	}
	peersText, err := readOrEmpty(root, peersPath)
	if err != nil {
		return nil // meshFindings reports it
	}
	m, err := parseMesh(peersText)
	if err != nil {
		return nil
	}
	var out []finding
	n := 0
	check := func(table, name, ip string) {
		n++
		if !est.Contains(ip) {
			out = append(out, finding{"error", peersPath, fmt.Sprintf("%s.%s is %s, outside the estate's mesh %s (estate.toml)", table, name, ip, est.Mesh)})
		}
	}
	for _, name := range sortedNames(m.Peers) {
		check("peers", name, m.Peers[name].IP)
	}
	for _, name := range sortedNames(m.Clients) {
		check("clients", name, m.Clients[name].IP)
	}
	for _, name := range sortedNames(m.Remotes) {
		check("remotes", name, m.Remotes[name].IP)
	}
	for _, h := range sortedNames(hosts) {
		if !est.Contains(hosts[h].MeshIP) {
			out = append(out, finding{"error", manifest.Path(h), fmt.Sprintf("mesh_ip %s is outside the estate's mesh %s (estate.toml)", hosts[h].MeshIP, est.Mesh)})
		}
	}
	if len(out) == 0 {
		out = append(out, finding{"ok", estate.Path, fmt.Sprintf("mesh %s — %d address(es) inside it", est.Mesh, n)})
	}
	return out
}

// --- the gate's view (fort check --all)

// meshFindings: the committed ssh_config is what the tree renders, and
// known_hosts names every fort — with the mesh address peers.json gives
// it — and nothing that is not a fort. A missing key line is a warning,
// not an error: verifying the key needs the admin age key, which CI does
// not hold, and a fort minted before this file existed gets its line
// from the next `fort ssh-config --write` run with the key.
func meshFindings(root string, hosts map[string]manifest.Host) []finding {
	var out []finding
	peersText, err := readOrEmpty(root, peersPath)
	if err != nil {
		return []finding{{"error", peersPath, err.Error()}}
	}
	m, err := parseMesh(peersText)
	if err != nil {
		return []finding{{"error", peersPath, err.Error()}}
	}
	want := renderSSHConfig(m, hosts)
	have, err := readOrEmpty(root, sshConfigPath)
	switch {
	case err != nil:
		out = append(out, finding{"error", sshConfigPath, err.Error()})
	case have == "":
		out = append(out, finding{"error", sshConfigPath, "missing — `fort ssh-config --write` renders it from peers.json + the manifests"})
	case have != want:
		out = append(out, finding{"error", sshConfigPath, "stale — differs from what peers.json + the manifests render; `fort ssh-config --write`"})
	default:
		out = append(out, finding{"ok", sshConfigPath, fmt.Sprintf("%d peer(s) rendered", len(m.Peers))})
	}
	khText, err := readOrEmpty(root, knownHostsPath)
	if err != nil {
		return append(out, finding{"error", knownHostsPath, err.Error()})
	}
	entries := parseKnownHosts(khText)
	missing := 0
	for _, h := range sortedNames(hosts) {
		p, onMesh := m.Peers[h]
		if !onMesh {
			continue // mint has not run; nix-eval is that finding
		}
		e, ok := entries[h]
		switch {
		case !ok:
			missing++
			out = append(out, finding{"warning", knownHostsPath, fmt.Sprintf("no host key line for %s — `fort ssh-config --write` (with the admin age key) or `fort mint %s --write` adds it", h, h)})
		case e.ip != p.IP:
			out = append(out, finding{"error", knownHostsPath, fmt.Sprintf("%s is keyed to %s but peers.json says %s — `fort ssh-config --write`", h, e.ip, p.IP)})
		}
	}
	for _, h := range sortedNames(entries) {
		if _, ok := hosts[h]; !ok {
			out = append(out, finding{"error", knownHostsPath, fmt.Sprintf("line for %s, which has no manifest — dismantle removes it; `fort ssh-config --write`", h)})
		}
	}
	if khText != "" && missing == 0 {
		out = append(out, finding{"ok", knownHostsPath, fmt.Sprintf("%d fort key(s)", len(entries))})
	}
	return out
}
