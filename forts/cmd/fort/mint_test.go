package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fakeGitea is the deploy-key API: a list that POST appends to.
func fakeGitea(t *testing.T, keys []giteaKey) (*giteaDeployKeys, *[]giteaKey) {
	list := &keys
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token t" || r.URL.Path != "/keys" {
			w.WriteHeader(404)
			return
		}
		switch r.Method {
		case "GET":
			json.NewEncoder(w).Encode(*list)
		case "POST":
			var k giteaKey
			json.NewDecoder(r.Body).Decode(&k)
			k.ID = len(*list) + 1
			*list = append(*list, k)
			w.WriteHeader(201)
		}
	}))
	t.Cleanup(srv.Close)
	return &giteaDeployKeys{client: srv.Client(), api: srv.URL, token: "t"}, list
}

// Mints a new cloud fort on a copy of the real tree's identity files,
// checks every artifact against the shape preseed-identity produced,
// re-runs (a no-op that restores the identity from the secrets file),
// then dismantles and expects the original files back byte-for-byte.
func TestMint(t *testing.T) {
	repo, err := repoRootFrom(mustGetwd(t))
	if err != nil {
		t.Skip("not inside the deployments checkout")
	}
	const host = "mint-fort"
	tmp := t.TempDir()
	files := []string{"provisioning/inventory.yaml", ".sops.yaml", "nixos/.sops.yaml", "wireguard/peers.json",
		"fleet/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml"}
	for _, rel := range files {
		copyFile(t, filepath.Join(repo, rel), filepath.Join(tmp, rel))
	}
	original := map[string]string{}
	for _, rel := range files {
		original[rel] = readFile(t, filepath.Join(tmp, rel))
	}
	write := func(rel, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(tmp, rel)), 0o755)
		os.WriteFile(filepath.Join(tmp, rel), []byte(content), 0o644)
	}
	write("provisioning/inventory.yaml", strings.TrimRight(original["provisioning/inventory.yaml"], "\n")+"\n"+
		"        "+host+":\n          provider: linode\n          region: us-sea\n          instance_type: g6-standard-2\n          dns_zone: example.cloud\n          dns_name: "+host+"\n          production: true\n          flux_cluster: true\n          wg_vpn_ip: 10.1.0.97\n")

	// gate: scaffold has not run
	tool := &fakeTool{}
	gitea, registered := fakeGitea(t, nil)
	if _, err := planMint(tmp, host, tool, gitea); err == nil || !strings.Contains(err.Error(), "fort scaffold") {
		t.Fatalf("without the scaffold: want the gate, got %v", err)
	}
	write("nixos/hosts/"+host+"/default.nix", "{ }\n")
	write("fleet/apps/"+host+"/kustomization.yaml", "resources: []\n")
	write("fleet/clusters/"+host+"/apps.yaml", "kind: Kustomization\n")

	p, err := planMint(tmp, host, tool, gitea)
	if err != nil {
		t.Fatal(err)
	}
	if !p.newFile || len(p.missing) != 6 || !p.registerOn || p.canaryPath == "" || len(p.rekey) != 1 || len(p.edits) != 3 {
		t.Fatalf("plan: newFile=%v missing=%v register=%v canary=%q rekey=%v edits=%d", p.newFile, p.missing, p.registerOn, p.canaryPath, p.rekey, len(p.edits))
	}
	if err := p.apply(tool, gitea); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	p.report(&sb, true)
	rep := sb.String()

	// the secrets file: all six keys, restorable, never in the report
	secrets := filepath.Join(tmp, "nixos/secrets", host+".yaml")
	var have map[string]string
	if err := yamlUnmarshalFile(secrets, &have); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{keyK3sToken, keyWgPrivate, keySSHHost, keySSHHostPub, keyAge, keyDeploy} {
		if have[k] == "" {
			t.Errorf("secrets file lacks %s", k)
		} else if k != keySSHHostPub && strings.Contains(rep, strings.TrimSpace(have[k])) {
			t.Errorf("report leaks %s", k)
		}
	}
	if hk, err := sshKeyFrom(have[keySSHHost], host); err != nil || hk.public != have[keySSHHostPub] {
		t.Errorf("host key pair: %v", err)
	}
	if !contains(tool.encrypted, secrets) || !contains(tool.encrypted, filepath.Join(tmp, p.canaryPath)) {
		t.Errorf("encrypted: %v", tool.encrypted)
	}
	if !contains(tool.rekeyed, filepath.Join(tmp, p.rekey[0])) {
		t.Errorf("rekeyed: %v", tool.rekeyed)
	}
	// rules: the shared admin set + the host / cluster recipient, above the anchors
	nixos := readFile(t, filepath.Join(tmp, "nixos/.sops.yaml"))
	block := markerBlock(nixos, host)
	if strings.Count(block, "- age1") != 4 || !strings.Contains(block, p.hostRecip) || !strings.Contains(block, "path_regex: secrets/"+host+"\\.yaml$") {
		t.Errorf("nixos rule:\n%s", block)
	}
	if strings.Index(nixos, "sops rule: "+host) > strings.Index(nixos, "path_regex: secrets/.*") {
		t.Error("host rule landed below the catch-all")
	}
	fleet := readFile(t, filepath.Join(tmp, ".sops.yaml"))
	block = markerBlock(fleet, host)
	if strings.Count(block, "- age1") != 2 || !strings.Contains(block, p.cluster.recipient+" # "+host) || !strings.Contains(block, "# admin") {
		t.Errorf("fleet rule:\n%s", block)
	}
	if !strings.Contains(fleet, "      - "+p.cluster.recipient+" # cluster: "+host+"\n") {
		t.Error("infrastructure recipient line missing")
	}
	peers := readFile(t, filepath.Join(tmp, "wireguard/peers.json"))
	if !strings.Contains(peers, `"`+host+`": {`) || !strings.Contains(peers, `"wg_endpoint_ip": "`+host+`.example.cloud"`) || !strings.Contains(peers, `"public_key": "`+p.wg.public+`"`) {
		t.Errorf("peer:\n%s", peers)
	}
	if len(*registered) != 1 || (*registered)[0].Title != "flux-"+host || !strings.HasPrefix((*registered)[0].Key, "ssh-ed25519 ") {
		t.Errorf("gitea: %+v", *registered)
	}
	for _, want := range []string{"● nixos/secrets/" + host + ".yaml", "○ nixos/.sops.yaml", "○ wireguard/peers.json", "○ .sops.yaml", "◆ fleet/infrastructure/", "◆ fleet/apps/" + host + "/sops-canary.yaml", "registered flux-" + host, "staged."} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}

	// re-run: identity restored from the file, nothing to do, key verified on gitea
	after := map[string]string{}
	for _, rel := range files {
		after[rel] = readFile(t, filepath.Join(tmp, rel))
	}
	tool2 := &fakeTool{encrypted: tool.encrypted}
	p2, err := planMint(tmp, host, tool2, gitea)
	if err != nil {
		t.Fatal(err)
	}
	if p2.newFile || len(p2.missing) != 0 || len(p2.edits) != 0 || len(p2.rekey) != 0 || p2.canaryPath != "" || p2.registerOn || !strings.Contains(p2.deployKey, "verified match") {
		t.Errorf("re-run plan: missing=%v edits=%d rekey=%v canary=%q deploy=%q", p2.missing, len(p2.edits), p2.rekey, p2.canaryPath, p2.deployKey)
	}
	if p2.hostRecip != p.hostRecip || p2.cluster.recipient != p.cluster.recipient || p2.wg.public != p.wg.public {
		t.Error("re-run derived a different identity")
	}
	if err := p2.apply(tool2, gitea); err != nil {
		t.Fatal(err)
	}
	if len(tool2.sets) != 0 || len(*registered) != 1 {
		t.Errorf("re-run wrote: sets=%v gitea=%d", tool2.sets, len(*registered))
	}
	for _, rel := range files {
		if readFile(t, filepath.Join(tmp, rel)) != after[rel] {
			t.Errorf("re-run changed %s", rel)
		}
	}

	// a stale key on gitea refuses before anything is written
	(*registered)[0].Key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx6GXsI2LCgCru4Jm5qAV4xKqMfWHKrmW2pVHvG6GGa stale"
	if _, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Errorf("stale gitea key: want refusal, got %v", err)
	}

	// a secrets file that does not hold the identity the rules name refuses:
	// a fresh host key would derive a recipient the nixos rule lacks
	keep := readFile(t, secrets)
	other, _ := newSSHKey(host)
	var doc map[string]string
	yaml.Unmarshal([]byte(keep), &doc)
	doc[keySSHHost], doc[keySSHHostPub] = other.private, other.public
	out, _ := yaml.Marshal(doc)
	os.WriteFile(secrets, out, 0o600)
	if _, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea); err == nil || !strings.Contains(err.Error(), "host recipient is not the one") {
		t.Errorf("foreign host key: want refusal, got %v", err)
	}
	delete(doc, keySSHHost)
	delete(doc, keySSHHostPub)
	out, _ = yaml.Marshal(doc)
	os.WriteFile(secrets, out, 0o600)
	if _, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea); err == nil || !strings.Contains(err.Error(), "real ssh key is not in the secrets file") {
		t.Errorf("missing host key under an existing rule: want refusal, got %v", err)
	}
	yaml.Unmarshal([]byte(keep), &doc)
	doc[keyAge] = "AGE-SECRET-KEY-104U5P2EWFVZE2WA8X3AZLTY5YQKPD782FPYSLD0FEQFJFS6EJ7VS3FZH2S"
	out, _ = yaml.Marshal(doc)
	os.WriteFile(secrets, out, 0o600)
	if _, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea); err == nil || !strings.Contains(err.Error(), "cluster recipient is not the one") {
		t.Errorf("foreign cluster key: want refusal, got %v", err)
	}
	yaml.Unmarshal([]byte(keep), &doc)
	doc[keyWgPrivate] = "dwdtDXMYpX08FsFyUbJmRd9MLofrwJkqsXf7pR25LCo="
	out, _ = yaml.Marshal(doc)
	os.WriteFile(secrets, out, 0o600)
	if _, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea); err == nil || !strings.Contains(err.Error(), "peers.json already lists") {
		t.Errorf("foreign wireguard key: want refusal, got %v", err)
	}
	os.WriteFile(secrets, []byte(keep), 0o600)
	(*registered)[0].Key = strings.TrimSpace(p.deploy.public)

	// round trip: dismantle gives the original files back
	d, err := planDismantle(tmp, host)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.apply(&fakeTool{}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".sops.yaml", "nixos/.sops.yaml", "wireguard/peers.json"} {
		if got := readFile(t, filepath.Join(tmp, rel)); got != original[rel] {
			t.Errorf("%s after mint+dismantle differs from the original", rel)
		}
	}
	if got := strings.TrimSpace(readFile(t, filepath.Join(tmp, "provisioning/inventory.yaml"))); got != strings.TrimSpace(original["provisioning/inventory.yaml"]) {
		t.Error("inventory after mint+dismantle differs from the original")
	}
}

// A lab host that is not a flux cluster: host key, wg key, k3s token,
// the nixos rule and the peer (endpoint from inventory) — no cluster
// key, no fleet rule, no canary, no gitea.
func TestMintLabHost(t *testing.T) {
	const host = "lab-plain"
	tmp := t.TempDir()
	write := func(rel, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(tmp, rel)), 0o755)
		os.WriteFile(filepath.Join(tmp, rel), []byte(content), 0o644)
	}
	write("provisioning/inventory.yaml", "all:\n  children:\n    nixos:\n      hosts:\n        "+host+":\n          ansible_host: 10.0.0.77\n          wg_vpn_ip: 10.1.0.77\n          wg_endpoint_ip: 10.0.0.77\n")
	write("nixos/.sops.yaml", "creation_rules:\n  - path_regex: secrets/.*\\.yaml$\n    age:\n      - age1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # admin\n")
	write("wireguard/peers.json", "{\n  \"peers\": {}\n}\n")
	write("nixos/hosts/"+host+"/default.nix", "{ }\n")
	tool := &fakeTool{}
	p, err := planMint(tmp, host, tool, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.missing) != 4 || len(p.edits) != 2 || p.canaryPath != "" || len(p.rekey) != 0 || p.deployKey != "" {
		t.Fatalf("plan: missing=%v edits=%d canary=%q rekey=%v deploy=%q", p.missing, len(p.edits), p.canaryPath, p.rekey, p.deployKey)
	}
	if err := p.apply(tool, nil); err != nil {
		t.Fatal(err)
	}
	nixos := readFile(t, filepath.Join(tmp, "nixos/.sops.yaml"))
	if !strings.Contains(nixos, "      - age1aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa # admin\n      - "+p.hostRecip+"\n") {
		t.Errorf("fresh config: the catch-all's recipients + the host:\n%s", nixos)
	}
	if peers := readFile(t, filepath.Join(tmp, "wireguard/peers.json")); !strings.Contains(peers, `"wg_endpoint_ip": "10.0.0.77"`) {
		t.Errorf("peer endpoint from inventory:\n%s", peers)
	}
	var have map[string]string
	if err := yamlUnmarshalFile(filepath.Join(tmp, "nixos/secrets", host+".yaml"), &have); err != nil {
		t.Fatal(err)
	}
	if _, ok := have[keyAge]; ok || len(have) != 4 {
		t.Errorf("lab host secrets: %v", keysOf(have))
	}
}

func TestSharedRecipients(t *testing.T) {
	blockA := "  # BEGIN ANSIBLE sops rule: a\n  - path_regex: x\n    age:\n      - age1admin # admin\n      - age1ops\n      - age1hosta # a\n  # END ANSIBLE sops rule: a\n"
	blockB := "  # BEGIN ANSIBLE sops rule: b\n  - path_regex: y\n    age:\n      - age1ops\n      - age1admin # admin\n      - age1hostb # b\n  # END ANSIBLE sops rule: b\n"
	catchAll := "  - path_regex: .*\n    age:\n      - age1admin # admin\n      - age1fallback\n"
	if got := strings.Join(sharedRecipients("creation_rules:\n"+blockA+blockB+catchAll), ","); got != "age1admin # admin,age1ops" {
		t.Errorf("two blocks: %q", got)
	}
	// one block: its host's own recipient must not become "shared" — the catch-all joins
	if got := strings.Join(sharedRecipients("creation_rules:\n"+blockA+catchAll), ","); got != "age1admin # admin" {
		t.Errorf("one block: %q", got)
	}
	if got := strings.Join(sharedRecipients("creation_rules:\n"+catchAll), ","); got != "age1admin # admin,age1fallback" {
		t.Errorf("fallback: %q", got)
	}
}

func TestMarkerBlock(t *testing.T) {
	cfg := "creation_rules:\n  # BEGIN ANSIBLE sops rule: sfi-fort-e33e2d5\n  - path_regex: x\n    age:\n      - age1other\n  # END ANSIBLE sops rule: sfi-fort-e33e2d5\n"
	if markerBlock(cfg, "sfi-fort") != "" {
		t.Error("a host-name prefix matched another host's block")
	}
	if b := markerBlock(cfg, "sfi-fort-e33e2d5"); !strings.HasPrefix(b, "  # BEGIN") || !strings.HasSuffix(b, "# END ANSIBLE sops rule: sfi-fort-e33e2d5") {
		t.Errorf("block: %q", b)
	}
}

func TestDeployKeyStatus(t *testing.T) {
	const pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx6GXsI2LCgCru4Jm5qAV4xKqMfWHKrmW2pVHvG6GGa flux-h\n"
	ours := strings.TrimSpace(pub)
	stale := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIStale flux-h"
	for _, tc := range []struct {
		keys          []giteaKey
		want, wantErr string
		wantRegister  bool
	}{
		{nil, "would register", "", true},
		{[]giteaKey{{ID: 1, Title: "flux-other", Key: stale}}, "would register", "", true},
		{[]giteaKey{{ID: 7, Title: "flux-h", Key: ours}}, "id 7, verified match", "", false},
		{[]giteaKey{{ID: 7, Title: "flux-h", Key: ours}, {ID: 9, Title: "flux-h", Key: ours}}, "ids 7, 9 — duplicates", "", false},
		{[]giteaKey{{ID: 7, Title: "flux-h", Key: ours}, {ID: 9, Title: "flux-h", Key: stale}}, "", "id 9) differs", false},
		{[]giteaKey{{ID: 9, Title: "flux-h", Key: stale}, {ID: 7, Title: "flux-h", Key: ours}}, "", "id 9) differs", false},
	} {
		g, _ := fakeGitea(t, tc.keys)
		got, register, err := deployKeyStatus(g, "h", pub)
		switch {
		case tc.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%v: want error %q, got %q %v", tc.keys, tc.wantErr, got, err)
			}
		case err != nil || !strings.Contains(got, tc.want) || register != tc.wantRegister:
			t.Errorf("%v: got %q register=%v %v", tc.keys, got, register, err)
		}
	}
}

func TestMintFluxNeedsGitea(t *testing.T) {
	repo, err := repoRootFrom(mustGetwd(t))
	if err != nil {
		t.Skip("not inside the deployments checkout")
	}
	tmp := t.TempDir()
	copyFile(t, filepath.Join(repo, "provisioning/inventory.yaml"), filepath.Join(tmp, "provisioning/inventory.yaml"))
	if _, err := planMint(tmp, "sfi-prod-439bf72", &fakeTool{}, nil); err == nil || !strings.Contains(err.Error(), "needs the gitea client") {
		t.Errorf("flux cluster without a client: want refusal, got %v", err)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func keysOf(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func yamlUnmarshalFile(path string, v any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(raw, v)
}
