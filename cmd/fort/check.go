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
	"gitea.zen.lofi/sfi/forts/internal/tools"
)

// check is the merge gate over the core's tree: every manifest parses,
// the tree matches the manifests, every flux layer builds, strict
// envsubst is satisfiable (fleet.go), the mesh, the links, the forge and
// the operator keys hold. The contract gate over an app's Secrets was
// the apps layer's and left with it (forts#143, sfi/fort-layers): a
// layer's check is its own gate step after this one (docs/layers.md).
//
// Strict on every fort: every fort is the hardened host profile (that is
// what a fort is, not "serves users"), so there is no lenient tier to
// speak of — a missing required key is an error
// anywhere, extras and missing optional keys are warnings.
func check(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	all := fs.Bool("all", false, "every host with a manifest")
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
	o := checkOpts{root: root, annotate: os.Getenv("GITEA_ACTIONS") != "" || os.Getenv("GITHUB_ACTIONS") != ""}
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
	if *all {
		shared.findings = append(shared.findings, meshFindings(root, parsed)...)
		shared.findings = append(shared.findings, meshSubnetFindings(root, est, parsed)...)
		shared.findings = append(shared.findings, zoneFindings(root, est, parsed)...)
		shared.findings = append(shared.findings, forgeFindings(root, est, parsed)...)
		shared.findings = append(shared.findings, operatorFindings(root, est, parsed)...)
		shared.findings = append(shared.findings, knownHostsFindings(est)...)
		shared.findings = append(shared.findings, linkFindings(root, est, parsed, sopsCLI{root: root})...)
		shared.findings = append(shared.findings, layerFindings(root)...)
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

// knownHostsFindings (forts#139): the forge's SSH host key is an estate
// fact every clone a fort makes pins to, and the framework ships none.
// An estate that names none has no fort that evaluates (the throw in
// modules/estate.nix), and the gate says so where a reader is looking.
func knownHostsFindings(est estate.Estate) []finding {
	if est.Source.KnownHosts == "" {
		return []finding{{"error", estate.Path, "no known_hosts: the forge's SSH host key, one known_hosts line (ssh-keyscan -p <port> <forge>) — flux's clone and the rebuild loop pin it, and no fort evaluates until the estate names it (fort init --known-hosts, or this file)"}}
	}
	return []finding{{"ok", estate.Path, "known_hosts pins the forge's host key (" + strings.Fields(est.Source.KnownHosts)[0] + ")"}}
}

type checkOpts struct {
	root     string
	annotate bool
}

type finding struct {
	level string // "error" | "warning" | "ok"
	dir   string
	msg   string
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
