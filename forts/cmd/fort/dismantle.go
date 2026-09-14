package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/inventory"
)

// dismantle is the inverse of `fort scaffold` + provisioning/preseed-
// identity.sh: a pure repo mutation that removes exactly what they
// generated for a host and reports what it did not touch (#217). Git is
// the record and the reviewed merge is the gate — the irreversible half
// (`fort destroy`: pulumi, the deploy key) is only reachable once main
// says the host is gone, so nothing here reaches outside the tree.
//
// Two edits are not mere deletions and are worth knowing: the shared
// fleet/infrastructure sops rule loses the host's recipient line(s), and
// every file under that rule is re-encrypted (`sops updatekeys`) — until
// then the retired host's key keeps decrypting them; and the wireguard
// peer is removed from peers.json, which the other peers shed on their
// next rebuild.
func dismantle(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: fort dismantle <host> [--write]")
	}
	host := args[0]
	fs := flag.NewFlagSet("dismantle", flag.ContinueOnError)
	write := fs.Bool("write", false, "apply and stage (default: print the plan)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	p, err := planDismantle(root, host)
	if err != nil {
		return err
	}
	if *write {
		if err := p.apply(sopsCLI{root: root}); err != nil {
			return err
		}
	}
	p.report(os.Stdout, *write)
	return nil
}

type dismantlePlan struct {
	root, host string
	entry      inventory.Host
	removes    []string // repo-relative paths (files or dirs)
	edits      []dEdit
	rekey      []string // sops files under the shared infrastructure rule
	residue    []string // "path: N mention(s)"
	crossRefs  []string // other forts' contracts sourcing from this fort
	notes      []string
}

type dEdit struct {
	path string
	note string
	fn   func(string) (string, error)
}

func planDismantle(root, host string) (*dismantlePlan, error) {
	entry, err := inventory.LoadHost(filepath.Join(root, "provisioning/inventory.yaml"), host)
	if err != nil {
		return nil, err
	}
	p := &dismantlePlan{root: root, host: host, entry: entry}

	// 1. inventory
	p.edits = append(p.edits, dEdit{"provisioning/inventory.yaml", "- " + host + " entry (with its attached comment)", func(s string) (string, error) { return removeInventoryBlock(s, host) }})

	// 2. node layer
	for _, rel := range []string{filepath.Join("nixos/hosts", host), filepath.Join("nixos/secrets", host+".yaml")} {
		if exists(root, rel) {
			p.removes = append(p.removes, rel)
		}
	}
	p.edits = append(p.edits, dEdit{"nixos/.sops.yaml", "- BEGIN/END ANSIBLE sops rule: " + host, func(s string) (string, error) { return removeMarkerBlock(s, host) }})

	// 3. fleet layer (flux_cluster hosts): rule block + shared recipient(s)
	if entry.FluxCluster {
		fleetSops, _ := os.ReadFile(filepath.Join(root, ".sops.yaml"))
		recipients := strings.Count(string(fleetSops), "# cluster: "+host)
		p.edits = append(p.edits, dEdit{".sops.yaml", fmt.Sprintf("- BEGIN/END ANSIBLE sops rule: %s; - %d `# cluster: %s` recipient line(s) in the shared fleet/infrastructure rule", host, recipients, host), func(s string) (string, error) {
			s, err := removeMarkerBlock(s, host)
			if err != nil {
				return "", err
			}
			s, _ = removeLinesContaining(s, "# cluster: "+host)
			return s, nil
		}})
		if recipients > 0 {
			files, _ := sopsFilesUnder(root, "fleet/infrastructure")
			p.rekey = files
		}
		for _, rel := range []string{filepath.Join("fleet/clusters", host), filepath.Join("fleet/apps", host)} {
			if exists(root, rel) {
				p.removes = append(p.removes, rel)
			}
		}
	}

	// 4. wireguard peer
	if entry.WgVpnIP != "" {
		p.edits = append(p.edits, dEdit{"wireguard/peers.json", "- .peers." + host + " (the other peers shed it on their next rebuild)", func(s string) (string, error) { return removePeer(s, host) }})
	}

	// 5. flake cloudHosts (hand-maintained list; a deleted host dir
	// without this edit breaks flake eval)
	flake, _ := os.ReadFile(filepath.Join(root, "nixos/flake.nix"))
	if strings.Contains(string(flake), `"`+host+`"`) {
		p.edits = append(p.edits, dEdit{"nixos/flake.nix", "- \"" + host + "\" from cloudHosts", func(s string) (string, error) { return removeFromCloudHosts(s, host) }})
	}

	// 6. residue: everything else that still names the host — counted
	// on the post-edit text for the files this plan edits, so a dry run
	// shows what the reviewer will actually face.
	edited := map[string]string{}
	for _, e := range p.edits {
		raw, err := os.ReadFile(filepath.Join(root, e.path))
		if err != nil {
			continue
		}
		if out, err := e.fn(string(raw)); err == nil {
			edited[e.path] = out
		}
	}
	p.residue, p.crossRefs = scanResidue(root, host, p.removes, edited)
	if exists(root, filepath.Join("forts/pulumi", "Pulumi."+host+".yaml")) || exists(root, filepath.Join("forts/pulumi/state/.pulumi/stacks/forts", host+".json")) {
		p.notes = append(p.notes, "pulumi stack `"+host+"` still exists — untouched by design; `fort destroy "+host+"` after this merges (forts/DECOMISSIONING.md until it lands). `just iac-drift` is red for it in between, which is the correct signal.")
	}
	if entry.Provider == "" {
		p.notes = append(p.notes, "lab host (libvirt): the VM itself goes with provisioning/vms/delete.yaml on soundship (`just delete "+host+"`) after this merges.")
	}
	p.notes = append(p.notes, "the gitea deploy key `flux-"+host+"` stays registered until fort destroy revokes it.")
	sort.Strings(p.removes)
	return p, nil
}

func (p *dismantlePlan) apply(tool secretsTool) error {
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
	for _, rel := range p.removes {
		if err := os.RemoveAll(filepath.Join(p.root, rel)); err != nil {
			return err
		}
		if err := tool.GitAdd(rel); err != nil {
			return err
		}
	}
	// After the rule edit, so the departed recipient is gone from the
	// rule the files are re-keyed against.
	for _, rel := range p.rekey {
		if err := tool.UpdateKeys(filepath.Join(p.root, rel)); err != nil {
			return err
		}
		if err := tool.GitAdd(rel); err != nil {
			return err
		}
	}
	return nil
}

func (p *dismantlePlan) report(w io.Writer, written bool) {
	fmt.Fprintf(w, "dismantle %s   (%s)\n", p.host, map[bool]string{true: "cloud: " + p.entry.Provider, false: "lab: libvirt"}[p.entry.Provider != ""])
	for _, rel := range p.removes {
		fmt.Fprintf(w, "✗ %s\n", rel)
	}
	for _, e := range p.edits {
		fmt.Fprintf(w, "○ %-32s %s\n", e.path, e.note)
	}
	for _, rel := range p.rekey {
		fmt.Fprintf(w, "◆ %-32s sops updatekeys (lost the %s recipient)\n", rel, p.host)
	}
	if len(p.crossRefs) > 0 {
		fmt.Fprintln(w, "! cross-fort sops refs INTO this fort — those contracts lose their source (fort check will fail them):")
		for _, r := range p.crossRefs {
			fmt.Fprintf(w, "    %s\n", r)
		}
	}
	if len(p.residue) > 0 {
		fmt.Fprintf(w, "! residue — %d file(s) still mention %s; triage in the same PR (incident comments may be worth keeping as provenance — the reviewer's call):\n", len(p.residue), p.host)
		for _, r := range p.residue {
			fmt.Fprintf(w, "    %s\n", r)
		}
	}
	for _, n := range p.notes {
		fmt.Fprintf(w, "· %s\n", n)
	}
	fmt.Fprintln(w)
	if !written {
		fmt.Fprintln(w, "dry run — nothing changed (dismantle --write).")
		return
	}
	fmt.Fprintln(w, "staged. Next: triage the residue, commit, PR — then fort destroy after the merge.")
}

// --- edits

// removeMarkerBlock deletes the `# BEGIN ANSIBLE sops rule: <host>` …
// `# END …` block preseed-identity / cluster-init wrote (inclusive).
func removeMarkerBlock(text, host string) (string, error) {
	lines := strings.Split(text, "\n")
	begin, end := -1, -1
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case "# BEGIN ANSIBLE sops rule: " + host:
			begin = i
		case "# END ANSIBLE sops rule: " + host:
			end = i
		}
	}
	if begin < 0 || end < begin {
		return "", fmt.Errorf("no `# BEGIN/END ANSIBLE sops rule: %s` block", host)
	}
	return strings.Join(append(append([]string{}, lines[:begin]...), lines[end+1:]...), "\n"), nil
}

func removeLinesContaining(text, needle string) (string, int) {
	var out []string
	n := 0
	for _, l := range strings.Split(text, "\n") {
		if strings.Contains(l, needle) {
			n++
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n"), n
}

// removeInventoryBlock drops the host's mapping under
// all.children.nixos.hosts (8-space key) together with the comment
// lines directly above it, up to the next sibling key.
func removeInventoryBlock(text, host string) (string, error) {
	lines := strings.Split(text, "\n")
	const indent = "        "
	start := -1
	for i, l := range lines {
		if l == indent+host+":" {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no `%s%s:` entry", indent, host)
	}
	end := start + 1
	for end < len(lines) {
		l := lines[end]
		if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, indent+" ") {
			break
		}
		end++
	}
	first := start
	for first > 0 && strings.HasPrefix(lines[first-1], indent+"#") {
		first--
	}
	return strings.Join(append(append([]string{}, lines[:first]...), lines[end:]...), "\n"), nil
}

// removePeer is `jq 'del(.peers[$n])'` — preseed wrote the file with jq,
// so the same tool keeps its formatting and key order byte-for-byte.
func removePeer(text, host string) (string, error) {
	cmd := exec.Command("jq", "--arg", "n", host, "del(.peers[$n])")
	cmd.Stdin = strings.NewReader(text)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("jq: %s", exitDetail(err))
	}
	if !strings.Contains(text, `"`+host+`"`) {
		return "", fmt.Errorf("no peer %q", host)
	}
	return string(out), nil
}

var cloudHostsRe = regexp.MustCompile(`(cloudHosts\s*=\s*\[)([^\]]*)(\])`)

func removeFromCloudHosts(text, host string) (string, error) {
	m := cloudHostsRe.FindStringSubmatchIndex(text)
	if m == nil {
		return "", fmt.Errorf("no cloudHosts list")
	}
	inner := text[m[4]:m[5]]
	fields := strings.Fields(inner)
	var kept []string
	for _, f := range fields {
		if f != `"`+host+`"` {
			kept = append(kept, f)
		}
	}
	if len(kept) == len(fields) {
		return "", fmt.Errorf("%q not in cloudHosts", host)
	}
	repl := " " + strings.Join(kept, " ") + " "
	if len(kept) == 0 {
		repl = " "
	}
	return text[:m[4]] + repl + text[m[5]:], nil
}

// --- residue

var textExt = map[string]bool{".yaml": true, ".yml": true, ".nix": true, ".md": true, ".json": true, ".py": true, ".go": true, ".gotpl": true, ".sh": true, ".txt": true, ".conf": true, "": true}

// scanResidue lists every tracked text file outside the removed paths
// that still mentions the host (edited files are judged on their
// post-edit text), tagged for triage, and every other fort's contract
// whose sops refs point into this fort's app tree.
func scanResidue(root, host string, removed []string, edited map[string]string) (residue, crossRefs []string) {
	skip := map[string]bool{"forts/pulumi/state": true}
	for _, r := range removed {
		skip[r] = true
	}
	skipped := func(rel string) bool {
		for d := rel; d != "." && d != "/"; d = filepath.Dir(d) {
			if skip[d] {
				return true
			}
		}
		return false
	}
	for _, rel := range trackedFiles(root) {
		if skipped(rel) || !textExt[filepath.Ext(rel)] {
			continue
		}
		text, ok := edited[rel]
		if !ok {
			raw, err := os.ReadFile(filepath.Join(root, rel))
			if err != nil {
				continue
			}
			text = string(raw)
		}
		n := 0
		sc := bufio.NewScanner(strings.NewReader(text))
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			if strings.Contains(sc.Text(), host) {
				n++
			}
		}
		if n > 0 {
			residue = append(residue, fmt.Sprintf("%s: %d%s", rel, n, residueTag(rel)))
		}
	}
	sort.Strings(residue)

	contracts, _ := filepath.Glob(filepath.Join(root, "fleet/apps/*/*/secrets.contract.yaml"))
	target := filepath.Join(root, "fleet/apps", host) + string(filepath.Separator)
	for _, c := range contracts {
		if strings.HasPrefix(c, target) {
			continue
		}
		raw, err := os.ReadFile(c)
		if err != nil || !isFullForm(raw) {
			continue
		}
		ff, err := parseFullForm(raw)
		if err != nil {
			continue
		}
		dir := filepath.Dir(c)
		check := func(what string, s Spec) {
			if s.Sops != nil && strings.HasPrefix(filepath.Clean(filepath.Join(dir, s.Sops[0]))+string(filepath.Separator), target) {
				rel, _ := filepath.Rel(root, c)
				crossRefs = append(crossRefs, rel+": "+what+" ← "+s.Sops[0])
			}
		}
		for _, v := range ff.Vars {
			check("var "+v.Name, v.Spec)
		}
		for _, sf := range ff.Secrets {
			for _, k := range sf.Keys {
				check(sf.File+"/"+k.Name, k.Spec)
			}
		}
	}
	return residue, crossRefs
}

// trackedFiles: `git ls-files` when root is a checkout (so untracked
// build output and ignored files never show up as residue); a plain
// walk otherwise (tests run on a temp copy).
func trackedFiles(root string) []string {
	cmd := exec.Command("git", "ls-files", "-z")
	cmd.Dir = root
	if out, err := cmd.Output(); err == nil {
		var files []string
		for _, f := range strings.Split(string(out), "\x00") {
			if f != "" {
				files = append(files, f)
			}
		}
		return files
	}
	var files []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		files = append(files, rel)
		return nil
	})
	return files
}

// residueTag hints at the triage: fixtures and docs are usually kept
// (history, goldens); manifests and monitoring usually go.
func residueTag(rel string) string {
	switch {
	case strings.HasPrefix(rel, "forts/cmd/fort/"):
		return "   (CLI test fixture / golden — keep)"
	case strings.HasSuffix(rel, ".md"):
		return "   (doc)"
	case strings.HasPrefix(rel, "fleet/apps/luxor/monitoring/"):
		return "   (central monitoring — per-cluster rule/datasource/dashboard)"
	}
	return ""
}

func sopsFilesUnder(root, dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || filepath.Ext(path) != ".yaml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if strings.Contains(string(raw), "\nsops:\n") {
			rel, _ := filepath.Rel(root, path)
			out = append(out, rel)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}
