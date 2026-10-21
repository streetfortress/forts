package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

// preview is the read-only half of provision: the stock `pulumi preview`
// with inherited stdio, credentials sourced the same way (env, else
// provisioning/secrets.yaml). Day-2 edits (firewall, DNS) get read here
// before `fort provision` applies them; `--refresh` is the drift check
// (program vs provider, not program vs state). `--all` walks every cloud
// host with a manifest — with --refresh --expect-no-changes that is the
// fleet drift check (`just iac-drift`), one summary line per host and a
// nonzero exit if any stack proposed a change or failed to preview.
func preview(args []string) error {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	all := fs.Bool("all", false, "every host with a manifest")
	refresh := fs.Bool("refresh", false, "refresh state from the provider before diffing (drift)")
	expectNone := fs.Bool("expect-no-changes", false, "exit nonzero if the preview proposes any change")
	var host string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		host, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if (host == "") == !*all {
		return fmt.Errorf("usage: fort preview (<host> | --all) [--refresh] [--expect-no-changes]")
	}

	root, err := repoRoot()
	if err != nil {
		return err
	}
	var targets, skipped []string
	hosts := map[string]manifest.Host{}
	if *all {
		loaded, problems, err := manifest.LoadAll(root)
		if err != nil {
			return err
		}
		if len(problems) > 0 {
			var bad []string
			for name := range problems {
				bad = append(bad, name)
			}
			sort.Strings(bad)
			return fmt.Errorf("bad manifest(s): %s — fort check --all says why", strings.Join(bad, ", "))
		}
		hosts = loaded
		for name, h := range hosts {
			if !h.Realized() {
				// nothing in provisioning/pulumi to diff against: a [baremetal]
				// host has no stack by design (docs/baremetal.md).
				// Listed in the summary so the walk is visibly complete.
				skipped = append(skipped, name)
				continue
			}
			targets = append(targets, name)
		}
		sort.Strings(targets)
		if len(targets) == 0 {
			return fmt.Errorf("no manifests in %s", manifest.Dir)
		}
	} else {
		entry, err := manifest.Load(root, host)
		if err != nil {
			return err
		}
		if !entry.Realized() {
			return fmt.Errorf("host %q is [%s]: no backend, no stack, nothing to preview (docs/baremetal.md)", host, entry.Provider)
		}
		hosts[host] = entry
		targets = []string{host}
	}

	pulumiArgs := []string{"preview", "--diff"}
	if *refresh {
		pulumiArgs = append(pulumiArgs, "--refresh")
	}
	if *expectNone {
		pulumiArgs = append(pulumiArgs, "--expect-no-changes")
	}
	if os.Getenv("CI") != "" {
		pulumiArgs = append(pulumiArgs, "--non-interactive")
	}

	failed := 0
	results := make([]string, 0, len(targets))
	for _, name := range targets {
		if err := cloudEnv(root, hosts[name].Provider, hosts[name].LibvirtURI()); err != nil {
			return err
		}
		err := runPulumi(filepath.Join(root, "provisioning", "pulumi"), append(append([]string{}, pulumiArgs...), "--stack", name)...)
		if !*all {
			return err
		}
		verdict := "OK"
		if err != nil {
			failed++
			verdict = "FAILED"
			if *expectNone {
				verdict = "DRIFT"
			}
		}
		results = append(results, fmt.Sprintf("%-6s %s", verdict, name))
	}
	if *all {
		sort.Strings(skipped)
		for _, name := range skipped {
			results = append(results, fmt.Sprintf("%-6s %s   [%s]: no stack by design", "SKIP", name, hosts[name].Provider))
		}
		// State without a manifest means destroy pending (#217): the
		// dismantle PR merged, `fort destroy` has not run. Say so rather
		// than silently skipping a stack no manifest names.
		for _, name := range stacksInState(filepath.Join(root, "provisioning", "pulumi")) {
			if _, ok := hosts[name]; !ok {
				failed++
				results = append(results, fmt.Sprintf("%-6s %s   destroy pending: state without a manifest — fort destroy %s", "PENDING", name, name))
			}
		}
	}
	fmt.Println(strings.Join(results, "\n"))
	if failed > 0 {
		return fmt.Errorf("%d stack(s) not clean", failed)
	}
	return nil
}

// stacksInState lists the stacks the committed file backend holds
// (state/.pulumi/stacks/<project>/<stack>.json; .bak and .attrs are
// the backend's own residue, not stacks).
func stacksInState(pulumiDir string) []string {
	matches, _ := filepath.Glob(filepath.Join(pulumiDir, "state/.pulumi/stacks/*/*.json"))
	var out []string
	for _, m := range matches {
		out = append(out, strings.TrimSuffix(filepath.Base(m), ".json"))
	}
	sort.Strings(out)
	return out
}
