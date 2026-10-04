package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
	hostseed "gitea.zen.lofi/sfi/forts/internal/seed"
	"gitea.zen.lofi/sfi/forts/internal/tools"
)

// provision is the birth flow (#42): the bootstrap image is uploaded,
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
// The image holds no secret (#605, #659): it is the plain build output
// of `fort image`. The host's ssh host key travels through the
// platform's channel instead (#606, #657): the apply that creates the
// host reads the pair from nixos/secrets/<host>.yaml and hands the seed
// to the program in the environment (hostseed.Env). The Linode program
// sets it as metadata.user_data on the instance; the libvirt program
// writes it to a NoCloud cidata volume attached to the domain. Both as a
// pulumi secret, both read by the host's initrd before activation
// (nixos/modules/seed.nix). The read needs the admin age key, as the
// token lookups in cloudEnv already do.
//
// The delete after the apply is hygiene, not custody: Linode bills a
// custom image for as long as it exists, and the local copy is a few
// GiB in the cache dir. A copy of the image that outlives the delete
// gives an attacker nothing current.
func provision(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("provision needs the host first: fort provision <host> [--image <path>]")
	}
	host := args[0]
	fs := flag.NewFlagSet("provision", flag.ContinueOnError)
	imagePath := fs.String("image", "", "bootstrap image from fort image (linode: .img.gz to upload; libvirt: the .qcow2 the volume is streamed from); deleted locally on success")
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
		return fmt.Errorf("host %q is [%s]: no backend raises it — `fort image %s`, then write the image to the machine by hand (docs/baremetal.md)", host, entry.Provider, host)
	}
	if _, err := os.Stat(filepath.Join(root, "nixos/hosts", host, "default.nix")); err != nil {
		fmt.Fprintf(os.Stderr, "warning: nixos/hosts/%s/default.nix missing — `fort scaffold %s` first?\n", host, host)
	}

	pulumiDir := filepath.Join(root, "provisioning", "pulumi")
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
	// The seed for the platform's channel, read only when this apply
	// creates the host (PR #654 review): a day-2 apply on a live host
	// would otherwise put the key in plain text in the environment of
	// pulumi and of every plugin it spawns, for nothing — the programs
	// ignore the seed on an existing host either way. The question is
	// "does the stack hold the host", not "was --image given": a retry of
	// a failed birth runs without --image and must still create it with
	// the seed. A replacement never creates one here — the root disk's
	// image is gone after birth, so a replace is a repave through destroy
	// and provision, which reads the seed again — and a host deleted
	// behind pulumi's back is the same story.
	need, err := stackNeedsSeed(pulumiDir, host, entry.Provider)
	if err != nil {
		return err
	}
	if need {
		// A birth without a seed is a boot that stops in the initrd, and
		// the console is a worse place to learn that than here.
		ud, err := hostSeedUserData(sopsCLI{root: root}, root, host)
		if err != nil {
			return fmt.Errorf("the host key seed for the %s channel: %w", seedChannels[entry.Provider], err)
		}
		os.Setenv(hostseed.Env, ud)
		if entry.Provider == "libvirt" {
			// The provider writes the cidata ISO by exec'ing mkisofs on
			// this machine (terraform-provider-libvirt 0.8.1); the plugin
			// inherits our PATH, as for xsltproc.
			if err := tools.Ensure("mkisofs"); err != nil {
				return err
			}
		}
		// Committed provenance: which channel the host was born through
		// (#657). The libvirt program declares the seed volume only when
		// this says so, so a host born before the channel — adopted, or
		// born from an image that carried the key — never sees an empty
		// ISO proposed on a day-2 apply.
		if err := runPulumi(pulumiDir, "config", "set", "seed", seedChannels[entry.Provider], "--stack", host); err != nil {
			return fmt.Errorf("recording the seed channel: %w", err)
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
			return fmt.Errorf("recording imageId (image %s stays staged): %w", imageID, err)
		}
	}

	if err := runPulumi(pulumiDir, append([]string{"up", "--stack", host}, confirmArgs(*yes)...)...); err != nil {
		if flow != nil {
			fmt.Fprintf(os.Stderr, "pulumi up failed — image %s stays staged; rerun WITHOUT --image to retry (imageId is already set)\n", imageID)
		}
		return err
	}
	// The apply verbs stage the file backend: the checkpoint (and, at
	// birth, Pulumi.<host>.yaml) is the state PR.
	stage(root)

	if flow != nil {
		// `pulumi up` exits 0 when the preview is declined, so gate
		// deletion on the host actually existing in stack outputs.
		out, err := outputPulumi(pulumiDir, "stack", "output", "ip", "--stack", host)
		if err != nil || strings.TrimSpace(out) == "" {
			fmt.Fprintf(os.Stderr, "host not confirmed — image %s stays staged; %s\n", imageID, flow.Leftover(imageID))
			return nil
		}
		// Hygiene, not custody (#659): the image holds no secret. Linode
		// bills a custom image while it exists, and the local copy is a
		// few GiB; the host does not need either once it has booted.
		if err := flow.Release(ctx, imageID, *imagePath); err != nil {
			return err
		}
		fmt.Printf("image %s released (provider-side where applicable, and local) — the host no longer needs it\n", imageID)
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

// seedHostTypes is, per provider, the resource type of the host itself as
// it appears in state — the one whose creation consumes the seed: the
// Linode instance (cmd/fort-pulumi/linode.go), the libvirt domain
// (cmd/fort-pulumi/libvirt.go).
var seedHostTypes = map[string]string{
	"linode":  "linode:index/instance:Instance",
	"libvirt": "libvirt:index/domain:Domain",
}

// seedChannels is, per provider, the channel the seed travels through
// (nixos/modules/seed.nix's names) — recorded on the stack as
// `forts:seed` at birth.
var seedChannels = map[string]string{
	"linode":  "linode-metadata",
	"libvirt": "nocloud",
}

// stackNeedsSeed asks the stack, not the flags, whether this apply creates
// the host: no resource of the provider's host type in `pulumi stack
// export` means the apply creates one and must carry the seed. The CLI
// already holds the passphrase (cloudEnv), and the export is the same
// deployment pulumi itself reads — more robust than the checkpoint file's
// layout.
func stackNeedsSeed(pulumiDir, host, provider string) (bool, error) {
	typ, ok := seedHostTypes[provider]
	if !ok {
		return false, fmt.Errorf("provider %q: no seed channel", provider)
	}
	out, err := outputPulumi(pulumiDir, "stack", "export", "--stack", host)
	if err != nil {
		return false, fmt.Errorf("pulumi stack export %s (to decide whether this apply creates the host): %s", host, lastLine(exitDetail(err)))
	}
	has, err := hasResourceType([]byte(out), typ)
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

// runPulumi execs the stock CLI with inherited stdio in provisioning/pulumi —
// previews render and prompts work exactly as if run by hand, and
// `pulumi <anything> -s <host>` by hand stays a first-class escape hatch.
func runPulumi(dir string, args ...string) error {
	cmd, err := tools.Command("pulumi", args...)
	if err != nil {
		return err
	}
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func outputPulumi(dir string, args ...string) (string, error) {
	cmd, err := tools.Command("pulumi", args...)
	if err != nil {
		return "", err
	}
	cmd.Dir = dir
	out, err := cmd.Output()
	return string(out), err
}

// stage puts the file backend in the index: the checkpoint (and, at
// birth, Pulumi.<host>.yaml) is the state PR. Best effort, as it was
// before the tool set — the apply already happened, and the commit is
// the operator's either way.
func stage(root string) {
	if git, err := tools.Command("git", "-C", root, "add", "-A", "provisioning/pulumi"); err == nil {
		git.Run()
	}
}
