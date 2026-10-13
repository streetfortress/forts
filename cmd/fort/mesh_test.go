package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

// A small fleet: a plain fort, a lab cluster fort, two cloud forts sharing
// a stem (no alias for either), a fort named outside the convention, two
// hand-managed peers and a roaming client.
const testPeers = `{
  "peers": {
    "soundship": {"wg_vpn_ip": "10.1.0.1", "public_key": "SS="},
    "devbox": {"wg_vpn_ip": "10.1.0.3", "public_key": "DB="},
    "luxor": {"wg_vpn_ip": "10.1.0.5", "wg_endpoint_ip": "10.0.0.5", "public_key": "LX="},
    "sfi-dev-abc1234": {"wg_vpn_ip": "10.1.0.61", "wg_endpoint_ip": "10.0.0.61", "public_key": "DV="},
    "sfi-prod-1111111": {"wg_vpn_ip": "10.1.0.54", "wg_endpoint_ip": "p1.example.cloud", "public_key": "P1="},
    "sfi-prod-2222222": {"wg_vpn_ip": "10.1.0.55", "wg_endpoint_ip": "p2.example.cloud", "public_key": "P2="}
  },
  "clients": {
    "laptop": {"wg_vpn_ip": "10.1.0.100", "public_key": "LT="}
  },
  "remotes": {
    "forge": {"wg_vpn_ip": "10.1.0.4", "wg_endpoint_ip": "10.0.0.4", "wg_endpoint_port": 51821, "public_key": "FG="}
  }
}
`

var testManifests = map[string]string{
	"luxor":            "mesh_ip = \"10.1.0.5\"\n[cluster]\ndatastore = \"etcd\"\n[shape]\nvcpus = 4\nmemory = 16\ndisk = \"450G\"\n[libvirt]\nip = \"10.0.0.5\"\n",
	"sfi-dev-abc1234":  "mesh_ip = \"10.1.0.61\"\n[shape]\nvcpus = 4\nmemory = 8\ndisk = \"80G\"\n[libvirt]\nip = \"10.0.0.61\"\n",
	"sfi-prod-1111111": "mesh_ip = \"10.1.0.54\"\n[cluster]\n[dns]\nzone = \"example.cloud\"\nname = \"p1\"\n[linode]\nregion = \"us-sea\"\ntype = \"g6-standard-2\"\n",
	"sfi-prod-2222222": "mesh_ip = \"10.1.0.55\"\n[cluster]\n[dns]\nzone = \"example.cloud\"\nname = \"p2\"\n[linode]\nregion = \"us-sea\"\ntype = \"g6-standard-2\"\n",
}

func testHosts(t *testing.T) map[string]manifest.Host {
	t.Helper()
	hosts := map[string]manifest.Host{}
	for name, raw := range testManifests {
		h, err := manifest.Parse(name, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		hosts[name] = h
	}
	return hosts
}

// testTree lays the fleet down in a tmp root: peers.json + manifests,
// so the disk-reading paths (renderSSHConfigFrom, meshFindings,
// planSSHConfig) run against it.
func testTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755)
		os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644)
	}
	write(peersPath, testPeers)
	for name, raw := range testManifests {
		write(manifest.Path(name), raw)
	}
	return root
}

func TestRenderSSHConfig(t *testing.T) {
	m, err := parseMesh(testPeers)
	if err != nil {
		t.Fatal(err)
	}
	hosts := testHosts(t)
	got := renderSSHConfig(m, hosts)
	if got != renderSSHConfig(m, hosts) {
		t.Fatal("render is not deterministic")
	}
	for _, want := range []string{
		// the plain fort: role alias, deployer, key by name
		"# plain fort, libvirt — the sfi-dev role\nHost sfi-dev sfi-dev-abc1234\n  Hostname 10.1.0.61\n  User deployer\n  HostKeyAlias sfi-dev-abc1234\n",
		// a cluster fort outside the naming convention: no alias
		"# cluster fort, libvirt\nHost luxor\n  Hostname 10.1.0.5\n  User deployer\n  HostKeyAlias luxor\n",
		// two forts on one stem: neither gets the alias
		"# cluster fort, linode\nHost sfi-prod-1111111\n  Hostname 10.1.0.54\n",
		"# cluster fort, linode\nHost sfi-prod-2222222\n  Hostname 10.1.0.55\n",
		// hand-managed peers: steven, no HostKeyAlias
		"# hand-managed peer (no manifest)\nHost devbox\n  Hostname 10.1.0.3\n  User steven\n\n",
		"Host soundship\n  Hostname 10.1.0.1\n  User steven\n",
		"Include ~/deployments/mesh/ssh_config",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "sfi-prod ") || strings.Contains(got, "Host sfi-prod\n") {
		t.Errorf("sfi-prod alias with two prod forts:\n%s", got)
	}
	if strings.Contains(got, "laptop") {
		t.Errorf("a roaming client is not an ssh target:\n%s", got)
	}
	if strings.Index(got, "Host devbox") > strings.Index(got, "Host luxor") || strings.Index(got, "Host luxor") > strings.Index(got, "Host sfi-dev") {
		t.Errorf("blocks not in name order:\n%s", got)
	}
	// one prod fort gone: the alias returns
	delete(hosts, "sfi-prod-2222222")
	delete(m.Peers, "sfi-prod-2222222")
	if got := renderSSHConfig(m, hosts); !strings.Contains(got, "Host sfi-prod sfi-prod-1111111\n") {
		t.Errorf("alias not restored with one prod fort:\n%s", got)
	}
	if got := sshConfigLine("sfi-dev-abc1234", hosts, "10.1.0.61"); got != "Host sfi-dev sfi-dev-abc1234 → deployer@10.1.0.61" {
		t.Errorf("summary line: %q", got)
	}
}

func TestRoleStem(t *testing.T) {
	for host, want := range map[string]string{
		"sfi-prod-439bf72":  "sfi-prod",
		"sfi-stage-d95cc36": "sfi-stage",
		"sfi-dev-831c637":   "sfi-dev",
		"sfi-fort-zenbook":  "",
		"luxor":             "",
		"sfi-fort-e33e2d5":  "sfi-fort",
		"x-abcdefg":         "", // not hex
	} {
		if got := roleStem(host); got != want {
			t.Errorf("roleStem(%s) = %q, want %q", host, got, want)
		}
	}
}

func TestKnownHosts(t *testing.T) {
	pub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx6GXsI2LCgCru4Jm5qAV4xKqMfWHKrmW2pVHvG6GGa sfi-dev-abc1234\n"
	text := knownHostsSet("", "sfi-dev-abc1234", "10.1.0.61", pub)
	if !strings.HasPrefix(text, "# SSH host keys of the forts") || !strings.HasSuffix(text, "\nsfi-dev-abc1234,10.1.0.61 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx6GXsI2LCgCru4Jm5qAV4xKqMfWHKrmW2pVHvG6GGa\n") {
		t.Errorf("first line:\n%s", text)
	}
	if again := knownHostsSet(text, "sfi-dev-abc1234", "10.1.0.61", pub); again != text {
		t.Errorf("set is not idempotent:\n%s\n---\n%s", text, again)
	}
	text = knownHostsSet(text, "luxor", "10.1.0.5", "ssh-ed25519 LUXOR luxor")
	entries := parseKnownHosts(text)
	if len(entries) != 2 || entries["luxor"].ip != "10.1.0.5" || entries["luxor"].key != "ssh-ed25519 LUXOR" {
		t.Errorf("entries: %+v", entries)
	}
	if strings.Index(text, "\nluxor,") > strings.Index(text, "\nsfi-dev-abc1234,") {
		t.Errorf("not in name order:\n%s", text)
	}
	// a re-keyed fort replaces its line, never adds a second
	text = knownHostsSet(text, "luxor", "10.1.0.5", "ssh-ed25519 LUXOR2 luxor")
	if strings.Count(text, "luxor,") != 1 || !strings.Contains(text, "ssh-ed25519 LUXOR2\n") {
		t.Errorf("replace:\n%s", text)
	}
	text = knownHostsRemove(text, "sfi-dev-abc1234")
	if strings.Contains(text, "sfi-dev") || !strings.Contains(text, "luxor,10.1.0.5 ssh-ed25519 LUXOR2") {
		t.Errorf("remove:\n%s", text)
	}
	if got := knownHostsRemove(text, "luxor"); got != knownHostsHeader {
		t.Errorf("empty file is the header:\n%s", got)
	}
}

func TestRenderWGConf(t *testing.T) {
	m, err := parseMesh(testPeers)
	if err != nil {
		t.Fatal(err)
	}
	// a peer with an endpoint: dials the ones with endpoints, is dialed by the rest
	conf, err := renderWGConf(m, "wg0", "luxor")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[Interface]\nAddress = 10.1.0.5/24\nListenPort = 51820\n",
		"#   LX=\n", // its own public key, for the operator to compare with the app's
		"[Peer]\n# sfi-prod-1111111\nPublicKey = P1=\nAllowedIPs = 10.1.0.54/32\nEndpoint = p1.example.cloud:51820\nPersistentKeepalive = 25\n",
		"[Peer]\n# devbox\nPublicKey = DB=\nAllowedIPs = 10.1.0.3/32\n# no fixed endpoint: it dials this host",
		"[Peer]\n# laptop (roaming client: no endpoint, no keepalive)\nPublicKey = LT=\nAllowedIPs = 10.1.0.100/32\n",
		"[Peer]\n# forge (linked from another estate)\nPublicKey = FG=\nAllowedIPs = 10.1.0.4/32\nEndpoint = 10.0.0.4:51821\nPersistentKeepalive = 25\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("luxor: missing %q in:\n%s", want, conf)
		}
	}
	if !strings.HasPrefix(conf, "# wg0 for luxor") {
		t.Errorf("luxor header:\n%s", conf)
	}
	if c, _ := renderWGConf(m, "wg-second", "luxor"); !strings.HasPrefix(c, "# wg-second for luxor") {
		t.Errorf("named interface header:\n%s", c)
	}
	if strings.Contains(conf, "PrivateKey =") || strings.Contains(conf, "PublicKey = LX=") {
		t.Errorf("luxor: a PrivateKey line, or itself as a peer:\n%s", conf)
	}
	// a peer with no endpoint of its own facing another without one
	conf, _ = renderWGConf(m, "wg0", "devbox")
	if !strings.Contains(conf, "# soundship\nPublicKey = SS=\nAllowedIPs = 10.1.0.1/32\n# no fixed endpoint on either side") {
		t.Errorf("devbox↔soundship:\n%s", conf)
	}
	// a roaming client: no ListenPort, no clients, the DDNS note
	conf, err = renderWGConf(m, "wg0", "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(conf, "ListenPort") || strings.Contains(conf, "roaming client:") || !strings.Contains(conf, "Address = 10.1.0.100/24\n") {
		t.Errorf("laptop interface:\n%s", conf)
	}
	if !strings.Contains(conf, "# soundship\nPublicKey = SS=\nAllowedIPs = 10.1.0.1/32\n# no fixed endpoint in peers.json: a roaming client") || !strings.Contains(conf, "Endpoint = 10.0.0.5:51820") {
		t.Errorf("laptop peers:\n%s", conf)
	}
	if _, err := renderWGConf(m, "wg0", "macmini"); err == nil || !strings.Contains(err.Error(), "register it first") {
		t.Errorf("unknown member: %v", err)
	}
	// a roaming client dials the linked hosts too
	if !strings.Contains(conf, "# forge (linked from another estate)") || !strings.Contains(conf, "Endpoint = 10.0.0.4:51821") {
		t.Errorf("laptop remotes:\n%s", conf)
	}
	// a remote with no endpoint is refused by the parser
	if _, err := parseMesh(`{"peers":{},"remotes":{"x":{"wg_vpn_ip":"10.1.0.9","public_key":"X="}}}`); err == nil || !strings.Contains(err.Error(), "remotes.x has no wg_endpoint_ip") {
		t.Errorf("endpointless remote: %v", err)
	}
}

func TestMeshSubnetFindings(t *testing.T) {
	root := testTree(t)
	hosts := testHosts(t)
	if out := meshSubnetFindings(root, estate.Estate{}, hosts); out != nil {
		t.Errorf("no mesh declared: want nothing, got %v", out)
	}
	out := meshSubnetFindings(root, estate.Estate{Mesh: "10.1.0.0/24"}, hosts)
	if len(out) != 1 || out[0].level != "ok" || !strings.Contains(out[0].msg, "8 address(es)") {
		t.Errorf("inside: %+v", out)
	}
	out = meshSubnetFindings(root, estate.Estate{Mesh: "10.2.0.0/24"}, hosts)
	errs := 0
	for _, f := range out {
		if f.level == "error" {
			errs++
		}
	}
	// 6 peers + 1 client + 1 remote + 4 manifests, every one outside
	if errs != 12 {
		t.Errorf("outside: want 12 errors, got %d:\n%+v", errs, out)
	}
}

// The zone (forts#45): one ok line naming it and counting the names
// peers.json gives it; a host file that pins a .lofi name by hand, or
// routes .lofi over the mesh, is a warning per line — and the forge URL
// in auto-rebuild's repoUrl is not one.
func TestZoneFindings(t *testing.T) {
	root := testTree(t)
	hosts := testHosts(t)
	est := estate.Estate{Name: "deployments"}
	fs := zoneFindings(root, est, hosts)
	if len(fs) != 1 || fs[0].level != "ok" || !strings.Contains(fs[0].msg, "zone deployments.mesh — 7 name(s)") {
		t.Errorf("clean tree: %+v", fs)
	}
	write := func(rel, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755)
		os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644)
	}
	write("nixos/hosts/luxor/default.nix", "{ inputs, ... }:\n{\n  # gitea.zen.lofi is pinned by lab-dns.nix, not here\n  sfi.auto-rebuild.repoUrl = \"ssh://git@gitea.zen.lofi:30022/sfi/deployments.git\";\n  networking.hosts.\"10.1.0.4\" = [ \"gitea.zen.lofi\" ]; # the old pin\n  networking.extraHosts = ''\n    10.1.0.1 soundship.lofi\n    10.1.0.5 luxor.lofi\n  '';\n  networking.hosts = {\n    \"10.1.0.61\" = [ \"dev.example.org\" ];\n  };\n}\n")
	write("nixos/hosts/sfi-prod-1111111/default.nix", "{\n  services.resolved.enable = true;\n  networking.wg-quick.interfaces.wg0.dns = [ \"10.1.0.1\" \"~lofi\" ];\n}\n")
	write("nixos/hosts/sfi-prod-2222222/default.nix", "{\n  sfi.lab-dns.giteaAddress = \"10.1.0.4\"; # the forge's address, not a .lofi line\n}\n")
	fs = zoneFindings(root, est, hosts)
	var lines []string
	for _, f := range fs {
		if f.level != "warning" {
			t.Errorf("want warnings only, got %+v", f)
		}
		lines = append(lines, f.dir+":"+f.msg[:7])
	}
	if want := []string{"nixos/hosts/luxor/default.nix:line 5 ", "nixos/hosts/luxor/default.nix:line 7 ", "nixos/hosts/luxor/default.nix:line 8 ", "nixos/hosts/sfi-prod-1111111/default.nix:line 3 "}; strings.Join(lines, ",") != strings.Join(want, ",") {
		t.Errorf("hand lines: got %v, want %v\n%+v", lines, want, fs)
	}
}

// The forge's address (forts#26): derived from [hubs.forge] and
// peers.json, so the gate reports what a fort will derive, the two ways
// the derivation reads nothing, and a host file that sets the address by
// hand.
func TestForgeFindings(t *testing.T) {
	root := testTree(t)
	hosts := testHosts(t)
	const ownRepo = "ssh://git@gitea.zen.lofi:30022/sfi/deployments"
	const otherRepo = "ssh://git@gitea.zen.lofi:30022/sfi/infra"
	est := func(fort, hubEstate string) estate.Estate {
		e := estate.Estate{Name: "deployments", Repo: ownRepo}
		if fort != "" {
			e.Hubs = map[string]estate.Hub{estate.RoleForge: {Estate: hubEstate, Fort: fort, Name: "gitea.zen.lofi"}}
		}
		return e
	}
	only := func(fs []finding) finding {
		t.Helper()
		if len(fs) != 1 {
			t.Fatalf("want one finding, got %+v", fs)
		}
		return fs[0]
	}

	// No forge hub: a warning, and no address to derive.
	if f := only(forgeFindings(root, est("", ""), hosts)); f.level != "warning" || f.dir != estate.Path || !strings.Contains(f.msg, "no [hubs.forge]") {
		t.Errorf("no hub: %+v", f)
	}
	// A forge in another estate: its remotes entry is its address here.
	if f := only(forgeFindings(root, est("forge", otherRepo), hosts)); f.level != "ok" || !strings.Contains(f.msg, "forge forge at 10.1.0.4 (remotes.forge") {
		t.Errorf("linked forge: %+v", f)
	}
	// A forge that is a fort of this estate: its peers entry.
	if f := only(forgeFindings(root, est("luxor", ownRepo), hosts)); f.level != "ok" || !strings.Contains(f.msg, "forge luxor at 10.1.0.5 (peers.luxor") {
		t.Errorf("in-estate forge: %+v", f)
	}
	// Named and in neither table: the error names the entry to add, and
	// says `remotes` because the forge is another estate's fort.
	f := only(forgeFindings(root, est("zenbook", otherRepo), hosts))
	if f.level != "error" || f.dir != peersPath || !strings.Contains(f.msg, "add remotes.zenbook") {
		t.Errorf("unresolvable forge: %+v", f)
	}
	// A fort of this estate with no manifest: a hub is a fort, and the
	// address still derives.
	fs := forgeFindings(root, est("soundship", ownRepo), hosts)
	if len(fs) != 2 || fs[0].level != "ok" || fs[1].level != "error" || !strings.Contains(fs[1].msg, "forts/soundship.toml does not exist") {
		t.Errorf("hub without a manifest: %+v", fs)
	}
	// A host file that sets the address by hand: one warning per line,
	// naming what the estate derives.
	os.MkdirAll(filepath.Join(root, "nixos/hosts/luxor"), 0o755)
	os.WriteFile(filepath.Join(root, "nixos/hosts/luxor/default.nix"),
		[]byte("{\n  # sfi.lab-dns.giteaAddress is derived now\n  sfi.lab-dns.giteaAddress = \"10.1.0.9\";\n}\n"), 0o644)
	fs = forgeFindings(root, est("forge", otherRepo), hosts)
	if len(fs) != 2 || fs[1].level != "warning" || fs[1].dir != "nixos/hosts/luxor/default.nix" ||
		!strings.Contains(fs[1].msg, "line 3 sets the forge's address by hand") ||
		!strings.Contains(fs[1].msg, "the estate derives 10.1.0.4") {
		t.Errorf("hand line: %+v", fs)
	}
	// The same line with no forge hub: the warning says to declare one.
	fs = forgeFindings(root, est("", ""), hosts)
	if len(fs) != 2 || !strings.Contains(fs[1].msg, "declare [hubs.forge]") {
		t.Errorf("hand line, no hub: %+v", fs)
	}
}

func TestMeshFindings(t *testing.T) {
	root := testTree(t)
	hosts, problems, err := manifest.LoadAll(root)
	if err != nil || len(problems) > 0 {
		t.Fatalf("manifests: %v %v", err, problems)
	}
	levels := func(fs []finding) map[string]int {
		out := map[string]int{}
		for _, f := range fs {
			out[f.level]++
		}
		return out
	}
	// nothing rendered yet: ssh_config missing is the error, no key lines is silence
	// (no file) — the warnings come once the file exists
	fs := meshFindings(root, hosts)
	if l := levels(fs); l["error"] != 1 || !strings.Contains(fs[0].msg, "missing") {
		t.Errorf("empty tree: %+v", fs)
	}
	m, _ := parseMesh(testPeers)
	write := func(rel, content string) {
		os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644)
	}
	write(sshConfigPath, renderSSHConfig(m, hosts))
	kh := ""
	for _, h := range []string{"luxor", "sfi-dev-abc1234", "sfi-prod-1111111", "sfi-prod-2222222"} {
		kh = knownHostsSet(kh, h, m.Peers[h].IP, "ssh-ed25519 K"+h)
	}
	write(knownHostsPath, kh)
	if fs := meshFindings(root, hosts); levels(fs)["error"] != 0 || levels(fs)["warning"] != 0 || levels(fs)["ok"] != 2 {
		t.Errorf("current tree: %+v", fs)
	}
	// a hand edit to ssh_config
	write(sshConfigPath, renderSSHConfig(m, hosts)+"\nHost extra\n")
	if fs := meshFindings(root, hosts); levels(fs)["error"] != 1 || !strings.Contains(fs[0].msg, "stale") {
		t.Errorf("stale ssh_config: %+v", fs)
	}
	write(sshConfigPath, renderSSHConfig(m, hosts))
	// a fort with no key line: a warning; a line for no fort: an error; a
	// line at the wrong address: an error
	write(knownHostsPath, knownHostsSet(knownHostsRemove(kh, "luxor"), "gone-fort", "10.1.0.99", "ssh-ed25519 G"))
	fs = meshFindings(root, hosts)
	if l := levels(fs); l["warning"] != 1 || l["error"] != 1 {
		t.Errorf("gaps: %+v", fs)
	}
	write(knownHostsPath, knownHostsSet(kh, "luxor", "10.1.0.50", "ssh-ed25519 Kluxor"))
	fs = meshFindings(root, hosts)
	if l := levels(fs); l["error"] != 1 || !strings.Contains(fs[1].msg, "keyed to 10.1.0.50") {
		t.Errorf("moved: %+v", fs)
	}
}

// planSSHConfig with a fake sops: one fort's key readable, one not but
// committed (kept), one neither (reported), one line for a host that has
// no manifest (removed). The re-run finds nothing to do.
func TestPlanSSHConfig(t *testing.T) {
	root := testTree(t)
	tool := &fakeTool{plain: map[string]string{
		filepath.Join(root, "nixos/secrets/luxor.yaml"):            "ssh-host-key: x\nssh-host-key.pub: ssh-ed25519 LUXORKEY luxor\n",
		filepath.Join(root, "nixos/secrets/sfi-prod-1111111.yaml"): "ssh-host-key.pub: ssh-ed25519 P1KEY sfi-prod-1111111\n",
	}}
	kh := knownHostsSet("", "sfi-dev-abc1234", "10.1.0.61", "ssh-ed25519 DEVKEY")
	kh = knownHostsSet(kh, "sfi-prod-1111111", "10.1.0.54", "ssh-ed25519 OLDP1KEY")
	kh = knownHostsSet(kh, "retired-fort", "10.1.0.90", "ssh-ed25519 RETIRED")
	os.WriteFile(filepath.Join(root, knownHostsPath), []byte(kh), 0o644)

	p, err := planSSHConfig(root, tool)
	if err != nil {
		t.Fatal(err)
	}
	if p.sshConfig == "" || p.knownHosts == "" || p.peers != 6 || p.forts != 4 {
		t.Fatalf("plan: ssh=%d kh=%d peers=%d forts=%d", len(p.sshConfig), len(p.knownHosts), p.peers, p.forts)
	}
	if strings.Join(p.added, ",") != "luxor" || strings.Join(p.updated, ",") != "sfi-prod-1111111" || strings.Join(p.kept, ",") != "sfi-dev-abc1234" || strings.Join(p.removed, ",") != "retired-fort" || strings.Join(p.unreadable, ",") != "sfi-prod-2222222" {
		t.Errorf("plan: added=%v updated=%v kept=%v removed=%v unreadable=%v", p.added, p.updated, p.kept, p.removed, p.unreadable)
	}
	if err := p.apply(tool); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	p.report(&sb, true)
	rep := sb.String()
	for _, want := range []string{"◆ mesh/ssh_config", "◆ mesh/known_hosts", "+ luxor", "key replaced: sfi-prod-1111111", "- retired-fort (no manifest)", "kept as committed: sfi-dev-abc1234", "! no host key line for sfi-prod-2222222", "staged."} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
	got := parseKnownHosts(readFile(t, filepath.Join(root, knownHostsPath)))
	if len(got) != 3 || got["luxor"].key != "ssh-ed25519 LUXORKEY" || got["sfi-prod-1111111"].key != "ssh-ed25519 P1KEY" || got["sfi-dev-abc1234"].key != "ssh-ed25519 DEVKEY" {
		t.Errorf("known_hosts after: %+v", got)
	}
	if !contains(tool.added, sshConfigPath) || !contains(tool.added, knownHostsPath) {
		t.Errorf("staged: %v", tool.added)
	}
	// the gate agrees, up to the one fort whose key nobody could read
	hosts, _, _ := manifest.LoadAll(root)
	for _, f := range meshFindings(root, hosts) {
		if f.level == "error" || (f.level == "warning" && !strings.Contains(f.msg, "sfi-prod-2222222")) {
			t.Errorf("gate after write: %+v", f)
		}
	}
	p2, err := planSSHConfig(root, tool)
	if err != nil {
		t.Fatal(err)
	}
	if p2.sshConfig != "" || p2.knownHosts != "" || len(p2.added)+len(p2.updated)+len(p2.removed) != 0 {
		t.Errorf("re-run: ssh=%d kh=%d added=%v updated=%v removed=%v", len(p2.sshConfig), len(p2.knownHosts), p2.added, p2.updated, p2.removed)
	}
	// a manifest that does not load refuses the render rather than dropping the host
	os.WriteFile(filepath.Join(root, manifest.Path("broken")), []byte("mesh_ip = 7\n"), 0o644)
	if _, err := planSSHConfig(root, tool); err == nil || !strings.Contains(err.Error(), "broken") {
		t.Errorf("broken manifest: want refusal, got %v", err)
	}
}
