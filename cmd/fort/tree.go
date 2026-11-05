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

// The small helpers every author verb shares: a file's presence, the
// key names of a committed Secret, and the action a plan is made of.
// add, seed, check and dismantle all read them; the layers copy what
// they need of these (sfi/fort-layers).

// exists says whether a repo-relative path is there.
func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

// sopsRefPath joins a sops: ref's rel onto the dir that declares it. A
// ref may climb to a sibling app or to another fort, so the bound is
// flux/apps, not the dir; a rel that is absolute or that leaves flux/apps
// is refused before anything reads it.
func sopsRefPath(dir, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("sops ref %s is an absolute path", rel)
	}
	path := filepath.Join(dir, rel)
	if up, err := filepath.Rel("flux/apps", path); err != nil || up == ".." || strings.HasPrefix(up, "../") {
		return "", fmt.Errorf("sops ref %s leaves flux/apps", rel)
	}
	return path, nil
}

// secretKeyNames reads the key NAMES of a Secret file — stringData and
// data — which sops leaves in plaintext under the repo's encrypted_regex,
// so no age key is involved: the property every gate over a committed
// Secret runs on.
func secretKeyNames(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		StringData map[string]any `yaml:"stringData"`
		Data       map[string]any `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	var names []string
	for k := range doc.StringData {
		names = append(names, k)
	}
	for k := range doc.Data {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

// action is one entry of a plan: a new file (content), or an edit of an
// existing one (edit), reported in the marked-tree shape — ● new,
// ○ appended, ◆ seeded, ✗ removed.
type action struct {
	path    string // repo-relative
	mark    rune
	note    string
	content []byte
	edit    func(string) (string, error)
}

// exitDetail: a tool's stderr when the error carries one, else the error.
func exitDetail(err error) string {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}
