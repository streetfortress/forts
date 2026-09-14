package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/inventory"
)

// preview is the read-only half of provision: the stock `pulumi preview`
// with inherited stdio, credentials sourced the same way (env, else
// provisioning/secrets.yaml). Day-2 edits (firewall, DNS) get read here
// before `fort provision` applies them; `--refresh` is the drift check
// (program vs provider, not program vs state). `--all` walks every cloud
// host in inventory — with --refresh --expect-no-changes that is the
// fleet drift check (`just iac-drift`), one summary line per host and a
// nonzero exit if any stack proposed a change or failed to preview.
func preview(args []string) error {
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	all := fs.Bool("all", false, "every cloud host in inventory")
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
	hosts, err := inventory.Load(filepath.Join(root, "provisioning/inventory.yaml"))
	if err != nil {
		return err
	}
	var targets []string
	if *all {
		for name, h := range hosts {
			if h.Provider != "" {
				targets = append(targets, name)
			}
		}
		sort.Strings(targets)
		if len(targets) == 0 {
			return fmt.Errorf("no cloud hosts in inventory (no provider: set)")
		}
	} else {
		entry, ok := hosts[host]
		if !ok {
			return fmt.Errorf("host %q not in provisioning/inventory.yaml", host)
		}
		if entry.Provider == "" {
			return fmt.Errorf("host %q has no provider — set `provider: libvirt` (lab) or a cloud provider; nothing raises a provider-less host any more", host)
		}
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
		if err := cloudEnv(root, hosts[name].Provider); err != nil {
			return err
		}
		err := runPulumi(filepath.Join(root, "forts", "pulumi"), append(append([]string{}, pulumiArgs...), "--stack", name)...)
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
		// State without inventory means destroy pending (#217): the
		// dismantle PR merged, `fort destroy` has not run. Say so rather
		// than silently skipping a stack inventory no longer names.
		for _, name := range stacksInState(filepath.Join(root, "forts", "pulumi")) {
			if _, ok := hosts[name]; !ok {
				failed++
				results = append(results, fmt.Sprintf("%-6s %s   destroy pending: state without inventory — fort destroy %s", "PENDING", name, name))
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
