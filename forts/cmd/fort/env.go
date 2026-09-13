package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/inventory"
)

// cloudEnv puts the credentials a pulumi run needs into the environment
// of this process — and so of the pulumi (and its provider) we exec.
// Explicit env wins; otherwise each value is sops-decrypted from
// provisioning/secrets.yaml, the same home as the gitea provisioning
// token: repo-driven, no per-machine env setup, and the values are
// never printed.
func cloudEnv(root string, entry inventory.Host) error {
	// The nixpkgs pulumi-bin bundle puts its own provider binaries on
	// PATH, and pulumi PREFERS ambient plugins — we ran a v6 provider
	// against v4-generated SDK code, which mis-shapes outputs (specs as
	// list-of-one) and invoke results (Disk=0). Always use the plugin
	// version pinned by go.mod.
	os.Setenv("PULUMI_IGNORE_AMBIENT_PLUGINS", "true")

	if entry.Provider == "linode" && os.Getenv("LINODE_TOKEN") == "" {
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
