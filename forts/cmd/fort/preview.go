package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/inventory"
)

// preview is the read-only half of provision: the stock `pulumi preview`
// with inherited stdio, credentials sourced the same way (env, else
// provisioning/secrets.yaml). Day-2 edits (firewall, DNS) get read here
// before `fort provision` applies them; `--refresh` is the drift check
// (program vs provider, not program vs state — see `just iac-drift`).
func preview(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("preview needs the host first: fort preview <host> [--refresh] [--expect-no-changes]")
	}
	host := args[0]
	fs := flag.NewFlagSet("preview", flag.ContinueOnError)
	refresh := fs.Bool("refresh", false, "refresh state from the provider before diffing (drift)")
	expectNone := fs.Bool("expect-no-changes", false, "exit nonzero if the preview proposes any change")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	root, err := repoRoot()
	if err != nil {
		return err
	}
	entry, err := inventory.LoadHost(filepath.Join(root, "provisioning/inventory.yaml"), host)
	if err != nil {
		return err
	}
	if entry.Provider == "" {
		return fmt.Errorf("host %q has no provider — lab VMs stay on `just create-nixos`", host)
	}
	if err := cloudEnv(root, entry); err != nil {
		return err
	}

	pulumiArgs := []string{"preview", "--stack", host, "--diff"}
	if *refresh {
		pulumiArgs = append(pulumiArgs, "--refresh")
	}
	if *expectNone {
		pulumiArgs = append(pulumiArgs, "--expect-no-changes")
	}
	if os.Getenv("CI") != "" {
		pulumiArgs = append(pulumiArgs, "--non-interactive")
	}
	return runPulumi(filepath.Join(root, "forts", "pulumi"), pulumiArgs...)
}
