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

	"gitea.zen.lofi/sfi/deployments/forts/internal/contract"
	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

// seed produces a fort's sops-encrypted Secrets from contracts
// (docs/forts/fort-configuration-contracts.md): the Go successor of the retired provisioning/seed-secrets.py.
// For each app dir the effective contract is the fort-side full form
// when one is committed, else the chart's own contract (pulled at the
// tag the dir's OCIRepository pins) merged with the fort-side overlay
// and resolved against the fort's providers by convention. Custody
// rules are the python seeder's: decrypted material flows through
// memory only; a new file is written 0600 then encrypted in place under
// the repo's creation rules; an existing file is never rewritten —
// missing keys are added with `sops set`, rotation is deliberate. No
// value is ever printed.
func seed(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: fort seed <fort> [<dir>...] [--set [FILE/]KEY] [--chart-dir path] [--write]")
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
	fs.BoolVar(&o.write, "write", false, "seed (default: print the plan)")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	// flag stops at the first positional: `--write <dir>` leaves the dir
	// in fs.Args(); dropping it would silently widen the write scope to
	// the whole fort (review finding).
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
		fmt.Println("dry run — nothing seeded (seed --write).")
	}
	return nil
}

type seedOpts struct {
	root, fort, chartDir string
	dirs                 []string // repo-relative app dirs; empty = every contract dir of the fort
	setKey, setValue     string
	setFile              string // --set FILE/KEY: the one Secret file the key is set in
	// setValue is filled after planning (see seed), and only for --write:
	// a dry run reports what --set would touch without asking for it.
	write  bool
	tool   secretsTool
	charts map[string]pulledChart // optional pull cache (fort check); nil = pull + discard per dir
}

type pulledChart struct{ dir, version string }

type seedPlan struct {
	dir    string // repo-relative
	source string // where the contract came from
	ff     *FullForm
	files  []seedFile
}

type seedFile struct {
	sf      SecretFile
	path    string // repo-relative
	exists  bool
	missing []NamedSpec // contract keys (non-external) absent from the existing file
	absent  []NamedSpec // external keys that stay absent
	set     *NamedSpec  // --set target
	created bool        // set by execute
	added   []string    // set by execute
}

// planSeed resolves the effective contract for every dir, in dependency
// order (a dir whose sops: refs point into another dir of the same fort
// comes after it), and decides per Secret file what seeding would do.
func planSeed(o seedOpts) ([]*seedPlan, error) {
	if _, err := manifest.Load(o.root, o.fort); err != nil {
		return nil, err
	}
	apps := filepath.Join("fleet/apps", o.fort)
	dirs := o.dirs
	explicit := len(dirs) > 0
	if !explicit {
		entries, err := os.ReadDir(filepath.Join(o.root, apps))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			d := filepath.Join(apps, e.Name())
			if e.IsDir() && (contract.Present(filepath.Join(o.root, d)) || exists(o.root, filepath.Join(d, "repository.yaml"))) {
				dirs = append(dirs, d)
			}
		}
	}
	var plans []*seedPlan
	for _, d := range dirs {
		d = appDir(apps, d)
		if !strings.HasPrefix(d, apps+"/") {
			return nil, fmt.Errorf("%s is not under %s", d, apps)
		}
		ff, _, source, err := effectiveContract(o, d)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", d, err)
		}
		if ff == nil {
			if explicit {
				return nil, fmt.Errorf("%s: nothing to seed — no %s and no pinned chart shipping one (typo?)", d, contract.FileName)
			}
			continue // auto-enumerated dir with neither (e.g. gateway/)
		}
		p := &seedPlan{dir: d, source: source, ff: ff}
		for _, sf := range ff.Secrets {
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
			// key name (a Storage CR's access-key/secret-key per host —
			// #375); a bare KEY would then be set in every one of them,
			// and the prompt names only the first. Refuse and ask for
			// the qualified form, the same file/key a sops ref spells.
			if len(claimed) > 1 {
				return nil, fmt.Errorf("--set %s: a key of %d Secrets in %s (%s) — say which: --set %s/%s", o.setKey, len(claimed), d, strings.Join(claimed, ", "), claimed[0], o.setKey)
			}
		}
		plans = append(plans, p)
	}
	return orderPlans(plans)
}

// appDir resolves a <dir> argument: a bare app name (`sites-db`, the
// form docs/forts/fort-configuration-contracts.md uses) lives under the fort's apps root; anything with
// a slash is taken as repo-relative and must land there too.
func appDir(apps, d string) string {
	d = filepath.Clean(d)
	if !strings.Contains(d, "/") {
		return filepath.Join(apps, d)
	}
	return d
}

// effectiveContract returns the dir's full-form contract and where it
// came from: the committed full form, or the chart's contract merged
// with the committed overlay. nil for dirs with neither.
func effectiveContract(o seedOpts, dir string) (*FullForm, *contract.Contract, string, error) {
	var overlay []byte
	if raw, name, err := contract.Read(filepath.Join(o.root, dir)); err == nil {
		if isFullForm(raw) {
			ff, err := parseFullForm(raw)
			return ff, nil, name + " (full form)", err
		}
		overlay = raw
	}
	pin, err := readChartPin(filepath.Join(o.root, dir, "repository.yaml"))
	if err != nil || pin == nil {
		if overlay != nil {
			return nil, nil, "", fmt.Errorf("an overlay needs a chart to overlay: no OCIRepository pin in repository.yaml")
		}
		return nil, nil, "", nil
	}
	chartDir := o.chartDir
	version := pin.version
	if chartDir == "" {
		key := pin.url + "@" + pin.version + "@" + pin.followPR
		if pc, ok := o.charts[key]; ok {
			chartDir, version = pc.dir, pc.version
		} else {
			d, v, err := resolveChart(o.root, pin.url, pin.version, pin.followPR)
			if err != nil {
				return nil, nil, "", err
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
		if os.IsNotExist(err) {
			if overlay != nil {
				return nil, nil, "", fmt.Errorf("chart %s ships no contract for the overlay to extend", pin.url)
			}
			return nil, nil, "", nil
		}
		return nil, nil, "", err
	}
	app := filepath.Base(dir)
	ov, err := parseOverlay(overlay)
	if err != nil {
		return nil, nil, "", err
	}
	providers, err := providersForSeed(o.root, o.fort, app, c)
	if err != nil {
		return nil, nil, "", err
	}
	vars, keys, _, err := fullFormFromChart(c, providers)
	if err != nil {
		return nil, nil, "", err
	}
	ff := &FullForm{Sources: ov.Sources}
	for _, v := range vars {
		ff.Vars = append(ff.Vars, NamedSpec{v.Name, Spec{Sops: []string{v.File, v.Path}}})
	}
	// Overlay keys win; overlay-only keys are appended (a migration
	// superset).
	for i, k := range keys {
		if s, ok := ov.Keys[k.Name]; ok {
			keys[i].Spec = s
			delete(ov.Keys, k.Name)
		}
	}
	for _, name := range ov.KeyOrder {
		if s, ok := ov.Keys[name]; ok {
			keys = append(keys, NamedSpec{name, s})
		}
	}
	// No keys means no Secret (#460): the chart declares none, the
	// overlay adds none, so seed creates no file and `fort check`
	// demands none. `fort add` renders the same shape.
	if len(keys) > 0 {
		ff.Secrets = []SecretFile{{File: "secrets.sops.yaml", Name: app + "-env", Namespace: app, Keys: keys}}
	}
	return ff, c, fmt.Sprintf("chart contract (%s %s)%s", pin.url, version, map[bool]string{true: " + overlay", false: ""}[overlay != nil]), nil
}

// overlay is the fort-side file when it is not the full form: the same
// schema as the chart contract, of which seed honours `sources` and
// `secret.keys` (provider placement overrides are not live yet —
// placement is by convention).
type overlay struct {
	Sources  map[string]string
	Keys     map[string]Spec
	KeyOrder []string
}

func parseOverlay(raw []byte) (*overlay, error) {
	ov := &overlay{Sources: map[string]string{}, Keys: map[string]Spec{}}
	if raw == nil {
		return ov, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return ov, nil
	}
	for k, v := range pairs(doc.Content[0]) {
		switch k {
		case "sources":
			for name, val := range pairs(v) {
				ov.Sources[name] = val.Value
			}
		case "secret":
			for sk, sv := range pairs(v) {
				if sk != "keys" {
					continue
				}
				specs, err := namedSpecs(sv, "overlay secret.keys")
				if err != nil {
					return nil, err
				}
				for _, s := range specs {
					ov.Keys[s.Name] = s.Spec
					ov.KeyOrder = append(ov.KeyOrder, s.Name)
				}
			}
		}
	}
	return ov, nil
}

type chartPin struct {
	url, version, followPR string
}

var prFilter = regexp.MustCompile(`^\^0\\\.0\\\.0-pr\\\.(\d+)\\\.`)

// readChartPin reads the dir's OCIRepository: a tag pin, or the
// semver-filter form `fort add --follow-pr` writes.
func readChartPin(path string) (*chartPin, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var doc struct {
		Kind string `yaml:"kind"`
		Spec struct {
			URL string `yaml:"url"`
			Ref struct {
				Tag          string `yaml:"tag"`
				SemverFilter string `yaml:"semverFilter"`
			} `yaml:"ref"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil || doc.Kind != "OCIRepository" {
		return nil, nil
	}
	pin := &chartPin{url: doc.Spec.URL, version: doc.Spec.Ref.Tag}
	if m := prFilter.FindStringSubmatch(doc.Spec.Ref.SemverFilter); m != nil {
		pin.followPR = m[1]
	}
	if pin.version == "" && pin.followPR == "" {
		return nil, fmt.Errorf("repository.yaml: OCIRepository has neither ref.tag nor a --follow-pr semverFilter")
	}
	return pin, nil
}

// providersForSeed resolves the app's required instances the way add
// did: conventional outputs, overridden by what the HelmRelease's wire
// values actually say (the valkey db index, a moved host).
func providersForSeed(root, fort, app string, c *contract.Contract) (map[string]providerBinding, error) {
	apps := filepath.Join("fleet/apps", fort)
	values := releaseValues(filepath.Join(root, apps, app, "release.yaml"))
	out := map[string]providerBinding{}
	for _, r := range c.Instances() {
		key := r.Type + "." + r.Handle
		providerDir, known := providerDirs[r.Type]
		if !known {
			if r.Instance.Optional {
				continue
			}
			return nil, fmt.Errorf("requires.%s: provider type not implemented", key)
		}
		if !exists(root, filepath.Join(apps, providerDir)) {
			if r.Instance.Optional {
				continue
			}
			return nil, fmt.Errorf("requires.%s: no %s on %s (fort add renders it)", key, providerDir, fort)
		}
		// Conventions, then what the HelmRelease actually says (the db
		// index add assigned, a moved host) — the wire values are the
		// record once the tree exists.
		b := conventionalBinding(r.Type, r.Handle, app, 0)
		for outName, path := range r.Instance.Wire {
			if v, ok := lookupPath(values, path); ok {
				b.outputs[outName] = v
			}
		}
		out[key] = b
	}
	return out, nil
}

func releaseValues(path string) map[string]any {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var doc struct {
		Spec struct {
			Values map[string]any `yaml:"values"`
		} `yaml:"spec"`
	}
	if yaml.Unmarshal(raw, &doc) != nil {
		return nil
	}
	return doc.Spec.Values
}

func lookupPath(m map[string]any, path string) (string, bool) {
	var cur any = m
	for _, p := range strings.Split(path, ".") {
		mm, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		cur, ok = mm[p]
		if !ok {
			return "", false
		}
	}
	switch v := cur.(type) {
	case string:
		return v, true
	case int, int64, float64, bool:
		return fmt.Sprint(v), true
	}
	return "", false
}

// secretKeyNames reads the stringData/data key names of a committed
// Secret — plaintext under the repo's encrypted_regex, so no decryption.
func secretKeyNames(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		StringData map[string]any `yaml:"stringData"`
		Data       map[string]any `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var names []string
	for k := range doc.StringData {
		names = append(names, k)
	}
	for k := range doc.Data {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

// orderPlans sorts by the sops: refs between dirs of the same fort:
// providers first, then their tenants. Cross-fort refs are outside the
// set (they must already be seeded). Ties stay alphabetical.
func orderPlans(plans []*seedPlan) ([]*seedPlan, error) {
	byDir := map[string]*seedPlan{}
	for _, p := range plans {
		byDir[p.dir] = p
	}
	deps := map[string]map[string]bool{}
	for _, p := range plans {
		deps[p.dir] = map[string]bool{}
		refs := func(specs []NamedSpec) {
			for _, s := range specs {
				if s.Spec.Sops != nil {
					target := filepath.Clean(filepath.Join(p.dir, filepath.Dir(s.Spec.Sops[0])))
					if target != p.dir && byDir[target] != nil {
						deps[p.dir][target] = true
					}
				}
			}
		}
		refs(p.ff.Vars)
		for _, sf := range p.ff.Secrets {
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
	ff        *FullForm
	tool      secretsTool
	vars      map[string]string
	envs      map[string]map[string]string
	sopsDocs  map[string]any
}

func (p *seedPlan) execute(o seedOpts) error {
	r := &resolver{root: o.root, dir: p.dir, ff: p.ff, tool: o.tool, vars: map[string]string{}, envs: map[string]map[string]string{}, sopsDocs: map[string]any{}}
	for _, v := range p.ff.Vars {
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
		header := fmt.Sprintf("# Seeded by `fort seed` from %s\n# (%d keys; sources documented in the contract, values never in git history)\n", p.source, len(stringData.Content)/2)
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

func (r *resolver) resolve(s Spec, what string) (string, error) {
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
		if strings.Contains(out, "${") {
			return "", fmt.Errorf("%s: unresolved var in template", what)
		}
		return out, nil
	case s.Env != nil:
		source, key := s.Env[0], s.Env[1]
		env, ok := r.envs[source]
		if !ok {
			path, ok := r.ff.Sources[source]
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
		path := filepath.Join(r.root, r.dir, rel)
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
		// at runtime with an opaque error (review finding).
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
		fmt.Fprintln(w, "  no Secret — the contract declares no keys")
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
			note = "exists; missing " + strings.Join(names(f.missing), ", ") + " (seeder adds them)"
		case f.exists:
			note = "exists, complete (rotation is deliberate: fort rotate)"
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

func names(specs []NamedSpec) []string {
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

// readSetValue takes --set's value the way b2Session takes a credential
// (b2session.go): prompted without echo on a terminal, from stdin
// otherwise, so a script can still pipe it in. The prompt names the key
// and the file so that what is about to be encrypted is unambiguous.
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
