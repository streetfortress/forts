package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The golden trees: what `fort add` renders for one chart on a fresh
// fort, as a reviewed snapshot under testdata/golden (forts#250).
// UPDATE_GOLDEN=1 rewrites a golden (then review the diff like any
// other generated code).

// A chart with every block the core reads — a generated key, an
// optional external key, http and metrics — on a cloud fort whose
// estate carries a CA: the gateway from template, the app dir with its
// route, its PodMonitor, the CA copy for the OCIRepository, the
// inventory and the ◆ Secret, and the hostname in the manifest's public
// zone.
func TestAddFreshCloudFort(t *testing.T) {
	testAddFresh(t, addFixture{fort: "cloud-fort", golden: "testdata/golden/fresh-cloud", chart: "sfi-website", version: "0.3.0", ca: true})
}

// The lab twin: no [dns] zone, so the hostname is in the estate's mesh
// zone; no CA, so no certSecretRef and no copy.
func TestAddFreshLabFort(t *testing.T) {
	testAddFresh(t, addFixture{fort: "lab-fort", golden: "testdata/golden/fresh-lab", chart: "sfi-website", version: "0.3.0"})
}

// The secret-less shape: moment-tally-website ships a contract with
// `http:` and nothing else, so add renders no Secret machinery — no
// secrets.sops.yaml in the app kustomization, no inventory, no
// existingSecret value, and no ◆ line.
func TestAddSecretlessChart(t *testing.T) {
	testAddFresh(t, addFixture{fort: "lab-fort", golden: "testdata/golden/secretless", chart: "moment-tally-website", version: "0.1.0"})
}

// The contract-less chart: `add` plans from the empty contract — no
// Secret and no route, because nothing declares an http service. The
// fort gets the app dir alone, and the report says what the missing
// contract costs.
func TestAddContractlessChart(t *testing.T) {
	testAddFresh(t, addFixture{
		fort: "lab-fort", golden: "testdata/golden/no-contract",
		chart: "moment-tally-website", chartDir: "testdata/charts/moment-tally-website-bare",
		version: "0.0.0-pr.48.108.g42d6bce",
	})
}

// --http gives a contract-less app its ingress: the same replay, plus
// the Service the chart exposes. It renders the gateway, the
// certificate and the route that a contract's `http` block would have.
func TestAddContractlessChartHTTP(t *testing.T) {
	testAddFresh(t, addFixture{
		fort: "lab-fort", golden: "testdata/golden/no-contract-http",
		chart: "moment-tally-website", chartDir: "testdata/charts/moment-tally-website-bare",
		version: "0.0.0-pr.48.108.g42d6bce", http: "moment-tally-website:3000",
	})
}

// A staging fort follows the chart repo's main branch: the tree is the
// secret-less one above, and only the pin differs. repository.yaml
// names no version — the semver filter picks the newest per-commit
// prerelease of main on every reconcile — and the report says which
// build the filter resolved to when the operator ran add.
func TestAddFollowMain(t *testing.T) {
	testAddFresh(t, addFixture{
		fort: "lab-fort", golden: "testdata/golden/follow-main",
		chart: "moment-tally-website", version: "0.0.0-main.7.g1492bd3",
		followMain: true,
	})
}

// A second app on a fort that has a gateway: the gateway dir is
// appended to (○ its kustomization: the certificate listed, the
// listener patch added inside the patches: list), the root
// kustomization gains the app alone. The golden is the whole tree after
// both adds, with the second add's report.
func TestAddSecondApp(t *testing.T) {
	testAddFresh(t, addFixture{
		fort: "lab-fort", golden: "testdata/golden/second-app",
		chart: "sfi-website", version: "0.3.0",
		then: &addFixture{chart: "moment-tally-website", version: "0.1.0"},
	})
}

// A chart that requires a platform service (forts#250): the core
// renders the app dir and the route, writes the DATABASE_URL template
// verbatim into the inventory, and says that a layer renders the
// postgres instance and rewrites that key. Nothing of the provider is
// rendered here.
func TestAddLayeredChart(t *testing.T) {
	testAddFresh(t, addFixture{fort: "lab-fort", golden: "testdata/golden/layered", chart: "gallery", version: "0.2.0"})
}

// An estate that names no [acme] email carries no ClusterIssuer: the
// certificate renders all the same (issuerRef letsencrypt, the name the
// shared layer uses when it exists), and the report says it waits.
func TestAddNoIssuer(t *testing.T) {
	testAddFresh(t, addFixture{fort: "lab-fort", golden: "testdata/golden/no-issuer", chart: "moment-tally-website", version: "0.1.0", noACME: true})
}

// addFixture is one replay of `fort add` on a fresh fort: the manifest
// fixture, the golden tree, the chart (its OCI ref and its pin), and
// the flags the operator typed.
type addFixture struct {
	fort, golden, chart, version string
	chartDir                     string // default: testdata/charts/<chart>
	http                         string // --http <service>:<port>
	followMain                   bool   // --follow-main
	ca                           bool   // the estate carries nixos/certs/ca-root.pem
	noACME                       bool   // the estate names no [acme] email
	// then: a second add on the same tree, whose report the golden
	// records (the app-on-existing-gateway path).
	then *addFixture
}

func testAddFresh(t *testing.T, f addFixture) {
	t.Helper()
	fort, golden, chart, version := f.fort, f.golden, f.chart, f.version
	chartDir := f.chartDir
	if chartDir == "" {
		chartDir = "testdata/charts/" + chart
	}
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, fort)
	if f.noACME {
		raw := readFile(t, "testdata/estate.toml")
		raw = raw[:strings.Index(raw, "[acme]")]
		writeFile(t, tmp, "estate.toml", raw)
	} else {
		copyEstate(t, tmp)
	}
	if f.ca {
		copyFile(t, "testdata/step-ca-root.pem", filepath.Join(tmp, estateCA))
	}
	copyFile(t, "testdata/trees/fresh/kustomization.yaml", filepath.Join(tmp, "flux/apps", fort, "kustomization.yaml"))

	p, err := planAdd(addOpts{
		root: tmp, fort: fort,
		ref:        "oci://gitea.zen.lofi/sfi/" + chart,
		version:    version,
		followMain: f.followMain,
		http:       f.http,
		chartDir:   chartDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.apply(); err != nil {
		t.Fatal(err)
	}
	if f.then != nil {
		p, err = planAdd(addOpts{
			root: tmp, fort: fort,
			ref:      "oci://gitea.zen.lofi/sfi/" + f.then.chart,
			version:  f.then.version,
			chartDir: "testdata/charts/" + f.then.chart,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := p.apply(); err != nil {
			t.Fatal(err)
		}
	}
	var report strings.Builder
	p.report(&report, true)

	got := map[string]string{}
	base := filepath.Join(tmp, "flux/apps", fort)
	filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(base, path)
			got[rel] = readFile(t, path)
		}
		return nil
	})
	got["REPORT.txt"] = report.String()

	if os.Getenv("UPDATE_GOLDEN") != "" {
		os.RemoveAll(golden)
		for rel, content := range got {
			dst := filepath.Join(golden, rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		t.Logf("golden rewritten: %s (%d files)", golden, len(got))
		return
	}
	want := map[string]string{}
	filepath.Walk(golden, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(golden, path)
			want[rel] = readFile(t, path)
		}
		return nil
	})
	if len(want) == 0 {
		t.Fatalf("no golden under %s — run with UPDATE_GOLDEN=1 and review", golden)
	}
	for rel, w := range want {
		g, ok := got[rel]
		if !ok {
			t.Errorf("%s: in the golden, not generated", rel)
			continue
		}
		if g != w {
			t.Errorf("%s differs from the golden:\n--- golden\n%s\n--- generated\n%s", rel, w, g)
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("%s: generated, not in the golden (UPDATE_GOLDEN=1 to accept)", rel)
		}
	}
}

// add never overwrites: an app dir that exists is refused before any
// file is planned, and a fort with no apps root (not scaffolded) too.
func TestAddRefusals(t *testing.T) {
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, "lab-fort")
	copyEstate(t, tmp)
	o := addOpts{root: tmp, fort: "lab-fort", ref: "oci://x/y/moment-tally-website", version: "0.1.0", chartDir: "testdata/charts/moment-tally-website"}
	if _, err := planAdd(o); err == nil || !strings.Contains(err.Error(), "no flux/apps tree") {
		t.Errorf("no apps root: err = %v", err)
	}
	copyFile(t, "testdata/trees/fresh/kustomization.yaml", filepath.Join(tmp, "flux/apps/lab-fort/kustomization.yaml"))
	writeFile(t, tmp, "flux/apps/lab-fort/moment-tally-website/release.yaml", "x")
	if _, err := planAdd(o); err == nil || !strings.Contains(err.Error(), "never overwrites") {
		t.Errorf("existing app dir: err = %v", err)
	}
}

// --http parses as <service>:<port>, and the chart's own contract wins
// wherever there is one: the flag fills a gap, it never contradicts a
// declaration.
func TestHTTPFlag(t *testing.T) {
	for _, bad := range []string{"app", "app:", ":3000", "app:http", "app:0", "app:70000"} {
		if h, err := parseHTTPFlag(bad); err == nil {
			t.Errorf("--http %q parsed as %+v, want an error", bad, h)
		}
	}
	h, err := parseHTTPFlag("moment-tally-website:3000")
	if err != nil {
		t.Fatal(err)
	}
	if h.Service != "moment-tally-website" || h.Port != 3000 {
		t.Errorf("parseHTTPFlag = %+v", h)
	}

	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, "lab-fort")
	copyEstate(t, tmp)
	copyFile(t, "testdata/trees/fresh/kustomization.yaml", filepath.Join(tmp, "flux/apps/lab-fort/kustomization.yaml"))
	_, err = planAdd(addOpts{
		root: tmp, fort: "lab-fort",
		ref:      "oci://gitea.zen.lofi/sfi/moment-tally-website",
		version:  "0.1.0",
		http:     "moment-tally-website:3000",
		chartDir: "testdata/charts/moment-tally-website",
	})
	if err == nil || !strings.Contains(err.Error(), "already routes to") {
		t.Errorf("--http against a contract that declares http: err = %v", err)
	}
}

// The pin is one of three forms, and `add` refuses any other count.
// --follow-main resolves against main's per-commit prereleases, which a
// PR's must stay outside of: semver ranks every numeric prerelease
// identifier below every alphanumeric one, so `main.z` bounds the range
// from above and `pr.*` sorts past it.
func TestChartPinConstraint(t *testing.T) {
	for _, c := range []struct {
		pin  chartPin
		want string
	}{
		{chartPin{version: "0.1.1"}, "0.1.1"},
		{chartPin{followPR: "51"}, ">=0.0.0-pr.51.0 <0.0.0-pr.52.0"},
		{chartPin{followMain: true}, ">=0.0.0-main.0 <0.0.0-main.z"},
	} {
		got, err := c.pin.constraint()
		if err != nil || got != c.want {
			t.Errorf("%+v: want %q, got %q (%v)", c.pin, c.want, got, err)
		}
	}
	if _, err := (chartPin{followPR: "ninety"}).constraint(); err == nil {
		t.Error("--follow-pr ninety: want an error")
	}
	if a, b := (chartPin{url: "oci://x/y", followMain: true}).key(), (chartPin{url: "oci://x/y"}).key(); a == b {
		t.Errorf("a main pin and a tag pin share the cache key %q", a)
	}
}

// Exactly one of --version, --follow-pr and --follow-main pins the
// chart. The check runs before add reads the repo, so a bad count is an
// error the operator gets immediately.
func TestAddPinCount(t *testing.T) {
	for _, args := range [][]string{
		{"lab-fort", "oci://x/y"},
		{"lab-fort", "oci://x/y", "--version", "0.1.1", "--follow-main"},
		{"lab-fort", "oci://x/y", "--follow-pr", "51", "--follow-main"},
		{"lab-fort", "oci://x/y", "--version", "0.1.1", "--follow-pr", "51"},
	} {
		err := add(args)
		if err == nil || !strings.Contains(err.Error(), "exactly one of") {
			t.Errorf("add %v: want the pin-count error, got %v", args, err)
		}
	}
}

// The gateway's listener patch lands inside the patches: list, never at
// the end of the file: a list after patches: (a configMapGenerator)
// would otherwise swallow the entry and fail the build with `unknown
// field "path"`.
func TestAppendPatch(t *testing.T) {
	out := appendPatch("resources:\n  - a\n", "# why\n", "p.yaml", "g", "v1", "K", "n", "ns")
	if !strings.HasSuffix(out, "# why\npatches:\n  - path: p.yaml\n    target:\n      group: g\n      version: v1\n      kind: K\n      name: n\n      namespace: ns\n") {
		t.Errorf("appendPatch (new block):\n%s", out)
	}
	out2 := appendPatch(out, "# why\n", "q.yaml", "g", "v1", "K", "n", "ns")
	if strings.Count(out2, "patches:") != 1 || !strings.Contains(out2, "  - path: q.yaml\n") {
		t.Errorf("appendPatch (existing block):\n%s", out2)
	}
	trailing := "patches:\n  - path: p.yaml\n    target:\n      kind: K\n\n# the seed scripts\nconfigMapGenerator:\n  - name: g\n    files:\n      - s.sh\n"
	out3 := appendPatch(trailing, "# why\n", "q.yaml", "g", "v1", "K", "n", "ns")
	if want := "patches:\n  - path: p.yaml\n    target:\n      kind: K\n  - path: q.yaml\n    target:\n      group: g\n      version: v1\n      kind: K\n      name: n\n      namespace: ns\n\n# the seed scripts\nconfigMapGenerator:\n  - name: g\n    files:\n      - s.sh\n"; out3 != want {
		t.Errorf("appendPatch (list after patches:):\n%s", out3)
	}
}
