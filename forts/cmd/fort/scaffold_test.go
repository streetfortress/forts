package main

import (
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/inventory"
)

// The two shapes from the fixture inventory: a lab VM (qcow runtime,
// static LAN) and a cloud fort (linode runtime, split DNS over the mesh,
// zram, no LAN block) — both flux clusters, so both get the fleet dirs.
func TestRenderScaffold(t *testing.T) {
	hosts, err := inventory.Load("testdata/inventory.yaml")
	if err != nil {
		t.Fatal(err)
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
	for _, s := range []string{"qcow-runtime", "systemd.network.networks", "qemu-guest", "../../modules/interactive.nix"} {
		if strings.Contains(c, s) {
			t.Errorf("cloud default.nix should not carry %q", s)
		}
	}
	l := lab["nixos/hosts/lab-fort/default.nix"]
	for _, s := range []string{"../../modules/qcow-runtime.nix", `address = [ "10.0.0.99/24" ]`, "../../modules/interactive.nix", "services.k3s.clusterInit = true"} {
		if !strings.Contains(l, s) {
			t.Errorf("lab default.nix lacks %q", s)
		}
	}
	for _, s := range []string{"linode-runtime", "zramSwap", "production.nix"} {
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

	// provider: libvirt is still the lab shape (fort adopt, the libvirt backend)
	h2 := hosts["lab-fort"]
	h2.Provider = "libvirt"
	if got := string(render(t, "lab-fort", h2)[0].content); !strings.Contains(got, "qcow-runtime") || strings.Contains(got, "linode") {
		t.Errorf("libvirt host should render the lab shape")
	}

	// a flux cluster off the mesh is refused (MESH_IP is mandatory, #99)
	h := hosts["lab-fort"]
	h.WgVpnIP = ""
	if _, err := renderScaffold("lab-fort", h); err == nil {
		t.Errorf("flux_cluster without wg_vpn_ip should be refused")
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
	hosts, err := inventory.Load(filepath.Join(repo, "provisioning/inventory.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for name, h := range hosts {
		// luxor was bootstrapped by hand and carries a bespoke apps layer
		// (databasus-configs) and pre-module comments — the exception the
		// inventory comment records, not a template drift.
		if !h.FluxCluster || name == "luxor" {
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

func render(t *testing.T, host string, h inventory.Host) []scaffoldFile {
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
