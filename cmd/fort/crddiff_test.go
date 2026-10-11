package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/tools"
)

// isCRDsDir is the rule the whole verb turns on, and the one that is
// easy to get wrong: kube-prometheus-stack carries a SUBCHART named
// crds, whose CRDs are templates of the release that an upgrade
// applies, and reading charts/crds/templates/ as an install-only
// directory would report a safe chart as a dangerous one. The same
// chart also ships charts/crds/crds/, which IS install-only.
func TestCRDsDirShape(t *testing.T) {
	for _, tc := range []struct {
		path      string
		isDir     bool // the path names a crds/ directory
		holdsFile bool // a file at <path>/x.yaml is a CRD file
	}{
		{"crds", true, true},
		{"charts/kube-state-metrics/crds", true, true},
		{"charts/a/charts/b/crds", true, true},
		{"crds/extra", false, true}, // helm takes a crds/ file at any depth
		{"charts/crds/templates", false, false},
		{"charts/crds", false, false},
		{"templates", false, false},
		{"templates/crds", false, false},
	} {
		if got := isCRDsDir(tc.path); got != tc.isDir {
			t.Errorf("isCRDsDir(%q) = %v, want %v", tc.path, got, tc.isDir)
		}
		file := tc.path + "/x.yaml"
		if got := inCRDsDir(file); got != tc.holdsFile {
			t.Errorf("inCRDsDir(%q) = %v, want %v", file, got, tc.holdsFile)
		}
	}
	if inCRDsDir("Chart.yaml") {
		t.Error("inCRDsDir(Chart.yaml) is true — a file at the chart root is not a CRD")
	}
}

// parseHelmDocs reads the two kinds out of a stream of documents, and
// skips what it has no claim on: a chartRef release (an SFI chart,
// pinned by an OCIRepository), a release with no version, and a
// document that is not either kind.
func TestParseHelmDocs(t *testing.T) {
	const file = `apiVersion: v1
kind: Namespace
metadata: {name: traefik}
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata: {name: traefik, namespace: flux-system}
spec:
  url: https://traefik.github.io/charts
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: traefik, namespace: traefik}
spec:
  chart:
    spec:
      chart: traefik
      version: 41.6.0
      sourceRef: {kind: HelmRepository, name: traefik, namespace: flux-system}
  upgrade:
    crds: CreateReplace
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: symbology, namespace: symbology}
spec:
  chartRef: {kind: OCIRepository, name: symbology, namespace: flux-system}
`
	releases, repos := parseHelmDocs("flux/x.yaml", []byte(file))
	if len(releases) != 1 {
		t.Fatalf("got %d release(s), want 1 (the chartRef release has no chart version to compare)", len(releases))
	}
	r := releases[0]
	if r.id() != "traefik/traefik" || r.chart != "traefik" || r.version != "41.6.0" {
		t.Errorf("release = %+v", r)
	}
	if r.upgrade != "CreateReplace" || r.install != "" {
		t.Errorf("policies = %q / %q, want CreateReplace and the unset default", r.upgrade, r.install)
	}
	if r.srcNS != "flux-system" || r.srcName != "traefik" {
		t.Errorf("sourceRef = %s/%s", r.srcNS, r.srcName)
	}
	repo, ok := repos["flux-system/traefik"]
	if !ok || repo.url != "https://traefik.github.io/charts" || repo.oci {
		t.Errorf("repos = %+v", repos)
	}
}

// The sourceRef namespace defaults to the HelmRelease's own, as the
// flux API does, so a release that omits it still finds its repository.
func TestParseHelmDocsSourceRefNamespaceDefaults(t *testing.T) {
	releases, _ := parseHelmDocs("x.yaml", []byte(`apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: valkey, namespace: forts}
spec:
  chart:
    spec:
      chart: valkey
      version: 0.11.0
      sourceRef: {kind: HelmRepository, name: valkey}
`))
	if len(releases) != 1 || releases[0].srcNS != "forts" {
		t.Fatalf("releases = %+v, want one with srcNS forts", releases)
	}
}

// chartBumps reads the base revision out of git and keeps only the
// releases whose version moved. The fixture is a real one-commit
// repository, because the base is a revision and `git show <rev>:<path>`
// is how the verb reads it.
func TestChartBumps(t *testing.T) {
	git := gitRunner(t)
	root := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := git(args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	release := func(name, chart, version string) string {
		return `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: ` + name + `, namespace: infra}
spec:
  chart:
    spec:
      chart: ` + chart + `
      version: ` + version + `
      sourceRef: {kind: HelmRepository, name: up, namespace: infra}
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: HelmRepository
metadata: {name: up, namespace: infra}
spec:
  url: https://example.invalid/charts
`
	}
	writeFile(t, root, "flux/bumped.yaml", release("traefik", "traefik", "41.0.2"))
	writeFile(t, root, "flux/still.yaml", release("cert-manager", "cert-manager", "v1.21.2"))
	run("init", "--quiet", "-b", "main")
	run("-c", "user.email=t@t", "-c", "user.name=t", "add", ".")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "--quiet", "-m", "base")
	// The bump: one version moves, one file is new, one holds still.
	writeFile(t, root, "flux/bumped.yaml", release("traefik", "traefik", "41.6.0"))
	writeFile(t, root, "flux/fresh.yaml", release("loki", "loki", "7.3.0"))

	releases, repos, err := readHelmTree(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) != 3 {
		t.Fatalf("readHelmTree found %d release(s), want 3", len(releases))
	}
	bumps, err := chartBumps(root, git, "HEAD", releases, repos)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, b := range bumps {
		got[b.rel.chart] = b.old + "→" + b.new
	}
	want := map[string]string{"traefik": "41.0.2→41.6.0", "loki": "→7.3.0"}
	if len(got) != len(want) {
		t.Fatalf("bumps = %v, want %v (cert-manager held still)", got, want)
	}
	for chart, pair := range want {
		if got[chart] != pair {
			t.Errorf("%s = %q, want %q", chart, got[chart], pair)
		}
	}
	// A base that is not a commit names the fetch, rather than reporting
	// every release as new.
	if _, err := chartBumps(root, git, "origin/main", releases, repos); err == nil || !strings.Contains(err.Error(), "git fetch") {
		t.Errorf("a missing base revision: err = %v, want the fetch named", err)
	}
}

// A sourceRef that names no HelmRepository in the tree is a refusal,
// not a silent skip: the verb cannot pull the chart, and a detector
// that drops a finding quietly is the one failure it must not have.
func TestChartBumpsUnknownSource(t *testing.T) {
	git := gitRunner(t)
	root := t.TempDir()
	writeFile(t, root, "flux/r.yaml", `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: traefik, namespace: infra}
spec:
  chart:
    spec:
      chart: traefik
      version: 41.6.0
      sourceRef: {kind: HelmRepository, name: nowhere, namespace: infra}
`)
	for _, args := range [][]string{{"init", "--quiet", "-b", "main"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "add", "."},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "--quiet", "-m", "base"}} {
		cmd := git(args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	releases, repos, err := readHelmTree(root)
	if err != nil {
		t.Fatal(err)
	}
	// Move the version so the release becomes a bump.
	for k, r := range releases {
		r.version = "41.7.0"
		releases[k] = r
	}
	if _, err := chartBumps(root, git, "HEAD", releases, repos); err == nil || !strings.Contains(err.Error(), "no HelmRepository") {
		t.Errorf("err = %v, want the missing HelmRepository named", err)
	}
}

// The verdict, end to end over a fixture chart cache: the comparison,
// the printed report, the diff, and which of the two owners the finding
// belongs to. --chart-cache stands in for the two helm pulls, so the
// test needs no network.
func TestReportBumpVerdicts(t *testing.T) {
	cache := t.TempDir()
	const crd = `apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: middlewares.traefik.io
spec:
  group: traefik.io
`
	// traefik: one own CRD changed, one added, one removed.
	writeFile(t, cache, "old/traefik/crds/middlewares.yaml", crd)
	writeFile(t, cache, "old/traefik/crds/leaving.yaml", crd)
	writeFile(t, cache, "new/traefik/crds/middlewares.yaml", crd+"  errorRequestHeaders: true\n")
	writeFile(t, cache, "new/traefik/crds/arriving.yaml", crd)
	// stack: nothing of its own; a subchart's crds/ moves, and a
	// subchart NAMED crds holds templates, which must not count.
	writeFile(t, cache, "old/stack/charts/operator/crds/rules.yaml", crd)
	writeFile(t, cache, "new/stack/charts/operator/crds/rules.yaml", crd+"  extra: true\n")
	writeFile(t, cache, "old/stack/charts/crds/templates/rules.yaml", crd)
	writeFile(t, cache, "new/stack/charts/crds/templates/rules.yaml", crd+"  extra: true\n")
	// quiet: a chart with no CRD anywhere.
	writeFile(t, cache, "old/quiet/templates/deployment.yaml", "kind: Deployment\n")
	writeFile(t, cache, "new/quiet/templates/deployment.yaml", "kind: Deployment\n")
	// steady: install-only CRDs that this bump does not move.
	writeFile(t, cache, "old/steady/crds/middlewares.yaml", crd)
	writeFile(t, cache, "new/steady/crds/middlewares.yaml", crd)

	o := crdOpts{cache: cache, git: gitRunner(t)}
	bump := func(chart, upgrade string) chartBump {
		return chartBump{
			rel:  helmRelease{path: "flux/" + chart + ".yaml", chart: chart, upgrade: upgrade},
			repo: helmRepo{url: "https://example.invalid/charts"},
			old:  "old", new: "new",
		}
	}
	for _, tc := range []struct {
		name   string
		bump   chartBump
		levels []string
		want   []string
	}{{
		name:   "the default policy keeps the old schema",
		bump:   bump("traefik", ""),
		levels: []string{"error"},
		want: []string{"CRD files: 2 → 2; 1 differ, 1 added, 1 removed",
			"~ crds/middlewares.yaml", "+ crds/arriving.yaml", "- crds/leaving.yaml",
			"+  errorRequestHeaders: true",
			// --no-renames: an added and a removed CRD are two facts,
			// not one rename, however alike the two files are.
			"new file mode", "deleted file mode",
			"spec.upgrade.crds is unset, which the helm-controller reads as Skip",
			"CreateReplace"},
	}, {
		name:   "CreateReplace applies it, so the reviewer reads the diff",
		bump:   bump("traefik", "CreateReplace"),
		levels: []string{"warning"},
		want:   []string{"spec.upgrade.crds is CreateReplace, so flux applies every one of them"},
	}, {
		name:   "a subchart's crds/ is the subchart's condition, never an error",
		bump:   bump("stack", ""),
		levels: []string{"warning"},
		want: []string{"CRD files: 1 → 1; 1 differ, 0 added, 0 removed",
			"~ charts/operator/crds/rules.yaml", "SUBCHART"},
	}, {
		name:   "a chart with no crds/ passes and says so",
		bump:   bump("quiet", ""),
		levels: nil,
		want:   []string{"no crds/ directory on either version"},
	}, {
		// The bump moves no schema, so it is safe. The policy is what
		// is not: the next bump that does move one lands nothing.
		name:   "a quiet bump of an install-only chart still names the policy",
		bump:   bump("steady", ""),
		levels: []string{"warning"},
		want: []string{"CRD files: 1 → 1; 0 differ, 0 added, 0 removed",
			"leaves the chart's 1 install-only CRD file(s) unchanged",
			"the next bump that DOES move one changes nothing"},
	}, {
		name:   "the same quiet bump under CreateReplace says nothing",
		bump:   bump("steady", "CreateReplace"),
		levels: nil,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			findings, err := reportBump(&buf, o, tc.bump)
			if err != nil {
				t.Fatal(err)
			}
			var levels []string
			for _, f := range findings {
				levels = append(levels, f.level)
				buf.WriteString(f.msg)
			}
			if strings.Join(levels, ",") != strings.Join(tc.levels, ",") {
				t.Errorf("levels = %v, want %v", levels, tc.levels)
			}
			for _, want := range tc.want {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("the report does not carry %q\n%s", want, buf.String())
				}
			}
		})
	}
}

// A new release is not the failure case for install — spec.install.crds
// defaults to Create, so flux does install the CRDs — but it is the
// failure case for every bump after it, and the whole chart is not a
// diff worth printing.
func TestReportBumpNewRelease(t *testing.T) {
	cache := t.TempDir()
	writeFile(t, cache, "41.6.0/traefik/crds/middlewares.yaml", "kind: CustomResourceDefinition\n")
	o := crdOpts{cache: cache, git: gitRunner(t)}
	b := chartBump{rel: helmRelease{path: "flux/traefik.yaml", chart: "traefik"}, new: "41.6.0"}
	var buf bytes.Buffer
	findings, err := reportBump(&buf, o, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].level != "error" {
		t.Fatalf("findings = %+v, want one error", findings)
	}
	if !strings.Contains(findings[0].msg, "spec.install.crds defaults to Create") {
		t.Errorf("msg = %q", findings[0].msg)
	}
	if strings.Contains(buf.String(), "diff --git") {
		t.Errorf("a new release printed a diff of the whole chart:\n%s", buf.String())
	}
	b.rel.upgrade = "CreateReplace"
	findings, err = reportBump(&buf, o, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].level != "warning" {
		t.Fatalf("with CreateReplace: findings = %+v, want one warning", findings)
	}
	// The absent version is a NAME in the path. An empty segment would
	// make the old chart root the cache root itself, and the walk would
	// read every version in the cache as one chart.
	p, _, err := chartRoots(o, b)
	if err != nil {
		t.Fatal(err)
	}
	if p.oldRoot != filepath.Join(absentVersion, "traefik") {
		t.Errorf("oldRoot = %q, want the absent-version segment", p.oldRoot)
	}
}

// capLines keeps a chart that regenerates every CRD out of the job log,
// and names the two helm pulls that read the rest.
func TestCapLines(t *testing.T) {
	b := chartBump{rel: helmRelease{chart: "traefik"}, repo: helmRepo{url: "https://example.invalid/charts"}, old: "1.0.0", new: "2.0.0"}
	short := "a\nb\nc\n"
	if got := capLines(short, 3, b); got != short {
		t.Errorf("a diff at the cap was changed: %q", got)
	}
	got := capLines(strings.Repeat("line\n", 10), 4, b)
	if strings.Count(got, "line\n") != 4 {
		t.Errorf("want 4 kept lines, got:\n%s", got)
	}
	for _, want := range []string{"the diff is 10 lines", "helm pull traefik --repo https://example.invalid/charts --version 1.0.0", "--version 2.0.0"} {
		if !strings.Contains(got, want) {
			t.Errorf("the notice does not carry %q:\n%s", want, got)
		}
	}
}

func gitRunner(t *testing.T) func(...string) *exec.Cmd {
	t.Helper()
	git, err := tools.Runner("git")
	if err != nil {
		t.Skip(err)
	}
	return git
}

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
