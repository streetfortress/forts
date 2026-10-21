package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The lock an estate on a release carries: forts.url names the tag, and
// nix records it next to the rev it resolved to (sfi/staging on v0.0.1).
const lockOnTag = `{
  "nodes": {
    "forts": {
      "inputs": { "nixpkgs": "nixpkgs", "sops-nix": "sops-nix" },
      "locked": { "ref": "refs/tags/v0.0.1", "rev": "eef09bcfefccfd0c284252ec08f9c089b964d4f4", "type": "git", "url": "https://gitea.zen.lofi/sfi/forts" },
      "original": { "ref": "refs/tags/v0.0.1", "type": "git", "url": "https://gitea.zen.lofi/sfi/forts" }
    },
    "nixpkgs": { "locked": { "rev": "0" }, "original": {} },
    "sops-nix": { "locked": { "rev": "0" }, "original": {} },
    "root": { "inputs": { "forts": "forts", "nixpkgs": ["forts", "nixpkgs"], "sops-nix": ["forts", "sops-nix"] } }
  },
  "root": "root",
  "version": 7
}`

// The same lock when forts.url is a bare URL: nix tracks the default
// branch and writes no original ref.
const lockOnBranch = `{
  "nodes": {
    "forts": {
      "locked": { "ref": "refs/heads/main", "rev": "2b776af0000000000000000000000000deadbeef", "type": "git", "url": "https://gitea.zen.lofi/sfi/forts" },
      "original": { "type": "git", "url": "https://gitea.zen.lofi/sfi/forts" }
    },
    "root": { "inputs": { "forts": "forts" } }
  },
  "root": "root",
  "version": 7
}`

func writeLock(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "flake.lock")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// fort --version prints the tag an estate pins (forts#135: "on an
// estate's pin prints the tag"), and says when the pin is no release.
func TestReadPin(t *testing.T) {
	p, err := readPin(writeLock(t, lockOnTag))
	if err != nil {
		t.Fatal(err)
	}
	if p.tag != "v0.0.1" || !strings.HasPrefix(p.rev, "eef09bc") {
		t.Fatalf("pin = %+v", p)
	}
	if got := p.String(); got != "v0.0.1 (eef09bc)" {
		t.Fatalf("String() = %q", got)
	}

	p, err = readPin(writeLock(t, lockOnBranch))
	if err != nil {
		t.Fatal(err)
	}
	if p.tag != "" || !strings.Contains(p.String(), "forts.url names no ref") {
		t.Fatalf("branch pin = %+v / %q", p, p.String())
	}

	if _, err := readPin(filepath.Join(t.TempDir(), "flake.lock")); !os.IsNotExist(err) {
		t.Fatalf("missing lock: want IsNotExist, got %v", err)
	}
}

// Inside an estate the output joins the build and the pin, and flags a
// binary that is not the pin's build. The estate is a temp dir with a
// .git marker, which is all repoRoot looks for.
func TestVersionInEstate(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{".git", "nixos"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "nixos", "flake.lock"), []byte(lockOnTag), 0o644); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })

	defer func(old string) { buildRev = old }(buildRev)
	for _, tc := range []struct{ rev, want, absent string }{
		{"eef09bc", "pin  v0.0.1 (eef09bc)", "not the pin's build"},
		{"0123abc", "not the pin's build", ""},
		{"dev", "fort dev", "not the pin's build"},
	} {
		buildRev = tc.rev
		var out bytes.Buffer
		if err := version(&out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), tc.want) || (tc.absent != "" && strings.Contains(out.String(), tc.absent)) {
			t.Errorf("buildRev=%s:\n%s", tc.rev, out.String())
		}
	}
}
