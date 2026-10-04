package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// A retired fort's record, read back from git history: destroy runs after
// dismantle deleted the manifest and the secrets file, so the commit that
// deleted them is the only copy left (#432).

// retiredSecrets is a retired fort's nixos/secrets/<host>.yaml as the
// commit that deleted it had it: still encrypted, so the VALUES need the
// admin age key, but the key NAMES are plaintext — the property
// secretHasKey and `fort check` already run on. Destroy reads the file
// once and asks it two things (#432): which GPG key each account signed
// with (a decrypt), and whether the fort ever held a B2 key (a name: the
// key mint writes restic-s3-env-b2 exactly when it mints one).
type retiredSecrets struct {
	rel   string
	raw   []byte
	names []string // the plaintext top-level keys, sops's own block excluded
}

func retiredSecretsPath(host string) string { return filepath.Join("nixos/secrets", host+".yaml") }

// readRetiredSecrets is the one history read. nil, nil when no such file
// was ever deleted on origin/main — the fort never had one, or it is
// still live (destroy's gates rule that out) — and the caller treats
// "no record" as "unknown", never as "none".
func readRetiredSecrets(root, host string) (*retiredSecrets, error) {
	rel := retiredSecretsPath(host)
	raw, err := retiredFile(root, rel)
	if err != nil || raw == nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s (retired): %w", rel, err)
	}
	s := &retiredSecrets{rel: rel, raw: raw}
	for k := range doc {
		if k != "sops" {
			s.names = append(s.names, k)
		}
	}
	sort.Strings(s.names)
	return s, nil
}

// has reports whether the retired file held a key — a plaintext fact, no
// age key needed. False on a nil receiver, so callers must gate on the
// receiver first when "unknown" and "absent" mean different things.
func (s *retiredSecrets) has(key string) bool { return s != nil && hasKey(s.names, key) }

// values decrypts the file, through a temp path because sops reads files.
func (s *retiredSecrets) values(decrypt func(path string) ([]byte, error)) (map[string]string, error) {
	dir, err := os.MkdirTemp("", "fort-retired-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, filepath.Base(s.rel))
	if err := os.WriteFile(tmp, s.raw, 0o600); err != nil {
		return nil, err
	}
	plain, err := decrypt(tmp)
	if err != nil {
		return nil, fmt.Errorf("decrypting the retired %s from git history: %w", s.rel, err)
	}
	var have map[string]string
	if err := yaml.Unmarshal(plain, &have); err != nil {
		return nil, fmt.Errorf("%s (retired): %w", s.rel, err)
	}
	return have, nil
}

// retiredFile is a deleted file's last content: the commit that removed
// it from origin/main still holds the version before it. nil when the
// path was never deleted there (it never existed, or it is still live).
func retiredFile(root, rel string) ([]byte, error) {
	sha, err := exec.Command("git", "-C", root, "log", "-1", "--format=%H", "--diff-filter=D", "origin/main", "--", rel).Output()
	if err != nil {
		return nil, fmt.Errorf("git log %s: %w", rel, err)
	}
	if strings.TrimSpace(string(sha)) == "" {
		return nil, nil
	}
	raw, err := exec.Command("git", "-C", root, "show", strings.TrimSpace(string(sha))+"^:"+rel).Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s before its deletion: %w", rel, err)
	}
	return raw, nil
}
