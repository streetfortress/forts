package main

import (
	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The golden tree: fleet/apps/sfi-fort-e33e2d5 was laid down by hand
// (#230, #232) as what fort add is specified to render for symbology
// from the sfi/symbology#189 preview chart. The fort was dismantled on
// 2026-09-18 (#348); its tree and manifest live on as a snapshot under
// testdata/golden/sfi-fort-e33e2d5 + testdata/provisioning, so the test
// no longer depends on a fort that exists. This test replays the add
// against the fort's pre-add state and compares.
//
// Byte-exact for everything the generator owns outright; structural for
// the files the post-add report tells the operator to refine in place
// (release.yaml values, the root kustomization's comments) and for the
// seeding contract, where the human replaced `external` keys with sops
// refs to staging's Secret.
func TestAddReproducesDrillFort(t *testing.T) {
	repo, err := repoRootFrom(mustGetwd(t))
	if err != nil {
		t.Skip("not inside the deployments checkout")
	}
	const fort = "sfi-fort-e33e2d5"
	golden := filepath.Join("testdata/golden", fort)

	// Pre-add state: the manifest, the CA, the fort's root kustomization as
	// it was before #230, sops-canary + the whole sites-db dir as-is
	// (the symbology tenant already existed from the restore drill).
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, fort)
	copyFile(t, filepath.Join(repo, "fleet/infrastructure/configs/step-ca-root.pem"), filepath.Join(tmp, "fleet/infrastructure/configs/step-ca-root.pem"))
	copyFile(t, filepath.Join("testdata/forts", fort, "kustomization.yaml"), filepath.Join(tmp, "fleet/apps", fort, "kustomization.yaml"))
	copyFile(t, filepath.Join(golden, "sops-canary.yaml"), filepath.Join(tmp, "fleet/apps", fort, "sops-canary.yaml"))
	copyTree(t, filepath.Join(golden, "sites-db"), filepath.Join(tmp, "fleet/apps", fort, "sites-db"))

	// The fort followed #189's previews while the PR was open; since the
	// merge it pins the release tag like every fort, and so does the
	// replay (the chart fixture is still the #189 contract, unchanged
	// through 0.1.9).
	p, err := planAdd(addOpts{
		root: tmp, fort: fort,
		ref:      "oci://gitea.zen.lofi/sfi/symbology",
		version:  "0.1.9",
		chartDir: "testdata/charts/symbology",
		without:  map[string]bool{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.apply(); err != nil {
		t.Fatal(err)
	}

	exact := []string{
		"gateway/kustomization.yaml", "gateway/gateway.yaml", "gateway/redirect-https.yaml",
		"gateway/cluster-issuer-step-ca.yaml", "gateway/certificate-symbology.yaml", "gateway/listener-symbology.yaml",
		"valkey/kustomization.yaml", "valkey/namespace.yaml", "valkey/repository.yaml", "valkey/release.yaml",
		"valkey/acl-symbology.yaml", "valkey/secrets.contract.yaml", "valkey/monitoring.yaml",
		"symbology/kustomization.yaml", "symbology/namespace.yaml", "symbology/route.yaml",
		"symbology/monitoring.yaml", "symbology/step-ca-root.pem",
	}
	for _, rel := range exact {
		want := readFile(t, filepath.Join(golden, rel))
		got := readFile(t, filepath.Join(tmp, "fleet/apps", fort, rel))
		if want != got {
			t.Errorf("%s differs from the golden tree:\n--- golden\n%s\n--- generated\n%s", rel, want, got)
		}
	}

	// sites-db: tenant present → untouched.
	for _, rel := range []string{"kustomization.yaml", "secrets.contract.yaml"} {
		if readFile(t, filepath.Join(golden, "sites-db", rel)) != readFile(t, filepath.Join(tmp, "fleet/apps", fort, "sites-db", rel)) {
			t.Errorf("sites-db/%s was edited although the tenant already exists", rel)
		}
	}

	// Root kustomization: same resource list (comments are the human's).
	if want, got := yamlPath(t, filepath.Join(golden, "kustomization.yaml"), "resources"), yamlPath(t, filepath.Join(tmp, "fleet/apps", fort, "kustomization.yaml"), "resources"); !reflect.DeepEqual(want, got) {
		t.Errorf("root kustomization resources: want %v, got %v", want, got)
	}

	// repository.yaml: same spec (the comment names the PR differently).
	if want, got := yamlPath(t, filepath.Join(golden, "symbology/repository.yaml"), "spec"), yamlPath(t, filepath.Join(tmp, "fleet/apps", fort, "symbology/repository.yaml"), "spec"); !reflect.DeepEqual(want, got) {
		t.Errorf("repository.yaml spec: want %v, got %v", want, got)
	}

	// release.yaml: the wired values and the retry posture; the golden
	// carries hand refinements on top (inference, ui.env, replicas).
	for _, path := range []string{"spec/values/existingSecret", "spec/values/database", "spec/values/cache", "spec/install", "spec/upgrade", "spec/chartRef"} {
		want := yamlPath(t, filepath.Join(golden, "symbology/release.yaml"), path)
		got := yamlPath(t, filepath.Join(tmp, "fleet/apps", fort, "symbology/release.yaml"), path)
		if !reflect.DeepEqual(want, got) {
			t.Errorf("release.yaml %s: want %v, got %v", path, want, got)
		}
	}

	// secrets.contract.yaml: same vars; every generated key is in the
	// golden with the same spec unless it is `external` here and a
	// human-resolved source (sops/env) there.
	gv := yamlPath(t, filepath.Join(golden, "symbology/secrets.contract.yaml"), "vars")
	tv := yamlPath(t, filepath.Join(tmp, "fleet/apps", fort, "symbology/secrets.contract.yaml"), "vars")
	if !reflect.DeepEqual(gv, tv) {
		t.Errorf("contract vars: want %v, got %v", gv, tv)
	}
	gk := yamlPath(t, filepath.Join(golden, "symbology/secrets.contract.yaml"), "secrets/secrets.sops.yaml/keys").(map[string]any)
	tk := yamlPath(t, filepath.Join(tmp, "fleet/apps", fort, "symbology/secrets.contract.yaml"), "secrets/secrets.sops.yaml/keys").(map[string]any)
	for name, spec := range tk {
		gspec, ok := gk[name]
		if !ok {
			t.Errorf("contract key %s generated but not in the golden tree", name)
			continue
		}
		if _, ext := spec.(map[string]any)["external"]; ext {
			if m := gspec.(map[string]any); m["sops"] == nil && m["env"] == nil {
				t.Errorf("contract key %s: external here, but golden did not resolve it from a source: %v", name, gspec)
			}
			continue
		}
		if !reflect.DeepEqual(spec, gspec) {
			t.Errorf("contract key %s: want %v, got %v", name, gspec, spec)
		}
	}
	if len(p.externals) != 5 {
		t.Errorf("expected 5 external keys reported, got %d", len(p.externals))
	}
}

func TestAppendHelpers(t *testing.T) {
	in := "apiVersion: v1\nkind: Kustomization\nresources:\n  # c\n  - a\n  - b\ngeneratorOptions:\n  x: y\n"
	out, err := appendListItem(in, "resources", "c")
	if err != nil {
		t.Fatal(err)
	}
	if want := "apiVersion: v1\nkind: Kustomization\nresources:\n  # c\n  - a\n  - b\n  - c\ngeneratorOptions:\n  x: y\n"; out != want {
		t.Errorf("appendListItem:\n%s", out)
	}
	out = appendPatch("resources:\n  - a\n", "# why\n", "p.yaml", "g", "v1", "K", "n", "ns")
	if !strings.HasSuffix(out, "# why\npatches:\n  - path: p.yaml\n    target:\n      group: g\n      version: v1\n      kind: K\n      name: n\n      namespace: ns\n") {
		t.Errorf("appendPatch (new block):\n%s", out)
	}
	out2 := appendPatch(out, "# why\n", "q.yaml", "g", "v1", "K", "n", "ns")
	if strings.Count(out2, "patches:") != 1 || !strings.Contains(out2, "  - path: q.yaml\n") {
		t.Errorf("appendPatch (existing block):\n%s", out2)
	}
	out, err = appendMapEntry("secrets:\n  f:\n    keys:\n      a: {generate: hex24}\n", "    keys:", "      b: {generate: hex24}")
	if err != nil || out != "secrets:\n  f:\n    keys:\n      a: {generate: hex24}\n      b: {generate: hex24}\n" {
		t.Errorf("appendMapEntry: %v\n%s", err, out)
	}
}

// --- helpers

func repoRootFrom(dir string) (string, error) {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}

func mustGetwd(t *testing.T) string {
	d, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func readFile(t *testing.T, p string) string {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("%s: %v", p, err)
	}
	return string(b)
}

// copyManifest copies one host's manifest from a repo/fixture root
// (testdata, or the checkout) into a scratch root.
func copyManifest(t *testing.T, srcRoot, dstRoot, host string) {
	t.Helper()
	copyFile(t, filepath.Join(srcRoot, manifest.Path(host)), filepath.Join(dstRoot, manifest.Path(host)))
}

func copyFile(t *testing.T, src, dst string) {
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func copyTree(t *testing.T, src, dst string) {
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			copyTree(t, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
			continue
		}
		copyFile(t, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
	}
}

// yamlPath returns the value at a /-separated path of a YAML file (nil if absent).
func yamlPath(t *testing.T, file, path string) any {
	var doc any
	if err := yaml.Unmarshal([]byte(readFile(t, file)), &doc); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	cur := doc
	for _, part := range strings.Split(path, "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	return cur
}

// Second golden: a chart on a FRESH cloud fort — nothing but the root
// kustomization exists, so every provider and the gateway come from
// template on the cloud path (letsencrypt, <app>.<fort>.<dns_zone>).
// The expected tree is a reviewed snapshot under testdata/golden;
// UPDATE_GOLDEN=1 rewrites it (then review the diff like any other
// generated code).
func TestAddFreshCloudFort(t *testing.T) {
	repo, err := repoRootFrom(mustGetwd(t))
	if err != nil {
		t.Skip("not inside the deployments checkout")
	}
	const fort = "cloud-fort" // testdata manifest: linode, dns zone example.cloud
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, fort)
	copyFile(t, filepath.Join(repo, "fleet/infrastructure/configs/step-ca-root.pem"), filepath.Join(tmp, "fleet/infrastructure/configs/step-ca-root.pem"))
	copyFile(t, "testdata/forts/fresh-cloud/kustomization.yaml", filepath.Join(tmp, "fleet/apps", fort, "kustomization.yaml"))

	p, err := planAdd(addOpts{
		root: tmp, fort: fort,
		ref:      "oci://gitea.zen.lofi/sfi/sfi-website",
		version:  "0.3.0",
		chartDir: "testdata/charts/sfi-website",
		without:  map[string]bool{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.apply(); err != nil {
		t.Fatal(err)
	}
	var report strings.Builder
	p.report(&report, true)

	got := map[string]string{}
	base := filepath.Join(tmp, "fleet/apps", fort)
	filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(base, path)
			got[rel] = readFile(t, path)
		}
		return nil
	})
	got["REPORT.txt"] = report.String()

	golden := "testdata/golden/fresh-cloud"
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
