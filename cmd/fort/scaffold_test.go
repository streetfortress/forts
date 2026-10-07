package main

import (
	"os"
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
	want := []string{"default.nix", "secrets.nix"}
	fleet := []string{"flux-system/flux-operator.yaml", "flux-system/flux-instance.yaml", "flux-system/kustomization.yaml", "cluster-vars.yaml", "infrastructure.yaml", "apps.yaml"}

	cloud := byPath(render(t, "cloud-fort", hosts["cloud-fort"]))
	lab := byPath(render(t, "lab-fort", hosts["lab-fort"]))
	// nix host dir (2) + flux/clusters (fleet) + the apps root + the
	// cluster's monitoring for the hub and the hub kustomization append.
	for _, m := range []map[string]string{cloud, lab} {
		if len(m) != 2+len(fleet)+1+2 {
			t.Errorf("want %d files, got %d: %v", 2+len(fleet)+1+2, len(m), keys(m))
		}
	}
	// The per-cluster file: the dead man's switch on both, the expected
	// backup leg wherever a pv block promises one, and the edge
	// Alertmanager over the mesh (the hub's own is cluster DNS).
	for name, m := range map[string]map[string]string{"cloud-fort": cloud, "lab-fort": lab} {
		got := m[hubFilePath("luxor", name)]
		for _, s := range []string{`absent(up{cluster="` + name + `"})`, `absent_over_time({job="fluent-bit", cluster="` + name + `"}[15m])`, "uid: am-edge-" + name, "loki_rule:", "grafana_datasource:", "flux/hub/luxor/"} {
			if !strings.Contains(got, s) {
				t.Errorf("%s monitoring lacks %q:\n%s", name, s, got)
			}
		}
		if k, ok := m[hubKustomization("luxor")]; !ok || !strings.Contains(k, "  - "+name+".yaml\n") {
			t.Errorf("%s: no hub kustomization (or no line in a fresh one):\n%s", name, k)
		}
		// nothing renders into the hub's own tree (forts#38)
		for p := range m {
			if strings.HasPrefix(p, "flux/apps/luxor/") {
				t.Errorf("%s renders %s into the hub's tree", name, p)
			}
		}
	}
	for name, m := range map[string]map[string]string{"cloud-fort": cloud, "lab-fort": lab} {
		if !strings.Contains(m[hubFilePath("luxor", name)], "PVBackupNotConfigured") {
			t.Errorf("%s: a pv block must render PVBackupNotConfigured", name)
		}
	}
	// No pv block, no rule — the production fort's shape: db layers to
	// B2, nothing restic could be silent about.
	dbOnly := hosts["cloud-fort"]
	dbOnly.Backups.PV = nil
	if got := byPath(render(t, "cloud-fort", dbOnly))[hubFilePath("luxor", "cloud-fort")]; strings.Contains(got, "PVBackupNotConfigured") {
		t.Errorf("no pv block must render no PVBackupNotConfigured:\n%s", got)
	}
	if !strings.Contains(cloud[hubFilePath("luxor", "cloud-fort")], "url: http://10.1.0.98:30993") {
		t.Errorf("a fort's edge Alertmanager is reached over the mesh")
	}
	// the apps root of a fort that is not the hub lists no serves/
	if strings.Contains(cloud["flux/apps/cloud-fort/kustomization.yaml"], "serves") {
		t.Errorf("a fort that is not the hub carries no subscriptions:\n%s", cloud["flux/apps/cloud-fort/kustomization.yaml"])
	}
	for _, f := range want {
		if _, ok := cloud[filepath.Join("nixos/hosts/cloud-fort", f)]; !ok {
			t.Errorf("cloud: missing %s", f)
		}
	}
	for _, f := range fleet {
		if _, ok := cloud[filepath.Join("flux/clusters/cloud-fort", f)]; !ok {
			t.Errorf("cloud: missing flux/clusters/cloud-fort/%s", f)
		}
	}

	c := cloud["nixos/hosts/cloud-fort/default.nix"]
	for _, s := range []string{"inputs.forts.nixosModules.linode-runtime", "services.resolved.enable = true", "<host>.deployments.mesh", "zramSwap", "PermitRootLogin = lib.mkForce", "inputs.forts.nixosModules.production", "inputs.forts.nixosModules.flux-bootstrap", `networking.hostName = "cloud-fort"`, "deployer@10.1.0.98", "linode VPS (us-sea"} {
		if !strings.Contains(c, s) {
			t.Errorf("cloud default.nix lacks %q", s)
		}
	}
	// No split DNS over the mesh (forts#45): the mesh names are in
	// /etc/hosts, and a cloud fort knows no other .lofi name.
	for _, s := range []string{"qcow-runtime", "systemd.network.networks", "qemu-guest", "modules/interactive.nix", "modules/dotfiles.nix", `"~lofi"`, "10.1.0.1"} {
		if strings.Contains(c, s) {
			t.Errorf("cloud default.nix should not carry %q", s)
		}
	}
	l := lab["nixos/hosts/lab-fort/default.nix"]
	for _, s := range []string{"inputs.forts.nixosModules.qcow-runtime", `address = [ "10.0.0.99/24" ]`, "inputs.forts.nixosModules.production", "inputs.forts.nixosModules.flux-bootstrap", "deployer@10.1.0.99", `sfi.k3s.datastore = "sqlite";`, "zramSwap"} {
		if !strings.Contains(l, s) {
			t.Errorf("lab default.nix lacks %q", s)
		}
	}
	// The operator defaults (sfi/forts#24): the shell at a pane and the
	// console door on every shape, imported and enabled — the first fort
	// of a new estate came up with neither, and both had to be added by
	// hand after the provision. The secrets.nix names the key the door reads.
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
	for _, s := range []string{"linode-runtime", "modules/interactive.nix", "modules/dotfiles.nix"} {
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
		if !strings.Contains(cv, "HUB_MESH_IP: "+testHubIP) {
			t.Errorf("%s cluster-vars lacks the hub's address:\n%s", name, cv)
		}
		if !strings.Contains(cv, "CLUSTER_NAME: "+name+"\n  ESTATE_NAME: deployments\n") {
			t.Errorf("%s cluster-vars wrong:\n%s", name, cv)
		}
		// The estate's name rides every alert the hub raises about the
		// fort (forts#50), beside the cluster label.
		if mon := m[hubFilePath("luxor", name)]; strings.Count(mon, "estate: deployments") != 3 {
			t.Errorf("%s hub file: want estate: deployments on each of the 3 rules (the switch, the pv leg, the log switch):\n%s", name, mon)
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
	if got := lab["nixos/hosts/lab-fort/default.nix"]; !strings.Contains(got, "qcow-runtime") || strings.Contains(got, "linode") {
		t.Errorf("libvirt host should render the lab shape")
	}

	// a fort off the mesh is refused (MESH_IP is mandatory, #99)
	// The pv blocks reach the host file as declarations, one target per
	// store with the block's own words (store, cadence) and no endpoint
	// — and no pv block leaves no trace at all, so a host that is not an
	// origin reads as one rather than as a disabled module. The db blocks
	// never reach the host file: they are the cluster tree's (fort add).
	for name, m := range map[string]map[string]string{"cloud-fort": cloud, "lab-fort": lab} {
		got := m["nixos/hosts/"+name+"/default.nix"]
		want := map[string]string{"cloud-fort": `{ store = "b2"; every = "6h"; }`, "lab-fort": `{ store = "rustfs-central"; every = "6h"; }`}[name]
		if !strings.Contains(got, "inputs.forts.nixosModules.pv-backup") {
			t.Errorf("%s: no pv-backup import", name)
		}
		if !strings.Contains(got, want) || !strings.Contains(got, "targets = [") {
			t.Errorf("%s: want sfi.pv-backup target %s:\n%s", name, want, got)
		}
		for _, no := range []string{`profile = "`, "db-physical", "backblazeb2", "30901"} {
			if strings.Contains(got, no) {
				t.Errorf("%s: host file must not carry %q", name, no)
			}
		}
	}
	two := byPath(render(t, "metal-fort", hosts["metal-fort"]))["nixos/hosts/metal-fort/default.nix"]
	if !strings.Contains(two, `{ store = "b2"; every = "6h"; }`) || !strings.Contains(two, `{ store = "rustfs-central"; every = "weekly"; }`) {
		t.Errorf("two pv blocks render two targets, in manifest order with the cadence resolved:\n%s", two)
	}
	none := hosts["lab-fort"]
	none.Backups.PV = nil
	nf := byPath(render(t, "lab-fort", none))["nixos/hosts/lab-fort/default.nix"]
	if strings.Contains(nf, "pv-backup") {
		t.Errorf(`no pv block must render no pv-backup wiring at all:\n%s`, nf)
	}

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
	if _, err := renderScaffold("lab-fort", h, testEstate, testHubIP); err == nil {
		t.Errorf("a manifest without mesh_ip should be refused")
	}

	// a [baremetal] host is the lab shape on hardware (#287): the
	// baremetal runtime instead of qcow, the LAN block matched by MAC
	// when the manifest names one, everything else as the lab VM.
	metal := byPath(render(t, "metal-fort", hosts["metal-fort"]))
	m := metal["nixos/hosts/metal-fort/default.nix"]
	for _, s := range append([]string{"inputs.forts.nixosModules.baremetal-runtime", "metal-fort-baremetal", `matchConfig.MACAddress = "f8:ce:72:02:05:07"`, `address = [ "10.0.0.97/24" ]`, "inputs.forts.nixosModules.production", "inputs.forts.nixosModules.flux-bootstrap", "deployer@10.1.0.97", `sfi.k3s.datastore = "sqlite";`, "zramSwap", `{ store = "rustfs-central"; every = "weekly"; }`, "docs/baremetal.md"}, operatorDefaults...) {
		if !strings.Contains(m, s) {
			t.Errorf("metal default.nix lacks %q", s)
		}
	}
	for _, s := range []string{"qcow-runtime", "-qcow", `matchConfig.Name`, "linode", "hypervisor's LAN bridge"} {
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
			// The cluster's monitoring for the hub is rendered for every
			// flux cluster, luxor's own included; flux/clusters/luxor
			// was bootstrapped by hand and carries a bespoke apps layer
			// (databasus-configs) and pre-module comments — the exception
			// its manifest's header records, not a template drift.
			isMon := strings.HasPrefix(f.path, hubDir("luxor")+"/")
			if !isMon && (name == "luxor" || !strings.HasPrefix(f.path, "flux/clusters/")) {
				continue
			}
			if got := readFile(t, filepath.Join(repo, f.path)); got != string(f.content) {
				t.Errorf("%s differs from the render", f.path)
			}
		}
	}
}

// testEstate is the estate the render tests assume: luxor as the
// in-estate monitoring hub, the deployments repo as the source — the
// shape the golden trees carry.
var testEstate = estate.Estate{Name: "deployments", Repo: "ssh://git@gitea.zen.lofi:30022/sfi/deployments.git", Hubs: map[string]estate.Hub{estate.RoleMonitoring: {Estate: "ssh://git@gitea.zen.lofi:30022/sfi/deployments", Fort: "luxor"}}}

// testHubIP is luxor's mesh address, as its manifest has it.
const testHubIP = "10.1.0.5"

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
	files, err := renderScaffold(host, h, testEstate, testHubIP)
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
	if len(files) != 4 {
		t.Fatalf("want 4 files (default.nix, secrets.nix, clusters/plain-fort.yaml, + kustomization line), got %d", len(files))
	}
	m := map[string]string{}
	for _, f := range files {
		m[f.path] = string(f.content)
	}
	d := m["nixos/hosts/plain-fort/default.nix"]
	// The baseline and nothing else: the hardened profile, the mesh,
	// deployer with the ops verbs (#647), the shell, the rebuild loop.
	for _, s := range append([]string{"inputs.forts.nixosModules.qcow-runtime", "inputs.forts.nixosModules.hardened", "inputs.forts.nixosModules.estate", "inputs.forts.nixosModules.metrics", "inputs.forts.nixosModules.lab-dns", "inputs.forts.nixosModules.ops-deployer", "inputs.forts.nixosModules.wireguard", "inputs.forts.nixosModules.auto-rebuild", "deployer@10.1.0.96", `address = [ "10.0.0.96/24" ]`, `networking.hostName = "plain-fort"`, "zramSwap", "PLAIN FORT"}, operatorDefaults...) {
		if !strings.Contains(d, s) {
			t.Errorf("plain default.nix lacks %q", s)
		}
	}
	for _, s := range []string{"k3s-server", "production.nix", "flux-bootstrap", "pv-backup", "sfi.k3s", "interactive.nix", "dotfiles.nix", "dev-fort.nix", "lab-host"} {
		if strings.Contains(d, s) {
			t.Errorf("plain default.nix should not carry %q", s)
		}
	}
	sec := m["nixos/hosts/plain-fort/secrets.nix"]
	if !strings.Contains(sec, "secrets/plain-fort.yaml") || !strings.Contains(sec, "secrets.wireguard-private-key") || !strings.Contains(sec, "No k3s-token") {
		t.Errorf("plain secrets.nix wrong:\n%s", sec)
	}
	mon := m[hubFilePath("luxor", "plain-fort")]
	// One ScrapeConfig, node-exporter: the operator owns `job` (relabelled
	// to jobName after the target labels), so the leg files under
	// host-node-exporter — the Host Overview's job (#442).
	// `estate` is stamped here (forts#50): a pulled target has no edge
	// Prometheus to add it as an external label.
	for _, s := range []string{"kind: ScrapeConfig", "name: host-plain-fort-node", "jobName: host-node-exporter", `targets: ["10.1.0.96:9100"]`, "component: node", "        cluster: plain-fort\n        estate: deployments\n", "alert: HostDown", "            cluster: plain-fort\n            estate: deployments\n"} {
		if !strings.Contains(mon, s) {
			t.Errorf("plain hub file lacks %q", s)
		}
	}
	for _, s := range []string{"ClusterStreamSilent", "grafana_datasource", "fluent-bit", "30993", "otel", "claude", "LogStreamSilent"} {
		if strings.Contains(mon, s) {
			t.Errorf("plain hub file should not carry %q", s)
		}
	}
	if strings.Contains(mon, "\n        job:") {
		t.Errorf("plain hub file sets job as a target label — the operator overrides it with jobName")
	}
	if strings.Count(mon, "kind: ScrapeConfig") != 1 {
		t.Errorf("plain hub file wants one ScrapeConfig, got %d", strings.Count(mon, "kind: ScrapeConfig"))
	}
	var hasEdit bool
	for _, f := range files {
		if f.path == hubKustomization("luxor") && f.edit != nil {
			hasEdit = true
			out, err := f.edit("resources:\n  - luxor.yaml\n")
			if err != nil || !strings.Contains(out, "  - plain-fort.yaml\n") {
				t.Errorf("kustomization edit: %v\n%s", err, out)
			}
			// idempotent: a line already there is kept, never doubled
			if again, err := f.edit(out); err != nil || again != out {
				t.Errorf("kustomization edit is not idempotent: %v\n%s", err, again)
			}
			// and the first fort of the estate creates the file whole
			if !strings.Contains(string(f.content), "  - plain-fort.yaml\n") || !strings.Contains(string(f.content), "kind: Kustomization") {
				t.Errorf("a fresh hub kustomization must carry the line:\n%s", f.content)
			}
		}
	}
	if !hasEdit {
		t.Error("a plain fort's file for the hub needs its kustomization line")
	}
}

// The hub in another estate (forts#38): the served estate renders the
// same files into flux/hub/<hub host>/ of its own tree — the path is the
// hub HOST's name, whichever estate holds it — and HUB_MESH_IP is the
// hub's remotes entry in peers.json, the address the link gave it here.
func TestRenderScaffoldRemoteHub(t *testing.T) {
	hosts, problems, err := manifest.LoadAll("testdata")
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	served := estate.Estate{Name: "staging", Repo: "ssh://git@gitea.zen.lofi:30022/sfi/staging", Hubs: map[string]estate.Hub{
		estate.RoleForge:      {Estate: "ssh://git@gitea.zen.lofi:30022/sfi/infra", Fort: "sfi-fort-zenbook", Name: "gitea.zen.lofi"},
		estate.RoleMonitoring: {Estate: "ssh://git@gitea.zen.lofi:30022/sfi/infra", Fort: "luxor"},
	}}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "mesh"), 0o755)
	os.WriteFile(filepath.Join(root, peersPath), []byte(`{"peers": {}, "clients": {}, "remotes": {"luxor": {"wg_vpn_ip": "10.3.0.5", "wg_endpoint_ip": "10.0.0.5", "wg_endpoint_port": 51822, "public_key": "k"}}}`), 0o644)
	ip, err := hubMeshIP(root, served)
	if err != nil || ip != "10.3.0.5" {
		t.Fatalf("hubMeshIP from the remotes entry: %q %v", ip, err)
	}
	// no remotes entry yet: the link's half is missing, and scaffold
	// says which entry, rather than rendering an empty HUB_MESH_IP
	os.WriteFile(filepath.Join(root, peersPath), []byte(`{"peers": {}, "clients": {}}`), 0o644)
	if _, err := hubMeshIP(root, served); err == nil || !strings.Contains(err.Error(), "remotes.luxor") {
		t.Errorf("no remotes entry: want the error naming it, got %v", err)
	}
	// no monitoring hub at all: nothing to push to, no error
	if ip, err := hubMeshIP(root, estate.Estate{Name: "e", Repo: "ssh://x/sfi/e"}); err != nil || ip != "" {
		t.Errorf("no hub: %q %v", ip, err)
	}

	files, err := renderScaffold("cloud-fort", hosts["cloud-fort"], served, "10.3.0.5")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, f := range files {
		m[f.path] = string(f.content)
	}
	if !strings.Contains(m["flux/clusters/cloud-fort/cluster-vars.yaml"], "HUB_MESH_IP: 10.3.0.5") {
		t.Errorf("cluster-vars lacks the hub's address on this mesh:\n%s", m["flux/clusters/cloud-fort/cluster-vars.yaml"])
	}
	got := m[hubFilePath("luxor", "cloud-fort")]
	if got == "" || !strings.Contains(got, `absent(up{cluster="cloud-fort"})`) || !strings.Contains(got, "url: http://10.1.0.98:30993") {
		t.Errorf("the fort's file for a hub in another estate renders at the same path:\n%s", got)
	}
	if k := m[hubKustomization("luxor")]; !strings.Contains(k, "  - cloud-fort.yaml\n") {
		t.Errorf("hub kustomization:\n%s", k)
	}
	for p := range m {
		if strings.HasPrefix(p, "flux/apps/luxor/") || strings.Contains(p, "/serves/") {
			t.Errorf("a served estate renders no subscription: %s", p)
		}
	}
}

// The hub host's own render (forts#38): the fort's file for itself (its
// edge Alertmanager by cluster DNS), and the subscriptions its tree
// carries — its own estate's through the flux-system source, and one per
// [[serves]] entry with the monitoring role, each with its GitRepository,
// a service account scoped to monitoring-central and the Kustomization on
// flux/hub/<hub>/ of that estate; the apps root lists serves/.
func TestRenderScaffoldHubHost(t *testing.T) {
	hubEstate := estate.Estate{Name: "infra", Repo: "ssh://git@gitea.zen.lofi:30022/sfi/infra", Hubs: map[string]estate.Hub{
		estate.RoleForge:      {Estate: "ssh://git@gitea.zen.lofi:30022/sfi/infra", Fort: "sfi-fort-zenbook", Name: "gitea.zen.lofi"},
		estate.RoleMonitoring: {Estate: "ssh://git@gitea.zen.lofi:30022/sfi/infra", Fort: "luxor"},
	}, Serves: map[string][]estate.ServeEntry{
		estate.RoleMonitoring: {{Fort: "luxor", Estates: []string{"ssh://git@gitea.zen.lofi:30022/sfi/staging"}}, {Fort: "luxor-b", Estates: []string{"ssh://git@gitea.zen.lofi:30022/sfi/staging-b"}}},
		estate.RoleForge:      {{Fort: "sfi-fort-zenbook", Estates: []string{"ssh://git@gitea.zen.lofi:30022/sfi/staging", "ssh://git@gitea.zen.lofi:30022/sfi/production"}}},
	}}
	if err := hubEstate.Validate(); err != nil {
		t.Fatal(err)
	}
	luxor := manifest.Host{MeshIP: "10.1.0.5", Cluster: &manifest.ClusterSpec{}, Backups: manifest.Backups{Declared: true}, Provider: "libvirt", Spec: &manifest.LibvirtSpec{IP: "10.0.0.5"}}
	files, err := renderScaffold("luxor", luxor, hubEstate, "10.1.0.5")
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]scaffoldFile{}
	for _, f := range files {
		m[f.path] = f
	}
	// its own file: the hub's edge Alertmanager by cluster DNS
	own := string(m[hubFilePath("luxor", "luxor")].content)
	if !strings.Contains(own, "alertmanager-operated.monitoring.svc.cluster.local:9093") || strings.Contains(own, "10.1.0.5:30993") {
		t.Errorf("the hub's own file reaches its Alertmanager by cluster DNS:\n%s", own)
	}
	// the subscriptions: its own estate and staging; production is served
	// the forge only and gets none
	if _, ok := m[servesPath("luxor", "sfi/production")]; ok {
		t.Errorf("production is served the forge alone: no subscription")
	}
	if _, ok := m[servesPath("luxor", "sfi/staging-b")]; ok {
		t.Errorf("staging-b is served by luxor-b, not luxor")
	}
	local := string(m[servesPath("luxor", "sfi/infra")].content)
	for _, s := range []string{"kind: Kustomization", "name: hub-infra", "serviceAccountName: hub-infra", "path: ./flux/hub/luxor", "name: flux-system", "namespace: monitoring-central", "prune: true", "kind: ServiceAccount", "kind: Role", "kind: RoleBinding", `resources: ["prometheusrules", "scrapeconfigs"]`, `resources: ["configmaps"]`} {
		if !strings.Contains(local, s) {
			t.Errorf("own subscription lacks %q:\n%s", s, local)
		}
	}
	if strings.Contains(local, "apiVersion: source.toolkit.fluxcd.io") {
		t.Errorf("own subscription reads the flux-system source, not a GitRepository of its own:\n%s", local)
	}
	remote := string(m[servesPath("luxor", "sfi/staging")].content)
	for _, s := range []string{"kind: GitRepository", "name: hub-staging", "url: ssh://git@gitea.zen.lofi:30022/sfi/staging.git", "secretRef:\n    name: flux-system", "!/flux/hub/luxor/", "serviceAccountName: hub-staging", "path: ./flux/hub/luxor", "prune: true"} {
		if !strings.Contains(remote, s) {
			t.Errorf("staging subscription lacks %q:\n%s", s, remote)
		}
	}
	if strings.Contains(remote, "name: flux-system\n    namespace: flux-system\n  path") {
		t.Errorf("staging subscription must source its own GitRepository:\n%s", remote)
	}
	// every subscription's Kustomization carries no decryption: no
	// secret rides flux/hub/ yet
	for _, c := range []string{local, remote} {
		if strings.Contains(c, "\n  decryption:") {
			t.Errorf("a subscription decrypts nothing:\n%s", c)
		}
	}
	// the kustomization lists both, and a re-run appends a new one
	sk := m[servesKustomization("luxor")]
	if !strings.Contains(string(sk.content), "  - infra.yaml\n  - staging.yaml\n") {
		t.Errorf("serves kustomization:\n%s", sk.content)
	}
	if out, err := sk.edit("resources:\n  - infra.yaml\n"); err != nil || !strings.Contains(out, "  - infra.yaml\n  - staging.yaml\n") || strings.Count(out, "infra.yaml") != 1 {
		t.Errorf("serves kustomization edit: %v\n%s", err, out)
	}
	// the apps root lists serves/, fresh and as an append
	apps := m["flux/apps/luxor/kustomization.yaml"]
	if !strings.Contains(string(apps.content), "  - sops-canary.yaml\n") || !strings.Contains(string(apps.content), "  - serves\n") {
		t.Errorf("hub apps root:\n%s", apps.content)
	}
	if apps.edit == nil {
		t.Fatal("hub apps root needs the append for a tree that exists")
	}
	if out, err := apps.edit("resources:\n  - sops-canary.yaml\n  - monitoring\n"); err != nil || !strings.HasSuffix(out, "  - monitoring\n  - serves\n") {
		t.Errorf("apps root append: %v\n%s", err, out)
	}
	if out, _ := apps.edit("resources:\n  - sops-canary.yaml\n  - serves\n"); strings.Count(out, "- serves") != 1 {
		t.Errorf("apps root append doubled the line:\n%s", out)
	}
	// a hub with no [[serves]] subscribes to its own estate alone
	alone := hubEstate
	alone.Serves = nil
	files, err = renderScaffold("luxor", luxor, alone, "10.1.0.5")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, f := range files {
		if strings.HasPrefix(f.path, servesDir("luxor")+"/") {
			n++
		}
	}
	if n != 2 {
		t.Errorf("a hub serving nobody else: want its own subscription and the kustomization (2 files), got %d", n)
	}
	// a second monitoring hub (luxor-b) that is not this estate's own:
	// its tree carries staging-b's subscription and no own-estate one,
	// the apps root lists serves/, and its own file for THIS estate's
	// hub (luxor) reaches its Alertmanager over the mesh like any fort
	luxorB := luxor
	luxorB.MeshIP = "10.1.0.6"
	files, err = renderScaffold("luxor-b", luxorB, hubEstate, "10.1.0.5")
	if err != nil {
		t.Fatal(err)
	}
	mb := map[string]scaffoldFile{}
	for _, f := range files {
		mb[f.path] = f
	}
	if _, ok := mb[servesPath("luxor-b", "sfi/infra")]; ok {
		t.Errorf("luxor-b is not this estate's hub: no own-estate subscription")
	}
	if sb := string(mb[servesPath("luxor-b", "sfi/staging-b")].content); !strings.Contains(sb, "path: ./flux/hub/luxor-b") || !strings.Contains(sb, "url: ssh://git@gitea.zen.lofi:30022/sfi/staging-b.git") {
		t.Errorf("luxor-b's subscription to staging-b:\n%s", sb)
	}
	if !strings.Contains(string(mb["flux/apps/luxor-b/kustomization.yaml"].content), "  - serves\n") {
		t.Errorf("luxor-b's apps root lists serves/")
	}
	if own := string(mb[hubFilePath("luxor", "luxor-b")].content); !strings.Contains(own, "http://10.1.0.6:30993") {
		t.Errorf("luxor-b's own file for luxor:\n%s", own)
	}
}

// tests/estate is the one-fort fixture estate flake.nix evaluates
// (nixosConfigurations.plain-fort): what `fort scaffold plain-fort
// --write` rendered into it, from the same manifest testdata carries and
// the estate.toml it holds (no hub: the host dir is the whole render). A
// template change re-renders the fixture or fails here.
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
	files, err := renderScaffold("plain-fort", hosts["plain-fort"], est, "")
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
