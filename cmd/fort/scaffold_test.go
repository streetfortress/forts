package main

import (
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

// The two shapes from the fixture manifests: a lab VM (qcow runtime,
// static LAN) and a cloud fort (linode runtime, zram, no LAN block) —
// both get the fleet dirs (every fort is a flux cluster).
func TestRenderScaffold(t *testing.T) {
	hosts, problems, err := manifest.LoadAll("testdata")
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	byPath := func(files []scaffoldFile) map[string]string {
		m := map[string]string{}
		for _, f := range files {
			m[f.path] = string(f.content)
		}
		return m
	}
	fleet := []string{"flux-system/flux-operator.yaml", "flux-system/flux-instance.yaml", "flux-system/kustomization.yaml", "cluster-vars.yaml", "infrastructure.yaml", "apps.yaml"}

	cloud := byPath(render(t, "cloud-fort", hosts["cloud-fort"]))
	lab := byPath(render(t, "lab-fort", hosts["lab-fort"]))
	c, l := cloud["nixos/hosts/cloud-fort/default.nix"], lab["nixos/hosts/lab-fort/default.nix"]
	// nix host dir (2) + flux/clusters (fleet) + the apps root. What a
	// hub asks of the fort is a layer's render (forts#143).
	for _, m := range []map[string]string{cloud, lab} {
		if len(m) != 2+len(fleet)+1 {
			t.Errorf("want %d files, got %d: %v", 2+len(fleet)+1, len(m), keys(m))
		}
	}
	for name, m := range map[string]map[string]string{"cloud-fort": cloud, "lab-fort": lab} {
		got := m["nixos/hosts/"+name+"/default.nix"]
		for _, s := range operatorDefaults {
			if !strings.Contains(got, s) {
				t.Errorf("%s default.nix lacks the operator default %q", name, s)
			}
		}
		if !strings.Contains(m["nixos/hosts/"+name+"/secrets.nix"], keyDeployerPasswordHash) {
			t.Errorf("%s secrets.nix does not name %s", name, keyDeployerPasswordHash)
		}
	}
	for _, s := range []string{"runtime-linode", "modules/interactive.nix", "modules/dotfiles.nix"} {
		if strings.Contains(l, s) {
			t.Errorf("lab default.nix should not carry %q", s)
		}
	}
	for name, m := range map[string]map[string]string{"cloud-fort": cloud, "lab-fort": lab} {
		sec := m["nixos/hosts/"+name+"/secrets.nix"]
		if !strings.Contains(sec, "secrets/"+name+".yaml") || !strings.Contains(sec, "secrets.wireguard-private-key") {
			t.Errorf("%s secrets.nix wrong:\n%s", name, sec)
		}
		cv := m["flux/clusters/"+name+"/cluster-vars.yaml"]
		if !strings.Contains(cv, "CLUSTER_NAME: "+name+"\n  ESTATE_NAME: deployments\n") {
			t.Errorf("%s cluster-vars wrong:\n%s", name, cv)
		}
		if !strings.Contains(m["flux/clusters/"+name+"/flux-system/flux-instance.yaml"], "path: flux/clusters/"+name) {
			t.Errorf("%s flux-instance sync path wrong", name)
		}
		if !strings.Contains(m["flux/clusters/"+name+"/apps.yaml"], "path: ./flux/apps/"+name) {
			t.Errorf("%s apps.yaml path wrong", name)
		}
		if !strings.Contains(m["flux/apps/"+name+"/kustomization.yaml"], "- sops-canary.yaml") {
			t.Errorf("%s apps root kustomization lacks the canary", name)
		}
	}
	if !strings.Contains(cloud["flux/clusters/cloud-fort/cluster-vars.yaml"], "MESH_IP: 10.1.0.98") {
		t.Errorf("cloud MESH_IP wrong")
	}

	// A cluster scaffolded before ESTATE_NAME existed (forts#50) gains
	// the line: the one key scaffold appends to an existing cluster-vars.
	// The rest of the file is the estate's — comments, order, a value
	// already set (right or wrong: the gate judges values).
	var cvEdit func(string) (string, error)
	for _, f := range render(t, "cloud-fort", hosts["cloud-fort"]) {
		if f.path == "flux/clusters/cloud-fort/cluster-vars.yaml" {
			cvEdit = f.edit
		}
	}
	if cvEdit == nil {
		t.Fatal("cluster-vars.yaml has no edit for the ESTATE_NAME line")
	}
	old := "# the estate's comment\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: cluster-vars\n  namespace: flux-system\ndata:\n  CLUSTER_NAME: cloud-fort\n  MESH_IP: 10.1.0.98\n  # hand-added\n  HUB_MESH_IP: 10.1.0.5\n"
	got, err := cvEdit(old)
	if err != nil || !strings.HasSuffix(got, "  HUB_MESH_IP: 10.1.0.5\n  ESTATE_NAME: deployments\n") || !strings.HasPrefix(got, "# the estate's comment\n") || strings.Count(got, "\n") != strings.Count(old, "\n")+1 {
		t.Errorf("cluster-vars edit: %v\n%s", err, got)
	}
	if again, err := cvEdit(got); err != nil || again != got {
		t.Errorf("cluster-vars edit is not idempotent: %v\n%s", err, again)
	}
	if kept, err := cvEdit("data:\n  CLUSTER_NAME: cloud-fort\n  ESTATE_NAME: other\n  MESH_IP: 10.1.0.98\n"); err != nil || strings.Count(kept, "ESTATE_NAME") != 1 {
		t.Errorf("cluster-vars edit must leave a set ESTATE_NAME alone: %v\n%s", err, kept)
	}
	if _, err := cvEdit("apiVersion: v1\nkind: ConfigMap\n"); err == nil {
		t.Error("cluster-vars edit with no data: block must fail, not invent one")
	}

	// a [libvirt] host is the lab shape (fort adopt, the libvirt backend)
	if got := lab["nixos/hosts/lab-fort/default.nix"]; !strings.Contains(got, "runtime-qemu") || strings.Contains(got, "linode") {
		t.Errorf("libvirt host should render the lab shape")
	}

	// a fort off the mesh is refused (MESH_IP is mandatory, #99)
	// [cluster] audit_logging (#626) reaches the host file the same way:
	// the fixture cloud fort says true and gets the block, the lab fort
	// says nothing and carries no trace of it.
	if !strings.Contains(c, "sfi.k3s.auditLog.enable = true;") {
		t.Errorf("cloud-fort: audit_logging = true must set sfi.k3s.auditLog.enable:\n%s", c)
	}
	if strings.Contains(l, "auditLog") {
		t.Errorf("lab-fort: no [cluster] table must render no audit wiring:\n%s", l)
	}
	audit := hosts["lab-fort"]
	audit.Cluster = &manifest.ClusterSpec{AuditLogging: true}
	if got := byPath(render(t, "lab-fort", audit))["nixos/hosts/lab-fort/default.nix"]; !strings.Contains(got, "sfi.k3s.auditLog.enable = true;") {
		t.Errorf("lab shape must render the audit block too:\n%s", got)
	}

	// [cluster] datastore (#426): the fixtures name none and get the
	// default, sqlite (asserted above); "etcd" renders the etcd line on
	// both shapes, and never the raw services.k3s.clusterInit — the
	// module owns that mapping.
	for _, name := range []string{"lab-fort", "cloud-fort"} {
		etcd := hosts[name]
		etcd.Cluster = &manifest.ClusterSpec{Datastore: "etcd"}
		got := byPath(render(t, name, etcd))["nixos/hosts/"+name+"/default.nix"]
		if !strings.Contains(got, `sfi.k3s.datastore = "etcd";`) || strings.Contains(got, `"sqlite"`) {
			t.Errorf("%s: datastore = \"etcd\" must render the etcd line and no sqlite:\n%s", name, got)
		}
		if clusterInitLine.MatchString(got) || strings.Contains(got, "k3s_cluster_init") {
			t.Errorf("%s: the host file names the datastore, not clusterInit:\n%s", name, got)
		}
	}
	if !strings.Contains(c, `sfi.k3s.datastore = "sqlite";`) {
		t.Errorf("cloud-fort: the default datastore is sqlite, rendered out loud:\n%s", c)
	}

	h := hosts["lab-fort"]
	h.MeshIP = ""
	if _, err := renderScaffold("lab-fort", h, testEstate); err == nil {
		t.Errorf("a manifest without mesh_ip should be refused")
	}

	// a [baremetal] host is the lab shape on hardware (#287): the
	// baremetal runtime instead of qcow, the LAN block matched by MAC
	// when the manifest names one, everything else as the lab VM.
	metal := byPath(render(t, "metal-fort", hosts["metal-fort"]))
	m := metal["nixos/hosts/metal-fort/default.nix"]
	for _, s := range append([]string{"inputs.forts.nixosModules.runtime-baremetal", "metal-fort-baremetal", `matchConfig.MACAddress = "f8:ce:72:02:05:07"`, `address = [ "10.0.0.97/24" ]`, "inputs.forts.nixosModules.hardened", "inputs.forts.nixosModules.ops-deployer", "inputs.forts.nixosModules.flux-bootstrap", "deployer@10.1.0.97", `sfi.k3s.datastore = "sqlite";`, "zramSwap", "docs/baremetal.md"}, operatorDefaults...) {
		if !strings.Contains(m, s) {
			t.Errorf("metal default.nix lacks %q", s)
		}
	}
	for _, s := range []string{"runtime-qemu", "-qcow", `matchConfig.Name`, "linode", "hypervisor's LAN bridge", "nixosModules.production"} {
		if strings.Contains(m, s) {
			t.Errorf("metal default.nix should not carry %q", s)
		}
	}
	if len(metal) != len(lab) {
		t.Errorf("metal renders %d files, lab %d — the fleet half is the same for every fort", len(metal), len(lab))
	}
	// without a mac the block matches en* like a lab VM's
	noMAC := hosts["metal-fort"]
	spec := *noMAC.Baremetal()
	spec.MAC = ""
	noMAC.Spec = &spec
	nm := byPath(render(t, "metal-fort", noMAC))["nixos/hosts/metal-fort/default.nix"]
	if !strings.Contains(nm, `matchConfig.Name = "en*"`) || strings.Contains(nm, "MACAddress") {
		t.Errorf("metal without mac should match en*:\n%s", nm)
	}
}

// The committed flux/clusters dirs are what the templates render — the
// generator reproduces the tree the playbook-era cluster-init laid down (modulo nothing:
// these files carry no hand edits today).
func TestScaffoldMatchesCommittedClusters(t *testing.T) {
	repo := estateRoot(t)
	hosts, problems, err := manifest.LoadAll(repo)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	for name, h := range hosts {
		for _, f := range render(t, name, h) {
			// flux/clusters/luxor was bootstrapped by hand and carries a
			// bespoke apps layer and pre-module comments — the exception
			// its manifest's header records, not a template drift.
			if name == "luxor" || !strings.HasPrefix(f.path, "flux/clusters/") {
				continue
			}
			if got := readFile(t, filepath.Join(repo, f.path)); got != string(f.content) {
				t.Errorf("%s differs from the render", f.path)
			}
		}
	}
}

// testEstate is the estate the render tests assume: the deployments
// repo as the source, no hub.
var testEstate = estate.Estate{Name: "deployments", Repo: "ssh://git@gitea.zen.lofi:30022/sfi/deployments.git"}

// operatorDefaults is what every host shape imports and enables, whatever
// else it is (sfi/forts#24): the shell and the console door. A shape
// that lacks one renders a fort an operator cannot reach at a pane or at
// the console after the mesh breaks.
var operatorDefaults = []string{
	"inputs.forts.nixosModules.shell",
	"inputs.forts.nixosModules.ops-console",
	"sfi.shell.enable = true;",
	"sfi.ops.console.enable = true;",
}

func render(t *testing.T, host string, h manifest.Host) []scaffoldFile {
	t.Helper()
	files, err := renderScaffold(host, h, testEstate)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A plain fort (no [cluster] table) renders the host dir — the baseline
// shape, no fleet surface — plus its pull-shaped file on the hub and the
// kustomization line that applies it.
func TestRenderScaffoldPlain(t *testing.T) {
	hosts, problems, err := manifest.LoadAll("testdata")
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	files := render(t, "plain-fort", hosts["plain-fort"])
	if len(files) != 2 {
		t.Fatalf("want 2 files (default.nix, secrets.nix), got %d", len(files))
	}
	m := map[string]string{}
	for _, f := range files {
		m[f.path] = string(f.content)
	}
	d := m["nixos/hosts/plain-fort/default.nix"]
	// The baseline and nothing else: the hardened profile, the mesh,
	// deployer with the ops verbs (#647), the shell, the rebuild loop.
	for _, s := range append([]string{"inputs.forts.nixosModules.runtime-qemu", "inputs.forts.nixosModules.hardened", "inputs.forts.nixosModules.estate", "inputs.forts.nixosModules.metrics", "inputs.forts.nixosModules.lab-dns", "inputs.forts.nixosModules.ops-deployer", "inputs.forts.nixosModules.wireguard", "inputs.forts.nixosModules.auto-rebuild", "deployer@10.1.0.96", `address = [ "10.0.0.96/24" ]`, `networking.hostName = "plain-fort"`, "zramSwap", "PLAIN FORT"}, operatorDefaults...) {
		if !strings.Contains(d, s) {
			t.Errorf("plain default.nix lacks %q", s)
		}
	}
	for _, s := range []string{"k3s-server", "production.nix", "flux-bootstrap", "sfi.k3s", "interactive.nix", "dotfiles.nix", "dev-fort.nix", "lab-host"} {
		if strings.Contains(d, s) {
			t.Errorf("plain default.nix should not carry %q", s)
		}
	}
	sec := m["nixos/hosts/plain-fort/secrets.nix"]
	if !strings.Contains(sec, "secrets/plain-fort.yaml") || !strings.Contains(sec, "secrets.wireguard-private-key") || !strings.Contains(sec, "No k3s-token") {
		t.Errorf("plain secrets.nix wrong:\n%s", sec)
	}
}

func TestFixtureEstateIsScaffoldOutput(t *testing.T) {
	hosts, problems, err := manifest.LoadAll("testdata")
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	const dir = "../../tests/estate"
	est, err := estate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(dir, manifest.Path("plain-fort"))) != readFile(t, filepath.Join("testdata", manifest.Path("plain-fort"))) {
		t.Fatal("the fixture estate's manifest differs from testdata's")
	}
	files, err := renderScaffold("plain-fort", hosts["plain-fort"], est)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("no hub: want the host dir alone (2 files), got %d", len(files))
	}
	for _, f := range files {
		if got := readFile(t, filepath.Join(dir, f.path)); got != string(f.content) {
			t.Errorf("%s: the fixture differs from the render — re-run fort scaffold plain-fort --write in tests/estate", f.path)
		}
	}
}
