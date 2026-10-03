package main

import (
	"fmt"
	"path/filepath"

	hostseed "gitea.zen.lofi/sfi/deployments/forts/internal/seed"
	"gopkg.in/yaml.v3"
)

// hostKeyPair is the host's ssh host key out of its sops file — the pair
// `fort mint` wrote (ssh-host-key, ssh-host-key.pub). Two callers: the
// seed `fort provision` hands the pulumi program for the Linode Metadata
// channel (#606), and the graft `fort image` still performs for the
// variants whose channel has not landed (qcow, baremetal; #605's order).
func hostKeyPair(tool secretsTool, root, host string) (private, public string, err error) {
	secretsPath := filepath.Join("nixos/secrets", host+".yaml")
	if !exists(root, secretsPath) {
		return "", "", fmt.Errorf("no %s — run `fort mint %s --write` first", secretsPath, host)
	}
	raw, err := tool.Decrypt(filepath.Join(root, secretsPath))
	if err != nil {
		return "", "", err
	}
	var have map[string]string
	if err := yaml.Unmarshal(raw, &have); err != nil {
		return "", "", fmt.Errorf("%s: %w", secretsPath, err)
	}
	private, public = have[keySSHHost], have[keySSHHostPub]
	if private == "" || public == "" {
		return "", "", fmt.Errorf("%s lacks %s / %s — run `fort mint %s --write` first", secretsPath, keySSHHost, keySSHHostPub, host)
	}
	return private, public, nil
}

// hostSeedUserData is the value the Linode instance's metadata.user_data
// takes for this host: the seed (internal/seed), base64 as the Metadata
// service stores it. `fort provision` sets it in the environment of
// every Linode apply (seed.Env) — a birth creates the instance with it,
// and a retry of a failed birth (without --image) must create the
// instance with it too, which is why it is read on every run and not
// only with --image. The program ignores it on a running host.
func hostSeedUserData(tool secretsTool, root, host string) (string, error) {
	private, public, err := hostKeyPair(tool, root, host)
	if err != nil {
		return "", err
	}
	return hostseed.UserData(private, public)
}
