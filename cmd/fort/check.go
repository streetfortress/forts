package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/contract"
	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gitea.zen.lofi/sfi/forts/internal/tools"
	"gopkg.in/yaml.v3"
)

// check is the merge gate (docs/fort-configuration-contracts.md, "The gate is the
// product"): for every app dir of a fort, the committed Secret must
// carry what its effective contract requires. Key names are plaintext
// under the repo's encrypted_regex, so no age key is involved; the
// chart's contract is pulled at the pinned tag once per run. A Renovate
// chart bump whose new contract adds a required key therefore goes red
// here until seeded. The fleet half (fleet.go) runs first: tree ↔
// manifests, every flux layer builds, strict envsubst is satisfiable.
//
// Strict on every fort: every fort is the hardened host profile (that is
// what a fort is, not "serves users"), so there is no lenient tier to
// speak of — a missing required key is an error
// anywhere, extras and missing optional keys are warnings.
func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	all := fs.Bool("all", false, "every host with a manifest")
	chartDir := fs.String("chart-dir", "", "read every chart's contract from this local dir instead of pulling (tests)")
	var forts []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		forts = append(forts, args[0])
		args = args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	var problems map[string]error
	var parsed map[string]manifest.Host
	if *all {
		hosts, p, err := manifest.LoadAll(root)
		if err != nil {
			return err
		}
		problems, parsed = p, hosts
		forts = nil
		for name := range hosts {
			forts = append(forts, name)
		}
		sort.Strings(forts)
	}
	if len(forts) == 0 && !*all {
		return fmt.Errorf("usage: fort check (<fort>... | --all)")
	}
	o := checkOpts{root: root, chartDir: *chartDir, charts: map[string]pulledChart{}, annotate: os.Getenv("GITEA_ACTIONS") != "" || os.Getenv("GITHUB_ACTIONS") != ""}
	defer func() {
		for _, pc := range o.charts {
			os.RemoveAll(filepath.Dir(pc.dir))
		}
	}()
	// kustomize is one of the declared tools (internal/tools): the
	// fort package carries it, CI installs its own, a checkout run
	// builds it. Resolved here, before the gate reads a single tree.
	if err := tools.Ensure("kustomize"); err != nil {
		return err
	}
	errors := 0
	if *all {
		errors += reportFindings(os.Stdout, manifest.Dir, manifestFindings(problems), o.annotate)
	}
	known := map[string]bool{}
	for _, f := range forts {
		known[f] = true
	}
	shared := checkFleetShared(root, known, *all)
	// The estate's name is what every cluster's ESTATE_NAME must say
	// (forts#50): read once, judged per fort.
	est, err := estate.Load(root)
	if err != nil {
		return err
	}
	shared.estate = est.Name
	// Cross-fort providers are a fleet-wide invariant, not a per-fort
	// one: the declaration lives in the hub's tree, so it can only be
	// traced with every manifest in hand (--all). The hub relationship
	// (forts#38) is checked from both ends: every fort's file for the
	// monitoring hub, and every subscription the hub owes an estate. The
	// forge's half is its address on this mesh (forts#26).
	if *all {
		shared.findings = append(shared.findings, rustfsIdentityFindings(root, est.LocalHub(estate.RoleMonitoring), parsed)...)
		shared.findings = append(shared.findings, hubFileFindings(root, est, parsed)...)
		shared.findings = append(shared.findings, hubBuildFindings(root, est)...)
		shared.findings = append(shared.findings, servesFindings(root, est, parsed)...)
		shared.findings = append(shared.findings, meshFindings(root, parsed)...)
		shared.findings = append(shared.findings, meshSubnetFindings(root, est, parsed)...)
		shared.findings = append(shared.findings, zoneFindings(root, est, parsed)...)
		shared.findings = append(shared.findings, forgeFindings(root, est, parsed)...)
		shared.findings = append(shared.findings, operatorFindings(root, est, parsed)...)
		shared.findings = append(shared.findings, pruneHolderFindings(root, parsed)...)
	}
	errors += reportFindings(os.Stdout, "fleet", shared.findings, o.annotate)
	for _, fort := range forts {
		entry, ok := parsed[fort]
		if !ok {
			if entry, err = manifest.Load(root, fort); err != nil {
				return err
			}
		}
		findings := checkFleetFort(root, fort, entry, shared)
		// The console failsafe (#524): its two halves live in different
		// files, and either one alone fails quietly (console.go).
		findings = append(findings, consoleFindings(root, fort)...)
		// The pruning key's holder (#400): a host that sweeps a tier of
		// sfi.pv-prune names that tier's key, and the inverse (prune.go).
		findings = append(findings, pruneFindings(root, fort)...)
		if entry.IsCluster() && exists(root, filepath.Join("flux/apps", fort)) {
			cf, err := checkFort(o, fort)
			if err != nil {
				return err
			}
			findings = append(findings, cf...)
		}
		errors += reportFindings(os.Stdout, fort, findings, o.annotate)
	}
	if errors > 0 {
		return fmt.Errorf("%d error(s)", errors)
	}
	fmt.Println("all checks passed")
	return nil
}

// manifestFindings: every manifest parses and validates — exactly one
// provider table, the neutral fields consistent, the table complete
// (manifest.Validate). A host with a bad manifest is inert — nothing
// provisions, previews or destroys it — and that is an error, not a lab
// default.
func manifestFindings(problems map[string]error) []finding {
	var out []finding
	for name, err := range problems {
		out = append(out, finding{"error", manifest.Path(name), strings.TrimPrefix(err.Error(), manifest.Path(name)+": ")})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].dir < out[j].dir })
	return out
}

// operatorFindings (forts#81): who holds `deployer`, the operator
// account of every fort. The framework ships no operator key — the key
// list it carried MERGED with the estate's, so SFI's devices reached
// every estate's forts — so estate.toml's operator_keys is what
// modules/ops/deployer.nix puts in the account's authorized_keys. An
// estate that names none has a fort that does not evaluate (the
// assertion in that module), and the gate says so here, where a reader
// is already looking.
//
// A host file that sets sfi.ops.deployer.authorizedKeys REPLACES the
// estate's list for that one fort, which is how a fort of a shared
// estate carries one team's keys alone. That is a positive declaration,
// so the gate names it per host: a device added to the estate does not
// reach that fort.
func operatorFindings(root string, est estate.Estate, hosts map[string]manifest.Host) []finding {
	var out []finding
	overrides := map[string]bool{}
	for _, name := range sortedNames(hosts) {
		path := filepath.Join("nixos/hosts", name, "default.nix")
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			continue // scaffold has not run; nix-eval is that finding
		}
		for i, line := range strings.Split(string(raw), "\n") {
			code := strings.TrimSpace(line)
			if j := strings.Index(code, "#"); j >= 0 {
				code = strings.TrimSpace(code[:j])
			}
			if !strings.Contains(code, "sfi.ops.deployer.authorizedKeys") {
				continue
			}
			overrides[name] = true
			out = append(out, finding{"warning", path, fmt.Sprintf("line %d sets the operator keys of this fort alone — it replaces the estate's list, so a device added to %s does not reach this host", i+1, estate.Path)})
		}
	}
	ops := est.Operators()
	var devices []string
	for i, op := range ops {
		if op.Name == "" {
			out = append(out, finding{"warning", estate.Path, fmt.Sprintf("operator_keys #%d carries no comment — nothing says which device holds it, and vetting trims the list by device (#415)", i+1)})
			devices = append(devices, fmt.Sprintf("#%d (no device named)", i+1))
			continue
		}
		devices = append(devices, op.Name)
	}
	switch {
	case len(ops) > 0:
		out = append(out, finding{"ok", estate.Path, fmt.Sprintf("%d operator device(s) hold `deployer` on every fort of this estate: %s", len(ops), strings.Join(devices, ", "))})
	case len(hosts) > 0 && len(overrides) == len(hosts):
		out = append(out, finding{"warning", estate.Path, "no operator_keys, and every host file names its own operator keys — the next fort of this estate carries no operator key, and does not evaluate until a host file or this file names one"})
	default:
		out = append(out, finding{"error", estate.Path, "no operator_keys: the framework ships no operator key, so `deployer` holds none and no fort of this estate evaluates (the assertion in modules/ops/deployer.nix). Add one line per operator device, the device as the key's comment: operator_keys = [ \"ssh-ed25519 AAAAC3Nza… macbook-air\" ]"})
	}
	return out
}

type checkOpts struct {
	root, chartDir string
	charts         map[string]pulledChart
	annotate       bool
}

type finding struct {
	level string // "error" | "warning" | "ok"
	dir   string
	msg   string
}

// checkFort evaluates every contract-bearing dir of one fort.
func checkFort(o checkOpts, fort string) ([]finding, error) {
	if _, err := manifest.Load(o.root, fort); err != nil {
		return nil, err
	}
	apps := filepath.Join("flux/apps", fort)
	dirs, err := contractDirs(o.root, apps)
	if err != nil {
		return nil, fmt.Errorf("%s: no flux/apps tree (%w)", fort, err)
	}
	var out []finding
	consumed := sopsRefs{}
	for _, dir := range dirs {
		so := seedOpts{root: o.root, fort: fort, chartDir: o.chartDir, charts: o.charts}
		ff, c, source, err := effectiveContract(so, dir)
		if err != nil {
			out = append(out, finding{"error", dir, err.Error()})
			continue
		}
		if ff == nil {
			continue // pinned chart without a contract, or a dir like gateway/
		}
		before := len(out)
		out = append(out, checkSecrets(o.root, dir, ff)...)
		out = append(out, checkVars(o.root, dir, ff)...)
		if c == nil {
			// A committed full form wins for seeding, but the chart's
			// own declaration (if the pinned chart ships one) is still
			// the source of truth for WHAT is required — this is where a
			// chart bump that adds a key goes red on today's forts.
			pc, err := pinnedContract(so, dir)
			if err != nil {
				out = append(out, finding{"error", dir, err.Error()})
			}
			if pc != nil {
				c = pc
				source += " + chart contract"
				out = append(out, checkDeclared(dir, ff, c)...)
			}
		}
		consumed.collect(dir, ff)
		if c != nil {
			out = append(out, checkWires(o.root, fort, dir, c)...)
			if c.File == contract.LegacyFileName {
				// The chart still ships the pre-#464 name. Loading it
				// keeps working for good; this is the nudge to rename
				// it in the chart repo when that repo is next open.
				out = append(out, finding{"warning", dir, "the pinned chart ships " + contract.LegacyFileName + " — rename it to " + contract.FileName + " in the chart repo (the old name keeps loading)"})
			}
		}
		if len(out) == before {
			out = append(out, finding{"ok", dir, source})
		}
	}
	out = append(out, checkFortsNamespace(o.root, fort)...)
	out = append(out, checkRustfsIdentities(o.root, fort, consumed)...)
	return out, nil
}

// legacyProviderDirs are the pre-#671 provider dirs at the apps root,
// each with a namespace of its own. A fort that still has one converges
// at its re-provision (a renamed CNPG Cluster is a new cluster plus a
// restore, never an edit in place); until then check says so, once per
// dir, and nothing else about them.
var legacyProviderDirs = map[string]string{"postgres": "sites-db", "valkey": "valkey", "s3": "rustfs"}

// checkFortsNamespace: every provider the fort runs lives under forts/,
// in the one Namespace that directory owns (deployments#671). The
// namespace is the container of the set — no label enumerates it — so
// the check is the shape: forts/namespace.yaml is Namespace forts,
// forts/kustomization.yaml lists it and every provider dir that is
// there, and no provider dir carries a namespace of its own (kustomize
// refuses two Namespace documents with the same id).
func checkFortsNamespace(root, fort string) []finding {
	var out []finding
	apps := filepath.Join("flux/apps", fort)
	for _, typ := range contract.Types {
		old := legacyProviderDirs[typ]
		if exists(root, filepath.Join(apps, old)) {
			out = append(out, finding{"warning", filepath.Join(apps, old), "a provider dir at the apps root predates the forts namespace (deployments#671) — the fort converges at the re-provision: fort add renders " + providerDirs[typ] + "/ on the new fort"})
		}
	}
	fortsRoot := filepath.Join(apps, fortsDir)
	var present []string
	for _, typ := range contract.Types {
		dir := filepath.Join(apps, providerDirs[typ])
		if !exists(root, dir) {
			continue
		}
		present = append(present, providerSubdir(typ))
		if exists(root, filepath.Join(dir, "namespace.yaml")) {
			out = append(out, finding{"error", dir, "namespace.yaml: a provider dir carries no namespace of its own — forts/namespace.yaml is the one Namespace every provider shares"})
		}
	}
	if len(present) == 0 {
		return out
	}
	raw, err := os.ReadFile(filepath.Join(root, fortsRoot, "namespace.yaml"))
	if err != nil {
		out = append(out, finding{"error", fortsRoot, "namespace.yaml: " + err.Error()})
	} else {
		var ns struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal(raw, &ns); err != nil {
			out = append(out, finding{"error", fortsRoot, "namespace.yaml: " + err.Error()})
		} else if ns.Kind != "Namespace" || ns.Metadata.Name != fortsNamespace {
			out = append(out, finding{"error", fortsRoot, fmt.Sprintf("namespace.yaml: want Namespace %s, got %s %q — the one namespace every provider the fort runs shares", fortsNamespace, ns.Kind, ns.Metadata.Name)})
		}
	}
	kraw, err := os.ReadFile(filepath.Join(root, fortsRoot, "kustomization.yaml"))
	if err != nil {
		out = append(out, finding{"error", fortsRoot, "kustomization.yaml: " + err.Error()})
		return out
	}
	var k struct {
		Resources []string `yaml:"resources"`
	}
	if err := yaml.Unmarshal(kraw, &k); err != nil {
		out = append(out, finding{"error", fortsRoot, "kustomization.yaml: " + err.Error()})
		return out
	}
	listed := map[string]bool{}
	for _, r := range k.Resources {
		listed[strings.TrimSuffix(r, "/")] = true
	}
	if !listed["namespace.yaml"] {
		out = append(out, finding{"error", fortsRoot, "kustomization.yaml: namespace.yaml not listed — the directory owns the Namespace"})
	}
	for _, sub := range present {
		if !listed[sub] {
			out = append(out, finding{"error", fortsRoot, fmt.Sprintf("kustomization.yaml: %s/ is there but not listed — flux never applies it", sub)})
		}
	}
	return out
}

// sopsRefs is the set of (provider file, key) pairs the fort's effective
// contracts source — "flux/apps/<fort>/rustfs/users.sops.yaml" → keys —
// gathered while the gate walks the app dirs, for the reverse trace.
type sopsRefs map[string]map[string]bool

func (r sopsRefs) collect(dir string, ff *FullForm) {
	add := func(s Spec) {
		if s.Sops == nil {
			return
		}
		parts := strings.SplitN(s.Sops[1], ".", 2)
		if len(parts) != 2 || (parts[0] != "stringData" && parts[0] != "data") {
			return
		}
		file := filepath.Clean(filepath.Join(dir, s.Sops[0]))
		if r[file] == nil {
			r[file] = map[string]bool{}
		}
		r[file][parts[1]] = true
	}
	for _, v := range ff.Vars {
		add(v.Spec)
	}
	for _, sf := range ff.Secrets {
		for _, k := range sf.Keys {
			add(k.Spec)
		}
	}
}

// checkRustfsIdentities is the reverse of checkVars for the fort's
// rustfs: checkVars proves every app's sops ref lands on a key of
// rustfs-users; this proves every key of rustfs-users is sourced by an
// app on the fort. The forward trace catches a missing credential; the
// reverse catches an orphaned one — a live identity scoped to a bucket
// whose objects are still there, which is worse than a stale dir. The
// policy document is traced the same way: the reconciler skips a user
// with no policies/<name>.json, and a document with no user is dead
// text.
func checkRustfsIdentities(root, fort string, consumed sopsRefs) []finding {
	dir := filepath.Join("flux/apps", fort, providerDirs["s3"])
	if !exists(root, dir) {
		return nil
	}
	var out []finding
	declared := map[string]bool{}
	users := filepath.Join(dir, "users.sops.yaml")
	if raw, _, err := contract.Read(filepath.Join(root, dir)); err == nil && isFullForm(raw) {
		if ff, err := parseFullForm(raw); err == nil {
			for _, sf := range ff.Secrets {
				if sf.File == "users.sops.yaml" {
					for _, k := range sf.Keys {
						declared[k.Name] = true
					}
				}
			}
		}
	}
	if exists(root, users) {
		if names, err := secretKeyNames(filepath.Join(root, users)); err == nil {
			for _, n := range names {
				declared[n] = true
			}
		}
	}
	policies, _ := filepath.Glob(filepath.Join(root, dir, "policies", "*.json"))
	hasPolicy := map[string]bool{}
	for _, p := range policies {
		hasPolicy[strings.TrimSuffix(filepath.Base(p), ".json")] = true
	}
	names := make([]string, 0, len(declared))
	for n := range declared {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !consumed[users][n] {
			out = append(out, finding{"error", dir, "ORPHANED identity " + n + ": no app on " + fort + " sources users.sops.yaml stringData." + n + " — a live credential with no consumer; drop the key from " + contract.FileName + " and users.sops.yaml (the CronJob then removes the user)"})
		}
		if !hasPolicy[n] {
			out = append(out, finding{"error", dir, "identity " + n + " has no policies/" + n + ".json — the IAM CronJob skips a user with no policy document"})
		}
	}
	for n := range hasPolicy {
		if !declared[n] {
			out = append(out, finding{"warning", dir, "policies/" + n + ".json names no declared identity — dead document, or a key missing from " + contract.FileName})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].msg < out[j].msg })
	return out
}

// checkSecrets: each Secret file the contract describes exists and
// carries its non-external keys; required externals are errors, optional
// ones and extras are warnings.
func checkSecrets(root, dir string, ff *FullForm) []finding {
	var out []finding
	for _, sf := range ff.Secrets {
		path := filepath.Join(dir, sf.File)
		if !exists(root, path) {
			out = append(out, finding{"error", dir, sf.File + ": not seeded (fort seed)"})
			continue
		}
		names, err := secretKeyNames(filepath.Join(root, path))
		if err != nil {
			out = append(out, finding{"error", dir, sf.File + ": " + err.Error()})
			continue
		}
		have := map[string]bool{}
		for _, n := range names {
			have[n] = true
		}
		want := map[string]bool{}
		var missing, missingOpt, missingExt []string
		for _, k := range sf.Keys {
			want[k.Name] = true
			if have[k.Name] {
				continue
			}
			switch {
			case k.Spec.External != "" && k.Spec.Optional:
				missingOpt = append(missingOpt, k.Name)
			case k.Spec.External != "":
				missingExt = append(missingExt, k.Name+" ("+k.Spec.External+")")
			default:
				missing = append(missing, k.Name)
			}
		}
		var extra []string
		for n := range have {
			if !want[n] {
				extra = append(extra, n)
			}
		}
		sort.Strings(extra)
		if len(missing) > 0 {
			out = append(out, finding{"error", dir, sf.File + ": missing " + strings.Join(missing, ", ") + " — fort seed adds them"})
		}
		if len(missingExt) > 0 {
			out = append(out, finding{"error", dir, sf.File + ": required external key(s) not set: " + strings.Join(missingExt, "; ") + " — fort seed --set KEY"})
		}
		if len(missingOpt) > 0 {
			out = append(out, finding{"warning", dir, sf.File + ": optional external key(s) unset: " + strings.Join(missingOpt, ", ")})
		}
		if len(extra) > 0 {
			out = append(out, finding{"warning", dir, sf.File + ": extra key(s) not in the contract: " + strings.Join(extra, ", ") + " (migration superset?)"})
		}
	}
	return out
}

// pinnedContract loads the contract the dir's pinned chart ships, if
// the dir pins a chart and the chart ships one (nil otherwise).
func pinnedContract(o seedOpts, dir string) (*contract.Contract, error) {
	pin, err := readChartPin(filepath.Join(o.root, dir, "repository.yaml"))
	if err != nil || pin == nil {
		return nil, err
	}
	chartDir := o.chartDir
	if chartDir == "" {
		key := pin.key()
		pc, ok := o.charts[key]
		if !ok {
			d, v, err := resolveChart(o.root, *pin)
			if err != nil {
				return nil, err
			}
			pc = pulledChart{d, v}
			o.charts[key] = pc
		}
		chartDir = pc.dir
	}
	c, err := contract.Load(chartDir)
	if err != nil {
		return nil, err
	}
	if !c.Present {
		return nil, nil // the pinned chart ships no contract (#461)
	}
	return c, nil
}

// checkDeclared: the fort-side full form implements everything the
// chart declares — each required chart key has a fort-side spec (the
// fort may source it however it likes), optional ones warn if absent.
func checkDeclared(dir string, ff *FullForm, c *contract.Contract) []finding {
	have := map[string]bool{}
	for _, sf := range ff.Secrets {
		for _, k := range sf.Keys {
			have[k.Name] = true
		}
	}
	var missing, missingOpt []string
	for _, k := range c.Secret.Keys {
		if have[k.Name] {
			continue
		}
		if k.Optional {
			missingOpt = append(missingOpt, k.Name)
		} else {
			missing = append(missing, k.Name)
		}
	}
	var out []finding
	if len(missing) > 0 {
		out = append(out, finding{"error", dir, "chart contract declares " + strings.Join(missing, ", ") + " — the fort-side " + contract.FileName + " does not carry them (add the key with a source, or drop the full form so fort seed derives it)"})
	}
	if len(missingOpt) > 0 {
		out = append(out, finding{"warning", dir, "chart contract's optional key(s) not carried fort-side: " + strings.Join(missingOpt, ", ")})
	}
	return out
}

// checkVars: every sops: ref (vars and keys) points at a committed file
// that carries the key — a provider seeded before its tenant, or a
// cross-fort source still present.
func checkVars(root, dir string, ff *FullForm) []finding {
	var out []finding
	check := func(what string, s Spec) {
		if s.Sops == nil {
			return
		}
		rel, dotted := s.Sops[0], s.Sops[1]
		path := filepath.Join(dir, rel)
		if !exists(root, path) {
			out = append(out, finding{"error", dir, what + ": sops ref " + rel + " does not exist (seed the provider first)"})
			return
		}
		parts := strings.SplitN(dotted, ".", 2)
		if len(parts) == 2 && (parts[0] == "stringData" || parts[0] == "data") {
			names, err := secretKeyNames(filepath.Join(root, path))
			if err != nil {
				return
			}
			for _, n := range names {
				if n == parts[1] {
					return
				}
			}
			out = append(out, finding{"error", dir, what + ": " + rel + " has no key " + parts[1]})
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
	return out
}

// checkWires: each required provider instance is present on the fort
// (or optional) and its wire: paths are set in the HelmRelease values.
func checkWires(root, fort, dir string, c *contract.Contract) []finding {
	var out []finding
	apps := filepath.Join("flux/apps", fort)
	values := releaseValues(filepath.Join(root, dir, "release.yaml"))
	for _, r := range c.Instances() {
		providerDir := providerDirs[r.Type]
		if !exists(root, filepath.Join(apps, providerDir)) {
			if !r.Instance.Optional {
				out = append(out, finding{"error", dir, fmt.Sprintf("requires.%s.%s: no %s/ on %s (fort add renders it)", r.Type, r.Handle, providerDir, fort)})
			}
			continue
		}
		var unset []string
		for outName, path := range r.Instance.Wire {
			if _, ok := lookupPath(values, path); !ok {
				unset = append(unset, path+" ("+outName+")")
			}
		}
		sort.Strings(unset)
		if len(unset) > 0 {
			out = append(out, finding{"error", dir, fmt.Sprintf("requires.%s.%s: release.yaml values unset: %s", r.Type, r.Handle, strings.Join(unset, ", "))})
		}
	}
	return out
}

// reportFindings prints one fort's findings and returns its error count.
// With annotate, errors and warnings also go out as workflow commands.
func reportFindings(w io.Writer, fort string, findings []finding, annotate bool) int {
	fmt.Fprintf(w, "== %s\n", fort)
	errors := 0
	for _, f := range findings {
		switch f.level {
		case "ok":
			fmt.Fprintf(w, "  ✓ %-40s %s\n", strings.TrimPrefix(f.dir, "flux/apps/"+fort+"/"), f.msg)
		case "warning":
			fmt.Fprintf(w, "  ! %s: %s\n", f.dir, f.msg)
			if annotate {
				fmt.Fprintf(w, "::warning::%s: %s\n", f.dir, f.msg)
			}
		case "error":
			errors++
			fmt.Fprintf(w, "  ✗ %s: %s\n", f.dir, f.msg)
			if annotate {
				fmt.Fprintf(w, "::error::%s: %s\n", f.dir, f.msg)
			}
		default:
			// A typo'd level used to vanish here — the finding was
			// computed, then silently dropped by the switch (cost an
			// hour, 2026-09-16). A gate that loses findings quietly is
			// the one failure it must not have.
			errors++
			fmt.Fprintf(w, "  ✗ %s: %s [unknown finding level %q — this is a bug in the check]\n", f.dir, f.msg, f.level)
		}
	}
	return errors
}
