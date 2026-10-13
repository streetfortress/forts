package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The candidate sets, per verb, on a fixture estate: five manifests of
// five shapes, a mesh with a member that has no manifest, one stack with
// a manifest and one without, and one fort's apps tree. What each verb
// offers is the point of #113 — `destroy` and `wg-conf` are the inverse
// of the manifest list, and this is where that stays true.
func TestCompleteCandidates(t *testing.T) {
	t.Chdir(completionEstate(t))
	for _, tc := range []struct {
		// line is the words after `fort`, the cursor's own word last.
		line      []string
		want      []string
		directive string
	}{
		// The verb itself.
		{line: []string{""}, want: verbNames()},
		{line: []string{"sca"}, want: verbNames()}, // the shell filters by prefix, not this
		{line: []string{"nope", ""}},               // an unknown verb offers nothing, not everything

		// Every manifest.
		{line: []string{"scaffold", ""}, want: []string{"cloud-fort", "lab-fort", "metal-fort", "plain-fort", "sfi-fort-e33e2d5"}},
		{line: []string{"mint", "--write", ""}, want: []string{"cloud-fort", "lab-fort", "metal-fort", "plain-fort", "sfi-fort-e33e2d5"}},
		{line: []string{"console-password", ""}, want: []string{"cloud-fort", "lab-fort", "metal-fort", "plain-fort", "sfi-fort-e33e2d5"}},

		// The verbs a backend raises: [baremetal] has no stack by design.
		{line: []string{"provision", ""}, want: []string{"cloud-fort", "lab-fort", "plain-fort", "sfi-fort-e33e2d5"}},
		{line: []string{"preview", ""}, want: []string{"cloud-fort", "lab-fort", "plain-fort", "sfi-fort-e33e2d5"}},

		// A [libvirt] manifest with no stack yet. lab-fort has one.
		{line: []string{"adopt", ""}, want: []string{"plain-fort", "sfi-fort-e33e2d5"}},

		// The inverse of the manifest list: a stack whose manifest is gone.
		{line: []string{"destroy", ""}, want: []string{"gone-fort"}},

		// The mesh, not the manifests: macmini has no manifest and never will.
		{line: []string{"wg-conf", ""}, want: []string{"lab-fort", "macmini", "steven-mac"}},

		// The forts with a [cluster] table: a plain fort has no fleet surface.
		// The OCI ref comes from a registry, not from the tree.
		{line: []string{"add", "cloud-fort", ""}},

		// The fort's app dirs, in the short form seed accepts; gateway/
		// ships no contract and is not one.

		// Repeatable, and never twice.
		{line: []string{"check", "lab-fort", ""}, want: []string{"cloud-fort", "metal-fort", "plain-fort", "sfi-fort-e33e2d5"}},

		// The link verbs (#55): the sub-verb word, the fort, and a URL
		// nothing in the tree can offer.
		{line: []string{"link", ""}, want: []string{"add"}},
		{line: []string{"link", "add", ""}, want: []string{"cloud-fort", "lab-fort", "metal-fort", "plain-fort", "sfi-fort-e33e2d5"}},
		{line: []string{"link", "add", "lab-fort", ""}},
		{line: []string{"link", "add", "lab-fort", "ssh://x/y/z", "--"}, want: []string{"--address", "--port", "--write"}},
		{line: []string{"estate", ""}, want: []string{"link"}},
		{line: []string{"estate", "link", ""}},

		// Flag names, and the values that are closed.
		{line: []string{"destroy", "gone-fort", "-"}, want: []string{"--no-stack", "--yes"}},
		{line: []string{"init", "--hub", ""}}, // a role is a name the estate chooses (forts#105): nothing to offer

		// The values the shell completes, and the ones only the operator knows.
		{line: []string{"image", "lab-fort", "--from", ""}, directive: dirFiles},
	} {
		t.Run(strings.Join(tc.line, " "), func(t *testing.T) {
			want, directive := tc.want, tc.directive
			if directive == "" {
				directive = dirPlain
			}
			got, gotDirective := completeWords(len(tc.line)-1, tc.line)
			if gotDirective != directive {
				t.Errorf("directive = %q, want %q", gotDirective, directive)
			}
			if !slices.Equal(got, want) {
				t.Errorf("candidates =\n  %q\nwant\n  %q", got, want)
			}
		})
	}
}

// The drift guard, half one (#113): the completion table's verb set is
// main's switch, exactly. A verb added to one and not the other is a verb
// that completes nothing or completes a case that does not exist, and
// neither shows up in a run — only here.
func TestCompletionTableIsTheSwitch(t *testing.T) {
	verbs := switchVerbs(t)
	// The hidden verb is in the switch and not in the table on purpose:
	// a script types it, an operator never does.
	verbs = slices.DeleteFunc(verbs, func(v string) bool { return v == completeVerb })
	if table := verbNames(); !slices.Equal(table, verbs) {
		t.Errorf("the completion table and main's switch disagree:\n  table  %q\n  switch %q", table, verbs)
	}
}

// The drift guard, half two: every flag a verb registers on its
// flag.FlagSet is in the table, and the table names no flag the verb does
// not register. The FlagSet's name is the verb's own in every verb that
// has one, which is what ties the two halves together.
func TestCompletionFlagsAreTheFlagSets(t *testing.T) {
	registered := flagSetFlags(t)
	for verb, spec := range completions {
		got, ok := registered[verb]
		if !ok {
			// scaffold and wg-conf parse their arguments by hand. Keeping
			// the list here, rather than skipping silently, means the day
			// one of them takes a flag.FlagSet the test starts checking it
			// and this line fails until it is removed.
			if slices.Contains(handParsedVerbs, verb) {
				continue
			}
			t.Errorf("%s: no flag.NewFlagSet(%q) in cmd/fort — add it to handParsedVerbs if the verb parses by hand", verb, verb)
			continue
		}
		want := make([]string, 0, len(spec.flags))
		for _, f := range spec.flags {
			want = append(want, f.name)
		}
		sort.Strings(want)
		sort.Strings(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s flags:\n  table    %q\n  FlagSet  %q", verb, want, got)
		}
	}
	for _, verb := range handParsedVerbs {
		if _, ok := registered[verb]; ok {
			t.Errorf("%s has a flag.FlagSet now — drop it from handParsedVerbs so its flags are checked", verb)
		}
	}
}

// handParsedVerbs read their arguments without a flag.FlagSet, so the AST
// has nothing to compare their flag lists against.
var handParsedVerbs = []string{"scaffold", "wg-conf"}

// switchVerbs reads the verbs out of main's dispatch switch, sorted.
func switchVerbs(t *testing.T) []string {
	t.Helper()
	var verbs []string
	for _, decl := range parseFortPkg(t)["main.go"].Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "main" {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			for _, stmt := range sw.Body.List {
				for _, expr := range stmt.(*ast.CaseClause).List {
					if v, ok := stringLit(expr); ok {
						verbs = append(verbs, v)
					}
				}
			}
			return true
		})
	}
	if len(verbs) == 0 {
		t.Fatal("no verbs in main's switch — the parse found nothing to guard")
	}
	sort.Strings(verbs)
	return verbs
}

// flagSetFlags reads every flag.NewFlagSet("<verb>", …) in the package
// and the flag names registered on it, keyed by the FlagSet's name. The
// FlagSet lives in the function that parses the verb's arguments, which
// is not always the verb's own (b2-key parses in parseB2KeyArgs), so the
// name on the FlagSet is the key, never the function's.
func flagSetFlags(t *testing.T) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for name, file := range parseFortPkg(t) {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// The receiver variable of each FlagSet in this function,
			// by the verb it is named for: `fs := flag.NewFlagSet("seed", …)`.
			verbOf := map[string]string{}
			ast.Inspect(fn, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
					return true
				}
				recv, ok := as.Lhs[0].(*ast.Ident)
				if !ok {
					return true
				}
				if verb, ok := newFlagSetName(as.Rhs[0]); ok {
					verbOf[recv.Name] = verb
					out[verb] = nil
				}
				return true
			})
			if len(verbOf) == 0 {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				recv, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				verb, ok := verbOf[recv.Name]
				if !ok {
					return true
				}
				// fs.Bool("all", …) names the flag first; fs.BoolVar(&v,
				// "all", …) and fl.Var(&v, "hub", …) name it second.
				arg := 0
				if strings.HasSuffix(sel.Sel.Name, "Var") {
					arg = 1
				}
				if len(call.Args) <= arg {
					return true
				}
				if name, ok := stringLit(call.Args[arg]); ok {
					out[verb] = append(out[verb], name)
				}
				return true
			})
		}
	}
	return out
}

// newFlagSetName is the verb a flag.NewFlagSet call names, if that is
// what the expression is.
func newFlagSetName(expr ast.Expr) (string, bool) {
	call, ok := expr.(*ast.CallExpr)
	if !ok || len(call.Args) == 0 {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "NewFlagSet" {
		return "", false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "flag" {
		return "", false
	}
	return stringLit(call.Args[0])
}

func stringLit(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// parseFortPkg parses cmd/fort, by the source file this test lives in —
// not by the working directory, which a test before this one may have
// changed.
func parseFortPkg(t *testing.T) map[string]*ast.File {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller: no source path for this test")
	}
	pkgs, err := parser.ParseDir(token.NewFileSet(), filepath.Dir(self), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	pkg, ok := pkgs["main"]
	if !ok {
		t.Fatalf("no package main in %s", filepath.Dir(self))
	}
	files := map[string]*ast.File{}
	for path, f := range pkg.Files {
		files[filepath.Base(path)] = f
	}
	return files
}

// completionEstate lays down the estate the candidate sets read: the five
// manifest shapes from testdata, a mesh whose members are not the
// manifests, two stacks (one declared, one not), and one fort's apps
// tree. It is a git checkout because repoRoot walks up to a .git.
func completionEstate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mkdir := func(rel string) string {
		dir := filepath.Join(root, rel)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	write := func(rel, content string) {
		mkdir(filepath.Dir(rel))
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mkdir(".git")

	_, self, _, _ := runtime.Caller(0)
	fixtures := filepath.Join(filepath.Dir(self), "testdata", "forts")
	entries, err := os.ReadDir(fixtures)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(fixtures, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		write(filepath.Join("forts", e.Name()), string(raw))
	}

	// The mesh is its own list: lab-fort is a peer AND a manifest,
	// macmini a peer with no manifest, steven-mac a roaming client, and
	// the remote belongs to another estate (wg-conf renders none).
	write("mesh/peers.json", `{
  "peers": {
    "lab-fort": {"wg_vpn_ip": "10.1.0.99", "public_key": "bGFiLWZvcnQtZml4dHVyZS1wdWJsaWMta2V5PT09PT09PT0="},
    "macmini":  {"wg_vpn_ip": "10.1.0.3",  "public_key": "bWFjbWluaS1maXh0dXJlLXB1YmxpYy1rZXk9PT09PT09PQ=="}
  },
  "clients": {
    "steven-mac": {"wg_vpn_ip": "10.1.0.11", "public_key": "c3RldmVuLW1hYy1maXh0dXJlLXB1YmxpYy1rZXk9PT09PQ=="}
  },
  "remotes": {
    "forge": {"wg_vpn_ip": "10.3.0.4", "wg_endpoint_ip": "10.0.0.4", "public_key": "Zm9yZ2UtZml4dHVyZS1wdWJsaWMta2V5PT09PT09PT09PT0="}
  }
}
`)

	// Two stacks: lab-fort is declared (so adopt has nothing to do with
	// it), gone-fort is not (so destroy is what it is waiting for).
	for _, stack := range []string{"lab-fort", "gone-fort"} {
		write(filepath.Join(pulumiProject, "state/.pulumi/stacks/forts", stack+".json"), "{}\n")
	}

	// One fort's apps tree: an app with a contract, a shared provider
	// instance, and a dir that is neither.
	contract := "contract: v1\nsecret:\n  keys:\n    TOKEN: {generate: b64_32}\n"
	write("flux/apps/cloud-fort/symbology/forts.contract.yaml", contract)
	write("flux/apps/cloud-fort/forts/postgres/forts.contract.yaml", contract)
	mkdir("flux/apps/cloud-fort/gateway")
	return root
}
