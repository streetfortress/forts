package tools

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// repoRoot: this package sits two levels down.
const repoRoot = "../.."

func TestEveryPackagedToolHasAnAttr(t *testing.T) {
	if len(declared.Packaged) == 0 {
		t.Fatal("tools.json: no packaged tools")
	}
	for name, attr := range declared.Packaged {
		if strings.TrimSpace(attr) == "" {
			t.Errorf("%s: no nixpkgs attribute", name)
		}
		if strings.ContainsAny(name, "/ ") {
			t.Errorf("%q: a tool is a bare binary name, not a path", name)
		}
	}
	for name, a := range declared.Ambient {
		if strings.TrimSpace(a.Why) == "" {
			t.Errorf("%s: an ambient tool must say why the closure cannot carry it", name)
		}
		if strings.TrimSpace(a.Attr) == "" {
			t.Errorf("%s: an ambient tool must name the attribute an operator installs it from", name)
		}
		if _, both := declared.Packaged[name]; both {
			t.Errorf("%s: declared packaged and ambient at once", name)
		}
	}
}

var execCall = regexp.MustCompile(`exec\.Command(?:Context)?\((?:[A-Za-z0-9_.]+,\s*)?"([^"]+)"`)

// TestDeclaredSetCoversEveryExec: the set is the CLI's tool
// dependencies, so a binary the source execs by name belongs in it. A
// new exec.Command("foo") fails here until tools.json declares foo —
// which is what keeps the wrapper's PATH and the handbook's table
// complete. Test files are exempt: they exec their own fixtures.
func TestDeclaredSetCoversEveryExec(t *testing.T) {
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata", "templates":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range execCall.FindAllStringSubmatch(string(src), -1) {
			name := m[1]
			if _, ok := declared.Packaged[name]; ok {
				continue
			}
			if _, ok := declared.Ambient[name]; ok {
				continue
			}
			t.Errorf("%s: exec.Command(%q) — declare %s in internal/tools/tools.json and start it with tools.Command", path, name, name)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestFlakeReadsTheSet: the file is the set because two readers share
// it. If the flake stops reading it, the packaged tools and the
// wrapper's PATH drift apart silently.
func TestFlakeReadsTheSet(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, "flake.nix"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "internal/tools/tools.json") {
		t.Error("flake.nix does not read internal/tools/tools.json — packages.fort would wrap the binaries with a stale tool list")
	}
}
