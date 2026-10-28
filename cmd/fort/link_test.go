package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

const (
	stagingURL = "ssh://git@gitea.zen.lofi:30022/sfi/staging"
	infraURL   = "ssh://git@gitea.zen.lofi:30022/sfi/infra"
	stagingFix = "testdata/estates/staging"
	infraFix   = "testdata/estates/infra"
)

// linkEstate lays down the local estate fort link add runs in: infra,
// with one plain fort (hub) scaffolded and minted (a secrets file the
// fake tool decrypts), serving staging the forge role.
func linkEstate(t *testing.T) (root string, tool *fakeTool) {
	t.Helper()
	root = t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("estate.toml", readFile(t, filepath.Join(infraFix, "estate.toml")))
	write("mesh/peers.json", readFile(t, filepath.Join(infraFix, "mesh/peers.json")))
	write("nixos/.sops.yaml", "creation_rules:\n  - path_regex: secrets/.*\\.yaml$\n    age:\n      - age1admin\n")
	write(manifest.Path("hub"), "# The hub: a plain fort of infra.\nmesh_ip = \"10.1.0.5\"   # on infra's mesh\n\n[libvirt]\nip = \"10.0.0.5\"\n")
	write(manifest.Path("other"), readFile(t, filepath.Join(infraFix, "forts/other.toml")))
	entry, err := manifest.Load(root, "hub")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range render(t, "hub", entry) {
		if strings.HasPrefix(f.path, "nixos/hosts/hub/") {
			write(f.path, string(f.content))
		}
	}
	secrets := filepath.Join(root, "nixos/secrets/hub.yaml")
	write("nixos/secrets/hub.yaml", "ENC[...]\n")
	tool = &fakeTool{plain: map[string]string{secrets: "wireguard-private-key: dwdtDXMYpX08FsFyUbJmRd9MLofrwJkqsXf7pR25LCo=\n"}}
	return root, tool
}

// The local half (forts#56): the address is the highest free one on the
// other mesh with every holder counted (a peer, a client, a remote, a
// manifest not minted yet), the port the lowest free one beside 51820,
// the key minted into the secrets file, the table appended to the
// manifest and the block to the host file. A re-run restores the key and
// changes nothing; an address taken since is picked again.
func TestLinkAdd(t *testing.T) {
	root, tool := linkEstate(t)
	fetch := dirFetcher(stagingFix)
	p, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{}, tool, fetch)
	if err != nil {
		t.Fatal(err)
	}
	// .254 is remotes.forge, .253 is stage-b's mesh_ip (not minted), so .252
	if p.link.Address != "10.2.0.252" || p.link.Port != 51821 || p.link.Interface != "wg-staging" || p.link.Zone != "staging.mesh" || p.link.Estate != stagingURL || p.link.PublicKey != p.wg.public {
		t.Errorf("link: %+v", p.link)
	}
	if !p.keyMissing || p.had != nil || len(p.edits) != 2 || p.recorded != "" || len(p.roles) != 1 || p.roles[0] != "forge" {
		t.Errorf("plan: missing=%v had=%v edits=%d recorded=%q roles=%v", p.keyMissing, p.had, len(p.edits), p.recorded, p.roles)
	}
	var sb strings.Builder
	p.report(&sb, false)
	dry := sb.String()
	for _, want := range []string{"link add ⛫ hub → sfi/staging", "mesh 10.2.0.0/24 · wg-staging · staging.mesh", "10.2.0.252", "highest free address", "○ nixos/secrets/hub.yaml", "wireguard-private-key-staging", "○ forts/hub.toml", "+ [links.staging]", "○ nixos/hosts/hub/default.nix", "+ sfi.links.staging", "roles forge", "not recorded yet", "dry run"} {
		if !strings.Contains(dry, want) {
			t.Errorf("report lacks %q:\n%s", want, dry)
		}
	}
	if strings.Contains(dry, p.wg.private) {
		t.Error("the report leaks the private half")
	}
	if err := p.apply(tool); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(root, "nixos/secrets/hub.yaml")
	if len(tool.sets) != 1 || tool.sets[0][0] != secrets || tool.sets[0][1] != `["wireguard-private-key-staging"]` || tool.sets[0][2] != p.wg.private {
		t.Errorf("sets: %v", tool.sets)
	}
	entry, err := manifest.Load(root, "hub")
	if err != nil {
		t.Fatal(err)
	}
	l, ok := entry.Links["staging"]
	if !ok || l != p.link {
		t.Errorf("manifest after apply: %+v", entry.Links)
	}
	text := readFile(t, filepath.Join(root, manifest.Path("hub")))
	if !strings.HasPrefix(text, "# The hub: a plain fort of infra.\nmesh_ip = \"10.1.0.5\"   # on infra's mesh\n") || !strings.Contains(text, "\n[links.staging]\nestate = \""+stagingURL+"\"\naddress = \"10.2.0.252\"\nport = 51821\ninterface = \"wg-staging\"\nzone = \"staging.mesh\"\npublic_key = \""+p.wg.public+"\"") {
		t.Errorf("manifest text:\n%s", text)
	}
	nix := readFile(t, filepath.Join(root, "nixos/hosts/hub/default.nix"))
	if strings.Count(nix, "sfi.links.staging = {") != 1 || !strings.Contains(nix, linksImport) || !strings.Contains(nix, `address = "10.2.0.252";`) {
		t.Errorf("host file:\n%s", nix)
	}
	if !contains(tool.added, "nixos/secrets/hub.yaml") || !contains(tool.added, manifest.Path("hub")) || !contains(tool.added, "nixos/hosts/hub/default.nix") {
		t.Errorf("staged: %v", tool.added)
	}

	// re-run: the key is in the file now; nothing to do
	tool2 := &fakeTool{plain: map[string]string{secrets: tool.plain[secrets] + "wireguard-private-key-staging: " + p.wg.private + "\n"}}
	p2, err := planLinkAdd(root, "hub", "https://gitea.zen.lofi/sfi/staging.git", linkAddOpts{}, tool2, fetch)
	if err != nil {
		t.Fatal(err)
	}
	if p2.keyMissing || p2.had == nil || len(p2.edits) != 0 || p2.wg.public != p.wg.public || p2.link != p.link || !strings.HasPrefix(p2.addressWhy, "kept") || p2.portWhy != "kept" {
		t.Errorf("re-run: missing=%v edits=%d why=%q/%q link=%+v", p2.keyMissing, len(p2.edits), p2.addressWhy, p2.portWhy, p2.link)
	}
	if err := p2.apply(tool2); err != nil || len(tool2.sets) != 0 || len(tool2.added) != 0 {
		t.Errorf("re-run wrote: %v %v %v", err, tool2.sets, tool2.added)
	}

	// the collision fort estate link refuses on: .252 is somebody's now
	taken := t.TempDir()
	for _, rel := range []string{"estate.toml", "mesh/peers.json", "forts/stage-b.toml"} {
		copyFile(t, filepath.Join(stagingFix, rel), filepath.Join(taken, rel))
	}
	peers := strings.Replace(readFile(t, filepath.Join(taken, "mesh/peers.json")), `"clients": {`, `"clients": {
    "tablet": {"wg_vpn_ip": "10.2.0.252", "public_key": "dGFibGV0LWZpeHR1cmUtcHVibGljLWtleT09PT09PT09PT0="},`, 1)
	os.WriteFile(filepath.Join(taken, "mesh/peers.json"), []byte(peers), 0o644)
	p3, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{}, tool2, dirFetcher(taken))
	if err != nil {
		t.Fatal(err)
	}
	if p3.link.Address != "10.2.0.251" || !strings.Contains(p3.addressWhy, "clients.tablet") || p3.link.Port != 51821 || p3.wg.public != p.wg.public {
		t.Errorf("collision: %+v %q", p3.link, p3.addressWhy)
	}
	if len(p3.edits) != 2 || !strings.Contains(p3.edits[0].note, "address 10.2.0.252 → 10.2.0.251") || p3.edits[1].fn != nil || !strings.Contains(p3.edits[1].note, "by hand") {
		t.Errorf("collision edits: %+v", p3.edits)
	}
	if err := p3.apply(tool2); err != nil {
		t.Fatal(err)
	}
	text = readFile(t, filepath.Join(root, manifest.Path("hub")))
	if !strings.Contains(text, "address = \"10.2.0.251\"\n") || strings.Contains(text, "10.2.0.252") || !strings.Contains(text, "mesh_ip = \"10.1.0.5\"   # on infra's mesh") || strings.Count(text, "[links.staging]") != 1 {
		t.Errorf("re-picked manifest:\n%s", text)
	}
	if e, err := manifest.Load(root, "hub"); err != nil || e.Links["staging"].Address != "10.2.0.251" || e.Links["staging"].PublicKey != p.wg.public {
		t.Errorf("re-picked manifest parses: %v %+v", err, e.Links)
	}

	// the other estate has recorded the claim: said so, with its facts
	recorded := strings.Replace(readFile(t, filepath.Join(stagingFix, "mesh/peers.json")), `"remotes": {`, `"remotes": {
    "hub": {"wg_vpn_ip": "10.2.0.251", "wg_endpoint_ip": "10.0.0.5", "wg_endpoint_port": 51821, "public_key": "`+p.wg.public+`"},`, 1)
	os.WriteFile(filepath.Join(taken, "mesh/peers.json"), []byte(recorded), 0o644)
	p4, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{}, tool2, dirFetcher(taken))
	if err != nil {
		t.Fatal(err)
	}
	sb.Reset()
	p4.report(&sb, true)
	// The one edit is the note: the block still says .252 and does not
	// follow the table by itself, so every run says so until a hand fixes it.
	if len(p4.edits) != 1 || p4.edits[0].fn != nil || !strings.Contains(p4.edits[0].note, "by hand") || !strings.Contains(sb.String(), "recorded (10.2.0.251, endpoint 10.0.0.5:51821") {
		t.Errorf("recorded: edits=%+v\n%s", p4.edits, sb.String())
	}
}

// A table a hand spelled — the header with a comment, a key with its
// own spacing, interface and zone left to their defaults — is found,
// kept as it is on a re-run, and rewritten one key at a time when a
// value moves; the edited text is parsed before it is handed back.
func TestLinkAddKeepsHandSpelledTable(t *testing.T) {
	root, tool := linkEstate(t)
	secrets := filepath.Join(root, "nixos/secrets/hub.yaml")
	tool.plain[secrets] += "wireguard-private-key-staging: dwdtDXMYpX08FsFyUbJmRd9MLofrwJkqsXf7pR25LCo=\n"
	pub, err := wgKeyFrom("dwdtDXMYpX08FsFyUbJmRd9MLofrwJkqsXf7pR25LCo=")
	if err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(root, manifest.Path("hub"))
	hand := readFile(t, mp) + "\n[ links.staging ]   # by hand\nestate=\"" + stagingURL + "\"\naddress = \"10.2.0.252\" # mine\nport=51821\npublic_key = \"" + pub.public + "\"\n"
	os.WriteFile(mp, []byte(hand), 0o644)
	p, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{}, tool, dirFetcher(stagingFix))
	if err != nil {
		t.Fatal(err)
	}
	// the host file has no block yet: that edit, and that edit alone
	if len(p.edits) != 1 || p.edits[0].path != "nixos/hosts/hub/default.nix" || p.had == nil || p.link.Interface != "wg-staging" || p.link.Zone != "staging.mesh" {
		t.Errorf("hand-spelled table: edits=%+v had=%v link=%+v", p.edits, p.had, p.link)
	}
	if err := p.apply(tool); err != nil {
		t.Fatal(err)
	}
	if readFile(t, mp) != hand {
		t.Errorf("the manifest moved under a re-run:\n%s", readFile(t, mp))
	}
	// .252 taken since: the one line that moves is rewritten in place
	taken := t.TempDir()
	for _, rel := range []string{"estate.toml", "mesh/peers.json", "forts/stage-b.toml"} {
		copyFile(t, filepath.Join(stagingFix, rel), filepath.Join(taken, rel))
	}
	peers := strings.Replace(readFile(t, filepath.Join(taken, "mesh/peers.json")), `"clients": {`, `"clients": {
    "tablet": {"wg_vpn_ip": "10.2.0.252", "public_key": "dGFibGV0LWZpeHR1cmUtcHVibGljLWtleT09PT09PT09PT0="},`, 1)
	os.WriteFile(filepath.Join(taken, "mesh/peers.json"), []byte(peers), 0o644)
	p2, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{}, tool, dirFetcher(taken))
	if err != nil {
		t.Fatal(err)
	}
	if err := p2.apply(tool); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, mp)
	want := strings.Replace(hand, "address = \"10.2.0.252\" # mine", "address = \"10.2.0.251\"   # mine", 1)
	if got != want {
		t.Errorf("one key rewritten in place:\n%s", got)
	}
	if e, err := manifest.Load(root, "hub"); err != nil || e.Links["staging"].Address != "10.2.0.251" || len(e.Links) != 1 {
		t.Errorf("parses: %v %+v", err, e.Links)
	}
	// An edit whose result the parser refuses is refused before a write.
	p2.name = "staging\"\nbroken = \""
	if _, err := p2.manifestEdit(hand); err == nil || !strings.Contains(err.Error(), "does not parse") {
		t.Errorf("unparseable edit: %v", err)
	}
}

// What fort link add refuses, and what the flags do.
func TestLinkAddRefusals(t *testing.T) {
	root, tool := linkEstate(t)
	fetch := dirFetcher(stagingFix)
	for name, tc := range map[string]struct {
		repo string
		o    linkAddOpts
		want string
	}{
		"this estate":      {infraURL, linkAddOpts{}, "this estate"},
		"bad url":          {"staging", linkAddOpts{}, "git URL"},
		"wrong clone":      {"ssh://git@gitea.zen.lofi:30022/sfi/production", linkAddOpts{}, "not this URL"},
		"address taken":    {stagingURL, linkAddOpts{address: "10.2.0.10"}, "peers.stage-a"},
		"address manifest": {stagingURL, linkAddOpts{address: "10.2.0.253"}, "stage-b.toml mesh_ip"},
		"address outside":  {stagingURL, linkAddOpts{address: "10.9.0.1"}, "outside"},
		"address bad":      {stagingURL, linkAddOpts{address: "nope"}, "IPv4"},
		"mesh port":        {stagingURL, linkAddOpts{port: 51820}, "own mesh port"},
		"bad port":         {stagingURL, linkAddOpts{port: 70000}, "UDP port"},
	} {
		if _, err := planLinkAdd(root, "hub", tc.repo, tc.o, tool, fetch); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
	// --address and --port are taken as given when they pass
	p, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{address: "10.2.0.100", port: 51900}, tool, fetch)
	if err != nil || p.link.Address != "10.2.0.100" || p.link.Port != 51900 || p.addressWhy != "--address" || p.portWhy != "--port" {
		t.Errorf("flags: %v %+v", err, p)
	}
	// gates: scaffold and mint first
	os.Remove(filepath.Join(root, "nixos/secrets/hub.yaml"))
	if _, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{}, tool, fetch); err == nil || !strings.Contains(err.Error(), "fort mint hub") {
		t.Errorf("no secrets file: %v", err)
	}
	os.Remove(filepath.Join(root, "nixos/hosts/hub/default.nix"))
	if _, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{}, tool, fetch); err == nil || !strings.Contains(err.Error(), "fort scaffold hub") {
		t.Errorf("no host file: %v", err)
	}
	// an estate with no mesh cannot be allocated against — unless told
	root, tool = linkEstate(t)
	noMesh := t.TempDir()
	copyFile(t, filepath.Join(stagingFix, "mesh/peers.json"), filepath.Join(noMesh, "mesh/peers.json"))
	os.WriteFile(filepath.Join(noMesh, "estate.toml"), []byte("name = \"staging\"\n[source]\nrepo = \""+stagingURL+"\"\n"), 0o644)
	if _, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{}, tool, dirFetcher(noMesh)); err == nil || !strings.Contains(err.Error(), "declares no mesh") {
		t.Errorf("no mesh: %v", err)
	}
	if p, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{address: "10.2.0.7"}, tool, dirFetcher(noMesh)); err != nil || p.link.Interface != "wg-staging" || p.link.Zone != "staging.mesh" {
		// an estate on wg0 gives the link the module's default name
		t.Errorf("no mesh, --address: %v %+v", err, p.link)
	}
	// a table whose key is not the one the secrets file derives
	mp := filepath.Join(root, manifest.Path("hub"))
	os.WriteFile(mp, []byte(readFile(t, mp)+"\n[links.staging]\nestate = \""+stagingURL+"\"\naddress = \"10.2.0.250\"\nport = 51821\npublic_key = \""+testWGPub+"\"\n"), 0o644)
	if _, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{}, tool, fetch); err == nil || !strings.Contains(err.Error(), "gone from the secrets file") {
		t.Errorf("key missing under a table: %v", err)
	}
	secrets := filepath.Join(root, "nixos/secrets/hub.yaml")
	tool.plain[secrets] += "wireguard-private-key-staging: dwdtDXMYpX08FsFyUbJmRd9MLofrwJkqsXf7pR25LCo=\n"
	if _, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{}, tool, fetch); err == nil || !strings.Contains(err.Error(), "one of the two is stale") {
		t.Errorf("key mismatch: %v", err)
	}
}

// The allocator counts the other hosts of THIS estate too: a second hub
// linking to the same estate claims an address no peers.json has yet.
func TestLinkAddCountsSiblingClaims(t *testing.T) {
	root, tool := linkEstate(t)
	other := filepath.Join(root, manifest.Path("other"))
	os.WriteFile(other, []byte(readFile(t, other)+"\n[links.staging]\nestate = \""+stagingURL+"\"\naddress = \"10.2.0.252\"\nport = 51821\npublic_key = \""+testWGPub+"\"\n"), 0o644)
	p, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{}, tool, dirFetcher(stagingFix))
	if err != nil || p.link.Address != "10.2.0.251" {
		t.Errorf("sibling claim: %v %+v", err, p.link)
	}
	if _, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{address: "10.2.0.252"}, tool, dirFetcher(stagingFix)); err == nil || !strings.Contains(err.Error(), "forts/other.toml [links.staging]") {
		t.Errorf("sibling claim by flag: %v", err)
	}
}

func TestTomlSetInTable(t *testing.T) {
	text := "mesh_ip = \"10.1.0.5\"\n\n[links.staging]\nestate = \"x\"\naddress = \"10.2.0.1\"   # the claim\nport = 51821\n\n[libvirt]\nip = \"10.0.0.5\"\n"
	got := tomlSetInTable(text, "links.staging", "address", `"10.2.0.2"`)
	if !strings.Contains(got, "address = \"10.2.0.2\"   # the claim\nport = 51821\n") || strings.Contains(got, "10.2.0.1") {
		t.Errorf("rewrite:\n%s", got)
	}
	got = tomlSetInTable(text, "links.staging", "zone", `"staging.mesh"`)
	if !strings.Contains(got, "port = 51821\nzone = \"staging.mesh\"\n\n[libvirt]") {
		t.Errorf("new key lands after the last key of the table:\n%s", got)
	}
	got = tomlSetInTable(text, "links.production", "estate", `"y"`)
	if !strings.HasSuffix(got, "ip = \"10.0.0.5\"\n\n[links.production]\nestate = \"y\"\n") {
		t.Errorf("new table appended:\n%s", got)
	}
	if !tomlHasTable(text, "links.staging") || tomlHasTable(text, "links") || tomlHasTable(text, "links.prod") {
		t.Error("tomlHasTable")
	}
	// A header spelled by hand names the same table; an array-of-tables
	// header and a header with code after it do not.
	for line, want := range map[string]string{
		"[links.staging]":               "links.staging",
		"  [ links.staging ]  ":         "links.staging",
		"[links.staging]   # a comment": "links.staging",
		"[links.staging] x = 1":         "",
		"[[serves.metrics]]":            "",
		"[links.staging":                "",
		"links.staging = 1":             "",
	} {
		if got := tomlHeader(line); got != want {
			t.Errorf("tomlHeader(%q) = %q, want %q", line, got, want)
		}
	}
	spelled := strings.Replace(text, "[links.staging]", "[ links.staging ]   # by hand", 1)
	if got := tomlSetInTable(spelled, "links.staging", "port", "51822"); strings.Count(got, "links.staging") != 1 || !strings.Contains(got, "[ links.staging ]   # by hand\nestate = \"x\"\naddress = \"10.2.0.1\"   # the claim\nport = 51822\n") {
		t.Errorf("hand-spelled header:\n%s", got)
	}
}

func TestPickAddressEdges(t *testing.T) {
	mk := func(cidr string) estate.Estate {
		var e estate.Estate
		e.Mesh.CIDR = cidr
		return e
	}
	for _, c := range []string{"10.0.0.0/32", "10.0.0.0/31"} {
		if ip, err := pickAddress(mk(c), nil); err == nil {
			t.Errorf("%s: picked %s, want an error", c, ip)
		}
	}
	if ip, err := pickAddress(mk("10.0.0.0/30"), map[string]string{"10.0.0.2": "x"}); err != nil || ip != "10.0.0.1" {
		t.Errorf("/30: got %s, %v", ip, err)
	}
	if _, err := pickAddress(mk("10.0.0.0/30"), map[string]string{"10.0.0.2": "x", "10.0.0.1": "y"}); err == nil {
		t.Error("/30 full: want an error")
	}
	if ip, err := pickAddress(mk("10.1.0.5/24"), nil); err != nil || ip != "10.1.0.254" {
		t.Errorf("unmasked /24: got %s, %v", ip, err)
	}
	if ip, err := pickAddress(mk("0.0.0.0/0"), nil); err != nil || ip != "255.255.255.254" {
		t.Errorf("/0: got %s, %v", ip, err)
	}
}
