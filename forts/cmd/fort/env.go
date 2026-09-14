package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// cloudEnv puts the credentials a pulumi run needs into the environment
// for one provider (inventory's, or — for destroy — the one the stack's
// checkpoint records: the host is gone from inventory by then)
// of this process — and so of the pulumi (and its provider) we exec.
// Explicit env wins; otherwise each value is sops-decrypted from
// provisioning/secrets.yaml, the same home as the gitea provisioning
// token: repo-driven, no per-machine env setup, and the values are
// never printed.
// libvirtPoolName mirrors pulumi/libvirt.go (one lab, one pool).
const libvirtPoolName = "images"

const libvirtURI = "qemu+ssh://steven@soundship/system?sshauth=privkey&keyfile=%s"

func cloudEnv(root string, provider string) error {
	// The nixpkgs pulumi-bin bundle puts its own provider binaries on
	// PATH, and pulumi PREFERS ambient plugins — we ran a v6 provider
	// against v4-generated SDK code, which mis-shapes outputs (specs as
	// list-of-one) and invoke results (Disk=0). Always use the plugin
	// version pinned by go.mod.
	os.Setenv("PULUMI_IGNORE_AMBIENT_PLUGINS", "true")

	// The state backend is THIS checkout's committed file:// dir, always.
	// `pulumi login` records a global "current backend" in
	// ~/.pulumi/credentials.json; a login run from a worktree leaves every
	// later run — from any checkout — writing state into that worktree
	// (2026-09-13: a `just provision` from the primary checkout landed its
	// state in ~/worktrees/…/fort-wildcard-dns). PULUMI_BACKEND_URL takes
	// precedence over the recorded login, so the one-time `pulumi login`
	// in forts/README.md becomes unnecessary for fort commands.
	os.Setenv("PULUMI_BACKEND_URL", "file://"+filepath.Join(root, "forts", "pulumi", "state"))

	// The lab hypervisor, reached over ssh from wherever fort runs. The
	// provider carries its own ssh client (no ~/.ssh/config, no agent
	// unless asked): the key file goes in the URI. One lab today; the
	// per-fort manifest's [libvirt] table (#252) is where a second
	// hypervisor would name itself.
	if provider == "libvirt" {
		if os.Getenv("LIBVIRT_DEFAULT_URI") == "" {
			home, _ := os.UserHomeDir()
			os.Setenv("LIBVIRT_DEFAULT_URI", fmt.Sprintf(libvirtURI, filepath.Join(home, ".ssh", "id_ed25519")))
		}
		// The provider applies the domain's XSLT (the qcow2 driver fix,
		// pulumi/libvirt.go) by exec'ing xsltproc; the plugin inherits
		// our PATH, so put nixpkgs' copy on it when the host has none —
		// the guestfish / qemu-img fallback, one layer down.
		if err := ensureTool("xsltproc", "nixpkgs#libxslt.bin"); err != nil {
			return err
		}
	}
	if provider == "linode" && os.Getenv("LINODE_TOKEN") == "" {
		v, err := secret(root, "linode-token")
		if err != nil {
			return fmt.Errorf("LINODE_TOKEN not set and no linode-token in provisioning/secrets.yaml — add it with: sops --config .sops.yaml set provisioning/secrets.yaml '[\"linode-token\"]' '\"<token>\"'")
		}
		os.Setenv("LINODE_TOKEN", v)
	}
	// Pulumi.yaml: passphrase secrets provider, passphrase itself
	// sops-managed.
	if os.Getenv("PULUMI_CONFIG_PASSPHRASE") == "" {
		v, err := secret(root, "pulumi-passphrase")
		if err != nil {
			return fmt.Errorf("PULUMI_CONFIG_PASSPHRASE not set and no pulumi-passphrase in provisioning/secrets.yaml")
		}
		os.Setenv("PULUMI_CONFIG_PASSPHRASE", v)
	}
	return nil
}

func secret(root, key string) (string, error) {
	out, err := exec.Command("sops", "--config", filepath.Join(root, ".sops.yaml"),
		"-d", "--extract", `["`+key+`"]`,
		filepath.Join(root, "provisioning/secrets.yaml")).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// ensureTool prepends a nix-built package's bin dir to PATH when the
// named tool is not already there. `nix build --no-link` is a no-op
// once the store has it.
func ensureTool(tool, flakeRef string) error {
	if _, err := exec.LookPath(tool); err == nil {
		return nil
	}
	out, err := exec.Command("nix", "build", "--no-link", "--print-out-paths", flakeRef).Output()
	if err != nil {
		return fmt.Errorf("%s not on PATH and `nix build %s` failed: %w", tool, flakeRef, err)
	}
	bin := filepath.Join(strings.TrimSpace(string(out)), "bin")
	if _, err := os.Stat(filepath.Join(bin, tool)); err != nil {
		return fmt.Errorf("%s: not in %s", tool, bin)
	}
	return os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}
