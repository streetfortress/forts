package main

import (
	"os"
	"path/filepath"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

// The helpers the tests share: a scratch estate, a fixture copy, a fake
// sops tool that records what a verb would do. They lived in the apps
// layer's test files until forts#143 moved that layer out.

// estateRoot is the checkout this test mirrors against: a repo root that
// holds the manifests (manifest.Dir). The mirror tests compare the CLI
// with a committed estate — its manifests, host files, sops rules, the
// peers file — and that estate is not this repo once the CLI lives in
// sfi/forts. Outside an estate they skip; inside one (an estate's CI runs
// `go test` against the framework it pins) they are the gate.
func estateRoot(t *testing.T) string {
	t.Helper()
	repo, err := repoRootFrom(mustGetwd(t))
	if err != nil {
		t.Skip("not inside a git checkout")
	}
	// A manifest, not just the directory: in sfi/forts the root has a
	// forts/ too — the CLI's own source — and no manifest in it.
	if m, _ := filepath.Glob(filepath.Join(repo, manifest.Dir, "*.toml")); len(m) == 0 {
		t.Skipf("no estate at %s (no %s/*.toml): the mirror tests run in an estate checkout", repo, manifest.Dir)
	}
	return repo
}

func mustGetwd(t *testing.T) string {
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func readFile(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return string(b)
}

// copyEstate copies the fixture estate.toml into a scratch root: add reads
// the estate's zone from it (forts#45).
func copyEstate(t *testing.T, dstRoot string) {
	t.Helper()
	copyFile(t, "testdata/estate.toml", filepath.Join(dstRoot, "estate.toml"))
}

// copyManifest copies one host's manifest from a repo/fixture root
// (testdata, or the checkout) into a scratch root.
func copyManifest(t *testing.T, srcRoot, dstRoot, host string) {
	t.Helper()
	copyFile(t, filepath.Join(srcRoot, manifest.Path(host)), filepath.Join(dstRoot, manifest.Path(host)))
}

func copyFile(t *testing.T, src, dst string) {
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeTool records what the seeder would do with sops, and serves
// fixture plaintext for decrypts. Nothing here is encrypted; the test
// only ever inspects key names and the recorded calls.
type fakeTool struct {
	plain     map[string]string // path → decrypted content
	encrypted []string
	sets      [][3]string
	added     []string
	unset     []string
	rekeyed   []string
}

func (f *fakeTool) Decrypt(path string) ([]byte, error) {
	if s, ok := f.plain[path]; ok {
		return []byte(s), nil
	}
	// The fake never encrypts, so a file this tool "encrypted" is still
	// plaintext on disk — a re-run reads back what the first run wrote.
	if raw, err := os.ReadFile(path); err == nil {
		for _, e := range f.encrypted {
			if e == path {
				return raw, nil
			}
		}
	}
	return nil, os.ErrNotExist
}
func (f *fakeTool) EncryptInPlace(path string) error {
	f.encrypted = append(f.encrypted, path)
	return nil
}
func (f *fakeTool) Set(path, key, value string) error {
	f.sets = append(f.sets, [3]string{path, key, value})
	return nil
}
func (f *fakeTool) EncryptWith(config, path string) error {
	f.encrypted = append(f.encrypted, path)
	return nil
}
func (f *fakeTool) SetWith(config, path, jsonPath, value string) error {
	f.sets = append(f.sets, [3]string{path, jsonPath, value})
	return nil
}
func (f *fakeTool) UnsetWith(config, path, jsonPath string) error {
	f.unset = append(f.unset, path+" "+jsonPath)
	return nil
}
func (f *fakeTool) GitAdd(path string) error { f.added = append(f.added, path); return nil }
func (f *fakeTool) UpdateKeys(path string) error {
	f.rekeyed = append(f.rekeyed, path)
	return nil
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func repoRootFrom(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func copyTree(t *testing.T, src, dst string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			copyTree(t, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
			continue
		}
		copyFile(t, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
	}
}
