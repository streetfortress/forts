package main

import (
	"fmt"
	"path/filepath"

	hostseed "gitea.zen.lofi/sfi/forts/internal/seed"
	"gopkg.in/yaml.v3"
)

// hostKeyPair is the host's ssh host key out of its sops file — the pair
// `fort mint` wrote (ssh-host-key, ssh-host-key.pub). One caller: the
// seed `fort provision` hands the pulumi program for the platform's
// channel (Linode Metadata, #606; the NoCloud volume of a lab fort,
// #657). The baremetal channel (#658) will be the second. `fort image`
// reads no key since #659.
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

// hostSeedUserData is the seed (internal/seed) for this host, base64 as
// the Linode Metadata service stores it in metadata.user_data; the
// libvirt program decodes the same value for the cidata volume. `fort
// provision` sets it in the environment (seed.Env) of the apply that
// creates the host — a birth, or a retry of a failed birth (without
// --image), which is why the question is the stack's and not the flag's.
// The programs ignore it on a running host.
func hostSeedUserData(tool secretsTool, root, host string) (string, error) {
	private, public, err := hostKeyPair(tool, root, host)
	if err != nil {
		return "", err
	}
	return hostseed.UserData(private, public)
}
