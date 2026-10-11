package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/tools"
	"gopkg.in/yaml.v3"
)

// crd-diff is the detector of sfi/forts#117: CI reads the chart, not the
// release notes.
//
// A chart delivers a CRD in one of three shapes (sfi/knowledge
// config-management/crd-schema-ownership.md), and one of them leaves the
// release lifecycle: a file under a chart's own crds/ directory is
// install-only by Helm's design, and the helm-controller defaults
// spec.upgrade.crds to Skip, which neither replaces an existing CRD nor
// creates a new one. So a chart bump can move the schema that validates
// every custom object of a kind in the cluster, and the only place that
// fact appears is one line in the two hundred lines of upstream release
// notes Renovate pastes into the PR body. Nobody reads that line.
//
// The comparison is two helm pulls, so CI does it in seconds: for every
// HelmRelease whose chart version differs between a base revision and
// this tree, pull both versions and compare their CRD files — every
// file that differs, every file the new version adds, every file it
// removes — then print the diff.
//
// sfi/forts#116 decided the ownership half: every HelmRelease whose
// chart ships crds/ sets install.crds and upgrade.crds to
// CreateReplace. This verb reads those two keys, because they decide
// whether the schema change it found ever reaches a cluster. A changed
// crds/ under the default policy is an error; the same change under
// CreateReplace is a warning the reviewer must read.
func crdDiff(args []string) error {
	fs := flag.NewFlagSet("crd-diff", flag.ContinueOnError)
	base := fs.String("base", "origin/main", "the revision that holds the OLD chart versions")
	cache := fs.String("chart-cache", "", "read <dir>/<version>/<chart> instead of pulling (tests)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	git, err := tools.Runner("git")
	if err != nil {
		return err
	}
	releases, repos, err := readHelmTree(root)
	if err != nil {
		return err
	}
	bumps, err := chartBumps(root, git, *base, releases, repos)
	if err != nil {
		return err
	}
	o := crdOpts{cache: *cache, git: git, annotate: os.Getenv("GITEA_ACTIONS") != "" || os.Getenv("GITHUB_ACTIONS") != ""}
	if *cache == "" && len(bumps) > 0 {
		if o.helm, err = tools.Runner("helm"); err != nil {
			return err
		}
	}
	fmt.Printf("== chart versions that moved against %s\n", *base)
	if len(bumps) == 0 {
		fmt.Printf("  none of the %d HelmRelease(s) in this tree names a different chart version — nothing to compare\n", len(releases))
		return nil
	}
	var findings []finding
	for _, b := range bumps {
		f, err := reportBump(os.Stdout, o, b)
		if err != nil {
			return err
		}
		findings = append(findings, f...)
	}
	if n := reportFindings(os.Stdout, "chart CRDs", findings, o.annotate); n > 0 {
		return fmt.Errorf("%d error(s)", n)
	}
	return nil
}

type crdOpts struct {
	cache     string
	git, helm func(args ...string) *exec.Cmd
	annotate  bool
}

// helmRelease is one HelmRelease's chart pin as the tree states it. Only
// the spec.chart form is here: a chartRef release pins an OCIRepository,
// which is this estate's own chart (the shape fort add renders), and an
// SFI chart ships no CRD.
type helmRelease struct {
	path             string // repo-relative path of the file that holds it
	name, namespace  string
	chart, version   string
	srcName, srcNS   string
	install, upgrade string // spec.install.crds, spec.upgrade.crds ("" = the API default)
}

// id identifies the HelmRelease inside its file: one file can hold
// several, so the base revision's version comes from the document with
// this id, never from the first document that parses.
func (r helmRelease) id() string { return r.namespace + "/" + r.name }

// helmRepo is a HelmRepository the releases point at.
type helmRepo struct {
	url string
	oci bool
}

// releaseDoc and repoDoc are the two kinds crd-diff reads out of the
// tree's YAML. Both carry kind, because a file is a stream of documents.
type releaseDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		Chart struct {
			Spec struct {
				Chart     string `yaml:"chart"`
				Version   string `yaml:"version"`
				SourceRef struct {
					Kind      string `yaml:"kind"`
					Name      string `yaml:"name"`
					Namespace string `yaml:"namespace"`
				} `yaml:"sourceRef"`
			} `yaml:"spec"`
		} `yaml:"chart"`
		Install struct {
			CRDs string `yaml:"crds"`
		} `yaml:"install"`
		Upgrade struct {
			CRDs string `yaml:"crds"`
		} `yaml:"upgrade"`
	} `yaml:"spec"`
}

type repoDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
	Spec struct {
		URL  string `yaml:"url"`
		Type string `yaml:"type"`
	} `yaml:"spec"`
}

// readHelmTree walks the checkout once and collects every HelmRelease
// that pins a chart version, and every HelmRepository they can point
// at. It walks the whole tree on purpose: the framework holds its
// charts under cmd/fort/templates, an estate under flux/, and the next
// chart to move its CRDs into crds/ gives no warning — so no list of
// known directories can be the scope. .git holds no YAML the tree
// owns; testdata is the CLI's own golden fixtures, where a chart
// version is a test input, not a pin a cluster follows.
func readHelmTree(root string) (map[string]helmRelease, map[string]helmRepo, error) {
	releases := map[string]helmRelease{}
	repos := map[string]helmRepo{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata":
				return fs.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".yaml", ".yml":
		default:
			return nil // a .gotpl is Go template text, not YAML
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Contains(raw, []byte("HelmRelease")) && !bytes.Contains(raw, []byte("HelmRepository")) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		rs, hr := parseHelmDocs(rel, raw)
		for _, r := range rs {
			releases[rel+" "+r.id()] = r
		}
		for k, v := range hr {
			repos[k] = v
		}
		return nil
	})
	return releases, repos, err
}

// parseHelmDocs reads one file's documents. A document that does not
// parse is skipped, not an error: the tree carries YAML this verb has
// no claim on, and `fort check` is the gate that judges a malformed
// manifest.
func parseHelmDocs(rel string, raw []byte) ([]helmRelease, map[string]helmRepo) {
	var releases []helmRelease
	repos := map[string]helmRepo{}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var node yaml.Node
		if err := dec.Decode(&node); err != nil {
			return releases, repos
		}
		var kind struct {
			Kind string `yaml:"kind"`
		}
		if node.Decode(&kind) != nil {
			continue
		}
		switch kind.Kind {
		case "HelmRelease":
			var doc releaseDoc
			if node.Decode(&doc) != nil {
				continue
			}
			cs := doc.Spec.Chart.Spec
			if cs.Chart == "" || cs.Version == "" || cs.SourceRef.Kind != "HelmRepository" {
				continue
			}
			ns := cs.SourceRef.Namespace
			if ns == "" {
				ns = doc.Metadata.Namespace
			}
			releases = append(releases, helmRelease{
				path: rel, name: doc.Metadata.Name, namespace: doc.Metadata.Namespace,
				chart: cs.Chart, version: cs.Version,
				srcName: cs.SourceRef.Name, srcNS: ns,
				install: doc.Spec.Install.CRDs, upgrade: doc.Spec.Upgrade.CRDs,
			})
		case "HelmRepository":
			var doc repoDoc
			if node.Decode(&doc) != nil || doc.Spec.URL == "" {
				continue
			}
			repos[doc.Metadata.Namespace+"/"+doc.Metadata.Name] = helmRepo{
				url: doc.Spec.URL,
				oci: doc.Spec.Type == "oci" || strings.HasPrefix(doc.Spec.URL, "oci://"),
			}
		}
	}
}

// chartBump is one HelmRelease whose chart version moved. old is empty
// when the base revision does not carry the release at all.
type chartBump struct {
	rel      helmRelease
	repo     helmRepo
	old, new string
}

func (b chartBump) isNew() bool { return b.old == "" }

// label names the version pair in one phrase, for a heading and for a
// finding.
func (b chartBump) label() string {
	if b.isNew() {
		return b.rel.chart + " " + b.new + " (a release the base revision does not have)"
	}
	return b.rel.chart + " " + b.old + " → " + b.new
}

// chartBumps reads the same releases out of the base revision and keeps
// the ones whose version differs. The base is a revision, not the
// merge base of the pull request: the question a reviewer asks of a
// bump branch is what the chart version does against main, and that
// answer needs one commit, which a depth-1 fetch of the base branch
// already gives.
func chartBumps(root string, git func(...string) *exec.Cmd, base string, releases map[string]helmRelease, repos map[string]helmRepo) ([]chartBump, error) {
	if len(releases) == 0 {
		return nil, nil
	}
	rev := git("rev-parse", "--verify", "--quiet", base+"^{commit}")
	rev.Dir = root
	if err := rev.Run(); err != nil {
		return nil, fmt.Errorf("%s is not a commit in this checkout — fetch the base branch first (git fetch --depth=1 origin main), or name another revision with --base", base)
	}
	versions := map[string]map[string]string{} // file path → release id → version
	var out []chartBump
	for _, key := range sortedKeys(releases) {
		r := releases[key]
		if _, read := versions[r.path]; !read {
			versions[r.path] = map[string]string{}
			show := git("show", base+":"+r.path)
			show.Dir = root
			if raw, err := show.Output(); err == nil {
				rs, _ := parseHelmDocs(r.path, raw)
				for _, old := range rs {
					versions[r.path][old.id()] = old.version
				}
			}
		}
		old := versions[r.path][r.id()]
		if old == r.version {
			continue
		}
		repo, ok := repos[r.srcNS+"/"+r.srcName]
		if !ok {
			return nil, fmt.Errorf("%s: HelmRelease %s names sourceRef %s/%s, and no HelmRepository in this tree carries that name", r.path, r.id(), r.srcNS, r.srcName)
		}
		out = append(out, chartBump{rel: r, repo: repo, old: old, new: r.version})
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// crdPair is the two chart roots to compare, as paths under one base
// directory, so the diff git prints reads as old/… → new/….
type crdPair struct {
	base             string
	oldRoot, newRoot string
}

// crdDiffLines caps the diff of one bump. The traefik 41.0.2 → 41.6.0
// bump is 456 lines, which a reviewer reads; a chart that regenerates
// every CRD is tens of thousands, which fills a job log and says
// nothing the file list above it does not.
const crdDiffLines = 1200

// reportBump compares one bump's CRD files, prints what it found and
// the diff, and returns the verdict as findings.
func reportBump(w io.Writer, o crdOpts, b chartBump) ([]finding, error) {
	fmt.Fprintf(w, "  %s  (%s)\n", b.label(), b.rel.path)
	pair, cleanup, err := chartRoots(o, b)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		return []finding{{"error", b.rel.path, b.label() + ": the chart could not be read, so the CRD comparison did not run: " + err.Error()}}, nil
	}
	cmp, err := compareCRDs(pair)
	if err != nil {
		return nil, err
	}
	if cmp.empty() {
		fmt.Fprintf(w, "    no crds/ directory on either version — this chart's CRDs, if it has any, are templates of the release, which an upgrade applies\n")
		return nil, nil
	}
	fmt.Fprintf(w, "    CRD files: %d → %d; %d differ, %d added, %d removed\n",
		cmp.oldN, cmp.newN, len(cmp.changed), len(cmp.added), len(cmp.removed))
	for _, group := range []struct {
		mark  string
		files []crdFile
	}{{"~", cmp.changed}, {"+", cmp.added}, {"-", cmp.removed}} {
		for _, f := range group.files {
			fmt.Fprintf(w, "      %s %-56s %s\n", group.mark, f.name, f.delta)
		}
	}
	// A new release has no old version to diff against, so the list
	// above is the whole story and the diff would be the chart's every
	// CRD line.
	if !cmp.unchanged() && !b.isNew() {
		diff, err := crdDiffText(o, pair, cmp.dirs)
		if err != nil {
			return nil, err
		}
		fmt.Fprintln(w)
		fmt.Fprint(w, indent(capLines(diff, crdDiffLines, b), "    "))
		fmt.Fprintln(w)
	}
	return crdVerdicts(b, cmp), nil
}

// capLines keeps a long diff out of the job log and says how to get the
// rest, with the two commands the knowledge base's detection recipe
// names.
func capLines(diff string, max int, b chartBump) string {
	lines := strings.Split(strings.TrimRight(diff, "\n"), "\n")
	if len(lines) <= max {
		return diff
	}
	return strings.Join(lines[:max], "\n") + fmt.Sprintf(`

[the diff is %d lines; the first %d are above. Read all of it with:
   helm pull %s --repo %s --version %s --untar --untardir old
   helm pull %s --repo %s --version %s --untar --untardir new
   diff -r old/%s/crds new/%s/crds]
`, len(lines), max,
		b.rel.chart, b.repo.url, b.old,
		b.rel.chart, b.repo.url, b.new,
		b.rel.chart, b.rel.chart)
}

// crdVerdicts judges the change by WHERE the CRD files are, because the
// two places have two different owners.
//
// The chart's own crds/ is the release's: the two keys of sfi/forts#116
// decide whether the new schema reaches a cluster, and a change under
// the API default is an error — the cluster keeps the old schema, and
// the gap stays silent until a later manifest needs the new field.
//
// A subchart's crds/ is the subchart's condition: the fleet disables
// every CRD-bearing subchart it pins (kube-prometheus-stack's crds,
// loki's and thanos's operators), so those files install nothing and
// no policy changes that. The finding is a warning on purpose. It is
// still a finding, because the day somebody enables the subchart, the
// cluster inherits install-only CRDs that nobody read.
func crdVerdicts(b chartBump, cmp crdCompare) []finding {
	own, sub := cmp.partition()
	var out []finding
	switch {
	case !own.unchanged():
		out = append(out, crdOwnVerdict(b, own))
	case cmp.ownN > 0 && b.rel.upgrade != "CreateReplace":
		// This bump moved no CRD, so it is safe, and the schema on the
		// cluster still matches. The policy is the one that is not: the
		// next bump that DOES move a CRD lands nothing, and this is the
		// cheapest moment to say so, because the verb already knows the
		// chart ships an install-only directory.
		out = append(out, finding{"warning", b.rel.path, fmt.Sprintf(
			"%s leaves the chart's %d install-only CRD file(s) unchanged, so this bump moves no schema. spec.upgrade.crds is %s, so the next bump that DOES move one changes nothing on a cluster. Set install.crds and upgrade.crds to CreateReplace on this HelmRelease (sfi/forts#116)",
			b.label(), cmp.ownN, policyWord(b.rel.upgrade))})
	}
	if !sub.unchanged() {
		out = append(out, finding{"warning", b.rel.path, fmt.Sprintf(
			"%s moves CRD files in a SUBCHART's crds/ (%d changed, %d added, %d removed). A subchart that the values disable installs no CRD, and no CRDsPolicy changes that; enable the subchart and the cluster inherits these files install-only. Read the diff above before you enable it",
			b.label(), len(sub.changed), len(sub.added), len(sub.removed))})
	}
	return out
}

func crdOwnVerdict(b chartBump, own crdCompare) finding {
	moved := fmt.Sprintf("%s moves the chart's own crds/ directory (%d changed, %d added, %d removed)",
		b.label(), len(own.changed), len(own.added), len(own.removed))
	// A first install is not the failure case: spec.install.crds
	// defaults to Create, so flux does install these CRDs. The next
	// bump is the one that needs the policy.
	if b.isNew() {
		if b.rel.upgrade == "CreateReplace" {
			return finding{"warning", b.rel.path, moved + ": flux installs them at the first reconcile, and spec.upgrade.crds is CreateReplace, so a later chart bump moves them too"}
		}
		return finding{"error", b.rel.path, moved + ": flux installs them at the first reconcile (spec.install.crds defaults to Create), but spec.upgrade.crds is " + policyWord(b.rel.upgrade) + ", so no later chart bump moves them. Set install.crds and upgrade.crds to CreateReplace on this HelmRelease (sfi/forts#116)"}
	}
	if b.rel.upgrade == "CreateReplace" {
		return finding{"warning", b.rel.path, moved + ": spec.upgrade.crds is CreateReplace, so flux applies every one of them. Read the diff above — a CRD change is a schema change for every object of that kind"}
	}
	return finding{"error", b.rel.path, moved + ": spec.upgrade.crds is " + policyWord(b.rel.upgrade) + ", so the cluster keeps the old schema and the gap stays silent until a manifest needs the new field. Set install.crds and upgrade.crds to CreateReplace on this HelmRelease (sfi/forts#116)"}
}

func policyWord(policy string) string {
	if policy == "" {
		return "unset, which the helm-controller reads as Skip"
	}
	return policy
}

// chartRoots puts both versions of the chart on disk and returns their
// directories.
func chartRoots(o crdOpts, b chartBump) (crdPair, func(), error) {
	if o.cache != "" {
		return crdPair{base: o.cache, oldRoot: filepath.Join(versionDir(b.old), b.rel.chart), newRoot: filepath.Join(versionDir(b.new), b.rel.chart)}, nil, nil
	}
	tmp, err := os.MkdirTemp("", "fort-crd-diff-")
	if err != nil {
		return crdPair{}, nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	p := crdPair{base: tmp, oldRoot: filepath.Join("old", b.rel.chart), newRoot: filepath.Join("new", b.rel.chart)}
	if !b.isNew() {
		if err := pullChart(o, tmp, "old", b, b.old); err != nil {
			return p, cleanup, err
		}
	}
	if err := pullChart(o, tmp, "new", b, b.new); err != nil {
		return p, cleanup, err
	}
	return p, cleanup, nil
}

// absentVersion stands where a version is not there — a release the base
// revision does not carry. It is a name, not an empty string, because
// filepath.Join drops an empty segment: the old chart root would
// collapse onto the cache root, and the walk would read the whole cache
// as one enormous chart.
const absentVersion = "(absent)"

func versionDir(v string) string {
	if v == "" {
		return absentVersion
	}
	return v
}

// pullChart is the one network call of the verb: helm against the
// HelmRepository the release names. No --ca-file is passed — every
// HelmRepository in the fleet is a public upstream, and a CA bundle
// given here REPLACES the host's trust store and breaks those pulls. A
// HelmRepository on the forge would need one, and when the first
// appears, the x509 error names itself.
func pullChart(o crdOpts, into, label string, b chartBump, version string) error {
	args := []string{"pull"}
	if b.repo.oci {
		args = append(args, strings.TrimSuffix(b.repo.url, "/")+"/"+b.rel.chart)
	} else {
		args = append(args, b.rel.chart, "--repo", b.repo.url)
	}
	args = append(args, "--version", version, "--untar", "--untardir", filepath.Join(into, label))
	out, err := o.helm(args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("helm pull %s@%s from %s: %s", b.rel.chart, version, b.repo.url, strings.TrimSpace(string(out)))
	}
	return nil
}

// crdFile is one CRD file of the comparison, named by its path inside
// the chart, and how many lines moved in it.
type crdFile struct{ name, delta string }

type crdCompare struct {
	oldN, newN              int
	ownN, subN              int // the NEW version's files, by owner: the chart's own crds/, a subchart's
	changed, added, removed []crdFile
	dirs                    []string // every crds/ directory either version has, relative to the chart root
}

func (c crdCompare) empty() bool     { return c.oldN == 0 && c.newN == 0 }
func (c crdCompare) unchanged() bool { return len(c.changed)+len(c.added)+len(c.removed) == 0 }

// partition splits the moved files by owner: the chart's own crds/, and
// the crds/ of a subchart under charts/. The counts carry over
// unsplit — they describe the chart, and the verdicts read the file
// lists.
func (c crdCompare) partition() (own, sub crdCompare) {
	own, sub = crdCompare{oldN: c.oldN, newN: c.newN}, crdCompare{oldN: c.oldN, newN: c.newN}
	for _, g := range []struct {
		files        []crdFile
		ownTo, subTo *[]crdFile
	}{
		{c.changed, &own.changed, &sub.changed},
		{c.added, &own.added, &sub.added},
		{c.removed, &own.removed, &sub.removed},
	} {
		for _, f := range g.files {
			if strings.HasPrefix(f.name, "charts/") {
				*g.subTo = append(*g.subTo, f)
			} else {
				*g.ownTo = append(*g.ownTo, f)
			}
		}
	}
	return own, sub
}

// compareCRDs is the comparison itself: the CRD file sets of both chart
// versions, and a byte comparison of the files both carry. The sets are
// read from the two trees rather than parsed out of a diff, so "added"
// and "removed" are facts about the two charts and not a guess at git's
// rename detection. An ADDED file is reported as loudly as a changed
// one: on an upgrade the default policy installs a new CRD no more than
// it replaces an existing one, so a chart that adds a CRD adds nothing
// to the cluster.
func compareCRDs(p crdPair) (crdCompare, error) {
	oldFiles, oldDirs, err := crdFileSet(filepath.Join(p.base, p.oldRoot))
	if err != nil {
		return crdCompare{}, err
	}
	newFiles, newDirs, err := crdFileSet(filepath.Join(p.base, p.newRoot))
	if err != nil {
		return crdCompare{}, err
	}
	cmp := crdCompare{oldN: len(oldFiles), newN: len(newFiles)}
	for d := range newDirs {
		oldDirs[d] = true
	}
	cmp.dirs = sortedKeys(oldDirs)
	for _, name := range sortedKeys(oldFiles) {
		switch nf, ok := newFiles[name]; {
		case !ok:
			cmp.removed = append(cmp.removed, crdFile{name, "-" + strconv.Itoa(countLines(oldFiles[name]))})
		case !bytes.Equal(oldFiles[name], nf):
			cmp.changed = append(cmp.changed, crdFile{name, lineDelta(oldFiles[name], nf)})
		}
	}
	for _, name := range sortedKeys(newFiles) {
		if _, ok := oldFiles[name]; !ok {
			cmp.added = append(cmp.added, crdFile{name, "+" + strconv.Itoa(countLines(newFiles[name]))})
		}
		if strings.HasPrefix(name, "charts/") {
			cmp.subN++
		} else {
			cmp.ownN++
		}
	}
	return cmp, nil
}

// crdFileSet reads every CRD file of one unpacked chart: the files
// under its own crds/ directory, and the files under the crds/
// directory of every subchart in charts/. The subcharts are here
// because helm collects their crds/ too, and a condition-disabled
// subchart is exactly where a CRD-bearing chart hides (loki and thanos
// both carry one). The key is the path inside the chart, which says
// which chart the file came from. Returns the files and the set of
// crds/ directories, as paths relative to the chart root.
func crdFileSet(chart string) (map[string][]byte, map[string]bool, error) {
	files := map[string][]byte{}
	dirs := map[string]bool{}
	err := filepath.WalkDir(chart, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil // the chart is absent (a new release) or has no CRD
			}
			return err
		}
		rel, err := filepath.Rel(chart, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if isCRDsDir(rel) {
				dirs[rel] = true
			}
			return nil
		}
		if !inCRDsDir(rel) {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files[rel] = raw
		return nil
	})
	return files, dirs, err
}

// isCRDsDir: the directory is a chart's install-only CRD directory.
// The shape must be exact — crds at a chart root, where a chart root is
// the top of the archive or charts/<subchart> inside it, repeated.
// "Any directory called crds" is the wrong rule and it reads a safe
// chart as a dangerous one: kube-prometheus-stack carries a SUBCHART
// named crds, so its path is charts/crds/templates/, those CRDs are
// templates of the release, and an upgrade applies them.
func isCRDsDir(rel string) bool {
	parts := strings.Split(rel, "/")
	i := 0
	for i+1 < len(parts) && parts[i] == "charts" {
		i += 2
	}
	return i == len(parts)-1 && parts[i] == "crds"
}

// inCRDsDir: the file sits inside one of those directories, at any
// depth below it.
func inCRDsDir(rel string) bool {
	parts := strings.Split(rel, "/")
	for i := len(parts) - 1; i > 0; i-- {
		if isCRDsDir(strings.Join(parts[:i], "/")) {
			return true
		}
	}
	return false
}

func countLines(raw []byte) int {
	if len(raw) == 0 {
		return 0
	}
	return bytes.Count(bytes.TrimSuffix(raw, []byte("\n")), []byte("\n")) + 1
}

// lineDelta counts the lines each side has that the other does not, as
// a multiset — not an edit script. It is the triage number beside a
// file name ("one field added" against "every schema regenerated"); the
// diff below the list is the record.
func lineDelta(old, new []byte) string {
	count := func(raw []byte) map[string]int {
		m := map[string]int{}
		for _, l := range strings.Split(string(raw), "\n") {
			m[l]++
		}
		return m
	}
	o, n := count(old), count(new)
	plus, minus := 0, 0
	for l, c := range n {
		if d := c - o[l]; d > 0 {
			plus += d
		}
	}
	for l, c := range o {
		if d := c - n[l]; d > 0 {
			minus += d
		}
	}
	return "+" + strconv.Itoa(plus) + " -" + strconv.Itoa(minus)
}

// crdDiffText is the diff a reviewer reads, one crds/ directory at a
// time. git does it: it is already a declared tool, `--no-index`
// compares two directories outside any repository, and it reports an
// added and a removed file as well as a changed one. A directory one
// version does not have is created empty first, because `git diff
// --no-index` refuses a path that is not there. One line of context is
// enough — a CRD is deep YAML, and the property name a reader wants
// sits on the changed line itself.
func crdDiffText(o crdOpts, p crdPair, dirs []string) (string, error) {
	var b strings.Builder
	for _, d := range dirs {
		oldDir, newDir := filepath.Join(p.oldRoot, d), filepath.Join(p.newRoot, d)
		for _, rel := range []string{oldDir, newDir} {
			if err := os.MkdirAll(filepath.Join(p.base, rel), 0o755); err != nil {
				return "", err
			}
		}
		// --no-renames: the list above this diff is computed from the
		// two file sets, and git's rename detection would show one
		// added and one removed CRD as a single rename, which reads as
		// a different fact than the list states.
		cmd := o.git("diff", "--no-index", "--no-renames", "--unified=1", "--", oldDir, newDir)
		cmd.Dir = p.base
		out, err := cmd.Output()
		// git diff exits 1 when the paths differ, which is the point.
		var ee *exec.ExitError
		if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 1) {
			return "", fmt.Errorf("git diff --no-index %s %s: %s", oldDir, newDir, exitDetail(err))
		}
		b.Write(out)
	}
	return b.String(), nil
}
