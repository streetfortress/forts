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
	for _, m := range []map[string]string{cloud, lab} {
		if len(m) != 2+len(fleet)+1 {
			t.Errorf("want %d files, got %d: %v", 2+len(fleet)+1, len(m), keys(m))
		}
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
		// luxor was bootstrapped by hand and carries a bespoke apps layer
		// (databasus-configs) and pre-module comments — the exception its
		// manifest's header records, not a template drift.
		if name == "luxor" {
			continue
		}
		for _, f := range render(t, name, h) {
			if !strings.HasPrefix(f.path, "fleet/clusters/") {
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
