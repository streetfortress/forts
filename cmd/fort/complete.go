// Completion for the fort CLI (#113). `fort __complete` is a hidden verb
// that answers one <TAB>: it prints one candidate per line, and a last
// line that starts with ':' when the shell must do the work itself (a
// path) or must not add a space. The scripts under completions/ hold no
// knowledge of their own — they pass the words and print what comes
// back — so a new verb, a new flag or a new candidate set lands here and
// both shells learn it.
//
// The candidate set is per verb, and two of them are the INVERSE of the
// manifest list: `destroy` wants a stack whose manifest is gone, and
// `wg-conf` wants a mesh member, which a hand-managed peer is and never has a
// manifest. A completion that offered every manifest everywhere would be
// worse than useful for those two, so every verb names its own set
// below.
//
// Two properties this file must keep:
//
//   - It reads the tree and nothing else. The callback runs per <TAB>,
//     so no verb's completion may `git fetch` the way destroy's gate
//     does (destroy.go): destroyable reads the working tree alone and is
//     wrong only between the merge of a dismantle PR and the pull, which
//     is the right trade for a keystroke.
//   - It never prints an error. A broken manifest, a missing directory,
//     a checkout that is not an estate — every one of them is an empty
//     candidate list, because the operator is pressing <TAB>, not
//     running a verb.
//
// The drift guard is complete_test.go: the verb set here must equal
// main's switch, and each verb's flags must equal the ones its
// flag.FlagSet registers.
package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

// completeVerb is the name of the hidden verb. Two underscores, cobra's
// convention: it is for a script, it is not in usage(), and a verb nobody
// types cannot collide with one somebody does.
const completeVerb = "__complete"

// completeCx is what a candidate function may read: the estate's root,
// and the positionals the operator already typed for this verb (the
// cursor's own word is NOT one of them). `fort link add <fort> <TAB>` asks
// for a URL after a fort, so the positionals have to be legible here.
type completeCx struct {
	root string
	args []string
}

// candidates answers one slot — a positional, or a flag's value.
type candidates func(cx *completeCx) []string

// argKind is what the shell must do with a flag's value.
type argKind int

const (
	noValue  argKind = iota // a boolean flag: --dry-run, --yes
	opaque                  // a value nothing can enumerate: a name, a URL, a number
	paths                   // a path: the shell's own file completion
	dirsOnly                // a directory: the shell's own, directories only
	closed                  // a set this file knows: --without, --hub
)

type flagSpec struct {
	name string // without the dashes; the scripts print --name
	kind argKind
	// values answers `--flag <TAB>` for a closed kind.
	values candidates
	// noSpace keeps the cursor on the candidate: --hub completes to
	// `monitoring=`, which the operator finishes.
	noSpace bool
}

// verbSpec is one verb's surface: its flags, and its positionals in
// order. args[i] answers the i-th positional; with repeat, the last
// entry answers every further one (`fort check <fort>...`) and the ones
// already typed are dropped.
type verbSpec struct {
	flags  []flagSpec
	args   []candidates
	repeat bool
}

// write, yes and the other switches every verb spells the same way.
var (
	writeFlag = flagSpec{name: "dry-run"}
	yesFlag   = flagSpec{name: "yes"}
)

// completions is the table — the third reader of the verb list, after
// main's switch and usage(). complete_test.go asserts it equals the
// first; a verb added to one and not the other fails the build's tests.
var completions = map[string]verbSpec{
	"init": {flags: []flagSpec{
		{name: "name", kind: opaque},
		{name: "repo", kind: opaque},
		{name: "hub", kind: opaque},
		{name: "forge-name", kind: opaque},
		{name: "mesh", kind: opaque},
		{name: "interface", kind: opaque},
		{name: "zone", kind: opaque},
		{name: "forts", kind: opaque},
		{name: "known-hosts", kind: opaque},
		{name: "acme-email", kind: opaque},
		{name: "admin", kind: opaque},
		{name: "operator-key", kind: opaque},
		writeFlag,
	}},
	"scaffold": {flags: []flagSpec{writeFlag}, args: []candidates{manifests}},
	"image": {flags: []flagSpec{
		{name: "from", kind: paths},
		{name: "out", kind: dirsOnly},
	}, args: []candidates{manifests}},
	"provision": {flags: []flagSpec{
		{name: "image", kind: paths},
		yesFlag,
	}, args: []candidates{realized}},
	"preview":          {flags: []flagSpec{{name: "all"}, {name: "refresh"}, {name: "expect-no-changes"}}, args: []candidates{realized}},
	"mint":             {flags: []flagSpec{writeFlag}, args: []candidates{manifests}},
	"console-password": {flags: []flagSpec{writeFlag, {name: "rotate"}, {name: "show"}}, args: []candidates{manifests}},
	"check":            {flags: []flagSpec{{name: "all"}, {name: "eval"}}, args: []candidates{manifests}, repeat: true},
	"ssh-config":       {flags: []flagSpec{writeFlag}},
	"wg-conf":          {args: []candidates{meshMembers}},
	"--version":        {}, // a flag in the verb position (forts#135): completes, takes nothing
	"dismantle":        {flags: []flagSpec{writeFlag}, args: []candidates{manifests}},
	"destroy":          {flags: []flagSpec{yesFlag, {name: "no-stack"}}, args: []candidates{destroyable}},
	// The link verbs (#55): a sub-verb word first, then the fort, then
	// the other estate's git URL, which nothing in the tree can offer.
	"link": {flags: []flagSpec{
		{name: "address", kind: opaque},
		{name: "port", kind: opaque},
		writeFlag,
	}, args: []candidates{words("add"), manifests, nil}},
	"estate": {flags: []flagSpec{writeFlag}, args: []candidates{words("init", "link"), nil}},
	// The layer verbs (#144): add takes a flake url nothing in the tree
	// can offer; remove takes an installed layer, which the tree can.
	"layer": {flags: []flagSpec{writeFlag}, args: []candidates{words("add", "remove"), layerNames}},
}

// layerNames: the layers/<name>/ directories of the estate.
func layerNames(cx *completeCx) []string {
	names, _ := installedLayers(cx.root)
	return names
}

// words is a closed positional: the sub-verb words a verb takes.
func words(w ...string) candidates {
	return func(*completeCx) []string { return w }
}

// complete answers one <TAB>:
//
//	fort __complete <index> [<word>...]
//
// The words are the command line after `fort`, the cursor's own word
// included (empty when the cursor sits on a fresh word); index is its
// 0-based position among them. The verb is word 0, so index 0 completes
// the verb itself and no second contract is needed for it.
//
// It writes to stdout and returns nil whatever happens: the caller is a
// shell callback, and an "error:" line on stderr in the middle of a
// completion is noise the operator cannot act on.
func complete(args []string) error {
	if len(args) < 1 {
		return nil
	}
	index, err := strconv.Atoi(args[0])
	if err != nil || index < 0 {
		return nil
	}
	words := args[1:]
	for len(words) <= index {
		// The scripts drop a trailing empty word in some shells; the
		// cursor is on a word that is not there, which is an empty one.
		words = append(words, "")
	}
	out, directive := completeWords(index, words[:index+1])
	for _, c := range out {
		fmt.Println(c)
	}
	fmt.Println(":" + directive)
	return nil
}

// The directives: what the shell does after the candidates. One per
// answer, the last line of the output.
const (
	dirPlain   = "plain"   // the candidates are the whole answer
	dirFiles   = "files"   // no candidates: the shell completes a path
	dirDirs    = "dirs"    // no candidates: the shell completes a directory
	dirNoSpace = "nospace" // the candidates are prefixes; add no space
)

// completeWords is the whole of the logic, over the words up to and
// including the cursor's own. The shells filter by prefix themselves
// (compadd, compgen -W), so nothing here looks at the partial word.
func completeWords(index int, words []string) ([]string, string) {
	if index == 0 {
		return verbNames(), dirPlain
	}
	spec, ok := completions[words[0]]
	if !ok {
		return nil, dirPlain // an unknown verb: offer nothing, not everything
	}
	root, err := repoRoot()
	if err != nil {
		return nil, dirPlain
	}
	cur := words[index]
	typed := words[1:index]

	// A flag's value, when the word before the cursor is a flag that
	// takes one. `--dry-run <TAB>` is a positional; `--out <TAB>` is not.
	if len(typed) > 0 && !strings.HasPrefix(cur, "-") {
		if f, ok := spec.flag(typed[len(typed)-1]); ok && f.kind != noValue && !strings.Contains(typed[len(typed)-1], "=") {
			return flagValues(f, &completeCx{root: root})
		}
	}
	if strings.HasPrefix(cur, "-") {
		return spec.flagNames(), dirPlain
	}

	// Which positional the cursor is: every typed word that is neither a
	// flag nor the value of one.
	var positionals []string
	for i := 0; i < len(typed); i++ {
		w := typed[i]
		if strings.HasPrefix(w, "-") {
			if f, ok := spec.flag(w); ok && f.kind != noValue && !strings.Contains(w, "=") {
				i++ // the next word is this flag's value
			}
			continue
		}
		positionals = append(positionals, w)
	}
	slot := len(positionals)
	if slot >= len(spec.args) {
		if !spec.repeat || len(spec.args) == 0 {
			return nil, dirPlain
		}
		slot = len(spec.args) - 1
	}
	fn := spec.args[slot]
	if fn == nil {
		return nil, dirPlain
	}
	out := fn(&completeCx{root: root, args: positionals})
	if spec.repeat && slot == len(spec.args)-1 {
		// A repeating slot never offers what it already holds.
		out = slices.DeleteFunc(out, func(c string) bool {
			return slices.Contains(positionals[slot:], c)
		})
	}
	return out, dirPlain
}

func flagValues(f flagSpec, cx *completeCx) ([]string, string) {
	switch f.kind {
	case paths:
		return nil, dirFiles
	case dirsOnly:
		return nil, dirDirs
	case closed:
		if f.noSpace {
			return f.values(cx), dirNoSpace
		}
		return f.values(cx), dirPlain
	}
	return nil, dirPlain // opaque: a value only the operator knows
}

// flag finds the spec of a word the operator typed as a flag, in any of
// the forms Go's flag package accepts: -name, --name, --name=value.
func (s verbSpec) flag(word string) (flagSpec, bool) {
	name := strings.TrimLeft(word, "-")
	name, _, _ = strings.Cut(name, "=")
	for _, f := range s.flags {
		if f.name == name {
			return f, true
		}
	}
	return flagSpec{}, false
}

func (s verbSpec) flagNames() []string {
	out := make([]string, 0, len(s.flags))
	for _, f := range s.flags {
		out = append(out, "--"+f.name)
	}
	sort.Strings(out)
	return out
}

func verbNames() []string {
	out := make([]string, 0, len(completions))
	for v := range completions {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// --- the candidate sets

// manifests: every fort the estate declares — the same glob as
// `--all` (manifest.Names), in Go, where the repo root is known.
func manifests(cx *completeCx) []string {
	names, _ := manifest.Names(cx.root)
	return names
}

// manifestsWhere lists the manifests whose host satisfies keep. A
// manifest that does not parse is left out: every verb refuses it anyway,
// and `fort check` is where it gets named.
func manifestsWhere(cx *completeCx, keep func(manifest.Host) bool) []string {
	hosts, _, err := manifest.LoadAll(cx.root)
	if err != nil {
		return nil
	}
	var out []string
	for name, h := range hosts {
		if keep(h) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// realized: the hosts a backend raises — `provision` and `preview` both
// refuse a [baremetal] host by name, which has no stack by design.
func realized(cx *completeCx) []string {
	return manifestsWhere(cx, manifest.Host.Realized)
}

// clusters: the forts with a [cluster] table, for the two verbs that
// render into flux/apps/<fort>. A plain fort has no fleet surface at all
// (fleet.go: a flux dir on one is an error of its own).
func clusters(cx *completeCx) []string {
	return manifestsWhere(cx, manifest.Host.IsCluster)
}

// destroyable: the inverse of the manifest list — a stack whose manifest
// is gone, which is exactly what destroy's gate wants (destroy.go: it
// refuses a host still declared). The gate also reads origin/main; this
// reads the working tree only, because a <TAB> must not `git fetch`.
func destroyable(cx *completeCx) []string {
	var out []string
	for _, stack := range stacksInState(filepath.Join(cx.root, pulumiProject)) {
		if !exists(cx.root, manifest.Path(stack)) {
			out = append(out, stack)
		}
	}
	return out
}

// meshMembers: who is on the mesh — peers and roaming clients from
// mesh/peers.json, not manifests. A Mac joins the mesh and gets its
// wg-conf without ever having one (docs/mesh.md). A remote is another
// estate's host and renders no config here.
func meshMembers(cx *completeCx) []string {
	m, err := loadMesh(cx.root)
	if err != nil {
		return nil
	}
	out := append(sortedNames(m.Peers), sortedNames(m.Clients)...)
	sort.Strings(out)
	return out
}
