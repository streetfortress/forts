package main

import (
	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fakeGitea is the deploy-key half of the fake instance (agent_test.go
// has the whole of it): a list that POST appends to.
func fakeGitea(t *testing.T, keys []giteaKey) (*giteaDeployKeys, *[]giteaKey) {
	st := &fakeGiteaState{deploy: keys}
	return newFakeGitea(t, st), &st.deploy
}

// Mints a new cloud fort on a copy of the real tree's identity files,
// checks every artifact against the shape preseed-identity produced,
// re-runs (a no-op that restores the identity from the secrets file),
// then dismantles and expects the original files back byte-for-byte.
// The fixture declares profile "none": TestMint is about the identity,
// and every other profile pulls fleet credentials out of
// provisioning/secrets.yaml through sops, which a tmpdir has no age key
// for. The backup half's pure logic is covered in mintbackup_test.go.
func TestMint(t *testing.T) {
	repo := estateRoot(t)
	const host = "mint-fort"
	tmp := t.TempDir()
	files := []string{".sops.yaml", "nixos/.sops.yaml", "wireguard/peers.json",
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
	write(manifest.Path(host), "mesh_ip = \"10.1.0.97\"\n[cluster]\n[backups]\nprofile = \"none\"\n[dns]\nzone = \"example.cloud\"\nname = \""+host+"\"\n[linode]\nregion = \"us-sea\"\ntype = \"g6-standard-2\"\n")

	// gate: scaffold has not run
	tool := &fakeTool{}
	st := &fakeGiteaState{}
	gitea, registered := newFakeGitea(t, st), &st.deploy
	fleetSecret = func(root, key string) (string, error) { return "fleet-claude-secret", nil }
	t.Cleanup(func() { fleetSecret = secret })
	if _, err := planMint(tmp, host, tool, gitea, false); err == nil || !strings.Contains(err.Error(), "fort scaffold") {
		t.Fatalf("without the scaffold: want the gate, got %v", err)
	}
	write("nixos/hosts/"+host+"/default.nix", "{ }\n")
	write("fleet/apps/"+host+"/kustomization.yaml", "resources: []\n")
	write("fleet/clusters/"+host+"/apps.yaml", "kind: Kustomization\n")

	p, err := planMint(tmp, host, tool, gitea, false)
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
	if strings.Count(block, "- age1") != len(sharedRecipients(original["nixos/.sops.yaml"]))+1 || !strings.Contains(block, p.hostRecip) || !strings.Contains(block, "path_regex: secrets/"+host+"\\.yaml$") {
		t.Errorf("nixos rule:\n%s", block)
	}
	if strings.Index(nixos, "sops rule: "+host) > strings.Index(nixos, "path_regex: secrets/.*") {
		t.Error("host rule landed below the catch-all")
	}
	fleet := readFile(t, filepath.Join(tmp, ".sops.yaml"))
	block = markerBlock(fleet, host)
	if strings.Count(block, "- age1") != len(sharedRecipients(original[".sops.yaml"]))+1 || !strings.Contains(block, p.cluster.recipient+" # "+host) || !strings.Contains(block, "# admin") {
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
	for _, want := range []string{"● nixos/secrets/" + host + ".yaml", "○ nixos/.sops.yaml", "○ wireguard/peers.json", "○ .sops.yaml", "◆ fleet/infrastructure/", "◆ fleet/apps/" + host + "/sops-canary.yaml", "registered flux-" + host, "staged.",
		"◆ wireguard/ssh_config", "Host " + host + " → deployer@10.1.0.97", "◆ wireguard/known_hosts", "+ " + host + ",10.1.0.97"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
	// the client side (#434): the fort's Host block and its host key, by name
	if sc := readFile(t, filepath.Join(tmp, sshConfigPath)); !strings.Contains(sc, "Host "+host+"\n  Hostname 10.1.0.97\n  User deployer\n  HostKeyAlias "+host+"\n") {
		t.Errorf("ssh_config:\n%s", sc)
	}
	if kh := parseKnownHosts(readFile(t, filepath.Join(tmp, knownHostsPath))); kh[host].ip != "10.1.0.97" || kh[host].key != hostKeyLine(p.hostKey.public) {
		t.Errorf("known_hosts: %+v", kh)
	}

	// re-run: identity restored from the file, nothing to do, key verified on gitea
	after := map[string]string{}
	for _, rel := range files {
		after[rel] = readFile(t, filepath.Join(tmp, rel))
	}
	tool2 := &fakeTool{encrypted: tool.encrypted}
	p2, err := planMint(tmp, host, tool2, gitea, false)
	if err != nil {
		t.Fatal(err)
	}
	if p2.newFile || len(p2.missing) != 0 || len(p2.edits) != 0 || len(p2.rekey) != 0 || p2.canaryPath != "" || p2.registerOn || !strings.Contains(p2.deployKey, "verified match") || p2.sshConfig != "" || p2.knownHosts != "" {
		t.Errorf("re-run plan: missing=%v edits=%d rekey=%v canary=%q deploy=%q ssh=%d kh=%d", p2.missing, len(p2.edits), p2.rekey, p2.canaryPath, p2.deployKey, len(p2.sshConfig), len(p2.knownHosts))
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
	if _, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea, false); err == nil || !strings.Contains(err.Error(), "differs") {
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
	if _, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea, false); err == nil || !strings.Contains(err.Error(), "host recipient is not the one") {
		t.Errorf("foreign host key: want refusal, got %v", err)
	}
	delete(doc, keySSHHost)
	delete(doc, keySSHHostPub)
	out, _ = yaml.Marshal(doc)
	os.WriteFile(secrets, out, 0o600)
	if _, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea, false); err == nil || !strings.Contains(err.Error(), "real ssh key is not in the secrets file") {
		t.Errorf("missing host key under an existing rule: want refusal, got %v", err)
	}
	yaml.Unmarshal([]byte(keep), &doc)
	doc[keyAge] = "AGE-SECRET-KEY-104U5P2EWFVZE2WA8X3AZLTY5YQKPD782FPYSLD0FEQFJFS6EJ7VS3FZH2S"
	out, _ = yaml.Marshal(doc)
	os.WriteFile(secrets, out, 0o600)
	if _, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea, false); err == nil || !strings.Contains(err.Error(), "cluster recipient is not the one") {
		t.Errorf("foreign cluster key: want refusal, got %v", err)
	}
	yaml.Unmarshal([]byte(keep), &doc)
	doc[keyWgPrivate] = "dwdtDXMYpX08FsFyUbJmRd9MLofrwJkqsXf7pR25LCo="
	out, _ = yaml.Marshal(doc)
	os.WriteFile(secrets, out, 0o600)
	if _, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea, false); err == nil || !strings.Contains(err.Error(), "peers.json already lists") {
		t.Errorf("foreign wireguard key: want refusal, got %v", err)
	}
	os.WriteFile(secrets, []byte(keep), 0o600)
	khPath := filepath.Join(tmp, knownHostsPath)
	khKeep := readFile(t, khPath)
	os.WriteFile(khPath, []byte(knownHostsSet(khKeep, host, "10.1.0.97", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx6GXsI2LCgCru4Jm5qAV4xKqMfWHKrmW2pVHvG6GGa stale")), 0o644)
	if _, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea, false); err == nil || !strings.Contains(err.Error(), "known_hosts already lists") {
		t.Errorf("foreign host key line: want refusal, got %v", err)
	}
	os.WriteFile(khPath, []byte(khKeep), 0o644)
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
	if exists(tmp, manifest.Path(host)) {
		t.Error("manifest still exists after dismantle")
	}
	if sc := readFile(t, filepath.Join(tmp, sshConfigPath)); strings.Contains(sc, host) {
		t.Errorf("ssh_config still names %s after dismantle:\n%s", host, sc)
	}
	if kh := readFile(t, khPath); strings.Contains(kh, host) || kh != knownHostsHeader {
		t.Errorf("known_hosts after dismantle:\n%s", kh)
	}
}

func TestMintNeedsGitea(t *testing.T) {
	repo := estateRoot(t)
	tmp := t.TempDir()
	copyManifest(t, repo, tmp, "sfi-prod-439bf72")
	if _, err := planMint(tmp, "sfi-prod-439bf72", &fakeTool{}, nil, false); err == nil || !strings.Contains(err.Error(), "needs the gitea client") {
		t.Errorf("without a client: want refusal, got %v", err)
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

// A dev fort ([dev], #413): host key, wireguard key and the deploy key
// (auto-rebuild's credential) are minted; the cluster identity — k3s
// token, cluster age key, fleet rule, infra recipient, canary — is not,
// and mint's gate wants only the host dir. Dismantle round-trips without
// a fleet rule to remove.
func TestMintDevFort(t *testing.T) {
	repo := estateRoot(t)
	const host = "mint-dev"
	tmp := t.TempDir()
	files := []string{".sops.yaml", "nixos/.sops.yaml", "wireguard/peers.json",
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
	// Two accounts (#532): the dispatch account, which rides the fort's
	// own name, and a narrower one with its own alias and scopes.
	accounts := []manifest.DevAccount{
		testAccount("agent", "pickup", "sfi-agent", "write:repository", "write:issue"),
		testAccount("analyst", "review", "sfi-analyst", "write:issue"),
	}
	write(manifest.Path(host), devManifest("10.1.0.96", "10.0.0.96", accounts...))
	tool := &fakeTool{}
	st := &fakeGiteaState{}
	gitea, registered := newFakeGitea(t, st), &st.deploy
	fleetSecret = func(root, key string) (string, error) { return "fleet-claude-secret", nil }
	t.Cleanup(func() { fleetSecret = secret })
	if _, err := planMint(tmp, host, tool, gitea, false); err == nil || !strings.Contains(err.Error(), "fort scaffold") {
		t.Fatalf("without the scaffold: want the gate, got %v", err)
	}
	// the host dir alone satisfies the gate — no fleet files wanted
	write("nixos/hosts/"+host+"/default.nix", "{ }\n")
	p, err := planMint(tmp, host, tool, gitea, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.newFile || len(p.missing) != 13 || !p.registerOn || p.canaryPath != "" || len(p.rekey) != 0 || len(p.edits) != 2 {
		t.Fatalf("plan: newFile=%v missing=%v register=%v canary=%q rekey=%v edits=%d", p.newFile, p.missing, p.registerOn, p.canaryPath, p.rekey, len(p.edits))
	}
	for _, k := range []string{keyK3sToken, keyAge} {
		if contains(p.missing, k) {
			t.Errorf("dev fort must not mint %s", k)
		}
	}
	for _, k := range []string{keyAgentClaude,
		keyAccountSSH("agent"), keyAccountSSHPub("agent"), keyAccountGPG("agent"), keyAccountToken("agent"),
		keyAccountSSH("analyst"), keyAccountSSHPub("analyst"), keyAccountGPG("analyst"), keyAccountToken("analyst")} {
		if !contains(p.missing, k) {
			t.Errorf("dev fort must mint %s", k)
		}
	}
	if len(p.agents) != 2 || p.claude != "fleet-claude-secret" {
		t.Fatalf("agent plans: %d, claude %q", len(p.agents), p.claude)
	}
	for _, a := range p.agents {
		if !a.mintToken || !a.registerSSH || !a.registerGPG {
			t.Fatalf("%s plan: %+v", a.account.User, a)
		}
	}
	if err := p.apply(tool, gitea); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	p.report(&sb, true)
	rep := sb.String()
	if strings.Contains(rep, "cluster recipient") || strings.Contains(rep, "sops-canary") || !strings.Contains(rep, "dev fort") || !strings.Contains(rep, "registered flux-"+host) ||
		!strings.Contains(rep, "ssh key agent-"+host+": registered") || !strings.Contains(rep, "registered + verified (key id "+p.agents[0].gpg.keyID) || !strings.Contains(rep, "token agent-"+host+": minted") || !strings.Contains(rep, "fleet-wide") {
		t.Errorf("dev report:\n%s", rep)
	}
	// One bundle per gitea user: one of each, the gpg key verified by the
	// signature mint sent, the token with that account's own scopes.
	for _, a := range p.agents {
		u := st.user(a.account.GiteaUser)
		if len(u.keys) != 1 || u.keys[0].Title != "agent-"+host || len(u.gpg) != 1 || !u.gpg[0].Verified {
			t.Errorf("%s bundle: keys=%+v gpg=%+v", a.account.GiteaUser, u.keys, u.gpg)
		}
		if len(u.tokens) != 1 || u.tokens[0].Name != "agent-"+host || strings.Join(u.tokens[0].Scopes, ",") != strings.Join(a.account.TokenScopes, ",") {
			t.Errorf("%s token: %+v", a.account.GiteaUser, u.tokens)
		}
	}
	// the secrets file holds each minted token and the fleet claude token, and no value was printed
	sec := readFile(t, filepath.Join(tmp, "nixos/secrets", host+".yaml"))
	if !strings.Contains(sec, keyAccountToken("agent")+": minted-sfi-agent-agent-"+host) || !strings.Contains(sec, keyAccountToken("analyst")+": minted-sfi-analyst-agent-"+host) ||
		!strings.Contains(sec, keyAgentClaude+": fleet-claude-secret") || !strings.Contains(sec, "BEGIN PGP PRIVATE KEY BLOCK") {
		t.Errorf("secrets file:\n%s", sec)
	}
	if strings.Contains(rep, "minted-sfi-") || strings.Contains(rep, "fleet-claude-secret") {
		t.Errorf("report leaks a value:\n%s", rep)
	}
	if got := readFile(t, filepath.Join(tmp, ".sops.yaml")); got != original[".sops.yaml"] {
		t.Error("a dev fort must not touch the fleet .sops.yaml")
	}
	// The fort's own name answers as the first account; the second has its
	// own alias (#532).
	sc := readFile(t, filepath.Join(tmp, sshConfigPath))
	if !strings.Contains(sc, "# dev fort, libvirt\nHost "+host+"\n  Hostname 10.1.0.96\n  User agent\n") ||
		!strings.Contains(sc, "Host sfi-analyst\n  Hostname 10.1.0.96\n  User analyst\n  HostKeyAlias "+host+"\n") {
		t.Errorf("a dev fort answers as each of its accounts:\n%s", sc)
	}
	if markerBlock(readFile(t, filepath.Join(tmp, "nixos/.sops.yaml")), host) == "" {
		t.Error("nixos rule missing")
	}
	if !strings.Contains(readFile(t, filepath.Join(tmp, "wireguard/peers.json")), `"`+host+`": {`) {
		t.Error("peer missing")
	}
	if len(*registered) != 1 || (*registered)[0].Title != "flux-"+host {
		t.Errorf("gitea: %+v", *registered)
	}
	// re-run: nothing to do
	p2, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea, false)
	if err != nil {
		t.Fatal(err)
	}
	if p2.newFile || len(p2.missing) != 0 || len(p2.edits) != 0 || p2.registerOn {
		t.Errorf("re-run plan: missing=%v edits=%d register=%v", p2.missing, len(p2.edits), p2.registerOn)
	}
	if len(p2.agents) != 2 {
		t.Fatalf("re-run agent plans: %d", len(p2.agents))
	}
	for i, a := range p2.agents {
		if a.mintToken || a.registerSSH || a.registerGPG || a.verifyGPG || a.gpg.keyID != p.agents[i].gpg.keyID || !strings.Contains(a.tokStatus, "registered") {
			t.Errorf("re-run %s plan: %+v", a.account.User, a)
		}
	}
	// round trip: dismantle without a fleet rule to remove
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
}
