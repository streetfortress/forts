package main

import (
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

// The two shapes from the fixture manifests: a lab VM (qcow runtime,
// static LAN) and a cloud fort (linode runtime, split DNS over the mesh,
// zram, no LAN block) — both get the fleet dirs (every fort is a flux cluster).
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
	// nix host dir (2) + fleet/clusters (fleet) + the apps root + the
	// cluster's monitoring on the hub and the hub kustomization append.
	for _, m := range []map[string]string{cloud, lab} {
		if len(m) != 2+len(fleet)+1+2 {
			t.Errorf("want %d files, got %d: %v", 2+len(fleet)+1+2, len(m), keys(m))
		}
	}
	// The per-cluster file: the dead man's switch on both, the expected
	// backup leg only where the profile promises one, and the edge
	// Alertmanager over the mesh (the hub's own is cluster DNS).
	for name, m := range map[string]map[string]string{"cloud-fort": cloud, "lab-fort": lab} {
		got := m[clusterMonitoringPath(name)]
		for _, s := range []string{`absent(up{cluster="` + name + `"})`, `absent_over_time({job="fluent-bit", cluster="` + name + `"}[15m])`, "uid: am-edge-" + name, "loki_rule:", "grafana_datasource:"} {
			if !strings.Contains(got, s) {
				t.Errorf("%s monitoring lacks %q:\n%s", name, s, got)
			}
		}
		if _, ok := m[clusterMonitoringKustomization]; !ok {
			t.Errorf("%s: no hub kustomization append", name)
		}
	}
	if !strings.Contains(cloud[clusterMonitoringPath("cloud-fort")], "PVBackupNotConfigured") {
		t.Errorf("production profile must render PVBackupNotConfigured")
	}
	if strings.Contains(lab[clusterMonitoringPath("lab-fort")], "PVBackupNotConfigured") {
		t.Errorf("lab profile must not render PVBackupNotConfigured")
	}
	if !strings.Contains(cloud[clusterMonitoringPath("cloud-fort")], "url: http://10.1.0.98:30993") {
		t.Errorf("a fort's edge Alertmanager is reached over the mesh")
	}
	for _, f := range want {
		if _, ok := cloud[filepath.Join("nixos/hosts/cloud-fort", f)]; !ok {
			t.Errorf("cloud: missing %s", f)
		}
	}
	for _, f := range fleet {
		if _, ok := cloud[filepath.Join("fleet/clusters/cloud-fort", f)]; !ok {
			t.Errorf("cloud: missing fleet/clusters/cloud-fort/%s", f)
		}
	}

	c := cloud["nixos/hosts/cloud-fort/default.nix"]
	for _, s := range []string{"../../modules/linode-runtime.nix", "services.resolved.enable = true", `"~lofi"`, "zramSwap", "PermitRootLogin = lib.mkForce", "../../modules/production.nix", "../../modules/flux-bootstrap.nix", `networking.hostName = "cloud-fort"`, "deployer@10.1.0.98", "linode VPS (us-sea"} {
		if !strings.Contains(c, s) {
			t.Errorf("cloud default.nix lacks %q", s)
		}
	}
	for _, s := range []string{"qcow-runtime", "systemd.network.networks", "qemu-guest", "../../modules/interactive.nix", "../../modules/dotfiles.nix"} {
		if strings.Contains(c, s) {
			t.Errorf("cloud default.nix should not carry %q", s)
		}
	}
	l := lab["nixos/hosts/lab-fort/default.nix"]
	for _, s := range []string{"../../modules/qcow-runtime.nix", `address = [ "10.0.0.99/24" ]`, "../../modules/production.nix", "../../modules/flux-bootstrap.nix", "deployer@10.1.0.99", "services.k3s.clusterInit = true", "zramSwap"} {
		if !strings.Contains(l, s) {
			t.Errorf("lab default.nix lacks %q", s)
		}
	}
	for _, s := range []string{"linode-runtime", "../../modules/interactive.nix", "../../modules/dotfiles.nix"} {
		if strings.Contains(l, s) {
			t.Errorf("lab default.nix should not carry %q", s)
		}
	}
	for name, m := range map[string]map[string]string{"cloud-fort": cloud, "lab-fort": lab} {
		sec := m["nixos/hosts/"+name+"/secrets.nix"]
		if !strings.Contains(sec, "secrets/"+name+".yaml") || !strings.Contains(sec, "secrets.wireguard-private-key") {
			t.Errorf("%s secrets.nix wrong:\n%s", name, sec)
		}
		cv := m["fleet/clusters/"+name+"/cluster-vars.yaml"]
		if !strings.Contains(cv, "CLUSTER_NAME: "+name) {
			t.Errorf("%s cluster-vars wrong:\n%s", name, cv)
		}
		if !strings.Contains(m["fleet/clusters/"+name+"/flux-system/flux-instance.yaml"], "path: fleet/clusters/"+name) {
			t.Errorf("%s flux-instance sync path wrong", name)
		}
		if !strings.Contains(m["fleet/clusters/"+name+"/apps.yaml"], "path: ./fleet/apps/"+name) {
			t.Errorf("%s apps.yaml path wrong", name)
		}
		if !strings.Contains(m["fleet/apps/"+name+"/kustomization.yaml"], "- sops-canary.yaml") {
			t.Errorf("%s apps root kustomization lacks the canary", name)
		}
	}
	if !strings.Contains(cloud["fleet/clusters/cloud-fort/cluster-vars.yaml"], "MESH_IP: 10.1.0.98") {
		t.Errorf("cloud MESH_IP wrong")
	}

	// a [libvirt] host is the lab shape (fort adopt, the libvirt backend)
	if got := lab["nixos/hosts/lab-fort/default.nix"]; !strings.Contains(got, "qcow-runtime") || strings.Contains(got, "linode") {
		t.Errorf("libvirt host should render the lab shape")
	}

	// a fort off the mesh is refused (MESH_IP is mandatory, #99)
	// The profile reaches the host file as a declaration, not a toggle —
	// and "none" leaves no trace at all, so a host that is not an origin
	// reads as one rather than as a disabled module.
	for name, m := range map[string]map[string]string{"cloud-fort": cloud, "lab-fort": lab} {
		got := m["nixos/hosts/"+name+"/default.nix"]
		want := map[string]string{"cloud-fort": "production", "lab-fort": "lab"}[name]
		if !strings.Contains(got, "../../modules/pv-backup.nix") {
			t.Errorf("%s: no pv-backup import", name)
		}
		if !strings.Contains(got, `profile = "`+want+`";`) {
			t.Errorf("%s: want sfi.pv-backup profile %q", name, want)
		}
	}
	none := hosts["lab-fort"]
	none.Backups.Profile = "none"
	nf := byPath(render(t, "lab-fort", none))["nixos/hosts/lab-fort/default.nix"]
	if strings.Contains(nf, "pv-backup") {
		t.Errorf(`profile "none" must render no pv-backup wiring at all:\n%s`, nf)
	}

	h := hosts["lab-fort"]
	h.MeshIP = ""
	if _, err := renderScaffold("lab-fort", h); err == nil {
		t.Errorf("a manifest without mesh_ip should be refused")
	}

	// a [baremetal] host is the lab shape on hardware (#287): the
	// baremetal runtime instead of qcow, the LAN block matched by MAC
	// when the manifest names one, everything else as the lab VM.
	metal := byPath(render(t, "metal-fort", hosts["metal-fort"]))
	m := metal["nixos/hosts/metal-fort/default.nix"]
	for _, s := range []string{"../../modules/baremetal-runtime.nix", "metal-fort-baremetal", `matchConfig.MACAddress = "f8:ce:72:02:05:07"`, `address = [ "10.0.0.97/24" ]`, "../../modules/production.nix", "../../modules/flux-bootstrap.nix", "deployer@10.1.0.97", "services.k3s.clusterInit = false", "zramSwap", `profile = "production";`, "docs/forts/baremetal.md"} {
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

// The committed fleet/clusters dirs are what the templates render — the
// generator reproduces the tree the playbook-era cluster-init laid down (modulo nothing:
// these files carry no hand edits today).
func TestScaffoldMatchesCommittedClusters(t *testing.T) {
	repo, err := repoRootFrom(mustGetwd(t))
	if err != nil {
		t.Skip("not inside the deployments checkout")
	}
	hosts, problems, err := manifest.LoadAll(repo)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	for name, h := range hosts {
		for _, f := range render(t, name, h) {
			// The cluster's monitoring on the hub is rendered for every
			// flux cluster, luxor's own included; fleet/clusters/luxor
			// was bootstrapped by hand and carries a bespoke apps layer
			// (databasus-configs) and pre-module comments — the exception
			// its manifest's header records, not a template drift.
			isMon := strings.HasPrefix(f.path, clusterMonitoringDir+"/")
			if !isMon && (name == "luxor" || !strings.HasPrefix(f.path, "fleet/clusters/")) {
				continue
			}
			if got := readFile(t, filepath.Join(repo, f.path)); got != string(f.content) {
				t.Errorf("%s differs from the render", f.path)
			}
		}
	}
}

func render(t *testing.T, host string, h manifest.Host) []scaffoldFile {
	t.Helper()
	files, err := renderScaffold(host, h)
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

// A dev fort ([dev], #413) renders the host dir — the dev shape, no
// fleet surface — plus its pull-shaped file on the hub (#437) and the
// kustomization line that applies it.
func TestRenderScaffoldDev(t *testing.T) {
	hosts, problems, err := manifest.LoadAll("testdata")
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	files := render(t, "dev-fort", hosts["dev-fort"])
	if len(files) != 4 {
		t.Fatalf("want 4 files (default.nix, secrets.nix, clusters/dev-fort.yaml, + kustomization line), got %d", len(files))
	}
	m := map[string]string{}
	for _, f := range files {
		m[f.path] = string(f.content)
	}
	d := m["nixos/hosts/dev-fort/default.nix"]
	for _, s := range []string{"../../modules/qcow-runtime.nix", "../../modules/dev-fort.nix", "../../modules/wireguard.nix", "../../modules/auto-rebuild.nix", "agent@10.1.0.96", `address = [ "10.0.0.96/24" ]`, `networking.hostName = "dev-fort"`, "zramSwap", "DEV PROFILE"} {
		if !strings.Contains(d, s) {
			t.Errorf("dev default.nix lacks %q", s)
		}
	}
	for _, s := range []string{"k3s-server", "production.nix", "flux-bootstrap", "pv-backup", "clusterInit", "deployer", "interactive.nix", "dotfiles.nix"} {
		if strings.Contains(d, s) {
			t.Errorf("dev default.nix should not carry %q", s)
		}
	}
	sec := m["nixos/hosts/dev-fort/secrets.nix"]
	if !strings.Contains(sec, "secrets/dev-fort.yaml") || !strings.Contains(sec, "secrets.wireguard-private-key") || !strings.Contains(sec, "No k3s-token") {
		t.Errorf("dev secrets.nix wrong:\n%s", sec)
	}
	mon := m[clusterMonitoringPath("dev-fort")]
	// One ScrapeConfig per leg: the operator owns `job` (relabelled to
	// jobName after the target labels), so the node leg has to be its own
	// CR to file under host-node-exporter — the Host Overview's job (#442).
	for _, s := range []string{"kind: ScrapeConfig", "name: dev-fort-dev-fort-node", "jobName: host-node-exporter", `targets: ["10.1.0.96:9100"]`, "name: dev-fort-dev-fort-claude", "jobName: claude-code", `targets: ["10.1.0.96:9464"]`, "component: node", "component: otel", `cluster: dev-fort`, "alert: DevFortDown", "alert: DevFortCollectorDown", `{job="systemd", cluster="dev-fort"}`} {
		if !strings.Contains(mon, s) {
			t.Errorf("dev hub file lacks %q", s)
		}
	}
	for _, s := range []string{"ClusterStreamSilent", "grafana_datasource", "fluent-bit", "30993"} {
		if strings.Contains(mon, s) {
			t.Errorf("dev hub file should not carry %q (that is a cluster's)", s)
		}
	}
	if strings.Contains(mon, "\n        job:") {
		t.Errorf("dev hub file sets job as a target label — the operator overrides it with jobName")
	}
	if strings.Count(mon, "kind: ScrapeConfig") != 2 {
		t.Errorf("dev hub file wants one ScrapeConfig per leg, got %d", strings.Count(mon, "kind: ScrapeConfig"))
	}
	var hasEdit bool
	for _, f := range files {
		if f.path == clusterMonitoringKustomization && f.edit != nil {
			hasEdit = true
			out, err := f.edit("resources:\n  - clusters/luxor.yaml\n")
			if err != nil || !strings.Contains(out, "  - clusters/dev-fort.yaml\n") {
				t.Errorf("kustomization edit: %v\n%s", err, out)
			}
		}
	}
	if !hasEdit {
		t.Error("a dev fort's file on the hub needs its kustomization line")
	}
}
