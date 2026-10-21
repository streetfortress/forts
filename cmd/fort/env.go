package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/tools"
)

// cloudEnv puts the credentials a pulumi run needs into the environment
// for one provider (the manifest's, or — for destroy — the one the stack's
// checkpoint records: the manifest is gone by then)
// of this process — and so of the pulumi (and its provider) we exec.
// Explicit env wins; otherwise each value is sops-decrypted from
// provisioning/secrets.yaml, the same home as the forge's provisioning
// token: repo-driven, no per-machine env setup, and the values are
// never printed.
// libvirtPoolName mirrors pulumi/libvirt.go (one lab, one pool).
const libvirtPoolName = "images"

func cloudEnv(root string, provider string) error {
	// The nixpkgs pulumi-bin bundle puts its own provider binaries on
	// PATH, and pulumi PREFERS ambient plugins — we ran a v6 provider
	// against v4-generated SDK code, which mis-shapes outputs (specs as
	// list-of-one) and invoke results (Disk=0). Always use the plugin
	// version pinned by go.mod.
	os.Setenv("PULUMI_IGNORE_AMBIENT_PLUGINS", "true")

	// The program pulumi runs is fort-pulumi, built beside this binary
	// (provisioning/pulumi/Pulumi.yaml names it, `binary: fort-pulumi`),
	// so the pair always match: put this binary's directory first on PATH
	// for the pulumi child.
	if exe, err := os.Executable(); err == nil {
		os.Setenv("PATH", filepath.Dir(exe)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}

	// The state backend is THIS checkout's committed file:// dir, always.
	// `pulumi login` records a global "current backend" in
	// ~/.pulumi/credentials.json; a login run from a worktree leaves every
	// later run — from any checkout — writing state into that worktree
	// (2026-09-13: a `just provision` from the primary checkout landed its
	// state in ~/worktrees/…/fort-wildcard-dns). PULUMI_BACKEND_URL takes
	// precedence over the recorded login, so the one-time `pulumi login`
	// in forts/README.md becomes unnecessary for fort commands.
	os.Setenv("PULUMI_BACKEND_URL", "file://"+filepath.Join(root, "provisioning", "pulumi", "state"))

	// The lab hypervisor, reached over ssh from wherever fort runs. The
	// provider carries its own ssh client (no ~/.ssh/config, no agent
	// unless asked): the key file goes in the URI. The operator names it
	// (forts#139); the manifest's [libvirt] table is where it moves to
	// (forts#27, a `uri` key).
	if provider == "libvirt" {
		if os.Getenv("LIBVIRT_DEFAULT_URI") == "" {
			return fmt.Errorf("LIBVIRT_DEFAULT_URI is not set — the hypervisor a [libvirt] fort lives on, as the provider dials it: qemu+ssh://<user>@<host>/system?sshauth=privkey&keyfile=<path to the key> (forts#27 moves it into the manifest)")
		}
		// The provider applies the domain's XSLT (the qcow2 driver fix,
		// pulumi/libvirt.go) by exec'ing xsltproc; the plugin inherits
		// our PATH, so the tool has to be resolved before pulumi starts.
		if err := tools.Ensure("xsltproc"); err != nil {
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
	cmd, err := tools.Command("sops", "--config", filepath.Join(root, ".sops.yaml"),
		"-d", "--extract", `["`+key+`"]`,
		filepath.Join(root, "provisioning/secrets.yaml"))
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
