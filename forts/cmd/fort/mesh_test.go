package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
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
  }
}
`

var testManifests = map[string]string{
	"luxor":            "mesh_ip = \"10.1.0.5\"\n[cluster]\ndatastore = \"etcd\"\n[backups]\nprofile = \"none\"\n[shape]\nvcpus = 4\nmemory = 16\ndisk = \"450G\"\n[libvirt]\nip = \"10.0.0.5\"\n",
	"sfi-dev-abc1234":  "mesh_ip = \"10.1.0.61\"\n[backups]\nprofile = \"none\"\n[shape]\nvcpus = 4\nmemory = 8\ndisk = \"80G\"\n[libvirt]\nip = \"10.0.0.61\"\n",
	"sfi-prod-1111111": "mesh_ip = \"10.1.0.54\"\n[cluster]\n[backups]\nprofile = \"none\"\n[dns]\nzone = \"example.cloud\"\nname = \"p1\"\n[linode]\nregion = \"us-sea\"\ntype = \"g6-standard-2\"\n",
	"sfi-prod-2222222": "mesh_ip = \"10.1.0.55\"\n[cluster]\n[backups]\nprofile = \"none\"\n[dns]\nzone = \"example.cloud\"\nname = \"p2\"\n[linode]\nregion = \"us-sea\"\ntype = \"g6-standard-2\"\n",
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
	conf, err := renderWGConf(m, "luxor")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[Interface]\nAddress = 10.1.0.5/24\nListenPort = 51820\n",
		"#   LX=\n", // its own public key, for the operator to compare with the app's
		"[Peer]\n# sfi-prod-1111111\nPublicKey = P1=\nAllowedIPs = 10.1.0.54/32\nEndpoint = p1.example.cloud:51820\nPersistentKeepalive = 25\n",
		"[Peer]\n# devbox\nPublicKey = DB=\nAllowedIPs = 10.1.0.3/32\n# no fixed endpoint: it dials this host",
		"[Peer]\n# laptop (roaming client: no endpoint, no keepalive)\nPublicKey = LT=\nAllowedIPs = 10.1.0.100/32\n",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("luxor: missing %q in:\n%s", want, conf)
		}
	}
	if strings.Contains(conf, "PrivateKey =") || strings.Contains(conf, "PublicKey = LX=") {
		t.Errorf("luxor: a PrivateKey line, or itself as a peer:\n%s", conf)
	}
	// a peer with no endpoint of its own facing another without one
	conf, _ = renderWGConf(m, "devbox")
	if !strings.Contains(conf, "# soundship\nPublicKey = SS=\nAllowedIPs = 10.1.0.1/32\n# no fixed endpoint on either side") {
		t.Errorf("devbox↔soundship:\n%s", conf)
	}
	// a roaming client: no ListenPort, no clients, the DDNS note
	conf, err = renderWGConf(m, "laptop")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(conf, "ListenPort") || strings.Contains(conf, "roaming client:") || !strings.Contains(conf, "Address = 10.1.0.100/24\n") {
		t.Errorf("laptop interface:\n%s", conf)
	}
	if !strings.Contains(conf, "# soundship\nPublicKey = SS=\nAllowedIPs = 10.1.0.1/32\n# no fixed endpoint in peers.json: a roaming client") || !strings.Contains(conf, "Endpoint = 10.0.0.5:51820") {
		t.Errorf("laptop peers:\n%s", conf)
	}
	if _, err := renderWGConf(m, "macmini"); err == nil || !strings.Contains(err.Error(), "register it first") {
		t.Errorf("unknown member: %v", err)
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
