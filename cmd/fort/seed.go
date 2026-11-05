package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/contract"
	"gitea.zen.lofi/sfi/forts/internal/inventory"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// seed executes a fort's inventories into sops-encrypted Secrets
// (docs/reference/inventory.md). For each dir the effective inventory is
// the committed forts.inventory.yaml when there is one, else the one derived
// from the chart's own contract (pulled at the pin the dir's
// OCIRepository names) merged with the fort-side overlay. Custody
// rules: decrypted material flows through memory only; a new file is
// written 0600 then encrypted in place under the repo's creation rules;
// an existing file is never rewritten — missing keys are added with
// `sops set`, rotation is deliberate. No value is ever printed.
//
// The inventory is the core's schema, and a layer that renders a
// directory of its own writes one too (sfi/fort-layers): this one seed
// executes every inventory of the fort, in dependency order, whoever
// rendered it.
func seed(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: fort seed <fort> [<dir>...] [--set [FILE/]KEY] [--chart-dir path] [--dry-run]")
	}
	o := seedOpts{fort: args[0]}
	var dirs []string
	rest := args[1:]
	for len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		dirs = append(dirs, rest[0])
		rest = rest[1:]
	}
	fs := flag.NewFlagSet("seed", flag.ContinueOnError)
	fs.StringVar(&o.setKey, "set", "", "set this key from stdin (an external key, or a deliberate replace) — needs exactly one <dir>; FILE/KEY when two Secrets in it share the key name")
	fs.StringVar(&o.chartDir, "chart-dir", "", "read the chart's contract from a local dir instead of pulling (tests, chart authors)")
	dryRun := fs.Bool("dry-run", false, "print the plan and seed nothing (default: seed, encrypt, stage)")
	fs.Bool("write", true, "deprecated no-op: writing is the default")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	o.write = !*dryRun
	// flag stops at the first positional: `--dry-run <dir>` leaves the
	// dir in fs.Args(); dropping it would silently widen the scope to
	// the whole fort.
	for _, a := range fs.Args() {
		if strings.HasPrefix(a, "-") {
			return fmt.Errorf("unexpected argument %q after positional dirs", a)
		}
		dirs = append(dirs, a)
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	o.root, o.dirs, o.tool = root, dirs, sopsCLI{root: root}
	o.setFile, o.setKey = splitSetArg(o.setKey)
	if o.setKey != "" && len(dirs) != 1 {
		return fmt.Errorf("--set wants exactly one <dir>")
	}
	// Plan BEFORE taking the value: a key the contract does not declare
	// (or a checkout whose contract does not declare it yet) is refused
	// by name here, not after the operator has pasted a secret into it.
	plans, err := planSeed(o)
	if err != nil {
		return err
	}
	if o.setKey != "" && o.write {
		if o.setValue, err = readSetValue(o.setArg(), setTarget(plans)); err != nil {
			return err
		}
	}
	for _, p := range plans {
		if o.write {
			if err := p.execute(o); err != nil {
				return err
			}
		}
		p.report(os.Stdout, o.write)
	}
	if !o.write {
		fmt.Println("dry run — nothing seeded.")
	}
	return nil
}

type seedOpts struct {
	root, fort, chartDir string
	dirs                 []string // dirs relative to the fort's apps root; empty = every inventory dir of the fort
	setKey, setValue     string
	setFile              string // --set FILE/KEY: the one Secret file the key is set in
	// setValue is filled after planning (see seed), and only for a
	// write: a dry run reports what --set would touch without asking.
	write  bool
	tool   secretsTool
	charts map[string]pulledChart // optional pull cache; nil = pull + discard per dir
}

type pulledChart struct{ dir, version string }

type seedPlan struct {
	dir    string // repo-relative
	source string // where the inventory came from
	inv    *inventory.Inventory
	files  []seedFile
}

type seedFile struct {
	sf      inventory.SecretFile
	path    string // repo-relative
	exists  bool
	missing []inventory.NamedSpec // inventory keys (non-external) absent from the existing file
	absent  []inventory.NamedSpec // external keys that stay absent
	set     *inventory.NamedSpec  // --set target
	created bool                  // set by execute
	added   []string              // set by execute
}

// planSeed resolves the effective inventory for every dir, in
// dependency order (a dir whose sops: refs point into another dir of the
// same fort comes after it), and decides per Secret file what seeding
// would do.
func planSeed(o seedOpts) ([]*seedPlan, error) {
	if _, err := manifest.Load(o.root, o.fort); err != nil {
		return nil, err
	}
	apps := filepath.Join("flux/apps", o.fort)
	dirs := o.dirs
	explicit := len(dirs) > 0
	if !explicit {
		var err error
		if dirs, err = inventoryDirs(o.root, apps); err != nil {
			return nil, err
		}
	}
	var plans []*seedPlan
	for _, d := range dirs {
		d = appDir(apps, d)
		if !strings.HasPrefix(d, apps+"/") {
			return nil, fmt.Errorf("%s is not under %s", d, apps)
		}
		inv, source, err := effectiveInventory(o, d)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d, err)
		}
		if inv == nil {
			if explicit {
				return nil, fmt.Errorf("%s: nothing to seed — no %s and no pinned chart shipping a contract (typo?)", d, inventory.FileName)
			}
			continue // auto-enumerated dir with neither (e.g. gateway/)
		}
		p := &seedPlan{dir: d, source: source, inv: inv}
		for _, sf := range inv.Secrets {
			f := seedFile{sf: sf, path: filepath.Join(d, sf.File)}
			f.exists = exists(o.root, f.path)
			present := map[string]bool{}
			if f.exists {
				names, err := secretKeyNames(filepath.Join(o.root, f.path))
				if err != nil {
					return nil, fmt.Errorf("%s: %w", f.path, err)
				}
				for _, n := range names {
					present[n] = true
				}
			}
			for _, k := range sf.Keys {
				if o.setKey == k.Name && (o.setFile == "" || o.setFile == sf.File) {
					kk := k
					f.set = &kk
					continue
				}
				if k.Spec.External != "" {
					if !present[k.Name] {
						f.absent = append(f.absent, k)
					}
					continue
				}
				if f.exists && !present[k.Name] {
					f.missing = append(f.missing, k)
				}
			}
			p.files = append(p.files, f)
		}
		if o.setKey != "" {
			var claimed, known []string
			for _, f := range p.files {
				if f.set != nil {
					claimed = append(claimed, f.sf.File)
				}
				for _, k := range f.sf.Keys {
					known = append(known, f.sf.File+"/"+k.Name)
				}
			}
			if len(claimed) == 0 {
				return nil, fmt.Errorf("--set %s: not a key of any Secret in %s (keys: %s)", o.setArg(), d, strings.Join(known, ", "))
			}
			// One value, one file. Two Secrets in a dir may well share a
			// key name; a bare KEY would then be set in every one of
			// them, and the prompt names only the first. Refuse and ask
			// for the qualified form, the same file/key a sops ref spells.
			if len(claimed) > 1 {
				return nil, fmt.Errorf("--set %s: a key of %d Secrets in %s (%s) — say which: --set %s/%s", o.setKey, len(claimed), d, strings.Join(claimed, ", "), claimed[0], o.setKey)
			}
		}
		plans = append(plans, p)
	}
	return orderPlans(plans)
}

// inventoryDirs lists every dir under a fort's apps root that carries an
// inventory, an overlay or a pinned chart, at any depth — the app dirs
// the core renders one level down, and whatever a layer renders beneath
// them. A dir is listed once; its subdirectories are walked too.
func inventoryDirs(root, apps string) ([]string, error) {
	var dirs []string
	var walk func(d string) error
	walk = func(d string) error {
		entries, err := os.ReadDir(filepath.Join(root, d))
		if err != nil {
			return err
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			sub := filepath.Join(d, e.Name())
			if inventory.Present(filepath.Join(root, sub)) || contract.Present(filepath.Join(root, sub)) || exists(root, filepath.Join(sub, "repository.yaml")) {
				dirs = append(dirs, sub)
			}
			if err := walk(sub); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(apps); err != nil {
		return nil, err
	}
	return dirs, nil
}

// appDir resolves a <dir> argument: a path relative to the fort's apps
// root (`symbology`, `forts/postgres` — what a layer renders beneath
// it), or the repo-relative path itself when it already starts with
// the apps root.
func appDir(apps, d string) string {
	d = filepath.Clean(d)
	if d == apps || strings.HasPrefix(d, apps+"/") {
		return d
	}
	return filepath.Join(apps, d)
}

// effectiveInventory returns the dir's inventory and where it came
// from: the committed forts.inventory.yaml, or the one derived from the
// chart's contract merged with the committed overlay. nil for dirs with
// neither.
func effectiveInventory(o seedOpts, dir string) (*inventory.Inventory, string, error) {
	if inv, err := inventory.Load(filepath.Join(o.root, dir)); inv != nil || err != nil {
		return inv, inventory.FileName, err
	}
	var overlayRaw []byte
	if raw, _, err := contract.Read(filepath.Join(o.root, dir)); err == nil {
		overlayRaw = raw
	}
	pin, err := readChartPin(filepath.Join(o.root, dir, "repository.yaml"))
	if err != nil || pin == nil {
		if overlayRaw != nil {
			return nil, "", fmt.Errorf("an overlay needs a chart to overlay: no OCIRepository pin in repository.yaml")
		}
		return nil, "", err
	}
	chartDir := o.chartDir
	version := pin.version
	if chartDir == "" {
		key := pin.key()
		if pc, ok := o.charts[key]; ok {
			chartDir, version = pc.dir, pc.version
		} else {
			d, v, err := resolveChart(o.root, *pin)
			if err != nil {
				return nil, "", err
			}
			if o.charts != nil {
				o.charts[key] = pulledChart{d, v}
			} else {
				defer os.RemoveAll(filepath.Dir(d))
			}
			chartDir, version = d, v
		}
	}
	c, err := contract.Load(chartDir)
	if err != nil {
		return nil, "", err
	}
	if !c.Present {
		if overlayRaw != nil {
			return nil, "", fmt.Errorf("chart %s ships no contract for the overlay to extend", pin.url)
		}
		return nil, "", nil
	}
	app := filepath.Base(dir)
	ov, err := parseOverlay(overlayRaw)
	if err != nil {
		return nil, "", err
	}
	keys, _, unresolved, err := inventoryFromChart(c, ov)
	if err != nil {
		return nil, "", err
	}
	if len(unresolved) > 0 {
		return nil, "", fmt.Errorf("%s compose provider outputs the core does not resolve — a layer renders the provider and writes each key's source into the dir's %s (sfi/fort-layers), or the overlay names it", unresolvedNote(unresolved), inventory.FileName)
	}
	inv := &inventory.Inventory{Sources: ov.Sources}
	// No keys means no Secret: the chart declares none, the overlay
	// adds none, so seed creates no file and `fort check` demands none.
	// `fort add` renders the same shape.
	if len(keys) > 0 {
		inv.Secrets = []inventory.SecretFile{{File: "secrets.sops.yaml", Name: app + "-env", Namespace: app, Keys: keys}}
	}
	return inv, fmt.Sprintf("chart contract (%s %s)%s", pin.url, version, map[bool]string{true: " + overlay", false: ""}[overlayRaw != nil]), nil
}

// orderPlans sorts by the sops: refs between dirs of the same fort: a
// source before the dirs that read it. Cross-fort refs are outside the
// set (they must already be seeded). Ties stay alphabetical.
func orderPlans(plans []*seedPlan) ([]*seedPlan, error) {
	byDir := map[string]*seedPlan{}
	for _, p := range plans {
		byDir[p.dir] = p
	}
	deps := map[string]map[string]bool{}
	for _, p := range plans {
		deps[p.dir] = map[string]bool{}
		refs := func(specs []inventory.NamedSpec) {
			for _, s := range specs {
				if s.Spec.Sops != nil {
					target := filepath.Clean(filepath.Join(p.dir, filepath.Dir(s.Spec.Sops[0])))
					if target != p.dir && byDir[target] != nil {
						deps[p.dir][target] = true
					}
				}
			}
		}
		refs(p.inv.Vars)
		for _, sf := range p.inv.Secrets {
			refs(sf.Keys)
		}
	}
	names := make([]string, 0, len(plans))
	for _, p := range plans {
		names = append(names, p.dir)
	}
	sort.Strings(names)
	var out []*seedPlan
	done := map[string]bool{}
	for len(done) < len(plans) {
		progressed := false
		for _, d := range names {
			if done[d] {
				continue
			}
			ready := true
			for dep := range deps[d] {
				if !done[dep] {
					ready = false
				}
			}
			if ready {
				out = append(out, byDir[d])
				done[d] = true
				progressed = true
			}
		}
		if !progressed {
			return nil, fmt.Errorf("sops: refs between %v form a cycle", names)
		}
	}
	return out, nil
}

// --- execution

type resolver struct {
	root, dir string
	inv       *inventory.Inventory
	tool      secretsTool
	vars      map[string]string
	envs      map[string]map[string]string
	sopsDocs  map[string]any
}

func (p *seedPlan) execute(o seedOpts) error {
	r := &resolver{root: o.root, dir: p.dir, inv: p.inv, tool: o.tool, vars: map[string]string{}, envs: map[string]map[string]string{}, sopsDocs: map[string]any{}}
	for _, v := range p.inv.Vars {
		val, err := r.resolve(v.Spec, "var "+v.Name)
		if err != nil {
			return err
		}
		r.vars[v.Name] = val
	}
	for i := range p.files {
		f := &p.files[i]
		full := filepath.Join(o.root, f.path)
		if f.set != nil {
			if !f.exists {
				return fmt.Errorf("--set %s: %s does not exist yet — seed it first", o.setArg(), f.path)
			}
			if err := o.tool.Set(full, f.set.Name, o.setValue); err != nil {
				return err
			}
			f.added = append(f.added, f.set.Name)
			continue
		}
		if f.exists {
			for _, k := range f.missing {
				val, err := r.resolve(k.Spec, k.Name)
				if err != nil {
					return err
				}
				if err := o.tool.Set(full, k.Name, val); err != nil {
					return err
				}
				f.added = append(f.added, k.Name)
			}
			continue
		}
		stringData := yaml.Node{Kind: yaml.MappingNode}
		for _, k := range f.sf.Keys {
			if k.Spec.External != "" {
				continue
			}
			val, err := r.resolve(k.Spec, k.Name)
			if err != nil {
				return err
			}
			stringData.Content = append(stringData.Content,
				&yaml.Node{Kind: yaml.ScalarNode, Value: k.Name},
				&yaml.Node{Kind: yaml.ScalarNode, Value: val, Style: yaml.DoubleQuotedStyle})
		}
		typ := f.sf.Type
		if typ == "" {
			typ = "Opaque"
		}
		doc := map[string]any{
			"apiVersion": "v1", "kind": "Secret",
			"metadata":   map[string]any{"name": f.sf.Name, "namespace": f.sf.Namespace},
			"type":       typ,
			"stringData": &stringData,
		}
		body, err := yaml.Marshal(doc)
		if err != nil {
			return err
		}
		fd, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		header := fmt.Sprintf("# Seeded by `fort seed` from %s\n# (%d keys; sources documented in the inventory, values never in git history)\n", p.source, len(stringData.Content)/2)
		if _, err := fd.WriteString(header + string(body)); err != nil {
			fd.Close()
			return err
		}
		fd.Close()
		if err := o.tool.EncryptInPlace(full); err != nil {
			os.Remove(full)
			return err
		}
		if err := o.tool.GitAdd(f.path); err != nil {
			return err
		}
		f.created = true
	}
	return nil
}

func (r *resolver) resolve(s inventory.Spec, what string) (string, error) {
	switch {
	case s.Literal != nil:
		return *s.Literal, nil
	case s.Generate != "":
		return generate(s.Generate)
	case s.Template != "":
		out := s.Template
		for name, val := range r.vars {
			out = strings.ReplaceAll(out, "${"+name+"}", val)
		}
		if refs := outputRefs(out); len(refs) > 0 {
			return "", fmt.Errorf("%s: composes %s, a provider output the core does not resolve — a layer rewrites the key to a source it renders (sfi/fort-layers)", what, strings.Join(refs, ", "))
		}
		if strings.Contains(out, "${") {
			return "", fmt.Errorf("%s: unresolved var in template", what)
		}
		return out, nil
	case s.Env != nil:
		source, key := s.Env[0], s.Env[1]
		env, ok := r.envs[source]
		if !ok {
			path, ok := r.inv.Sources[source]
			if !ok {
				return "", fmt.Errorf("%s: unknown source %q", what, source)
			}
			if strings.HasPrefix(path, "~/") {
				home, _ := os.UserHomeDir()
				path = filepath.Join(home, path[2:])
			}
			raw, err := r.tool.Decrypt(path)
			if err != nil {
				return "", err
			}
			env = parseDotenv(string(raw))
			r.envs[source] = env
		}
		v, ok := env[key]
		if !ok {
			return "", fmt.Errorf("%s: %s has no key %s", what, source, key)
		}
		return v, nil
	case s.Sops != nil:
		rel, dotted := s.Sops[0], s.Sops[1]
		rp, err := sopsRefPath(r.dir, rel)
		if err != nil {
			return "", fmt.Errorf("%s: %w", what, err)
		}
		path := filepath.Join(r.root, rp)
		doc, ok := r.sopsDocs[path]
		if !ok {
			raw, err := r.tool.Decrypt(path)
			if err != nil {
				return "", err
			}
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				return "", fmt.Errorf("%s: %s: %w", what, rel, err)
			}
			r.sopsDocs[path] = doc
		}
		cur := doc
		for _, part := range strings.Split(dotted, ".") {
			m, ok := cur.(map[string]any)
			if !ok {
				return "", fmt.Errorf("%s: %s has no %s", what, rel, dotted)
			}
			cur, ok = m[part]
			if !ok {
				return "", fmt.Errorf("%s: %s has no %s", what, rel, dotted)
			}
		}
		// A null or a submap would otherwise seed the literal "<nil>" /
		// "map[...]" into every template that composes it — failing only
		// at runtime with an opaque error.
		switch v := cur.(type) {
		case string:
			return v, nil
		case int, int64, float64, bool:
			return fmt.Sprint(v), nil
		case nil:
			return "", fmt.Errorf("%s: %s %s is null (blanked out? reseed or rotate the source)", what, rel, dotted)
		default:
			return "", fmt.Errorf("%s: %s %s is not a scalar (%T)", what, rel, dotted, v)
		}
	}
	return "", fmt.Errorf("%s: external keys are not resolved", what)
}

var generateRe = regexp.MustCompile(`^(hex|b64)_?(\d+)$`)

// generate: hexN / b64N (underscore optional) — N random bytes, encoded.
func generate(arg string) (string, error) {
	m := generateRe.FindStringSubmatch(arg)
	if m == nil {
		return "", fmt.Errorf("unknown generator %q", arg)
	}
	n, _ := strconv.Atoi(m[2])
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	if m[1] == "hex" {
		return hex.EncodeToString(buf), nil
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func parseDotenv(s string) map[string]string {
	env := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}
		k, v, _ := strings.Cut(line, "=")
		v = strings.TrimSpace(v)
		v = strings.Trim(v, `"'`)
		env[strings.TrimSpace(k)] = v
	}
	return env
}

// report prints the ◆ lines for one dir. Never a value.
func (p *seedPlan) report(w io.Writer, written bool) {
	fmt.Fprintf(w, "%s/   (%s)\n", p.dir, p.source)
	if len(p.files) == 0 {
		fmt.Fprintln(w, "  no Secret — the inventory declares no keys")
	}
	for _, f := range p.files {
		var note string
		switch {
		case f.set != nil:
			note = "set " + f.set.Name
		case f.created:
			note = fmt.Sprintf("created (%d keys)", len(f.sf.Keys)-len(f.absent))
		case f.exists && len(f.missing) > 0 && written:
			note = "+ " + strings.Join(f.added, ", ")
		case f.exists && len(f.missing) > 0:
			note = "exists; missing " + strings.Join(names(f.missing), ", ") + " (seed adds them)"
		case f.exists:
			note = "exists, complete (rotation is deliberate: --set replaces one key)"
		default:
			note = fmt.Sprintf("create (%d keys)", len(f.sf.Keys)-len(f.absent))
		}
		fmt.Fprintf(w, "  ◆ %-28s %s\n", f.sf.File, note)
		for _, k := range f.absent {
			opt := ""
			if k.Spec.Optional {
				opt = " (optional)"
			}
			fmt.Fprintf(w, "      absent until set%s: %s — %s\n", opt, k.Name, k.Spec.External)
		}
	}
}

func names(specs []inventory.NamedSpec) []string {
	var out []string
	for _, s := range specs {
		out = append(out, s.Name)
	}
	return out
}

// splitSetArg reads --set's argument: KEY, or FILE/KEY naming the one
// Secret file of the dir the key is set in — the file/key pair a sops
// ref in a contract spells, so the two read alike.
func splitSetArg(arg string) (file, key string) {
	if i := strings.LastIndex(arg, "/"); i >= 0 {
		return arg[:i], arg[i+1:]
	}
	return "", arg
}

// setArg is --set's argument as the operator typed it, for messages.
func (o seedOpts) setArg() string {
	if o.setFile != "" {
		return o.setFile + "/" + o.setKey
	}
	return o.setKey
}

// setTarget names the file a --set will write, for the prompt.
func setTarget(plans []*seedPlan) string {
	for _, p := range plans {
		for _, f := range p.files {
			if f.set != nil {
				return f.path
			}
		}
	}
	return ""
}

// stdinIsTTY says whether a human is at the keyboard; a variable so a
// test can take the scripted path.
var stdinIsTTY = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// readSetValue takes --set's value: prompted without echo on a
// terminal, from stdin otherwise, so a script can still pipe it in. The
// prompt names the key and the file so that what is about to be
// encrypted is unambiguous.
func readSetValue(key, file string) (string, error) {
	var raw []byte
	var err error
	if stdinIsTTY() {
		fmt.Fprintf(os.Stderr, "Setting %s in %s.\n  Paste the value (not echoed): ", key, file)
		raw, err = term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
	} else {
		raw, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return "", fmt.Errorf("--set %s: reading the value: %w", key, err)
	}
	v := strings.TrimRight(string(raw), "\r\n")
	if v == "" {
		return "", fmt.Errorf("--set %s: empty value", key)
	}
	return v, nil
}
