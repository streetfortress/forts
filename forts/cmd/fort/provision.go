package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

// provision is the birth flow (#42): the secret-bearing single-use image
// is uploaded, consumed by `pulumi up`, then deleted on both sides. The
// image is deliberately NOT a pulumi resource — it is a birth input
// (forts/README.md has the full argument); pulumi owns only the durable
// surface (instance, firewall, DNS), and the apply is still the stock
// CLI with inherited stdio: a human reads the preview and confirms.
//
// Without --image this is just a `pulumi up -s <host>` wrapper — the
// stack's forts:imageId is already set from the birth run, and the
// instance ignores changes to it.
func provision(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("provision needs the host first: fort provision <host> [--image <path>]")
	}
	host := args[0]
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	imagePath := fs.String("image", "", "injected bootstrap image (linode: .img.gz to upload; libvirt: the .qcow2 the volume is streamed from); deleted locally on success")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	root, err := repoRoot()
	if err != nil {
		return err
	}
	entry, err := manifest.Load(root, host)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, "nixos/hosts", host, "default.nix")); err != nil {
		fmt.Fprintf(os.Stderr, "warning: nixos/hosts/%s/default.nix missing — `fort scaffold %s` first?\n", host, host)
	}

	pulumiDir := filepath.Join(root, "forts", "pulumi")
	ctx := context.Background()

	if err := cloudEnv(root, entry.Provider); err != nil {
		return err
	}

	var flow imageFlow
	var imageID string
	if *imagePath != "" {
		f, ok := imageFlows[entry.Provider]
		if !ok {
			return fmt.Errorf("--image: no image flow for provider %q", entry.Provider)
		}
		flow = f
		id, err := flow.Stage(ctx, entry, host, *imagePath)
		if err != nil {
			return err
		}
		imageID = id
		// Committed provenance: the stack config records which image the
		// host was born from; the program reads it at creation only.
		if err := runPulumi(pulumiDir, "config", "set", "imageId", imageID, "--stack", host); err != nil {
			return fmt.Errorf("recording imageId (image %s left in place): %w", imageID, err)
		}
	}

	if err := runPulumi(pulumiDir, "up", "--stack", host); err != nil {
		if flow != nil {
			fmt.Fprintf(os.Stderr, "pulumi up failed — image %s left in place; rerun WITHOUT --image to retry (imageId is already set)\n", imageID)
		}
		return err
	}

	if flow != nil {
		// `pulumi up` exits 0 when the preview is declined, so gate
		// deletion on the host actually existing in stack outputs.
		out, err := outputPulumi(pulumiDir, "stack", "output", "ip", "--stack", host)
		if err != nil || strings.TrimSpace(out) == "" {
			fmt.Fprintf(os.Stderr, "host not confirmed — image %s left in place; %s\n", imageID, flow.Leftover(imageID))
			return nil
		}
		// Single-use by design: the artifact carries the host's key.
		if err := flow.Release(ctx, imageID, *imagePath); err != nil {
			return err
		}
		fmt.Printf("single-use image %s released (provider-side where applicable, and local)\n", imageID)
	}
	return nil
}

// runPulumi execs the stock CLI with inherited stdio in forts/pulumi —
// previews render and prompts work exactly as if run by hand, and
// `pulumi <anything> -s <host>` by hand stays a first-class escape hatch.
func runPulumi(dir string, args ...string) error {
	cmd := exec.Command("pulumi", args...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func outputPulumi(dir string, args ...string) (string, error) {
	cmd := exec.Command("pulumi", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}
