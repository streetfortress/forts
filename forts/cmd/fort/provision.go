package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
	hostseed "gitea.zen.lofi/sfi/deployments/forts/internal/seed"
)

// provision is the birth flow (#42): the single-use image is uploaded,
// consumed by `pulumi up`, then deleted on both sides. The image is
// deliberately NOT a pulumi resource — it is a birth input
// (forts/README.md has the full argument); pulumi owns only the durable
// surface (instance, firewall, DNS), and the apply is still the stock
// CLI with inherited stdio: a human reads the preview and confirms.
//
// Without --image this is just a `pulumi up -s <host>` wrapper — the
// stack's forts:imageId is already set from the birth run, and the
// instance ignores changes to it.
//
// A Linode host's ssh host key travels separately from the image (#606):
// every Linode apply reads the pair from nixos/secrets/<host>.yaml and
// hands the seed to the program in the environment (hostseed.Env), which sets
// it as metadata.user_data on the instance at creation, as a pulumi
// secret. The host's initrd reads it from the Metadata service before
// activation (nixos/modules/seed.nix). The read needs the admin age key,
// as the token lookups in cloudEnv already do.
func provision(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("provision needs the host first: fort provision <host> [--image <path>]")
	}
	host := args[0]
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	imagePath := fs.String("image", "", "bootstrap image from fort image (linode: .img.gz to upload; libvirt: the injected .qcow2 the volume is streamed from); deleted locally on success")
	yes := fs.Bool("yes", false, "confirm the pulumi apply without a prompt (an agent, a script, CI)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := needsTTYOrYes("provision", *yes); err != nil {
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
	if !entry.Realized() {
		return fmt.Errorf("host %q is [%s]: no backend raises it — `fort image %s`, then write the image to the machine by hand (docs/forts/baremetal.md)", host, entry.Provider, host)
	}
	if _, err := os.Stat(filepath.Join(root, "nixos/hosts", host, "default.nix")); err != nil {
		fmt.Fprintf(os.Stderr, "warning: nixos/hosts/%s/default.nix missing — `fort scaffold %s` first?\n", host, host)
	}

	pulumiDir := filepath.Join(root, "forts", "pulumi")
	ctx := context.Background()

	initNeeded, err := stackGate(pulumiDir, host, *imagePath != "")
	if err != nil {
		return err
	}
	if err := cloudEnv(root, entry.Provider); err != nil {
		return err
	}
	if initNeeded {
		// Birth: the stack is created here, not by hand — its config
		// file (Pulumi.<host>.yaml, the encryption salt) and the
		// checkpoint are what the state PR carries.
		if err := initStack(pulumiDir, host); err != nil {
			return err
		}
	}
	if entry.Cloud() {
		// The seed for the Metadata channel, read only when this apply
		// creates the instance (PR #654 review): a day-2 apply on a live
		// host would otherwise put the key in plain text in the
		// environment of pulumi and of every plugin it spawns, for
		// nothing — the program ignores metadatas on an existing
		// instance either way. The question is "does the stack hold an
		// instance", not "was --image given": a retry of a failed birth
		// runs without --image and must still create it with the seed.
		// A replacement never creates one here — the root disk's image
		// is gone after birth, so a replace is a repave through destroy
		// and provision, which reads the seed again — and an instance
		// deleted behind pulumi's back is the same story.
		need, err := stackNeedsSeed(pulumiDir, host)
		if err != nil {
			return err
		}
		if need {
			// A birth without a seed is a boot that stops in the initrd,
			// and Lish is a worse place to learn that than here.
			ud, err := hostSeedUserData(sopsCLI{root: root}, root, host)
			if err != nil {
				return fmt.Errorf("the host key seed for metadata.user_data: %w", err)
			}
			os.Setenv(hostseed.Env, ud)
		}
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

	if err := runPulumi(pulumiDir, append([]string{"up", "--stack", host}, confirmArgs(*yes)...)...); err != nil {
		if flow != nil {
			fmt.Fprintf(os.Stderr, "pulumi up failed — image %s left in place; rerun WITHOUT --image to retry (imageId is already set)\n", imageID)
		}
		return err
	}
	// The apply verbs stage the file backend: the checkpoint (and, at
	// birth, Pulumi.<host>.yaml) is the state PR.
	exec.Command("git", "-C", root, "add", "-A", "forts/pulumi").Run()

	if flow != nil {
		// `pulumi up` exits 0 when the preview is declined, so gate
		// deletion on the host actually existing in stack outputs.
		out, err := outputPulumi(pulumiDir, "stack", "output", "ip", "--stack", host)
		if err != nil || strings.TrimSpace(out) == "" {
			fmt.Fprintf(os.Stderr, "host not confirmed — image %s left in place; %s\n", imageID, flow.Leftover(imageID))
			return nil
		}
		// Single-use by design (#42). A libvirt artifact still carries the
		// host's key; a Linode one no longer does (#606), and the release
		// stays until #605's step 3 retires the lifecycle as a whole.
		if err := flow.Release(ctx, imageID, *imagePath); err != nil {
			return err
		}
		fmt.Printf("single-use image %s released (provider-side where applicable, and local)\n", imageID)
	}
	return nil
}

// confirmArgs is the --yes passthrough: pulumi's non-interactive guard
// is a default for humans at a TTY, and `--yes` on merge is how pulumi
// itself expects to be driven from CI. Under a TTY without --yes the
// stock prompt stands: you read the preview and type yes.
func confirmArgs(yes bool) []string {
	if yes {
		return []string{"--yes", "--non-interactive"}
	}
	return nil
}

// needsTTYOrYes fails an apply verb up front when nothing could confirm
// it: no TTY on stdin and no --yes. Before this the verb ran its safe
// part, then pulumi's guard refused and the message read as a failure
// (drill finding 12 — "pulumi up failed" when only the guard fired).
func needsTTYOrYes(verb string, yes bool) error {
	if yes {
		return nil
	}
	if fi, err := os.Stdin.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
		return nil
	}
	return fmt.Errorf("fort %s: no TTY to confirm the pulumi apply — pass --yes (an agent, a script, CI), or run it from a terminal", verb)
}

// hasStack reports whether the file backend holds a checkpoint for host —
// the same test adopt and destroy make.
func hasStack(pulumiDir, host string) bool {
	return exists(pulumiDir, filepath.Join("state/.pulumi/stacks/forts", host+".json"))
}

// stackGate decides, before anything touches the provider, whether
// provision creates the host's stack. A birth (--image) creates it when
// absent, so `pulumi stack init` is no longer a by-hand step (#250
// finding 15). A day-2 apply on a host with no stack refuses: that is a
// typo or a host not yet born, and an empty stack would put a host that
// never was on `preview --all`.
func stackGate(pulumiDir, host string, birth bool) (initNeeded bool, err error) {
	if hasStack(pulumiDir, host) {
		return false, nil
	}
	if !birth {
		return false, fmt.Errorf("host %q has no stack — a birth needs --image (fort image %s, then fort provision %s --image …)", host, host, host)
	}
	return true, nil
}

// linodeInstanceType is the resource type the Linode backend registers
// for the instance (forts/pulumi/linode.go) as it appears in state.
const linodeInstanceType = "linode:index/instance:Instance"

// stackNeedsSeed asks the stack, not the flags, whether this apply creates
// the Linode instance: no resource of the instance type in `pulumi stack
// export` means the apply creates one and must carry the seed. The CLI
// already holds the passphrase (cloudEnv), and the export is the same
// deployment pulumi itself reads — more robust than the checkpoint file's
// layout.
func stackNeedsSeed(pulumiDir, host string) (bool, error) {
	out, err := outputPulumi(pulumiDir, "stack", "export", "--stack", host)
	if err != nil {
		return false, fmt.Errorf("pulumi stack export %s (to decide whether this apply creates the instance): %s", host, lastLine(exitDetail(err)))
	}
	has, err := hasResourceType([]byte(out), linodeInstanceType)
	if err != nil {
		return false, fmt.Errorf("pulumi stack export %s: %w", host, err)
	}
	return !has, nil
}

// hasResourceType reads a `pulumi stack export` document for a resource
// of the given type. A freshly initialised stack exports a deployment
// with no resources at all.
func hasResourceType(export []byte, typ string) (bool, error) {
	var doc struct {
		Deployment struct {
			Resources []struct {
				Type string `json:"type"`
			} `json:"resources"`
		} `json:"deployment"`
	}
	if err := json.Unmarshal(export, &doc); err != nil {
		return false, fmt.Errorf("not a stack export: %w", err)
	}
	for _, r := range doc.Deployment.Resources {
		if r.Type == typ {
			return true, nil
		}
	}
	return false, nil
}

// initStack is the one `pulumi stack init` — passphrase secrets provider,
// the passphrase itself sops-managed (cloudEnv). Shared with adopt.
func initStack(pulumiDir, host string) error {
	return runPulumi(pulumiDir, "stack", "init", host, "--secrets-provider", "passphrase", "--non-interactive")
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
