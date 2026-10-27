package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"gitea.zen.lofi/sfi/forts/internal/tools"
)

// The layer protocol (forts#144; docs/layers.md). A layer is what sits
// on top of the core: a role a hub serves with a render on both ends,
// the chart contract and its batteries, a backup store — SFI's own
// standard, or anyone's. Core knows no layer by name. It knows the
// protocol, and the protocol is four things:
//
//  1. A layer owns layers/<layer>/ in the estate tree — its estate.toml
//     and its forts/<fort>.toml — and core never reads under it
//     (fort check --all lists what is installed and nothing more).
//  2. A layer is one binary, fort-<layer>, with the verbs the order
//     expects: init, scaffold, check, mint, dismantle. `fort <layer>
//     <verb> …` execs it from PATH, git style (execLayer).
//  3. `fort layer add <flake url>#<role> [--dry-run] [-- <the layer's init
//     flags>]` installs one: the flake input, the binary re-exported at
//     the layer's pin so the gate runs `nix run ./nixos#fort-<role>`,
//     the gate step, then the layer's own init with every flag after
//     `--`, passed through unread. `fort layer remove <role>` is the
//     reversal. Both are author verbs: apply by default, --dry-run plans.
//  4. The ports a role needs are on core's link entry ([[serves.<role>]]
//     ports, forts#105); the layer's check verifies them. A layer writes
//     no core file, and core writes no layer file.
//
// What this is not: a registry of layers, a schema per layer, a render
// hook, or a Go API — the layers copy the helpers they need, and a
// public Go module is a decision after v0.1.

const layersDir = "layers"

func layerCmd(args []string) error {
	if len(args) < 1 {
		return errors.New(layerUsage)
	}
	switch args[0] {
	case "add":
		return layerAdd(args[1:])
	case "remove":
		return layerRemove(args[1:])
	}
	return errors.New(layerUsage)
}

const layerUsage = "usage: fort layer add <flake url>#<role> [--dry-run] [-- <the layer's init flags>]\n       fort layer remove <role> [--dry-run]"

// layerRef is the one argument of fort layer add, split: the flake URL
// (the input, named by its last path label) and the role (the binary).
type layerRef struct {
	url, input, role string
}

func parseLayerRef(s string) (layerRef, error) {
	url, role, ok := strings.Cut(s, "#")
	if !ok || url == "" || role == "" {
		return layerRef{}, fmt.Errorf("want <flake url>#<role> (git+ssh://git@forge.example:22/acme/fort-layers#monitoring), got %q", s)
	}
	if !nameOK(role) {
		return layerRef{}, fmt.Errorf("role %q — want lowercase labels and dashes: the binary is fort-%s", role, role)
	}
	// The input is the repo's name: the last path label, .git stripped,
	// a query cut off (git+ssh://…/fort-layers.git?ref=v1 → fort-layers).
	base := url
	if i := strings.IndexAny(base, "?"); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimSuffix(strings.TrimSuffix(base, "/"), ".git")
	input := base[strings.LastIndexAny(base, "/:")+1:]
	if !nameOK(input) {
		return layerRef{}, fmt.Errorf("the flake url %q ends in %q, which is not a flake input name", url, input)
	}
	return layerRef{url: url, input: input, role: role}, nil
}

func nameOK(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return s[0] != '-' && s[len(s)-1] != '-'
}

func layerAdd(args []string) error {
	// Everything after `--` is the layer's; flag parsing stops there.
	var passthrough []string
	for i, a := range args {
		if a == "--" {
			passthrough = args[i+1:]
			args = args[:i]
			break
		}
	}
	fs := flag.NewFlagSet("layer", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the plan and write nothing (default: edit, stage, run the layer's init)")
	fs.Bool("write", true, "deprecated no-op: writing is the default")
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos = append(pos, args[0])
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	write := !*dryRun
	if len(pos) != 1 || fs.NArg() > 0 {
		return errors.New(layerUsage)
	}
	ref, err := parseLayerRef(pos[0])
	if err != nil {
		return err
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	p, err := planLayerAdd(root, ref)
	if err != nil {
		return err
	}
	if write {
		if err := p.apply(); err != nil {
			return err
		}
		stageLayerFiles(root)
	}
	p.report(os.Stdout, write, passthrough)
	if !write {
		return nil
	}
	return runLayerInit(root, ref, passthrough)
}

// layerPlan: the three core edits an install (or a removal) makes, in
// the marked-tree shape every author verb reports in.
type layerPlan struct {
	root    string
	ref     layerRef
	remove  bool
	actions []action
	// present: the files already carry the line, so the plan is a no-op
	// there (re-runnable, like every author verb).
	present []string
}

const (
	flakePath = "nixos/flake.nix"
	gatePath  = ".gitea/workflows/checks.yaml"
)

func planLayerAdd(root string, ref layerRef) (*layerPlan, error) {
	p := &layerPlan{root: root, ref: ref}
	flake, err := os.ReadFile(filepath.Join(root, flakePath))
	if err != nil {
		return nil, fmt.Errorf("%s: %w — fort layer add runs in an estate fort init rendered", flakePath, err)
	}
	gate, err := os.ReadFile(filepath.Join(root, gatePath))
	if err != nil {
		return nil, fmt.Errorf("%s: %w — the gate workflow fort init rendered", gatePath, err)
	}
	// Dry-run every edit now, so a flake rendered before layers (no
	// `layers = sys: {` block) refuses before anything is written.
	inputLine := fmt.Sprintf("%s.url = %q;", ref.input, ref.url)
	exportLine := fmt.Sprintf("fort-%s = inputs.%s.packages.${sys}.fort-%s;", ref.role, ref.input, ref.role)
	text := string(flake)
	t1, err := nixEnsureInput(text, ref.input, inputLine)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", flakePath, err)
	}
	t2, err := nixEnsureLayerExport(t1, "fort-"+ref.role, exportLine)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", flakePath, err)
	}
	if t2 == text {
		p.present = append(p.present, flakePath)
	} else {
		note := "+ " + exportLine
		if t1 != text {
			note = "+ " + inputLine + "; " + note
		}
		p.actions = append(p.actions, action{path: flakePath, mark: '○', note: note, edit: func(s string) (string, error) {
			s, err := nixEnsureInput(s, ref.input, inputLine)
			if err != nil {
				return "", err
			}
			return nixEnsureLayerExport(s, "fort-"+ref.role, exportLine)
		}})
	}
	step := layerGateStep(ref.role)
	g, err := yamlEnsureStepAfter(string(gate), coreGateStep, layerGateStepName(ref.role), step)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", gatePath, err)
	}
	if g == string(gate) {
		p.present = append(p.present, gatePath)
	} else {
		p.actions = append(p.actions, action{path: gatePath, mark: '○', note: "+ the step `" + layerGateStepName(ref.role) + "`", edit: func(s string) (string, error) {
			return yamlEnsureStepAfter(s, coreGateStep, layerGateStepName(ref.role), step)
		}})
	}
	return p, nil
}

func (p *layerPlan) apply() error {
	for _, a := range p.actions {
		full := filepath.Join(p.root, a.path)
		raw, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		out, err := a.edit(string(raw))
		if err != nil {
			return fmt.Errorf("%s: %w", a.path, err)
		}
		if err := os.WriteFile(full, []byte(out), 0o644); err != nil {
			return err
		}
	}
	if p.remove {
		dir := filepath.Join(p.root, layersDir, p.ref.role)
		if _, err := os.Stat(dir); err == nil {
			if err := os.RemoveAll(dir); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *layerPlan) report(w io.Writer, written bool, passthrough []string) {
	verb := "fort layer add"
	if p.remove {
		verb = "fort layer remove"
	}
	fmt.Fprintf(w, "%s %s   (the layer's binary: fort-%s)\n", verb, p.ref.role, p.ref.role)
	for _, a := range p.actions {
		fmt.Fprintf(w, "%c %-30s %s\n", a.mark, a.path, a.note)
	}
	if p.remove {
		fmt.Fprintf(w, "✗ %-30s the layer's files; the layer's own dismantle ran before this, per fort\n", layersDir+"/"+p.ref.role+"/")
	}
	for _, f := range p.present {
		fmt.Fprintf(w, "· %-30s already as planned\n", f)
	}
	if !written {
		fmt.Fprintf(w, "\ndry run — nothing written.\n")
		return
	}
	if p.remove {
		fmt.Fprintln(w, "\nstaged — commit the edited files. next: nix flake lock ./nixos")
		return
	}
	fmt.Fprintf(w, "\nnext: fort-%s init %s\n", p.ref.role, strings.Join(passthrough, " "))
}

// runLayerInit execs the layer's init with the operator's flags, after
// the core edits are staged: from PATH when the binary is there, else
// through nix at the url just written. Either way the layer writes
// layers/<role>/ itself — core writes nothing under it.
func runLayerInit(root string, ref layerRef, flags []string) error {
	bin := "fort-" + ref.role
	var cmd *exec.Cmd
	if path, err := exec.LookPath(bin); err == nil {
		cmd = exec.Command(path, append([]string{"init"}, flags...)...)
	} else {
		nix, err := tools.Command("nix", append([]string{"run", ref.url + "#" + bin, "--", "init"}, flags...)...)
		if err != nil {
			return fmt.Errorf("%s is not on PATH and %v — run the layer's init by hand: nix run %s#%s -- init %s", bin, err, ref.url, bin, strings.Join(flags, " "))
		}
		cmd = nix
	}
	cmd.Dir = root
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "FORT_ESTATE_ROOT="+root)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s init: %w", bin, err)
	}
	return nil
}

func layerRemove(args []string) error {
	fs := flag.NewFlagSet("layer", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the plan and write nothing (default: remove and stage)")
	fs.Bool("write", true, "deprecated no-op: writing is the default")
	var pos []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos = append(pos, args[0])
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	write := !*dryRun
	if len(pos) != 1 || fs.NArg() > 0 || !nameOK(pos[0]) {
		return errors.New(layerUsage)
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	p, err := planLayerRemove(root, pos[0])
	if err != nil {
		return err
	}
	if write {
		if err := p.apply(); err != nil {
			return err
		}
		stageLayerFiles(root)
	}
	p.report(os.Stdout, write, nil)
	return nil
}

func planLayerRemove(root, role string) (*layerPlan, error) {
	p := &layerPlan{root: root, ref: layerRef{role: role}, remove: true}
	flake, err := os.ReadFile(filepath.Join(root, flakePath))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", flakePath, err)
	}
	gate, err := os.ReadFile(filepath.Join(root, gatePath))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", gatePath, err)
	}
	t, input := nixDropLayerExport(string(flake), "fort-"+role)
	if t == string(flake) {
		p.present = append(p.present, flakePath)
	} else {
		note := "- fort-" + role
		// The input goes when no other role's line names it.
		if input != "" && !strings.Contains(t, "inputs."+input+".packages") {
			t2, err := nixDropInput(t, input)
			if err == nil && t2 != t {
				note += "; - " + input + ".url"
			}
		}
		p.actions = append(p.actions, action{path: flakePath, mark: '○', note: note, edit: func(s string) (string, error) {
			s, input := nixDropLayerExport(s, "fort-"+role)
			if input != "" && !strings.Contains(s, "inputs."+input+".packages") {
				return nixDropInput(s, input)
			}
			return s, nil
		}})
	}
	g := yamlDropStep(string(gate), layerGateStepName(role))
	if g == string(gate) {
		p.present = append(p.present, gatePath)
	} else {
		p.actions = append(p.actions, action{path: gatePath, mark: '○', note: "- the step `" + layerGateStepName(role) + "`", edit: func(s string) (string, error) {
			return yamlDropStep(s, layerGateStepName(role)), nil
		}})
	}
	return p, nil
}

func stageLayerFiles(root string) {
	if git, err := tools.Command("git", "-C", root, "add", "-A", flakePath, gatePath, layersDir); err == nil {
		git.Run()
	}
}

// --- the gate step ---

const coreGateStep = "The gate (fort check --all)"

func layerGateStepName(role string) string {
	return fmt.Sprintf("The gate of the %s layer (fort %s check --all)", role, role)
}

func layerGateStep(role string) string {
	return fmt.Sprintf(`      - name: %s
        # A layer's own check, at the layer's pin (docs/layers.md). Core
        # reads nothing under layers/; this step is the layer's gate.
        env:
          GITEA_ACTIONS: "true"
        run: nix run ./nixos#fort-%s -- check --all
`, layerGateStepName(role), role)
}

// yamlEnsureStepAfter inserts block (a whole `- name:` step, already
// indented) after the step named after, unless a step named name is
// already in the file. A step runs from its `- name:` line to the next
// line at the same indent that starts with `- `, or a dedent.
func yamlEnsureStepAfter(text, after, name, block string) (string, error) {
	if yamlStepBounds(text, name) != nil {
		return text, nil
	}
	b := yamlStepBounds(text, after)
	if b == nil {
		return "", fmt.Errorf("no step named %q — the gate workflow fort init renders", after)
	}
	lines := strings.Split(text, "\n")
	out := append([]string{}, lines[:b[1]]...)
	// One blank line between steps, as the template keeps.
	if strings.TrimSpace(lines[b[1]-1]) != "" {
		out = append(out, "")
	}
	out = append(out, strings.Split(strings.TrimRight(block, "\n"), "\n")...)
	out = append(out, lines[b[1]:]...)
	return strings.Join(out, "\n"), nil
}

// yamlDropStep removes the step named name, and the blank line before
// it, when the file has one; else the text unchanged.
func yamlDropStep(text, name string) string {
	b := yamlStepBounds(text, name)
	if b == nil {
		return text
	}
	lines := strings.Split(text, "\n")
	start := b[0]
	if start > 0 && strings.TrimSpace(lines[start-1]) == "" {
		start--
	}
	out := append([]string{}, lines[:start]...)
	out = append(out, lines[b[1]:]...)
	return strings.Join(out, "\n")
}

// yamlStepBounds finds the step whose `- name:` line names name:
// [start, end) in lines, end being the first line after it at the step
// indent or less that is not blank and not a deeper line. nil when none.
func yamlStepBounds(text, name string) []int {
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		code := strings.TrimSpace(l)
		if code != "- name: "+name {
			continue
		}
		indent := len(l) - len(strings.TrimLeft(l, " "))
		end := len(lines)
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "" {
				continue
			}
			if len(lines[j])-len(strings.TrimLeft(lines[j], " ")) <= indent {
				end = j
				break
			}
		}
		// Trailing blank lines belong to the gap, not the step.
		for end > i+1 && strings.TrimSpace(lines[end-1]) == "" {
			end--
		}
		return []int{i, end}
	}
	return nil
}

// --- the flake ---

// nixEnsureInput adds `    <line>` as the last entry of the top-level
// `inputs = {` set unless a line of it already sets <name>.url (the
// same repo under the same name, whatever its url: a pin the estate
// moved by hand is the estate's).
func nixEnsureInput(text, name, line string) (string, error) {
	return nixEnsureInBlock(text, "inputs = {", name+".url", line)
}

// nixDropInput removes the `<name>.url = …;` line of the inputs set.
func nixDropInput(text, name string) (string, error) {
	return nixDropFromBlock(text, "inputs = {", name+".url")
}

// nixEnsureLayerExport adds `        <line>` to the `layers = sys: {`
// set the estate flake carries for the layers' binaries (the init
// template since forts#144), unless a line already sets <attr>.
func nixEnsureLayerExport(text, attr, line string) (string, error) {
	if !strings.Contains(text, layersBlock) {
		return "", fmt.Errorf("no `%s` block — this flake was rendered before layers; add the block from the init template (docs/layers.md)", strings.TrimSpace(layersBlock))
	}
	return nixEnsureInBlock(text, layersBlock, attr, line)
}

// nixDropLayerExport removes the line that sets <attr> in the layers
// set, and returns the input that line named (inputs.<input>.packages),
// so the caller can drop the input when nothing else names it.
func nixDropLayerExport(text, attr string) (string, string) {
	input := ""
	for _, l := range strings.Split(text, "\n") {
		code := strings.TrimSpace(l)
		if strings.HasPrefix(code, attr+" =") {
			if _, rest, ok := strings.Cut(code, "inputs."); ok {
				input, _, _ = strings.Cut(rest, ".")
			}
		}
	}
	out, err := nixDropFromBlock(text, layersBlock, attr)
	if err != nil {
		return text, ""
	}
	return out, input
}

const layersBlock = "layers = sys: {"

// nixEnsureInBlock inserts line as the last entry of the attribute set
// that opens with the line open (trimmed) and closes at the first `};`
// after it, indented one level deeper than open, unless a line of the
// set already sets attr.
func nixEnsureInBlock(text, open, attr, line string) (string, error) {
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == open {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no `%s` block", open)
	}
	indent := strings.TrimSuffix(lines[start], strings.TrimLeft(lines[start], " "))
	for i := start + 1; i < len(lines); i++ {
		code := strings.TrimSpace(lines[i])
		if code == "};" {
			out := append([]string{}, lines[:i]...)
			out = append(out, indent+"  "+line)
			out = append(out, lines[i:]...)
			return strings.Join(out, "\n"), nil
		}
		if rest, ok := strings.CutPrefix(code, attr); ok && rest != "" && strings.ContainsRune(" =", rune(rest[0])) {
			return text, nil
		}
	}
	return "", fmt.Errorf("`%s` is not closed by `};`", open)
}

func nixDropFromBlock(text, open, attr string) (string, error) {
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == open {
			start = i
			break
		}
	}
	if start < 0 {
		return "", fmt.Errorf("no `%s` block", open)
	}
	for i := start + 1; i < len(lines); i++ {
		code := strings.TrimSpace(lines[i])
		if code == "};" {
			return text, nil
		}
		if rest, ok := strings.CutPrefix(code, attr); ok && rest != "" && strings.ContainsRune(" =", rune(rest[0])) {
			out := append([]string{}, lines[:i]...)
			out = append(out, lines[i+1:]...)
			return strings.Join(out, "\n"), nil
		}
	}
	return "", fmt.Errorf("`%s` is not closed by `};`", open)
}

// --- the exec ---

// execLayer is main's fallthrough: a first argument core has no verb for
// is a layer's name, and fort-<name> on PATH takes the rest, in this
// process (git style). The estate root rides in the environment so the
// layer need not find it again. A name with no binary is one error that
// names both, and says where a layer comes from.
func execLayer(verb string, args []string) error {
	if verb == "help" || strings.HasPrefix(verb, "-") {
		usage()
		os.Exit(2)
	}
	if !nameOK(verb) {
		return fmt.Errorf("unknown verb %q", verb)
	}
	bin := "fort-" + verb
	path, err := exec.LookPath(bin)
	if err != nil {
		return fmt.Errorf("unknown verb %q, and no %s on PATH — a layer's binary (docs/layers.md; fort layer add installs one)", verb, bin)
	}
	env := os.Environ()
	if root, err := repoRoot(); err == nil {
		env = append(env, "FORT_ESTATE_ROOT="+root)
	}
	return syscall.Exec(path, append([]string{bin}, args...), env)
}

// --- what check says ---

// installedLayers lists layers/<name>/ in the estate, sorted.
func installedLayers(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, layersDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && nameOK(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// layerFindings (fort check --all): the installed layers, by name, and
// nothing under them — each has its own gate step. A directory with no
// estate.toml is a layer whose init has not run, or a stray: a warning
// that names the verb.
func layerFindings(root string) []finding {
	names, err := installedLayers(root)
	if err != nil {
		return []finding{{"error", layersDir, err.Error()}}
	}
	if len(names) == 0 {
		return []finding{{"ok", layersDir, "no layer installed (docs/layers.md)"}}
	}
	out := []finding{{"ok", layersDir, "installed: " + strings.Join(names, ", ") + " — each runs its own gate (fort <layer> check --all)"}}
	// The one seam a layer has into the read-only ops identity (#169):
	// the aggregated role in the estate's ops-readonly.yaml. init never
	// rewrites that file, so an estate rendered before the seam adds the
	// two objects by hand; until then a layer's labelled rule has no
	// taker and ops-ro reads none of the layer's kinds.
	if !opsAggregates(root) {
		out = append(out, finding{"warning", opsReadFile, "carries no ClusterRole " + opsAggregatedRole + " — a layer's read rule (labelled sfi.ops/read: \"true\") has no taker, so ops-ro reads none of its kinds; add the aggregated role and its binding from the init template (docs/layers.md, 5)"})
	}
	for _, n := range names {
		if _, err := os.Stat(filepath.Join(root, layersDir, n, "estate.toml")); err != nil {
			out = append(out, finding{"warning", filepath.Join(layersDir, n), "no estate.toml — the layer's init writes it (fort " + n + " init), or the directory is a stray"})
		}
	}
	return out
}

// The read-only ops identity's seam for a layer (#169).
const (
	opsReadFile       = "flux/infrastructure/configs/ops-readonly.yaml"
	opsAggregatedRole = "ops-ro-layers-read"
)

// opsAggregates says the estate's ops-readonly.yaml carries the
// aggregated role, by line.
func opsAggregates(root string) bool {
	raw, err := os.ReadFile(filepath.Join(root, opsReadFile))
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) == "name: "+opsAggregatedRole {
			return true
		}
	}
	return false
}
