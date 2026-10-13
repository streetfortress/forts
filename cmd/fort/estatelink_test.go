package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

// stagingEstate lays down the local estate fort estate link runs in:
// staging, as the fixture has it, with one fort minted (a peer) and one
// not (a manifest whose mesh_ip is in no table).
func stagingEstate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range []string{"estate.toml", "mesh/peers.json", "forts/stage-b.toml"} {
		copyFile(t, filepath.Join(stagingFix, rel), filepath.Join(root, rel))
	}
	return root
}

// The other half (forts#57): the fetched estate's manifests name this
// estate in a [links.<name>] table, the claim is verified against this
// peers.json, and the remotes entry is written — address, the host's
// endpoint, the link's port, the public key. A re-run is current; a
// claim that moved follows the manifest; a collision refuses the run.
func TestEstateLink(t *testing.T) {
	root := stagingEstate(t)
	tool := &fakeTool{}
	p, err := planEstateLink(root, infraURL, dirFetcher(infraFix))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.claims) != 1 || p.errors != 0 || len(p.stale) != 0 {
		t.Fatalf("plan: %+v errors=%d stale=%v", p.claims, p.errors, p.stale)
	}
	c := p.claims[0]
	want := meshPeer{IP: "10.2.0.252", Endpoint: "10.0.0.5", Port: 51821, PublicKey: testWGPub}
	if c.host != "hub" || c.status != "new" || c.entry != want {
		t.Errorf("claim: %+v", c)
	}
	var sb strings.Builder
	p.report(&sb, false)
	for _, s := range []string{"estate link ← sfi/infra", "2 manifest(s) read; 1 link(s) to sfi/staging", "hub", "10.2.0.252", "endpoint 10.0.0.5:51821", "+ remotes.hub", "○ mesh/peers.json", "dry run"} {
		if !strings.Contains(sb.String(), s) {
			t.Errorf("report lacks %q:\n%s", s, sb.String())
		}
	}
	if err := p.apply(tool); err != nil {
		t.Fatal(err)
	}
	m, err := loadMesh(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Remotes["hub"]; got != want {
		t.Errorf("remotes.hub: %+v", got)
	}
	if _, ok := m.Remotes["forge"]; !ok || len(m.Peers) != 1 || len(m.Clients) != 1 {
		t.Errorf("the other tables moved: %+v", m)
	}
	if !contains(tool.added, peersPath) {
		t.Errorf("staged: %v", tool.added)
	}

	// re-run: current, nothing to write
	p2, err := planEstateLink(root, "https://gitea.zen.lofi/sfi/infra.git", dirFetcher(infraFix))
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.claims) != 1 || p2.claims[0].status != "current" || len(p2.writes()) != 0 {
		t.Errorf("re-run: %+v", p2.claims)
	}
	before := readFile(t, filepath.Join(root, peersPath))
	tool2 := &fakeTool{}
	if err := p2.apply(tool2); err != nil || len(tool2.added) != 0 || readFile(t, filepath.Join(root, peersPath)) != before {
		t.Errorf("re-run wrote: %v %v", err, tool2.added)
	}

	// the claim moved (fort link add picked again, or the hub re-minted): the entry follows
	moved := t.TempDir()
	for _, rel := range []string{"estate.toml", "mesh/peers.json", "forts/hub.toml", "forts/other.toml"} {
		copyFile(t, filepath.Join(infraFix, rel), filepath.Join(moved, rel))
	}
	hub := filepath.Join(moved, "forts/hub.toml")
	os.WriteFile(hub, []byte(strings.Replace(strings.Replace(readFile(t, hub), "10.2.0.252", "10.2.0.251", 1), "51821", "51822", 1)), 0o644)
	p3, err := planEstateLink(root, infraURL, dirFetcher(moved))
	if err != nil {
		t.Fatal(err)
	}
	if len(p3.claims) != 1 || p3.claims[0].status != "update" || !strings.Contains(p3.claims[0].note, "address 10.2.0.252 → 10.2.0.251") || !strings.Contains(p3.claims[0].note, "port 51821 → 51822") {
		t.Errorf("moved: %+v", p3.claims)
	}
	if err := p3.apply(&fakeTool{}); err != nil {
		t.Fatal(err)
	}
	if m, _ := loadMesh(root); m.Remotes["hub"].IP != "10.2.0.251" || m.Remotes["hub"].Port != 51822 || len(m.Remotes) != 2 {
		t.Errorf("after the move: %+v", m.Remotes)
	}
	// a re-minted key is followed, and said
	os.WriteFile(hub, []byte(strings.Replace(readFile(t, hub), testWGPub, "dwdtDXMYpX08FsFyUbJmRd9MLofrwJkqsXf7pR25LCo=", 1)), 0o644)
	if p4, err := planEstateLink(root, infraURL, dirFetcher(moved)); err != nil || p4.claims[0].status != "update" || !strings.Contains(p4.claims[0].note, "re-minted") {
		t.Errorf("re-minted key: %v %+v", err, p4.claims)
	}
}

// What the verb refuses, each one named, and all of them before a byte
// is written.
func TestEstateLinkRefusals(t *testing.T) {
	root := stagingEstate(t)
	for name, tc := range map[string]struct{ repo, want string }{
		"this estate": {stagingURL, "this estate"},
		"bad url":     {"infra", "git URL"},
		"wrong clone": {"ssh://git@gitea.zen.lofi:30022/sfi/production", "not this URL"},
	} {
		if _, err := planEstateLink(root, tc.repo, dirFetcher(infraFix)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
	// A claim judged against this mesh: each refusal lands on the claim,
	// the run refuses as a whole, apply writes nothing.
	fixture := func(edit func(hub string) string) string {
		dir := t.TempDir()
		for _, rel := range []string{"estate.toml", "mesh/peers.json", "forts/hub.toml", "forts/other.toml"} {
			copyFile(t, filepath.Join(infraFix, rel), filepath.Join(dir, rel))
		}
		hub := filepath.Join(dir, "forts/hub.toml")
		os.WriteFile(hub, []byte(edit(readFile(t, hub))), 0o644)
		return dir
	}
	for name, tc := range map[string]struct {
		edit func(string) string
		want string
	}{
		"peer holds it":     {func(s string) string { return strings.Replace(s, "10.2.0.252", "10.2.0.10", 1) }, "peers.stage-a's"},
		"client holds it":   {func(s string) string { return strings.Replace(s, "10.2.0.252", "10.2.0.20", 1) }, "clients.laptop's"},
		"remote holds it":   {func(s string) string { return strings.Replace(s, "10.2.0.252", "10.2.0.254", 1) }, "remotes.forge's"},
		"manifest holds it": {func(s string) string { return strings.Replace(s, "10.2.0.252", "10.2.0.253", 1) }, "stage-b.toml mesh_ip"},
		"outside the mesh":  {func(s string) string { return strings.Replace(s, "10.2.0.252", "10.9.0.2", 1) }, "outside this estate's mesh"},
		"wrong interface":   {func(s string) string { return strings.Replace(s, "wg-staging", "wg-stage", 1) }, "one name per mesh"},
		"wrong zone":        {func(s string) string { return strings.Replace(s, "staging.mesh", "stage.mesh", 1) }, "one zone per mesh"},
		// a cloud cluster fort with no [dns] and no mesh_endpoint has no endpoint
		"no endpoint": {func(s string) string {
			return strings.Replace(strings.Replace(s, "[libvirt]", "[cluster]\n[libvirt]", 1), "[libvirt]\nip = \"10.0.0.5\"", "[linode]\nregion = \"us-sea\"\ntype = \"g6-standard-2\"", 1)
		}, "no endpoint"},
		// an endpoint built from [dns] that is not one name is refused on the claim (#99)
		"endpoint not a name": {func(s string) string {
			return strings.Replace(strings.Replace(s, "[libvirt]", "[cluster]\n[libvirt]", 1), "[libvirt]\nip = \"10.0.0.5\"", "[dns]\nzone = \"evil\\n[Interface]\"\nname = \"hub\"\n[linode]\nregion = \"us-sea\"\ntype = \"g6-standard-2\"", 1)
		}, "not an IP address or a DNS name"},
	} {
		dir := fixture(tc.edit)
		p, err := planEstateLink(root, infraURL, dirFetcher(dir))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if p.errors != 1 || len(p.claims) != 1 || p.claims[0].status != "error" || !strings.Contains(p.claims[0].note, tc.want) {
			t.Errorf("%s: want a refused claim naming %q, got errors=%d %+v", name, tc.want, p.errors, p.claims)
		}
		before := readFile(t, filepath.Join(root, peersPath))
		if err := p.apply(&fakeTool{}); err == nil || readFile(t, filepath.Join(root, peersPath)) != before {
			t.Errorf("%s: apply wrote under a refusal: %v", name, err)
		}
		var sb strings.Builder
		p.report(&sb, true)
		if !strings.Contains(sb.String(), "✗") || strings.Contains(sb.String(), "staged") {
			t.Errorf("%s: report:\n%s", name, sb.String())
		}
	}
	// What crosses the boundary is content for root-level config, so its
	// shape is checked, not only its topology (#99, #100). A manifest
	// whose mesh_endpoint is not one address or one name does not parse
	// there (the manifest's own rule) and is reported, never claimed; a
	// file whose name is not a host name is the same; an endpoint built
	// from another field is refused on the claim.
	for name, tc := range map[string]struct {
		edit func(string) string
		want string
	}{
		"endpoint injects a section": {func(s string) string {
			return strings.Replace(s, "mesh_ip = ", "mesh_endpoint = \"10.0.0.5\\n[Interface]\\nPostUp = id > /tmp/pwned\"\nmesh_ip = ", 1)
		}, "mesh_endpoint"},
		"endpoint with a space": {func(s string) string {
			return strings.Replace(s, "mesh_ip = ", "mesh_endpoint = \"10.0.0.5 extra\"\nmesh_ip = ", 1)
		}, "mesh_endpoint"},
	} {
		dir := fixture(tc.edit)
		p, err := planEstateLink(root, infraURL, dirFetcher(dir))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var sb strings.Builder
		p.report(&sb, false)
		if len(p.claims) != 0 || p.other.problems["hub"] == nil || !strings.Contains(p.other.problems["hub"].Error(), tc.want) || !strings.Contains(sb.String(), "does not parse there") {
			t.Errorf("%s: want no claim and a problem naming %q, got claims=%+v problems=%v\n%s", name, tc.want, p.claims, p.other.problems, sb.String())
		}
	}
	named := fixture(func(s string) string { return s })
	evil := "evil\nforge IN A 10.9.9.9"
	if err := os.WriteFile(filepath.Join(named, "forts", evil+".toml"), []byte(readFile(t, filepath.Join(named, "forts/hub.toml"))), 0o644); err != nil {
		t.Skipf("the filesystem refuses the name: %v", err)
	}
	pn, err := planEstateLink(root, infraURL, dirFetcher(named))
	if err != nil {
		t.Fatal(err)
	}
	var rb strings.Builder
	pn.report(&rb, false)
	if _, ok := pn.other.hosts[evil]; ok || pn.other.problems[evil] == nil || !strings.Contains(pn.other.problems[evil].Error(), "not a host name") || strings.Contains(rb.String(), "\nforge IN A") {
		t.Errorf("a file name that is not a host name: hosts=%v problems=%v\n%s", sortedNames(pn.other.hosts), pn.other.problems, rb.String())
	}
	if len(pn.claims) != 1 || pn.claims[0].host != "hub" || pn.errors != 0 {
		t.Errorf("the well-named claim stands: %+v", pn.claims)
	}
	for _, ep := range []string{"10.0.0.5", "fe80::1", "hub.lan", "Hub-1.example.com"} {
		if !manifest.ValidEndpoint(ep) {
			t.Errorf("ValidEndpoint(%q) = false", ep)
		}
	}
	for _, ep := range []string{"", "10.0.0.5\n[Interface]", "10.0.0.5 x", "hub_1", "-hub.lan", "hub.lan:51820"} {
		if manifest.ValidEndpoint(ep) {
			t.Errorf("ValidEndpoint(%q) = true", ep)
		}
	}
	// Two hosts of the other estate that picked one address: the second
	// is refused, the first stands.
	dir := fixture(func(s string) string { return s })
	other := filepath.Join(dir, "forts/other.toml")
	os.WriteFile(other, []byte(readFile(t, other)+"\n[links.staging]\nestate = \""+stagingURL+"\"\naddress = \"10.2.0.252\"\nport = 51821\npublic_key = \"dwdtDXMYpX08FsFyUbJmRd9MLofrwJkqsXf7pR25LCo=\"\n"), 0o644)
	p, err := planEstateLink(root, infraURL, dirFetcher(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.claims) != 2 || p.errors != 1 || p.claims[0].status != "new" || p.claims[1].status != "error" || !strings.Contains(p.claims[1].note, "also what hub claims") {
		t.Errorf("two claims, one address: %+v", p.claims)
	}
	// No link to this estate at all: said, nothing to write.
	none := fixture(func(s string) string { return strings.Replace(s, "[links.staging]", "[links.production]", 1) })
	none2 := filepath.Join(none, "forts/hub.toml")
	os.WriteFile(none2, []byte(strings.Replace(readFile(t, none2), "sfi/staging\"", "sfi/production\"", 1)), 0o644)
	p, err = planEstateLink(root, infraURL, dirFetcher(none))
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	p.report(&sb, false)
	if len(p.claims) != 0 || !strings.Contains(sb.String(), "no fort of sfi/infra links to this estate") {
		t.Errorf("no link: %+v\n%s", p.claims, sb.String())
	}
	// A remotes entry named like a fort of that estate with no link here is a stale candidate.
	peers := filepath.Join(root, peersPath)
	os.WriteFile(peers, []byte(strings.Replace(readFile(t, peers), `"remotes": {`, `"remotes": {
    "other": {"wg_vpn_ip": "10.2.0.240", "wg_endpoint_ip": "10.0.0.6", "wg_endpoint_port": 51821, "public_key": "`+testWGPub+`"},`, 1)), 0o644)
	p, err = planEstateLink(root, infraURL, dirFetcher(infraFix))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.stale) != 1 || p.stale[0] != "other" {
		t.Errorf("stale: %v", p.stale)
	}
}
