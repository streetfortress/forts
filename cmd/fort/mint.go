package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gitea.zen.lofi/sfi/forts/internal/tools"
	"gopkg.in/yaml.v3"
)

// mint is the identity step — what provisioning/preseed-identity.sh
// (create-nixos's Phase A + cluster-init) did, as a verb: the host's
// ssh key (which doubles as its age identity), its wireguard key and
// mesh peer, the k3s token, and for a flux cluster the cluster age key,
// the flux deploy key registered read-only on the forge, and the sops
// canary; and, where the host file enables the console door, the
// console password in its two halves (console.go, sfi/forts#24). A pure
// repo mutation plus that one API call.
//
// The repo is the key store. Every private half is generated in memory
// and lands only in nixos/secrets/<host>.yaml, encrypted to the admin
// recipients and the host's own; nothing is written in the clear except
// that file for the instant before sops encrypts it (0600, removed if
// encryption fails). A re-run first reads what the secrets file already
// holds and only fills genuine gaps — rotation is a deliberate act,
// never a side effect — so a re-mint after `fort dismantle` + scaffold
// gives the host back its identity, and can never mint an orphan key
// or trip the deploy-key mismatch guard. No value is ever printed.
//
// Runs after `fort scaffold <host> --write` (the canary's home and the
// flake's view of the host) and before a layer's verbs. `fort dismantle` is
// the inverse.
func mint(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: fort mint <host> [--write]")
	}
	host := args[0]
	fs := flag.NewFlagSet("mint", flag.ContinueOnError)
	write := fs.Bool("write", false, "mint, encrypt, register and stage (default: print the plan)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	tool := sopsCLI{root: root}
	// The flux deploy key is registered on the forge (forge.go; the
	// token lives in sops).
	f, err := forgeClient(root)
	if err != nil {
		return err
	}
	p, err := planMint(root, host, tool, f, *write)
	if err != nil {
		return err
	}
	if *write {
		if err := p.apply(tool, f); err != nil {
			return err
		}
	}
	p.report(os.Stdout, *write)
	return nil
}

// Keys of nixos/secrets/<host>.yaml, in the order the file is written.
const (
	keyK3sToken   = "k3s-token"
	keyWgPrivate  = "wireguard-private-key"
	keySSHHost    = "ssh-host-key"
	keySSHHostPub = "ssh-host-key.pub"
	keyAge        = "flux-sops-age-key"
	keyDeploy     = "flux-git-deploy-key"
)

type mintPlan struct {
	est        estate.Estate // estate.toml: the forge and the roles
	root, host string
	entry      manifest.Host
	endpoint   string // wireguard endpoint, "" = none (a peer others dial out to is optional)

	// The identity. Private halves live here until apply encrypts them.
	hostKey   sshKey
	wg        wgKey
	cluster   ageKey
	deploy    sshKey
	k3sToken  string
	hostRecip string // ssh-to-age of hostKey

	secretsPath string            // repo-relative
	have        map[string]string // what the secrets file already holds
	missing     []string          // keys apply adds (all of them when the file is new)
	newFile     bool

	edits      []dEdit  // text edits (rules, peer), with "exists" ones filtered out
	sshConfig  string   // mesh/ssh_config re-rendered with this peer ("" = already current)
	sshNote    string   // the fort's Host block, one line, for the report
	knownHosts string   // mesh/known_hosts with this fort's host key ("" = already current)
	rekey      []string // flux/infrastructure sops files that gain the cluster recipient
	canaryPath string   // "" = not a flux cluster or already there
	admins     map[string][]string
	forge      string // the forge's kind (estate.ForgeKind), for the report
	deployKey  string // the deploy key's standing on the forge
	registerOn bool   // apply must POST the deploy key

	write bool // --write: the plan may be applied, so a missing credential is fatal now rather than later

	// The console door (console.go, sfi/forts#24): minted with the
	// identity when the host file enables sfi.ops.console. The hash is
	// one more key of the secrets file; the passphrase goes to the
	// registry, which apply writes first.
	console       bool   // the host's default.nix enables the module
	consoleMint   bool   // apply mints the passphrase and the hash
	consoleOrphan bool   // a hash with no passphrase behind it: console-password --rotate is the repair
	newRegistry   bool   // the registry does not exist yet
	consoleHash   string // filled by apply, and only there — a dry run mints nothing

}

// planMint reads the plan: f is the estate's forge (forge.go), shared
// with fort destroy. Every plan needs one — the deploy key's standing
// is read before anything is written.
func planMint(root, host string, tool secretsTool, f forge, write bool) (*mintPlan, error) {
	entry, err := manifest.Load(root, host)
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, fmt.Errorf("host %q: the flux deploy key needs the forge client", host)
	}
	est, err := estate.Load(root)
	if err != nil {
		return nil, err
	}
	p := &mintPlan{root: root, host: host, entry: entry, est: est, forge: f.Kind(), write: write, have: map[string]string{}, admins: map[string][]string{}}
	p.endpoint = entry.Endpoint()

	// Gate: the scaffold ran. Without it the canary has no home and the
	// flake would not see the host.
	var need []string
	if !exists(root, filepath.Join("nixos/hosts", host, "default.nix")) {
		need = append(need, filepath.Join("nixos/hosts", host, "default.nix"))
	}
	if entry.IsCluster() {
		for _, rel := range []string{filepath.Join("flux/apps", host, "kustomization.yaml"), filepath.Join("flux/clusters", host, "apps.yaml")} {
			if !exists(root, rel) {
				need = append(need, rel)
			}
		}
	}
	if len(need) > 0 {
		return nil, fmt.Errorf("missing %s — run `fort scaffold %s --write` first", strings.Join(need, ", "), host)
	}
	hostNix, err := os.ReadFile(filepath.Join(root, "nixos/hosts", host, "default.nix"))
	if err != nil {
		return nil, err
	}
	p.console = consoleModuleRE.Match(hostNix)

	// 1. what the repo already holds for this host
	p.secretsPath = filepath.Join("nixos/secrets", host+".yaml")
	if exists(root, p.secretsPath) {
		raw, err := tool.Decrypt(filepath.Join(root, p.secretsPath))
		if err != nil {
			return nil, err
		}
		if err := yaml.Unmarshal(raw, &p.have); err != nil {
			return nil, fmt.Errorf("%s: %w", p.secretsPath, err)
		}
	} else {
		p.newFile = true
	}
	take := func(key string) (string, bool) {
		v, ok := p.have[key]
		if !ok {
			p.missing = append(p.missing, key)
		}
		return v, ok
	}

	// 2. the identity: restored where the file has it, minted where not.
	// A plain fort (no [cluster]) has no cluster: no k3s token, no cluster
	// age key, no fleet rule, no canary. Host key, wireguard key and the
	// deploy key (auto-rebuild's pull credential) are every fort's.
	if entry.IsCluster() {
		if v, ok := take(keyK3sToken); ok {
			p.k3sToken = v
		} else if p.k3sToken, err = newToken(); err != nil {
			return nil, err
		}
	}
	if v, ok := take(keyWgPrivate); ok {
		p.wg, err = wgKeyFrom(v)
	} else {
		p.wg, err = newWGKey()
	}
	if err != nil {
		return nil, err
	}
	if v, ok := take(keySSHHost); ok {
		p.hostKey, err = sshKeyFrom(v, host)
	} else {
		p.hostKey, err = newSSHKey(host)
	}
	if err != nil {
		return nil, err
	}
	if pub, ok := p.have[keySSHHostPub]; ok && strings.TrimSpace(pub) != strings.TrimSpace(p.hostKey.public) {
		return nil, fmt.Errorf("%s: %s does not match %s", p.secretsPath, keySSHHostPub, keySSHHost)
	} else if !ok {
		p.missing = append(p.missing, keySSHHostPub)
	}
	if p.hostRecip, err = sshToAge(p.hostKey.public); err != nil {
		return nil, err
	}
	if entry.IsCluster() {
		if v, ok := take(keyAge); ok {
			p.cluster, err = ageKeyFrom(v)
		} else {
			p.cluster, err = newAgeKey()
		}
		if err != nil {
			return nil, err
		}
	}
	if v, ok := take(keyDeploy); ok {
		p.deploy, err = sshKeyFrom(v, "flux-"+host)
	} else {
		p.deploy, err = newSSHKey("flux-" + host)
	}
	if err != nil {
		return nil, err
	}

	// 2b. the console door (sfi/forts#24): the host file enables the
	// module, so the credential is part of the identity — the hash in
	// the secrets file beside the keys, the passphrase in the registry.
	// Restore-not-rotate holds here too: a hash already in the file is
	// kept, even when its passphrase is gone from the registry, because
	// somebody may still hold the value it was made from —
	// console-password --rotate is the repair, and the report says so. A
	// host that does not enable the module gets nothing: a credential no
	// getty reads is what `fort check` warns about. Nothing is hashed
	// until apply, so a dry run needs no mkpasswd.
	if p.console {
		if _, ok := take(keyDeployerPasswordHash); !ok {
			p.consoleMint = true
			p.newRegistry = !exists(root, consoleRegistry)
		} else if !sopsHasKey(filepath.Join(root, consoleRegistry), host) {
			p.consoleOrphan = true
		}
	}
	// 3. the rules and the peer — each only if absent, as text edits
	nixosSops, err := os.ReadFile(filepath.Join(root, "nixos/.sops.yaml"))
	if err != nil {
		return nil, err
	}
	if block := markerBlock(string(nixosSops), host); block != "" {
		// The rule predates this run: the host's identity is whatever it
		// names. A derived recipient that differs means the real host key
		// is not in the secrets file (a playbook-era host whose key lived
		// on the hypervisor) or the rule is stale — either way not ours
		// to paper over with a fresh key.
		if !strings.Contains(block, p.hostRecip) {
			return nil, fmt.Errorf("nixos/.sops.yaml already has a rule for %s but its host recipient is not the one %s derives (%s) — the host's real ssh key is not in the secrets file (import it: sops --config nixos/.sops.yaml set %s '[\"%s\"]' …), or the rule is stale; a re-key is a deliberate act", host, keySSHHost, p.hostRecip, p.secretsPath, keySSHHost)
		}
	} else {
		p.admins["nixos/.sops.yaml"] = sharedRecipients(string(nixosSops))
		recips := append(append([]string{}, p.admins["nixos/.sops.yaml"]...), p.hostRecip)
		p.edits = append(p.edits, dEdit{"nixos/.sops.yaml", fmt.Sprintf("+ rule secrets/%s.yaml → %d shared recipient(s) + host %s", host, len(recips)-1, p.hostRecip),
			func(s string) (string, error) { return addHostRule(s, host, recips) }})
	}
	peers, err := os.ReadFile(filepath.Join(root, "mesh/peers.json"))
	if err != nil {
		return nil, err
	}
	if pub, ok := peerPublicKey(string(peers), host); ok {
		if pub != p.wg.public {
			return nil, fmt.Errorf("mesh/peers.json already lists %s with a public key that is not the one %s derives — the real wireguard key is not in the secrets file, or the peer is stale; a re-key is a deliberate act", host, keyWgPrivate)
		}
	} else {
		note := "+ .peers." + host + " (" + entry.MeshIP
		if p.endpoint != "" {
			note += ", endpoint " + p.endpoint
		}
		p.edits = append(p.edits, dEdit{"mesh/peers.json", note + ")",
			func(s string) (string, error) { return addPeer(s, host, entry.MeshIP, p.endpoint, p.wg.public) }})
	}
	if err := p.planMesh(string(peers)); err != nil {
		return nil, err
	}
	if !entry.IsCluster() {
		// No fleet rule, no infra recipient, no canary; the deploy key
		// still — auto-rebuild.nix pulls main with it.
		status, register, err := deployKeyStatus(f, host, p.deploy.public)
		if err != nil {
			return nil, err
		}
		p.deployKey, p.registerOn = status, register
		return p, nil
	}
	fleetSops, err := os.ReadFile(filepath.Join(root, ".sops.yaml"))
	if err != nil {
		return nil, err
	}
	var notes []string
	var fns []func(string) (string, error)
	if block := markerBlock(string(fleetSops), host); block != "" {
		if !strings.Contains(block, p.cluster.recipient) {
			return nil, fmt.Errorf(".sops.yaml already has a rule for %s but its cluster recipient is not the one %s derives (%s) — the real cluster age key is not in the secrets file, or the rule is stale; a re-key is a deliberate act", host, keyAge, p.cluster.recipient)
		}
	} else {
		p.admins[".sops.yaml"] = sharedRecipients(string(fleetSops))
		admins := p.admins[".sops.yaml"]
		notes = append(notes, fmt.Sprintf("+ rule flux/(clusters|apps)/%s/ → %d shared + cluster %s", host, len(admins), p.cluster.recipient))
		fns = append(fns, func(s string) (string, error) { return addFleetRule(s, host, admins, p.cluster.recipient) })
	}
	infraLine := "      - " + p.cluster.recipient + " # cluster: " + host
	if !strings.Contains(string(fleetSops), infraLine) {
		notes = append(notes, "+ `# cluster: "+host+"` recipient in the shared flux/infrastructure rule")
		fns = append(fns, func(s string) (string, error) { return addInfraRecipient(s, infraLine) })
		p.rekey, _ = sopsFilesUnder(root, "flux/infrastructure")
	}
	if len(fns) > 0 {
		p.edits = append(p.edits, dEdit{".sops.yaml", strings.Join(notes, "; "), func(s string) (string, error) {
			for _, fn := range fns {
				var err error
				if s, err = fn(s); err != nil {
					return "", err
				}
			}
			return s, nil
		}})
	}
	if rel := filepath.Join("flux/apps", host, "sops-canary.yaml"); !exists(root, rel) {
		p.canaryPath = rel
	}

	// 4. the deploy key on the forge — checked now (read-only) so a
	// mismatch refuses before anything is written; registered by apply.
	status, register, err := deployKeyStatus(f, host, p.deploy.public)
	if err != nil {
		return nil, err
	}
	p.deployKey, p.registerOn = status, register
	return p, nil
}

func (p *mintPlan) apply(tool secretsTool, f forge) error {
	// Rules first: the secrets file and the canary encrypt under them.
	for _, e := range p.edits {
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
	// The client-side renders follow the peer they were computed from.
	if err := writeRendered(p.root, tool, p.sshConfig, p.knownHosts); err != nil {
		return err
	}
	nixosConfig := filepath.Join(p.root, "nixos/.sops.yaml")
	fleetConfig := filepath.Join(p.root, ".sops.yaml")

	// The console door: the passphrase into the registry BEFORE the hash
	// goes into the secrets file below (console.go says why: the other
	// order can leave a hash nobody can read back). The passphrase
	// reaches mkpasswd on stdin and sops' argv, and nothing else.
	if p.consoleMint {
		pass, hash, err := newConsoleCredential()
		if err != nil {
			return err
		}
		if err := recordPassphrase(tool, p.root, p.host, pass); err != nil {
			return err
		}
		p.consoleHash = hash
	}

	// The secrets file: written whole and encrypted once when new; gaps
	// added one key at a time (sops set) when it exists.
	full := filepath.Join(p.root, p.secretsPath)
	if p.newFile {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, p.secretsYAML(), 0o600); err != nil {
			return err
		}
		if err := tool.EncryptWith(nixosConfig, full); err != nil {
			os.Remove(full)
			return err
		}
	} else {
		for _, key := range p.missing {
			if err := tool.SetWith(nixosConfig, full, `["`+key+`"]`, p.value(key)); err != nil {
				return err
			}
		}
	}
	if err := tool.GitAdd(p.secretsPath); err != nil {
		return err
	}

	// After the rule edit, so the files pick up the new recipient.
	for _, rel := range p.rekey {
		if err := tool.UpdateKeys(filepath.Join(p.root, rel)); err != nil {
			return err
		}
		if err := tool.GitAdd(rel); err != nil {
			return err
		}
	}
	if p.canaryPath != "" {
		full := filepath.Join(p.root, p.canaryPath)
		canary := "apiVersion: v1\nkind: Secret\nmetadata:\n    name: sops-canary\n    namespace: default\ntype: Opaque\nstringData:\n    canary: " + p.host + "-canary\n"
		if err := os.WriteFile(full, []byte(canary), 0o644); err != nil {
			return err
		}
		if err := tool.EncryptWith(fleetConfig, full); err != nil {
			os.Remove(full)
			return err
		}
		if err := tool.GitAdd(p.canaryPath); err != nil {
			return err
		}
	}
	if p.registerOn {
		status, err := registerDeployKey(f, p.host, p.deploy.public)
		if err != nil {
			return err
		}
		p.deployKey, p.registerOn = status, false
	}
	return nil
}

// secretsYAML renders the whole host file in key order (a yaml.Node so
// the order is ours, not alphabetical); multi-line values come out as
// literal blocks, which sops re-encodes on encryption anyway.
func (p *mintPlan) secretsYAML() []byte {
	var m yaml.Node
	m.Kind = yaml.MappingNode
	for _, key := range p.missing {
		m.Content = append(m.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: key},
			&yaml.Node{Kind: yaml.ScalarNode, Value: p.value(key)})
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	enc.Encode(&m)
	enc.Close()
	return buf.Bytes()
}

func (p *mintPlan) value(key string) string {
	switch key {
	case keyK3sToken:
		return p.k3sToken
	case keyWgPrivate:
		return p.wg.private
	case keySSHHost:
		return p.hostKey.private
	case keySSHHostPub:
		return p.hostKey.public
	case keyAge:
		return p.cluster.identity
	case keyDeploy:
		return p.deploy.private
	case keyDeployerPasswordHash:
		return p.consoleHash
	}
	return ""
}

func (p *mintPlan) report(w io.Writer, written bool) {
	facts := []string{}
	switch {
	case p.entry.Cloud():
		facts = append(facts, "cloud: "+p.entry.Provider)
	case !p.entry.Realized():
		facts = append(facts, "lab hardware: no backend")
	default:
		facts = append(facts, "lab: libvirt backend")
	}
	if !p.entry.IsCluster() {
		facts = append(facts, "plain fort: no cluster identity")
	}
	facts = append(facts, "mesh "+p.entry.MeshIP)
	fmt.Fprintf(w, "mint %s   (%s)\n", p.host, strings.Join(facts, " · "))
	// A dry run mints in memory and discards: a recipient derived from a
	// key that does not exist yet is a throwaway, and printing it bare
	// invites copying it somewhere. Say so; --write prints the real one.
	throwaway := func(key string) string {
		if !written && hasKey(p.missing, key) {
			return "   (dry run: minted in memory and discarded — --write prints the real one)"
		}
		return ""
	}
	fmt.Fprintf(w, "  host age recipient %s%s\n", p.hostRecip, throwaway(keySSHHost))
	if p.entry.IsCluster() {
		fmt.Fprintf(w, "  cluster recipient  %s%s\n", p.cluster.recipient, throwaway(keyAge))
	}
	switch {
	case p.newFile:
		fmt.Fprintf(w, "● %-36s %s — minted, sops-encrypted (host key doubles as its age identity)\n", p.secretsPath, strings.Join(p.missing, ", "))
	case len(p.missing) > 0:
		fmt.Fprintf(w, "○ %-36s + %s (sops set; the rest restored from it)\n", p.secretsPath, strings.Join(p.missing, ", "))
	default:
		fmt.Fprintf(w, "○ %-36s complete — identity restored from it\n", p.secretsPath)
	}
	for _, e := range p.edits {
		fmt.Fprintf(w, "○ %-36s %s\n", e.path, e.note)
	}
	switch {
	case p.consoleMint && p.newRegistry:
		fmt.Fprintf(w, "● %-36s the console registry, created for %s (admin recipients only; the host holds the hash)\n", consoleRegistry, p.host)
	case p.consoleMint:
		fmt.Fprintf(w, "○ %-36s + %s: its console passphrase (sops set; the host holds the hash)\n", consoleRegistry, p.host)
	case p.consoleOrphan:
		fmt.Fprintf(w, "! %-36s has NO entry for %s — the host carries %s and nothing can read it back; a new passphrase is a deliberate act: fort console-password %s --rotate --write\n", consoleRegistry, p.host, keyDeployerPasswordHash, p.host)
	}
	if p.sshConfig != "" {
		fmt.Fprintf(w, "◆ %-36s re-rendered — %s\n", sshConfigPath, p.sshNote)
	}
	if p.knownHosts != "" {
		fmt.Fprintf(w, "◆ %-36s + %s,%s (its ssh host key; first login is prompt-free)\n", knownHostsPath, p.host, p.entry.MeshIP)
	}
	for _, rel := range p.rekey {
		fmt.Fprintf(w, "◆ %-36s sops updatekeys (gains the %s recipient)\n", rel, p.host)
	}
	if p.canaryPath != "" {
		fmt.Fprintf(w, "◆ %-36s encrypted under the new rule\n", p.canaryPath)
	}
	if p.deployKey != "" {
		fmt.Fprintf(w, "· %s deploy key flux-%s: %s\n", p.forge, p.host, p.deployKey)
	}
	fmt.Fprintln(w)
	if !written {
		fmt.Fprintln(w, "dry run — nothing minted (mint --write).")
		return
	}
	if p.consoleMint {
		// Never the value: the registry is the one copy (console.go).
		fmt.Fprintf(w, "The console passphrase is in %s and NOWHERE else — not in this output.\nRead it when you need it: fort console-password %s --show\n", consoleRegistry, p.host)
	}
	if !p.entry.IsCluster() {
		fmt.Fprintf(w, "staged. Identity is sops-encrypted in-repo; nothing is left on this machine.\nnext: PR → merge; then the image build + fort provision %s --image <path>\n", p.host)
		return
	}
	fmt.Fprintf(w, "staged. Identity is sops-encrypted in-repo; nothing is left on this machine.\nnext: the fort's workloads (the estate's flux, or a layer's verbs: docs/layers.md) → PR → merge; then the image build + fort provision %s --image <path>\n", p.host)
}

// planMesh is the client side of the peer (mesh.go): the fort's Host
// block in mesh/ssh_config, rendered on the peers.json this plan
// leaves behind, and its host-key line in mesh/known_hosts — the
// key is in hand here (minted, or restored from the secrets file) and
// nowhere else in plaintext. A line that names the host under another
// key is refused the way a foreign peer entry is: the host's real key is
// not in the secrets file, or the line is stale.
func (p *mintPlan) planMesh(peersText string) error {
	future := peersText
	for _, e := range p.edits {
		if e.path == peersPath {
			var err error
			if future, err = e.fn(peersText); err != nil {
				return err
			}
		}
	}
	rendered, hosts, err := renderSSHConfigFrom(p.root, future, "")
	if err != nil {
		return err
	}
	have, err := readOrEmpty(p.root, sshConfigPath)
	if err != nil {
		return err
	}
	if rendered != have {
		p.sshConfig = rendered
		p.sshNote = sshConfigLine(p.host, hosts, p.entry.MeshIP)
	}
	kh, err := readOrEmpty(p.root, knownHostsPath)
	if err != nil {
		return err
	}
	if e, ok := parseKnownHosts(kh)[p.host]; ok && e.key != hostKeyLine(p.hostKey.public) {
		return fmt.Errorf("%s already lists %s with a key that is not the one %s derives — the real host key is not in the secrets file, or the line is stale; a re-key is a deliberate act", knownHostsPath, p.host, keySSHHost)
	}
	if next := knownHostsSet(kh, p.host, p.entry.MeshIP, p.hostKey.public); next != kh {
		p.knownHosts = next
	}
	return nil
}

// --- edits (the inverses of dismantle's; same anchors preseed-identity used)

// sharedRecipients is the admin set for a new host: the recipient lines
// every existing host rule in the config shares ("readable by whoever
// can read every other host"), in the first rule's order; with a single
// host rule the catch-all (the last rule) joins the intersection, and
// with none (a fresh fork) it is the answer. So adding or removing an
// admin is a hand edit of the existing rules, and new hosts follow.
func sharedRecipients(config string) []string {
	var blocks [][]string
	var cur []string
	inBlock := false
	var last []string // recipients of the most recent rule, for the fallback
	for _, l := range strings.Split(config, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "# BEGIN fort sops rule:"):
			inBlock, cur = true, nil
		case strings.HasPrefix(t, "# END fort sops rule:"):
			if inBlock {
				blocks = append(blocks, cur)
			}
			inBlock = false
		case strings.HasPrefix(t, "- path_regex:"):
			if !inBlock {
				last = nil
			}
		case strings.HasPrefix(t, "- age1"):
			if inBlock {
				cur = append(cur, strings.TrimPrefix(t, "- "))
			} else {
				last = append(last, strings.TrimPrefix(t, "- "))
			}
		}
	}
	if len(blocks) == 0 {
		return last
	}
	// One block alone would make its host's own recipient "shared";
	// the catch-all joins the intersection until a second host exists.
	if len(blocks) == 1 {
		blocks = append(blocks, last)
	}
	recipient := func(line string) string { return strings.Fields(line)[0] }
	var shared []string
	for _, line := range blocks[0] {
		inAll := true
		for _, b := range blocks[1:] {
			found := false
			for _, o := range b {
				if recipient(o) == recipient(line) {
					found = true
					break
				}
			}
			if !found {
				inAll = false
				break
			}
		}
		if inAll {
			shared = append(shared, line)
		}
	}
	return shared
}

// addHostRule inserts the host's rule block above nixos/.sops.yaml's
// catch-all (`secrets/.*\.yaml$`).
func addHostRule(text, host string, recipients []string) (string, error) {
	anchor := "  - path_regex: secrets/.*\\.yaml$\n"
	if strings.Count(text, anchor) != 1 {
		return "", fmt.Errorf("catch-all anchor `%s` not found once", strings.TrimSpace(anchor))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  # BEGIN fort sops rule: %s\n  - path_regex: secrets/%s\\.yaml$\n    age:\n", host, host)
	for _, r := range recipients {
		fmt.Fprintf(&b, "      - %s\n", r)
	}
	fmt.Fprintf(&b, "  # END fort sops rule: %s\n", host)
	return strings.Replace(text, anchor, b.String()+anchor, 1), nil
}

// addFleetRule inserts the cluster's path-scoped rule above the shared
// flux/infrastructure rule in .sops.yaml.
func addFleetRule(text, host string, admins []string, clusterRecipient string) (string, error) {
	anchor := "  - path_regex: flux/infrastructure/"
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, anchor) {
			var b []string
			b = append(b, "  # BEGIN fort sops rule: "+host,
				fmt.Sprintf("  - path_regex: flux/(clusters|apps)/%s/.*", host),
				"    encrypted_regex: ^(data|stringData)$",
				"    age:")
			for _, a := range admins {
				b = append(b, "      - "+a)
			}
			b = append(b, "      - "+clusterRecipient+" # "+host, "  # END fort sops rule: "+host)
			out := append(append(append([]string{}, lines[:i]...), b...), lines[i:]...)
			return strings.Join(out, "\n"), nil
		}
	}
	return "", fmt.Errorf("anchor `%s` not found", strings.TrimSpace(anchor))
}

// addInfraRecipient adds the cluster's recipient line right under the
// `# fort-managed: one recipient per cluster` marker.
func addInfraRecipient(text, line string) (string, error) {
	anchor := "# fort-managed: one recipient per cluster"
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		if strings.Contains(l, anchor) {
			out := append(append(append([]string{}, lines[:i+1]...), line), lines[i+1:]...)
			return strings.Join(out, "\n"), nil
		}
	}
	return "", fmt.Errorf("anchor `%s` not found", anchor)
}

func peerPublicKey(text, host string) (string, bool) {
	var f struct {
		Peers map[string]struct {
			PublicKey string `json:"public_key"`
		} `json:"peers"`
	}
	if err := json.Unmarshal([]byte(text), &f); err != nil {
		return "", false
	}
	p, ok := f.Peers[host]
	return p.PublicKey, ok
}

// markerBlock returns the host's `# BEGIN … # END fort sops rule:
// <host>` block ("" when absent), matching the markers as whole trimmed
// lines the way dismantle's removeMarkerBlock does — a substring match
// would let `sfi-fort` find `sfi-fort-e33e2d5`'s block.
func markerBlock(text, host string) string {
	lines := strings.Split(text, "\n")
	begin := -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case "# BEGIN fort sops rule: " + host:
			begin = i
		case "# END fort sops rule: " + host:
			if begin >= 0 {
				return strings.Join(lines[begin:i+1], "\n")
			}
		}
	}
	return ""
}

// addPeer is preseed's jq expression, run through jq so the file keeps
// its formatting and key order byte-for-byte (dismantle removes the
// same way).
func addPeer(text, host, ip, endpoint, pub string) (string, error) {
	args := []string{"--arg", "n", host, "--arg", "ip", ip, "--arg", "pub", pub}
	expr := ".peers[$n] = {wg_vpn_ip: $ip, public_key: $pub}"
	if endpoint != "" {
		args = append(args, "--arg", "ep", endpoint)
		expr = ".peers[$n] = {wg_vpn_ip: $ip, wg_endpoint_ip: $ep, public_key: $pub}"
	}
	cmd, err := tools.Command("jq", append(args, expr)...)
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

// --- the deploy key (the write side; destroy has the revoke)

// keyStanding is deployKeyStatus's rule for any titled key list: every
// key under the title must match the local public half (a timed-out
// POST that the forge recorded anyway leaves duplicates; a stale one
// among them is a foreign key with our name on it), absent means
// register.
func keyStanding(keys []deployKey, title, pub, what, secretsPath, secretKey string) (status string, register bool, err error) {
	local := strings.Join(strings.Fields(pub)[:2], " ")
	var ids, stale []string
	for _, k := range keys {
		if k.Title != title {
			continue
		}
		ids = append(ids, fmt.Sprint(k.ID))
		if !strings.HasPrefix(k.Key, local) {
			stale = append(stale, fmt.Sprint(k.ID))
		}
	}
	switch {
	case len(stale) > 0:
		return "", false, fmt.Errorf("%s %s on the forge (id %s) differs from the one in %s (%s) — delete the stale key deliberately, then re-run", what, title, strings.Join(stale, ", "), secretsPath, secretKey)
	case len(ids) > 1:
		return fmt.Sprintf("registered %d× (ids %s — duplicates, all verified match; fort destroy revokes every one)", len(ids), strings.Join(ids, ", ")), false, nil
	case len(ids) == 1:
		return fmt.Sprintf("registered (id %s, verified match)", ids[0]), false, nil
	}
	return "would register", true, nil
}

// deployKeyStatus reports the key's standing on the forge without
// writing: registered and matching the local one, absent (register =
// true), or registered with a DIFFERENT key — refused, so a stale key
// is deleted deliberately rather than silently replaced.
func deployKeyStatus(f forge, host, pub string) (status string, register bool, err error) {
	keys, err := f.List()
	if err != nil {
		return "", false, err
	}
	return keyStanding(keys, "flux-"+host, pub, "deploy key", filepath.Join("nixos/secrets", host+".yaml"), keyDeploy)
}

func registerDeployKey(f forge, host, pub string) (string, error) {
	if err := f.Register("flux-"+host, pub); err != nil {
		return "", err
	}
	return "registered flux-" + host + " (read-only)", nil
}

func hasKey(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
