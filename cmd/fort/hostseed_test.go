package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hostseed "gitea.zen.lofi/sfi/forts/internal/seed"
)

func TestHostSeedUserData(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "nixos/secrets"), 0o755)
	key, _ := newSSHKey("cloud-fort")
	path := filepath.Join(root, "nixos/secrets/cloud-fort.yaml")
	os.WriteFile(path, []byte("ciphertext"), 0o600)
	tool := &fakeTool{plain: map[string]string{
		path: "ssh-host-key: |\n" + yamlIndent(key.private) + "ssh-host-key.pub: " + strings.TrimSpace(key.public) + "\nk3s-token: x\n",
	}}

	ud, err := hostSeedUserData(tool, root, "cloud-fort")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(ud)
	if err != nil {
		t.Fatal(err)
	}
	priv, pub, err := hostseed.Parse(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	if priv != key.private {
		t.Errorf("private key differs from the sops value:\n%q\n%q", priv, key.private)
	}
	if strings.TrimSpace(pub) != strings.TrimSpace(key.public) {
		t.Errorf("public line differs: %q", pub)
	}
	// the seed is the pair as minted: the derived public key agrees
	if hk, err := sshKeyFrom(priv, "cloud-fort"); err != nil || strings.TrimSpace(hk.public) != strings.TrimSpace(pub) {
		t.Errorf("seed's public line is not the private key's: %v", err)
	}

	// no sops file: the message names mint
	if _, err := hostSeedUserData(tool, root, "unminted"); err == nil || !strings.Contains(err.Error(), "fort mint") {
		t.Errorf("want a mint hint, got %v", err)
	}
	// a file without the pair: the same hint
	os.WriteFile(filepath.Join(root, "nixos/secrets/partial.yaml"), []byte("x"), 0o600)
	tool.plain[filepath.Join(root, "nixos/secrets/partial.yaml")] = "k3s-token: x\n"
	if _, err := hostSeedUserData(tool, root, "partial"); err == nil || !strings.Contains(err.Error(), "fort mint") {
		t.Errorf("want a mint hint, got %v", err)
	}
}

func yamlIndent(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("  " + l + "\n")
	}
	return b.String()
}
