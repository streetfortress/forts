package main

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// secretsTool is the seeder's only path to secret material: decrypt
// into memory, encrypt a plaintext file in place, set one key in an
// encrypted file. The real one execs sops (and git add); tests swap in
// a fake, so no test ever touches an age key or prints a value.
type secretsTool interface {
	Decrypt(path string) ([]byte, error)
	EncryptInPlace(path string) error
	Set(path, key, value string) error
	GitAdd(path string) error
}

type sopsCLI struct{ root string }

func (s sopsCLI) Decrypt(path string) ([]byte, error) {
	out, err := exec.Command("sops", "-d", path).Output()
	if err != nil {
		return nil, fmt.Errorf("sops -d %s: %s", path, lastLine(exitDetail(err)))
	}
	return out, nil
}

// EncryptInPlace runs under the repo's creation rules from the repo
// root, so the path-scoped rule for fleet/apps/<fort>/ picks the
// fort's recipients (identity must be seeded — cluster-init's job).
func (s sopsCLI) EncryptInPlace(path string) error {
	cmd := exec.Command("sops", "--config", filepath.Join(s.root, ".sops.yaml"), "-e", "-i", path)
	cmd.Dir = s.root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sops -e %s: %s", path, lastLine(string(out)))
	}
	return nil
}

// Set writes one stringData key into an existing encrypted file
// (re-encrypts just that value + the MAC — a reviewable diff). The
// value rides sops' argv, visible in the process list for the call's
// duration on this single-operator machine; the alternative (decrypt,
// edit, re-encrypt the whole file) churns every ciphertext in the diff.
func (s sopsCLI) Set(path, key, value string) error {
	v, _ := json.Marshal(value)
	cmd := exec.Command("sops", "--config", filepath.Join(s.root, ".sops.yaml"), "set", path,
		fmt.Sprintf(`["stringData"]["%s"]`, key), string(v))
	cmd.Dir = s.root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sops set %s %s: %s", path, key, lastLine(string(out)))
	}
	return nil
}

func (s sopsCLI) GitAdd(path string) error {
	cmd := exec.Command("git", "add", path)
	cmd.Dir = s.root
	return cmd.Run()
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
