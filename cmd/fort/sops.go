package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/tools"
)

// fortManagedMarker names the creation rules whose recipients the fort
// owns: a rule of the estate's .sops.yaml that carries this comment
// holds one recipient line per cluster, added by `fort mint` and
// removed by `fort dismantle`. init renders one such rule — the shared
// flux/infrastructure rule — and a layer that commits a sops file the
// whole fleet must decrypt brings a marked rule for its own tree
// (layers.md), below every fort rule so it shadows none of them.
const fortManagedMarker = "# fort-managed: one recipient per cluster"

// sopsRule is one creation rule of .sops.yaml as the line-based reads
// of this package see it: its path_regex, and whether it is
// fort-managed.
type sopsRule struct {
	regex   *regexp.Regexp
	managed bool
}

// sopsRules reads the creation rules in order. The parse is line-based
// like every .sops.yaml edit here — the file is fort-rendered. A
// path_regex that does not compile becomes a rule that matches nothing;
// sops itself refuses the file before that could matter.
func sopsRules(text string) []sopsRule {
	var rules []sopsRule
	for _, l := range strings.Split(text, "\n") {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "- path_regex:"):
			re, _ := regexp.Compile(strings.TrimSpace(strings.TrimPrefix(t, "- path_regex:")))
			rules = append(rules, sopsRule{regex: re})
		case strings.Contains(t, fortManagedMarker) && len(rules) > 0:
			rules[len(rules)-1].managed = true
		}
	}
	return rules
}

// fortManagedFiles walks the estate for committed sops files whose
// creation rule is fort-managed — the files that must re-encrypt
// (`sops updatekeys`) when mint adds a cluster recipient or dismantle
// removes one. The rule is found the way sops finds it: the first
// path_regex that matches the file's path from the root wins, so a
// file under a fort's own rule is never in this set. The walk skips
// .git and nixos/ — host secrets answer to nixos/.sops.yaml, whose
// recipients are per host, not per cluster — and reads everything
// else, so a layer's tree is covered the day its marked rule is.
func fortManagedFiles(root string) ([]string, error) {
	cfg, err := os.ReadFile(filepath.Join(root, ".sops.yaml"))
	if err != nil {
		return nil, err
	}
	rules := sopsRules(string(cfg))
	var out []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || (d.Name() == "nixos" && filepath.Dir(path) == root) {
				return fs.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".yaml" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(raw), "\nsops:\n") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for _, r := range rules {
			if r.regex != nil && r.regex.MatchString(rel) {
				if r.managed {
					out = append(out, rel)
				}
				break
			}
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// secretsTool is the seeder's only path to secret material: decrypt
// into memory, encrypt a plaintext file in place, set one key in an
// encrypted file. The real one execs sops (and git add); tests swap in
// a fake, so no test ever touches an age key or prints a value.
type secretsTool interface {
	Decrypt(path string) ([]byte, error)
	EncryptInPlace(path string) error
	Set(path, key, value string) error
	UpdateKeys(path string) error
	GitAdd(path string) error
	// The host-secrets shapes (fort mint): the same two operations under
	// an explicit config — nixos/.sops.yaml for nixos/secrets/<host>.yaml,
	// the fleet one for the canary — and a caller-supplied key path.
	EncryptWith(config, path string) error
	SetWith(config, path, jsonPath, value string) error
	// UnsetWith removes one key — the inverse of SetWith, and the only
	// way an entry of another file (a key a layer declared) can be
	// withdrawn by `fort dismantle` or a layer's.
	UnsetWith(config, path, jsonPath string) error
}

type sopsCLI struct{ root string }

func (s sopsCLI) Decrypt(path string) ([]byte, error) {
	cmd, err := tools.Command("sops", "-d", path)
	if err != nil {
		return nil, err
	}
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("sops -d %s: %s", path, lastLine(exitDetail(err)))
	}
	return out, nil
}

// EncryptInPlace runs under the repo's creation rules from the repo
// root, so the path-scoped rule for flux/apps/<fort>/ picks the
// fort's recipients (identity must be minted first — fort mint's job).
func (s sopsCLI) EncryptInPlace(path string) error {
	return s.EncryptWith(filepath.Join(s.root, ".sops.yaml"), path)
}

func (s sopsCLI) EncryptWith(config, path string) error {
	cmd, err := tools.Command("sops", "--config", config, "-e", "-i", path)
	if err != nil {
		return err
	}
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
	return s.SetWith(filepath.Join(s.root, ".sops.yaml"), path, fmt.Sprintf(`["stringData"]["%s"]`, key), value)
}

func (s sopsCLI) SetWith(config, path, jsonPath, value string) error {
	v, _ := json.Marshal(value)
	cmd, err := tools.Command("sops", "--config", config, "set", path, jsonPath, string(v))
	if err != nil {
		return err
	}
	cmd.Dir = s.root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sops set %s %s: %s", path, jsonPath, lastLine(string(out)))
	}
	return nil
}

func (s sopsCLI) UnsetWith(config, path, jsonPath string) error {
	cmd, err := tools.Command("sops", "--config", config, "unset", path, jsonPath)
	if err != nil {
		return err
	}
	cmd.Dir = s.root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sops unset %s %s: %s", path, jsonPath, lastLine(string(out)))
	}
	return nil
}

// UpdateKeys re-encrypts a file to the recipients its creation rule now
// lists — removing a recipient from .sops.yaml alone changes nothing:
// the departed key keeps decrypting until this runs (fort dismantle).
func (s sopsCLI) UpdateKeys(path string) error {
	cmd, err := tools.Command("sops", "--config", filepath.Join(s.root, ".sops.yaml"), "updatekeys", "-y", path)
	if err != nil {
		return err
	}
	cmd.Dir = s.root
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("sops updatekeys %s: %s", path, lastLine(string(out)))
	}
	return nil
}

// GitAdd stages a path — additions, edits and deletions alike (-A).
func (s sopsCLI) GitAdd(path string) error {
	cmd, err := tools.Command("git", "add", "-A", "--", path)
	if err != nil {
		return err
	}
	cmd.Dir = s.root
	return cmd.Run()
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
