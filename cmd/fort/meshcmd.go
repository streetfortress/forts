package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gopkg.in/yaml.v3"
)

// fort ssh-config [--write] — render mesh/ssh_config and
// mesh/known_hosts from the tree (mesh.go). mint and dismantle keep
// them current as a side effect of their own edit; this verb is the
// whole-fleet render: after a hand edit to peers.json, to backfill the
// key lines of forts minted before the file existed, or just to read
// what the tree says. ssh_config needs nothing but the tree; a fort's
// known_hosts line needs its ssh-host-key.pub, which is sops-encrypted
// with the rest of its secrets — so without the admin age key the verb
// keeps the committed line and says so, and with it the line is the
// truth in sops.
func sshConfigCmd(args []string) error {
	fs := flag.NewFlagSet("ssh-config", flag.ContinueOnError)
	write := fs.Bool("write", false, "write and stage (default: print the plan)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("usage: fort ssh-config [--write]")
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	tool := sopsCLI{root: root}
	p, err := planSSHConfig(root, tool)
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

type sshConfigPlan struct {
	root                  string
	peers, forts          int
	sshConfig, knownHosts string   // the rendered text when it differs from disk; "" = current
	added, updated        []string // known_hosts lines from the key in sops
	kept                  []string // lines left as committed: the key could not be read here
	removed               []string // lines whose host has no manifest
	unreadable            []string // forts with no line and no readable key
	decryptErr            string   // the first decrypt failure, for the report
}

func planSSHConfig(root string, tool secretsTool) (*sshConfigPlan, error) {
	hosts, problems, err := manifest.LoadAll(root)
	if err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		// A render from a partial fleet would drop the bad host's block
		// and its key line, and read as a retirement.
		var bad []string
		for h, e := range problems {
			bad = append(bad, h+": "+e.Error())
		}
		sort.Strings(bad)
		return nil, fmt.Errorf("manifest(s) do not load — fix them first (fort check --all):\n  %s", strings.Join(bad, "\n  "))
	}
	m, err := loadMesh(root)
	if err != nil {
		return nil, err
	}
	p := &sshConfigPlan{root: root, peers: len(m.Peers), forts: len(hosts)}

	rendered := renderSSHConfig(m, hosts)
	have, err := readOrEmpty(root, sshConfigPath)
	if err != nil {
		return nil, err
	}
	if rendered != have {
		p.sshConfig = rendered
	}

	khText, err := readOrEmpty(root, knownHostsPath)
	if err != nil {
		return nil, err
	}
	entries := parseKnownHosts(khText)
	next := map[string]knownHost{}
	for _, h := range sortedNames(hosts) {
		peer, onMesh := m.Peers[h]
		if !onMesh {
			continue // not minted yet — mint writes the line with the peer
		}
		pub, err := hostPublicKey(tool, root, h)
		if err != nil {
			if p.decryptErr == "" {
				p.decryptErr = err.Error()
			}
			if e, ok := entries[h]; ok {
				e.ip = peer.IP
				next[h] = e
				p.kept = append(p.kept, h)
			} else {
				p.unreadable = append(p.unreadable, h)
			}
			continue
		}
		e := knownHost{host: h, ip: peer.IP, key: hostKeyLine(pub)}
		next[h] = e
		switch old, ok := entries[h]; {
		case !ok:
			p.added = append(p.added, h)
		case old != e:
			p.updated = append(p.updated, h)
		}
	}
	for _, h := range sortedNames(entries) {
		if _, ok := hosts[h]; !ok {
			p.removed = append(p.removed, h)
		}
	}
	if text := renderKnownHosts(next); text != khText {
		p.knownHosts = text
	}
	return p, nil
}

// hostPublicKey is the fort's ssh-host-key.pub out of its sops file —
// the only key of that file this verb reads, and a public one.
func hostPublicKey(tool secretsTool, root, host string) (string, error) {
	raw, err := tool.Decrypt(filepath.Join(root, "nixos/secrets", host+".yaml"))
	if err != nil {
		return "", err
	}
	var have map[string]string
	if err := yaml.Unmarshal(raw, &have); err != nil {
		return "", fmt.Errorf("nixos/secrets/%s.yaml: %w", host, err)
	}
	pub := strings.TrimSpace(have[keySSHHostPub])
	if pub == "" {
		return "", fmt.Errorf("nixos/secrets/%s.yaml has no %s", host, keySSHHostPub)
	}
	return pub, nil
}

func (p *sshConfigPlan) apply(tool secretsTool) error {
	return writeRendered(p.root, tool, p.sshConfig, p.knownHosts)
}

func (p *sshConfigPlan) report(w io.Writer, written bool) {
	fmt.Fprintf(w, "ssh-config   (%d mesh peer(s): %d fort(s), %d hand-managed)\n", p.peers, p.forts, p.peers-p.forts)
	if p.sshConfig != "" {
		fmt.Fprintf(w, "◆ %-24s rendered from %s + forts/*.toml\n", sshConfigPath, peersPath)
	} else {
		fmt.Fprintf(w, "○ %-24s unchanged\n", sshConfigPath)
	}
	var parts []string
	if len(p.added) > 0 {
		parts = append(parts, "+ "+strings.Join(p.added, ", "))
	}
	if len(p.updated) > 0 {
		parts = append(parts, "key replaced: "+strings.Join(p.updated, ", "))
	}
	if len(p.removed) > 0 {
		parts = append(parts, "- "+strings.Join(p.removed, ", ")+" (no manifest)")
	}
	if len(p.kept) > 0 {
		parts = append(parts, "kept as committed: "+strings.Join(p.kept, ", "))
	}
	switch {
	case p.knownHosts != "" && len(parts) == 0:
		fmt.Fprintf(w, "◆ %-24s header only — no key readable here\n", knownHostsPath)
	case p.knownHosts != "":
		fmt.Fprintf(w, "◆ %-24s %s\n", knownHostsPath, strings.Join(parts, " · "))
	case len(parts) > 0:
		fmt.Fprintf(w, "○ %-24s unchanged (%s)\n", knownHostsPath, strings.Join(parts, " · "))
	default:
		fmt.Fprintf(w, "○ %-24s unchanged\n", knownHostsPath)
	}
	if len(p.unreadable) > 0 {
		fmt.Fprintf(w, "! no host key line for %s and none readable here — run with the admin age key (sops on PATH), or `fort mint <host> --write` per fort\n", strings.Join(p.unreadable, ", "))
	}
	if p.decryptErr != "" && (len(p.kept) > 0 || len(p.unreadable) > 0) {
		fmt.Fprintf(w, "  (%s)\n", firstLine(p.decryptErr))
	}
	fmt.Fprintln(w)
	switch {
	case !written:
		fmt.Fprintln(w, "dry run — nothing written (ssh-config --write).")
	case p.sshConfig == "" && p.knownHosts == "":
		fmt.Fprintln(w, "nothing to write — both files are current.")
	default:
		fmt.Fprintln(w, "staged — commit the rendered files.")
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// writeRendered lays the rendered files down (an empty text = leave
// that file alone) and stages them — the one write path mint, dismantle
// and ssh-config share.
func writeRendered(root string, tool secretsTool, sshConfig, knownHosts string) error {
	for _, f := range []struct{ rel, text string }{{sshConfigPath, sshConfig}, {knownHostsPath, knownHosts}} {
		if f.text == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(root, f.rel), []byte(f.text), 0o644); err != nil {
			return err
		}
		if err := tool.GitAdd(f.rel); err != nil {
			return err
		}
	}
	return nil
}

// fort wg-conf <member> — print a member's mesh interface the way its WireGuard app
// (or wg-quick) wants it, from peers.json. Read verb: nothing is
// written, and no key is handled — the PrivateKey line is the device's
// (mesh.go, renderWGConf).
func wgConf(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: fort wg-conf <member>")
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	m, err := loadMesh(root)
	if err != nil {
		return err
	}
	est, err := estate.Load(root)
	if err != nil {
		return err
	}
	conf, err := renderWGConf(m, est.InterfaceName(), est.MeshPrefixBits(), args[0])
	if err != nil {
		return err
	}
	fmt.Print(conf)
	return nil
}
