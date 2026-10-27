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

	// a [libvirt] host is the lab shape (the libvirt backend)
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
var testEstate = estate.Estate{Name: "deployments", Source: estate.Source{Repo: "ssh://git@gitea.zen.lofi:30022/sfi/deployments.git"}}

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
// shape, no fleet surface — on every provider table (forts#142). The two
// tables decide two things and do not depend on each other: [cluster]
// decides k3s and flux, the provider table decides the runtime module
// and the backend.
func TestRenderScaffoldPlain(t *testing.T) {
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
	// Every plain shape, whatever its provider: the baseline and nothing
	// else — the hardened profile, the mesh, deployer with the ops verbs
	// (#647), the shell, the rebuild loop; no k3s, no flux, no fleet dir.
	plain := map[string]string{}
	for _, name := range []string{"plain-fort", "plain-cloud-fort", "plain-metal-fort"} {
		files := render(t, name, hosts[name])
		if len(files) != 2 {
			t.Fatalf("%s: want 2 files (default.nix, secrets.nix), got %d", name, len(files))
		}
		m := byPath(files)
		d := m["nixos/hosts/"+name+"/default.nix"]
		plain[name] = d
		for _, s := range append([]string{"inputs.forts.nixosModules.hardened", "inputs.forts.nixosModules.estate", "inputs.forts.nixosModules.metrics", "inputs.forts.nixosModules.lab-dns", "inputs.forts.nixosModules.ops-deployer", "inputs.forts.nixosModules.wireguard", "inputs.forts.nixosModules.auto-rebuild", `networking.hostName = "` + name + `"`, "zramSwap", "PLAIN FORT"}, operatorDefaults...) {
			if !strings.Contains(d, s) {
				t.Errorf("%s default.nix lacks %q", name, s)
			}
		}
		for _, s := range []string{"k3s-server", "production.nix", "flux-bootstrap", "sfi.k3s", "interactive.nix", "dotfiles.nix", "dev-fort.nix", "lab-host"} {
			if strings.Contains(d, s) {
				t.Errorf("%s default.nix should not carry %q", name, s)
			}
		}
		sec := m["nixos/hosts/"+name+"/secrets.nix"]
		if !strings.Contains(sec, "secrets/"+name+".yaml") || !strings.Contains(sec, "secrets.wireguard-private-key") || !strings.Contains(sec, "No k3s-token") {
			t.Errorf("%s secrets.nix wrong:\n%s", name, sec)
		}
	}
	// The lab VM: the qcow runtime and the static LAN block.
	lab := plain["plain-fort"]
	for _, s := range []string{"inputs.forts.nixosModules.runtime-qemu", "plain-fort-qcow", "deployer@10.1.0.96", `matchConfig.Name = "en*"`, `address = [ "10.0.0.96/24" ]`, "NoCloud cidata"} {
		if !strings.Contains(lab, s) {
			t.Errorf("plain lab default.nix lacks %q", s)
		}
	}
	// The linode: the linode runtime, no LAN block, Lish as the
	// break-glass, and the root-login force upstream linode-config.nix
	// needs. The lab-dns import stays — on a cloud fort it pins the
	// forge's name and forwards nothing (modules/lab-dns.nix).
	cloud := plain["plain-cloud-fort"]
	for _, s := range []string{"inputs.forts.nixosModules.runtime-linode", "plain-cloud-fort-linode", "deployer@10.1.0.95", "{ inputs, lib, ... }:", `services.openssh.settings.PermitRootLogin = lib.mkForce "no";`, "services.resolved.enable = true;", "Lish in the Linode console", "Linode Metadata"} {
		if !strings.Contains(cloud, s) {
			t.Errorf("plain cloud default.nix lacks %q", s)
		}
	}
	for _, s := range []string{"runtime-qemu", "runtime-baremetal", "-qcow", "systemd.network.networks", "matchConfig", "NoCloud"} {
		if strings.Contains(cloud, s) {
			t.Errorf("plain cloud default.nix should not carry %q", s)
		}
	}
	// The machine on the LAN: the baremetal runtime, the LAN block
	// matched by MAC, and no key in the image (#658).
	metal := plain["plain-metal-fort"]
	for _, s := range []string{"inputs.forts.nixosModules.runtime-baremetal", "plain-metal-fort-baremetal", "deployer@10.1.0.94", "{ inputs, ... }:", `matchConfig.MACAddress = "f8:ce:72:02:05:08"`, `address = [ "10.0.0.94/24" ]`, "docs/baremetal.md", "the image carries no key"} {
		if !strings.Contains(metal, s) {
			t.Errorf("plain metal default.nix lacks %q", s)
		}
	}
	for _, s := range []string{"runtime-qemu", "runtime-linode", "-qcow", "linode", "hypervisor's LAN bridge", "lib.mkForce"} {
		if strings.Contains(metal, s) {
			t.Errorf("plain metal default.nix should not carry %q", s)
		}
	}
	// The note `fort scaffold` prints names the provider, so the two
	// decisions are both readable in the dry run.
	for name, want := range map[string]string{
		"plain-fort":       "plain fort shape (qcow runtime, static LAN, deployer + the ops verbs; no k3s/flux)",
		"plain-cloud-fort": "plain fort shape (linode runtime, no LAN, deployer + the ops verbs; no k3s/flux)",
		"plain-metal-fort": "plain fort shape (baremetal runtime, static LAN by MAC, deployer + the ops verbs; no k3s/flux)",
	} {
		files := render(t, name, hosts[name])
		if files[0].note != want {
			t.Errorf("%s note = %q, want %q", name, files[0].note, want)
		}
	}
	// scaffoldShape names the provider first and the cluster decision
	// second, on a plain fort as on a cluster fort.
	for name, want := range map[string]string{
		"plain-fort":       "lab VM, plain fort: no cluster",
		"plain-cloud-fort": "linode us-sea g6-nanode-1, plain fort: no cluster",
		"plain-metal-fort": "lab hardware, plain fort: no cluster",
		"lab-fort":         "lab VM, k3s on sqlite",
	} {
		if got := scaffoldShape(hosts[name]); got != want {
			t.Errorf("%s shape = %q, want %q", name, got, want)
		}
	}
}

// The fixture estate the flake evaluates is scaffold output, host for
// host: the lab VM and the linode of forts#142. Each manifest is the
// testdata manifest of the same name, so the render tests above and the
// nix eval in CI read one fort.
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
	fixtures, err := manifest.Names(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fixtures, ",") != "plain-cloud-fort,plain-fort" {
		t.Fatalf("the fixture estate declares %v — add the new fort to flake.nix and to the nix job of .gitea/workflows/checks.yaml, then name it here", fixtures)
	}
	for _, host := range fixtures {
		if readFile(t, filepath.Join(dir, manifest.Path(host))) != readFile(t, filepath.Join("testdata", manifest.Path(host))) {
			t.Fatalf("%s: the fixture estate's manifest differs from testdata's", host)
		}
		files, err := renderScaffold(host, hosts[host], est)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 2 {
			t.Fatalf("%s: a plain fort is the host dir alone (2 files), got %d", host, len(files))
		}
		for _, f := range files {
			if got := readFile(t, filepath.Join(dir, f.path)); got != string(f.content) {
				t.Errorf("%s: the fixture differs from the render — re-run fort scaffold %s --write in tests/estate", f.path, host)
			}
		}
	}
}

// The github.com fixture (tests/github-estate, forts#1): a cloud cluster
// fort on an estate with no hub, whose repo is on github.com. The host
// dir and the flux tree are the scaffold's render, and the estate names
// no forge anywhere but repo.
func TestGitHubFixtureEstateIsScaffoldOutput(t *testing.T) {
	const dir = "../../tests/github-estate"
	est, err := estate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if est.ForgeKind() != estate.ForgeGitHub || est.Source.Forge != "" || len(est.Hubs) != 0 || !strings.HasPrefix(est.Source.KnownHosts, "github.com ") {
		t.Fatalf("the fixture is tier one: github derived, no hub, github.com's host key; got %+v", est.Source)
	}
	hosts, problems, err := manifest.LoadAll(dir)
	if err != nil || len(problems) > 0 || len(hosts) != 1 {
		t.Fatal(err, problems, hosts)
	}
	files, err := renderScaffold("cloud-fort", hosts["cloud-fort"], est)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 9 {
		t.Fatalf("a cluster fort: the host dir (2) and the flux tree (7), got %d", len(files))
	}
	for _, f := range files {
		if got := readFile(t, filepath.Join(dir, f.path)); got != string(f.content) {
			t.Errorf("%s: the fixture differs from the render — re-run fort scaffold cloud-fort --write in tests/github-estate", f.path)
		}
		if strings.Contains(string(f.content), "gitea") {
			t.Errorf("%s: a github.com estate renders no lab forge", f.path)
		}
	}
}

// The anonymous route (forts#167): an https:// estate renders a
// FluxInstance with no pullSecret — source-controller clones the public
// repo with no credential, and flux-bootstrap.nix makes no git-auth
// secret for it to name — and a host file that names no deploy key. The
// ssh route renders both, byte for byte as before.
func TestRenderScaffoldAnonymousRoute(t *testing.T) {
	hosts, problems, err := manifest.LoadAll("testdata")
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	anon := testEstate
	anon.Source = estate.Source{Repo: "https://github.com/acme/public-estate"}
	if !anon.Anonymous() {
		t.Fatal("an https:// repo is the anonymous route")
	}
	for _, host := range []string{"cloud-fort", "lab-fort"} {
		files, err := renderScaffold(host, hosts[host], anon)
		if err != nil {
			t.Fatal(err)
		}
		ssh := render(t, host, hosts[host])
		for i, f := range files {
			got := string(f.content)
			switch f.path {
			case "flux/clusters/" + host + "/flux-system/flux-instance.yaml":
				if strings.Contains(got, "pullSecret") || !strings.Contains(got, "url: https://github.com/acme/public-estate.git\n") || !strings.HasSuffix(got, "path: flux/clusters/"+host+"\n") {
					t.Errorf("%s: the anonymous route names no pullSecret:\n%s", f.path, got)
				}
				if !strings.Contains(string(ssh[i].content), "pullSecret: flux-system\n") {
					t.Errorf("%s: the ssh route keeps the pullSecret", f.path)
				}
			case "nixos/hosts/" + host + "/default.nix":
				if strings.Contains(got, "flux-git-deploy-key") || !strings.Contains(got, "No deploy key") || !strings.Contains(got, `repoUrl = "https://github.com/acme/public-estate.git"`) {
					t.Errorf("%s: the anonymous host file names no deploy key:\n%s", f.path, got)
				}
				if !strings.Contains(string(ssh[i].content), "flux-git-deploy-key") {
					t.Errorf("%s: the ssh route keeps the deploy key", f.path)
				}
			default:
				if got != string(ssh[i].content) {
					t.Errorf("%s: the route changes nothing else", f.path)
				}
			}
		}
	}
}

// The public fixture (tests/public-estate, forts#167): the same cloud
// cluster fort on the anonymous route — an https:// repo, no host key,
// no hub. The host dir and the flux tree are the scaffold's render, the
// FluxInstance names no pullSecret, and the host's secrets file carries
// no deploy key.
func TestPublicFixtureEstateIsScaffoldOutput(t *testing.T) {
	const dir = "../../tests/public-estate"
	est, err := estate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !est.Anonymous() || est.Source.Auth != "" || est.Source.KnownHosts != "" || est.ForgeKind() != estate.ForgeGitHub || len(est.Hubs) != 0 {
		t.Fatalf("the fixture is the anonymous route, derived: an https:// repo, no auth key, no host key, no hub; got %+v", est.Source)
	}
	hosts, problems, err := manifest.LoadAll(dir)
	if err != nil || len(problems) > 0 || len(hosts) != 1 {
		t.Fatal(err, problems, hosts)
	}
	files, err := renderScaffold("public-fort", hosts["public-fort"], est)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 9 {
		t.Fatalf("a cluster fort: the host dir (2) and the flux tree (7), got %d", len(files))
	}
	for _, f := range files {
		got := readFile(t, filepath.Join(dir, f.path))
		if got != string(f.content) {
			t.Errorf("%s: the fixture differs from the render — re-run fort scaffold public-fort --write in tests/public-estate", f.path)
		}
		if strings.Contains(got, "gitea") || strings.Contains(got, "pullSecret") || strings.Contains(got, "flux-git-deploy-key") {
			t.Errorf("%s: the anonymous route renders no lab forge, no pullSecret and no deploy key", f.path)
		}
	}
	if secrets := readFile(t, filepath.Join(dir, "nixos/secrets/public-fort.yaml")); strings.Contains(secrets, keyDeploy) || !strings.Contains(secrets, keyAge) {
		t.Errorf("the fixture's secrets file carries the cluster key and no deploy key:\n%s", secrets)
	}
}
