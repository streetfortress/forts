package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// The small helpers every author verb shares: a file's presence, the
// key names of a committed Secret, and the action a plan is made of.
// They lived in the apps layer's files until forts#143 moved that layer
// out (sfi/fort-layers); the layers copy what they need of these.

// exists says whether a repo-relative path is there.
func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
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
