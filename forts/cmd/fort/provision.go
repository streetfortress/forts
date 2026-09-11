package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/inventory"
	"github.com/linode/linodego"
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
	imagePath := fs.String("image", "", "injected bootstrap image (.img.gz): upload, provision, then delete on success")
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
	if _, err := os.Stat(filepath.Join(root, "nixos/hosts", host, "default.nix")); err != nil {
		fmt.Fprintf(os.Stderr, "warning: nixos/hosts/%s/default.nix missing — `fort scaffold %s` first?\n", host, host)
	}

	pulumiDir := filepath.Join(root, "forts", "pulumi")
	ctx := context.Background()

	// The nixpkgs pulumi-bin bundle puts its own provider binaries on
	// PATH, and pulumi PREFERS ambient plugins — we ran a v6 provider
	// against v4-generated SDK code, which mis-shapes outputs (specs as
	// list-of-one) and invoke results (Disk=0). Always use the plugin
	// version pinned by go.mod.
	os.Setenv("PULUMI_IGNORE_AMBIENT_PLUGINS", "true")

	// LINODE_TOKEN from the env if set, else sops-decrypted from
	// provisioning/secrets.yaml (same home as the gitea provisioning
	// token — repo-driven, no per-machine env setup). Setenv so the
	// exec'd pulumi (its linode provider reads the same variable) and
	// linodego both inherit it; the value itself is never printed.
	if entry.Provider == "linode" && os.Getenv("LINODE_TOKEN") == "" {
		out, err := exec.Command("sops", "--config", filepath.Join(root, ".sops.yaml"),
			"-d", "--extract", `["linode-token"]`,
			filepath.Join(root, "provisioning/secrets.yaml")).Output()
		if err != nil {
			return fmt.Errorf("LINODE_TOKEN not set and no linode-token in provisioning/secrets.yaml — add it with: sops --config .sops.yaml set provisioning/secrets.yaml '[\"linode-token\"]' '\"<token>\"'")
		}
		os.Setenv("LINODE_TOKEN", strings.TrimSpace(string(out)))
	}
	// Same fallback for the stack passphrase (Pulumi.yaml: passphrase
	// secrets provider, passphrase itself sops-managed).
	if os.Getenv("PULUMI_CONFIG_PASSPHRASE") == "" {
		out, err := exec.Command("sops", "--config", filepath.Join(root, ".sops.yaml"),
			"-d", "--extract", `["pulumi-passphrase"]`,
			filepath.Join(root, "provisioning/secrets.yaml")).Output()
		if err != nil {
			return fmt.Errorf("PULUMI_CONFIG_PASSPHRASE not set and no pulumi-passphrase in provisioning/secrets.yaml")
		}
		os.Setenv("PULUMI_CONFIG_PASSPHRASE", strings.TrimSpace(string(out)))
	}

	var client linodego.Client
	var uploaded *linodego.Image
	if *imagePath != "" {
		if entry.Provider != "linode" {
			return fmt.Errorf("--image upload is only implemented for linode (host %q is %q)", host, entry.Provider)
		}
		c, err := linodego.NewClientFromEnv(nil) // LINODE_TOKEN
		if err != nil {
			return err
		}
		client = *c

		f, err := os.Open(*imagePath)
		if err != nil {
			return err
		}
		fmt.Printf("uploading %s → %s (%s)…\n", *imagePath, host+"-bootstrap", entry.Region)
		img, err := client.UploadImage(ctx, linodego.ImageUploadOptions{
			Region: entry.Region,
			Label:  host + "-bootstrap",
			Image:  f,
		})
		f.Close()
		if err != nil {
			return fmt.Errorf("image upload: %w", err)
		}
		uploaded = img
		// The instance create will fail while the image is still
		// processing; wait for it here rather than mid-preview.
		if _, err := client.WaitForImageStatus(ctx, img.ID, linodego.ImageStatusAvailable, 600); err != nil {
			return fmt.Errorf("image %s never became available (left in place): %w", img.ID, err)
		}
		// Committed provenance: the stack config records which image the
		// host was born from; the instance ignores later changes to it.
		if err := runPulumi(pulumiDir, "config", "set", "imageId", img.ID, "--stack", host); err != nil {
			return fmt.Errorf("recording imageId (image %s left in place): %w", img.ID, err)
		}
	}

	if err := runPulumi(pulumiDir, "up", "--stack", host); err != nil {
		if uploaded != nil {
			fmt.Fprintf(os.Stderr, "pulumi up failed — image %s left in place; rerun WITHOUT --image to retry (imageId is already set)\n", uploaded.ID)
		}
		return err
	}

	if uploaded != nil {
		// `pulumi up` exits 0 when the preview is declined, so gate
		// deletion on the instance actually existing in stack outputs.
		out, err := outputPulumi(pulumiDir, "stack", "output", "ip", "--stack", host)
		if err != nil || strings.TrimSpace(out) == "" {
			fmt.Fprintf(os.Stderr, "instance not confirmed — image %s left in place; rerun without --image, or delete it: linode-cli images delete %s\n", uploaded.ID, uploaded.ID)
			return nil
		}
		// Single-use by design: the artifact carries the host's key.
		// Provider-side first, then the local injected copy.
		if err := client.DeleteImage(ctx, uploaded.ID); err != nil {
			return fmt.Errorf("deleting provider-side image %s (delete it manually!): %w", uploaded.ID, err)
		}
		if err := os.Remove(*imagePath); err != nil {
			return fmt.Errorf("deleting local image %s (delete it manually!): %w", *imagePath, err)
		}
		fmt.Printf("single-use image %s deleted (provider-side and local)\n", uploaded.ID)
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
