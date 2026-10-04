// Package estate reads the estate manifest: estate.toml at the root of an
// estate checkout. It is the one file that says what the checkout IS —
// its name, its git URL (flux's source and auto-rebuild's pull) and, when
// it has one, the fort that is its monitoring hub. Per-host facts are the
// host manifests (internal/manifest); per-estate facts are here, and
// there are few of them on purpose: an estate is plain NixOS and a flux
// tree, and this file names only what a verb reads.
//
// `fort init` writes it. `fort scaffold` reads it for the flux source URL
// and the hub; `fort check`, `fort dismantle` read the hub. A checkout
// with no estate.toml is not an estate, and every verb says so.
package estate

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"
)

// Path is the manifest's place: the estate root.
const Path = "estate.toml"

// Estate is estate.toml. Keys are strict: a typo is a refusal, not a
// default, like a host manifest's.
type Estate struct {
	// Name is the estate's name: the gitea repo's, by convention.
	Name string `toml:"name"`
	// Repo is the estate's git URL as flux and auto-rebuild reach it
	// (ssh://git@<gitea>:<port>/<owner>/<name>, with or without .git).
	// Both clone it with the deploy key fort mint registers.
	Repo string `toml:"repo"`
	// Hub is the fort whose monitoring tree carries every other fort's
	// file (the dead man's switch, the edge Alertmanager datasource, a
	// plain fort's scrape). Optional: an estate with no hub has no one to
	// notice a silent fort, and says so by leaving this out.
	Hub string `toml:"hub"`
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// Load reads <root>/estate.toml.
func Load(root string) (Estate, error) {
	raw, err := os.ReadFile(filepath.Join(root, Path))
	if err != nil {
		if os.IsNotExist(err) {
			return Estate{}, fmt.Errorf("no %s at %s — this checkout is not an estate; `fort init` renders one", Path, root)
		}
		return Estate{}, err
	}
	return Parse(raw)
}

// Parse decodes the manifest text, strictly, and validates it.
func Parse(raw []byte) (Estate, error) {
	var e Estate
	md, err := toml.Decode(string(raw), &e)
	if err != nil {
		return Estate{}, fmt.Errorf("%s: %w", Path, err)
	}
	for _, k := range md.Undecoded() {
		return Estate{}, fmt.Errorf("%s: unknown key %s", Path, k.String())
	}
	if err := e.Validate(); err != nil {
		return Estate{}, fmt.Errorf("%s: %w", Path, err)
	}
	return e, nil
}

// Validate: a name that is a repo name, a repo URL a clone understands,
// a hub that is a host name if present.
func (e Estate) Validate() error {
	if !nameRE.MatchString(e.Name) {
		return fmt.Errorf("name %q — want a repo name (%s)", e.Name, nameRE)
	}
	if !strings.HasPrefix(e.Repo, "ssh://") && !strings.HasPrefix(e.Repo, "https://") {
		return fmt.Errorf("repo %q — want the estate's git URL (ssh://git@<gitea>:<port>/<owner>/<name>, or https://)", e.Repo)
	}
	if e.Hub != "" && !nameRE.MatchString(e.Hub) {
		return fmt.Errorf("hub %q — want the hub fort's host name (%s), or no hub", e.Hub, nameRE)
	}
	return nil
}

// RepoGit is the URL with its .git suffix, the form the FluxInstance
// carries; auto-rebuild clones the same.
func (e Estate) RepoGit() string {
	if strings.HasSuffix(e.Repo, ".git") {
		return e.Repo
	}
	return e.Repo + ".git"
}

// HasHub says whether the estate names a monitoring hub.
func (e Estate) HasHub() bool { return e.Hub != "" }
