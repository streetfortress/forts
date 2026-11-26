package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// buildRev is the commit this binary was built from: the flake stamps
// it (-X main.buildRev=<shortRev>) because a flake build sees the rev
// and never the tag — `self` carries no tag. A `go run` or `go build`
// outside nix leaves it "dev".
var buildRev = "dev"

// version — `fort --version`. The tag lives in one place only, the
// estate's lock (docs/release.md): forts.url names it (?ref=refs/tags/
// v0.0.1) and nixos/flake.lock records it next to the rev it resolved
// to. So the binary prints its own rev, and when it runs inside an
// estate it reads that lock and joins the two: the tag the estate
// pins, and whether this binary is that pin's build. Outside an estate,
// or on an estate that pins a branch, there is no tag to print and the
// line says which.
func version(w io.Writer) error {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintf(w, "⛫ fort %s\n", buildRev) // a checkout of nothing: the build is the answer
		return nil
	}
	pin, err := readPin(filepath.Join(root, "nixos", "flake.lock"))
	if err != nil {
		fmt.Fprintf(w, "⛫ fort %s\n", buildRev)
		if os.IsNotExist(err) {
			return nil // not an estate (or not yet locked)
		}
		return fmt.Errorf("nixos/flake.lock: %w", err)
	}
	if buildRev == "dev" {
		// An unstamped build cannot be compared with the lock.
		fmt.Fprintf(w, "⛫ fort %s (this binary)\n  the estate pins %s\n", buildRev, pin)
		return nil
	}
	if !strings.HasPrefix(pin.rev, buildRev) {
		// The binary and the lock disagree: say both, and which is which.
		fmt.Fprintf(w, "⛫ fort %s (this binary)\n", buildRev)
		fmt.Fprintf(w, "  the estate pins %s\n", pin)
		fmt.Fprintf(w, "  this binary is not the pin's build — a checkout, --override-input, or a profile installed from another lock\n")
		return nil
	}
	// One line, as `go version` gives one: the tag and the commit it is.
	fmt.Fprintf(w, "⛫ fort %s\n", pin)
	return nil
}

// releaseTag matches the name of a release tag.
var releaseTag = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// pin is what the estate's lock says about its forts input.
type pin struct {
	tag string // "v0.0.1" when forts.url names refs/tags/…; "" otherwise
	ref string // the original ref as written, tag or branch; "" when none
	rev string // the locked commit
}

func (p pin) String() string {
	short := p.rev
	if len(short) > 7 {
		short = short[:7]
	}
	switch {
	case p.tag != "":
		return fmt.Sprintf("%s at %s", p.tag, short)
	case p.ref != "":
		return fmt.Sprintf("%s at %s — a branch, not a release (docs/release.md)", p.ref, short)
	default:
		return fmt.Sprintf("%s — forts.url names no ref (docs/release.md)", short)
	}
}

// readPin reads the forts node of a flake.lock. The lock's shape is
// nix's: nodes.<name>.{original,locked}, with the input names resolved
// through the root node — the estate template names the input `forts`,
// and that is what the root node's inputs map says.
func readPin(lockPath string) (pin, error) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return pin{}, err
	}
	var lock struct {
		Root  string `json:"root"`
		Nodes map[string]struct {
			Inputs   map[string]json.RawMessage `json:"inputs"`
			Original struct {
				Ref string `json:"ref"`
			} `json:"original"`
			Locked struct {
				Rev string `json:"rev"`
			} `json:"locked"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(data, &lock); err != nil {
		return pin{}, err
	}
	root, ok := lock.Nodes[lock.Root]
	if !ok {
		return pin{}, fmt.Errorf("no root node %q", lock.Root)
	}
	raw, ok := root.Inputs["forts"]
	if !ok {
		return pin{}, fmt.Errorf("the root node has no input named forts")
	}
	var name string
	if err := json.Unmarshal(raw, &name); err != nil {
		// A follows entry is a path (["forts","nixpkgs"]); the forts
		// input itself is never one. Treat anything else as absent.
		return pin{}, fmt.Errorf("the forts input is not a node name")
	}
	node, ok := lock.Nodes[name]
	if !ok {
		return pin{}, fmt.Errorf("no node %q for the forts input", name)
	}
	p := pin{ref: node.Original.Ref, rev: node.Locked.Rev}
	switch {
	case strings.HasPrefix(p.ref, "refs/tags/"):
		p.tag = strings.TrimPrefix(p.ref, "refs/tags/")
	case releaseTag.MatchString(p.ref):
		// A github: url (github:owner/forts/v0.0.11) records the ref
		// bare, with no refs/tags/ prefix, so a tag and a branch look
		// alike. The release tags are vMAJOR.MINOR.PATCH (docs/release.md).
		p.tag = p.ref
	}
	return p, nil
}
