package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fort remove is add's inverse, so its tests are round trips against
// add's own renders (forts#251): add two apps and remove the first, and
// the tree equals the second app added alone; add one app and remove
// it, and the tree is the fresh tree plus a listener-less gateway dir
// that the next add appends to exactly as the template would.

// addApp renders one chart onto a scratch fort tree.
func addApp(t *testing.T, root, fort, chart, version, name string) *plan {
	t.Helper()
	p, err := planAdd(addOpts{
		root: root, fort: fort,
		ref: "oci://gitea.zen.lofi/sfi/" + chart, version: version, name: name,
		chartDir: "testdata/charts/" + chart,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.apply(); err != nil {
		t.Fatal(err)
	}
	return p
}

// freshFort lays the scratch root down: the manifest, the estate and the
// apps kustomization scaffold renders.
func freshFort(t *testing.T, fort string) string {
	t.Helper()
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, fort)
	copyEstate(t, tmp)
	copyFile(t, "testdata/trees/fresh/kustomization.yaml", filepath.Join(tmp, "flux/apps", fort, "kustomization.yaml"))
	return tmp
}

// appsTree reads every file under the fort's apps root, keyed by its
// path relative to that root.
func appsTree(t *testing.T, root, fort string) map[string]string {
	t.Helper()
	got := map[string]string{}
	base := filepath.Join(root, "flux/apps", fort)
	filepath.Walk(base, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(base, path)
			got[rel] = readFile(t, path)
		}
		return nil
	})
	return got
}

func sameTree(t *testing.T, got, want map[string]string) {
	t.Helper()
	for rel, w := range want {
		g, ok := got[rel]
		if !ok {
			t.Errorf("missing %s", rel)
			continue
		}
		if g != w {
			t.Errorf("%s differs:\n--- want\n%s\n--- got\n%s", rel, w, g)
		}
	}
	for rel := range got {
		if _, ok := want[rel]; !ok {
			t.Errorf("unexpected %s", rel)
		}
	}
}

func removeApp(t *testing.T, root, fort, target string) (*removePlan, string) {
	t.Helper()
	p, err := planRemove(root, fort, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.apply(); err != nil {
		t.Fatal(err)
	}
	var report strings.Builder
	p.report(&report, true)
	return p, report.String()
}

// Two apps, the first removed: the tree is the second added alone. The
// first carries the secret, the listener and the PodMonitor, so every
// edit remove makes is exercised.
func TestRemoveFirstOfTwo(t *testing.T) {
	root := freshFort(t, "lab-fort")
	addApp(t, root, "lab-fort", "sfi-website", "0.3.0", "")
	addApp(t, root, "lab-fort", "moment-tally-website", "0.1.0", "")
	p, report := removeApp(t, root, "lab-fort", "sfi-website")

	want := freshFort(t, "lab-fort")
	addApp(t, want, "lab-fort", "moment-tally-website", "0.1.0", "")
	sameTree(t, appsTree(t, root, "lab-fort"), appsTree(t, want, "lab-fort"))

	if p.gatewayEmpty {
		t.Error("a listener remains, yet gatewayEmpty")
	}
	if len(p.residue) > 0 {
		t.Errorf("residue on a clean tree: %v", p.residue)
	}
	wantReport := `flux/apps/lab-fort/   (remove sfi-website: oci://gitea.zen.lofi/sfi/sfi-website 0.3.0)
○ gateway/
  ✗ certificate-sfi-website.yaml
  ✗ listener-sfi-website.yaml
  ○ kustomization.yaml           - certificate-sfi-website.yaml; patches: - listener-sfi-website.yaml
○ kustomization.yaml             - sfi-website
✗ sfi-website/                   the app dir, whole

staged. then: review, commit, PR — flux prunes the app on merge.
`
	if report != wantReport {
		t.Errorf("report:\n--- want\n%s\n--- got\n%s", wantReport, report)
	}
}

// The last app, removed by its OCI ref: the apps kustomization keeps
// the gateway entry, the gateway dir keeps the Gateway and the
// redirect with an empty-list-free kustomization, and the report says
// no listener remains. Adding the app again then renders the same tree
// as a first add — appendPatch's appendBlock path meets the template.
func TestRemoveLastApp(t *testing.T) {
	root := freshFort(t, "lab-fort")
	addApp(t, root, "lab-fort", "moment-tally-website", "0.1.0", "")
	first := appsTree(t, root, "lab-fort")
	p, report := removeApp(t, root, "lab-fort", "oci://gitea.zen.lofi/sfi/moment-tally-website:0.1.0")
	if !p.gatewayEmpty {
		t.Error("no listener remains, yet !gatewayEmpty")
	}
	if !strings.Contains(report, "note: no listener remains. gateway/ keeps the Gateway and the redirect") {
		t.Errorf("report lacks the gateway note:\n%s", report)
	}
	got := appsTree(t, root, "lab-fort")
	want := map[string]string{
		"kustomization.yaml":          readFile(t, "testdata/trees/fresh/kustomization.yaml") + "  - gateway\n",
		"gateway/gateway.yaml":        first["gateway/gateway.yaml"],
		"gateway/redirect-https.yaml": first["gateway/redirect-https.yaml"],
		"gateway/kustomization.yaml":  "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n  - gateway.yaml\n  - redirect-https.yaml\n",
	}
	sameTree(t, got, want)

	addApp(t, root, "lab-fort", "moment-tally-website", "0.1.0", "")
	sameTree(t, appsTree(t, root, "lab-fort"), first)
}

func TestRemoveRefusals(t *testing.T) {
	root := freshFort(t, "lab-fort")
	addApp(t, root, "lab-fort", "moment-tally-website", "0.1.0", "")
	addApp(t, root, "lab-fort", "moment-tally-website", "0.1.0", "tally-two")
	writeFile(t, root, "flux/apps/lab-fort/by-hand/deployment.yaml", "kind: Deployment\n")
	cases := []struct{ fort, target, want string }{
		{"lab-fort", "nope", "no app nope under flux/apps/lab-fort"},
		{"lab-fort", "gateway", "gateway/ is the fort's"},
		{"lab-fort", "by-hand", "carries no OCIRepository pin"},
		{"lab-fort", "../other", "an app is the name of a dir"},
		{"lab-fort", "oci://gitea.zen.lofi/sfi/moment-tally-website", "pinned by moment-tally-website and tally-two"},
		{"lab-fort", "oci://gitea.zen.lofi/sfi/elsewhere", "no app under flux/apps/lab-fort pins oci://gitea.zen.lofi/sfi/elsewhere"},
		{"cloud-fort", "x", "has no manifest"},
	}
	copyManifest(t, "testdata", root, "plain-fort")
	cases = append(cases, struct{ fort, target, want string }{"plain-fort", "x", "has no flux/apps/plain-fort tree"})
	for _, c := range cases {
		_, err := planRemove(root, c.fort, c.target)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("remove %s %s: want error containing %q, got %v", c.fort, c.target, c.want, err)
		}
	}
	// The ambiguity resolves by name.
	if p, err := planRemove(root, "lab-fort", "tally-two"); err != nil || p.app != "tally-two" {
		t.Errorf("remove tally-two: %v", err)
	}
	// A tree that is not add's shape fails the plan, not the apply.
	writeFile(t, root, "flux/apps/lab-fort/kustomization.yaml", "resources:\n  - sops-canary.yaml\n  - gateway\n")
	if _, err := planRemove(root, "lab-fort", "tally-two"); err == nil || !strings.Contains(err.Error(), "no `- tally-two` entry under resources:") {
		t.Errorf("want the missing entry named, got %v", err)
	}
}

// What remains outside the plan that names the app is residue: another
// fort's inventory with a sops: ref into the app's Secret, and a dir a
// layer rendered beneath the app goes with the dir, named.
func TestRemoveResidueAndNested(t *testing.T) {
	root := freshFort(t, "lab-fort")
	addApp(t, root, "lab-fort", "sfi-website", "0.3.0", "")
	writeFile(t, root, "flux/apps/other-fort/tenant/forts.inventory.yaml", "secrets:\n  - file: secrets.sops.yaml\n    name: tenant-env\n    keys:\n      TOKEN: { sops: ../../lab-fort/sfi-website/secrets.sops.yaml#TOKEN }\n")
	writeFile(t, root, "flux/apps/lab-fort/sfi-website/postgres/repository.yaml", "kind: OCIRepository\nspec:\n  url: oci://x/y/postgres\n  ref:\n    tag: 1.0.0\n")
	p, report := removeApp(t, root, "lab-fort", "sfi-website")
	if len(p.residue) != 1 || !strings.HasPrefix(p.residue[0], "flux/apps/other-fort/tenant/forts.inventory.yaml: 1") {
		t.Errorf("residue: %v", p.residue)
	}
	if !strings.Contains(report, "residue — files outside this plan that still name sfi-website:") {
		t.Errorf("report lacks the residue:\n%s", report)
	}
	if !strings.Contains(report, "✗ sfi-website/                   the app dir, whole — with postgres beneath it (a layer's render)") {
		t.Errorf("report lacks the nested dir:\n%s", report)
	}
	if exists(root, "flux/apps/lab-fort/sfi-website") {
		t.Error("the app dir remains")
	}
}

func TestRemoveListItemAndPatchEntry(t *testing.T) {
	list := "resources:\n  - a\n  - b\n  - c\nother:\n  - b\n"
	if got, err := removeListItem(list, "resources", "b"); err != nil || got != "resources:\n  - a\n  - c\nother:\n  - b\n" {
		t.Errorf("removeListItem b: %q %v", got, err)
	}
	if _, err := removeListItem(list, "resources", "zz"); err == nil {
		t.Error("removeListItem: absent entry must fail")
	}
	if _, err := removeListItem(list, "nope", "a"); err == nil {
		t.Error("removeListItem: absent block must fail")
	}
	two := "resources:\n  - gateway.yaml\n" + gatewayPatchesComment + "patches:\n" +
		"  - path: listener-a.yaml\n    target:\n      kind: Gateway\n" +
		"  - path: listener-b.yaml\n    target:\n      kind: Gateway\n"
	one, err := removePatchEntry(two, "listener-a.yaml")
	if err != nil || one != "resources:\n  - gateway.yaml\n"+gatewayPatchesComment+"patches:\n  - path: listener-b.yaml\n    target:\n      kind: Gateway\n" {
		t.Errorf("removePatchEntry a: %q %v", one, err)
	}
	none, err := removePatchEntry(one, "listener-b.yaml")
	if err != nil || none != "resources:\n  - gateway.yaml\n" {
		t.Errorf("removePatchEntry last: %q %v", none, err)
	}
	if _, err := removePatchEntry(one, "listener-a.yaml"); err == nil {
		t.Error("removePatchEntry: absent entry must fail")
	}
	if _, err := removePatchEntry(none, "listener-b.yaml"); err == nil {
		t.Error("removePatchEntry: absent block must fail")
	}
	// The round trip: appendPatch on the emptied text re-adds the block.
	back := appendPatch(none, gatewayPatchesComment, "listener-b.yaml", "g", "v1", "Gateway", "homelab", "traefik")
	if removed, err := removePatchEntry(back, "listener-b.yaml"); err != nil || removed != none {
		t.Errorf("round trip: %q %v", removed, err)
	}
}
