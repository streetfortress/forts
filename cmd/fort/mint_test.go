package main

import (
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// fakeGitea is the fake forge (forge_test.go), gitea's kind, with
// deploy keys preloaded: a list that POST appends to.
func fakeGitea(t *testing.T, keys []deployKey) (forge, *[]deployKey) {
	st := &fakeForgeState{deploy: keys}
	return newFakeForge(t, st, "gitea"), &st.deploy
}

// Mints a new cloud fort on a copy of the real tree's identity files,
// checks every artifact against the shape preseed-identity produced,
// re-runs (a no-op that restores the identity from the secrets file),
// then dismantles and expects the original files back byte-for-byte.
// TestMint is about the identity alone: the backup credentials are the
// backups layer's to mint (forts#143).
func TestMint(t *testing.T) {
	repo := estateRoot(t)
	const host = "mint-fort"
	tmp := t.TempDir()
	files := []string{".sops.yaml", "nixos/.sops.yaml", "mesh/peers.json",
		"flux/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml"}
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
	write("estate.toml", "name = \"deployments\"\n[source]\nrepo = \"ssh://git@gitea.zen.lofi:30022/sfi/deployments\"\n")
	write(manifest.Path(host), "mesh_ip = \"10.1.0.97\"\n[cluster]\n[dns]\nzone = \"example.cloud\"\nname = \""+host+"\"\n[linode]\nregion = \"us-sea\"\ntype = \"g6-standard-2\"\n")

	// gate: scaffold has not run
	tool := &fakeTool{}
	st := &fakeForgeState{}
	gitea, registered := newFakeForge(t, st, "gitea"), &st.deploy
	if _, err := planMint(tmp, host, tool, gitea, false); err == nil || !strings.Contains(err.Error(), "fort scaffold") {
		t.Fatalf("without the scaffold: want the gate, got %v", err)
	}
	write("nixos/hosts/"+host+"/default.nix", "{ }\n")
	write("flux/apps/"+host+"/kustomization.yaml", "resources: []\n")
	write("flux/clusters/"+host+"/apps.yaml", "kind: Kustomization\n")

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
	peers := readFile(t, filepath.Join(tmp, "mesh/peers.json"))
	if !strings.Contains(peers, `"`+host+`": {`) || !strings.Contains(peers, `"wg_endpoint_ip": "`+host+`.example.cloud"`) || !strings.Contains(peers, `"public_key": "`+p.wg.public+`"`) {
		t.Errorf("peer:\n%s", peers)
	}
	if len(*registered) != 1 || (*registered)[0].Title != "flux-"+host || !strings.HasPrefix((*registered)[0].Key, "ssh-ed25519 ") {
		t.Errorf("gitea: %+v", *registered)
	}
	for _, want := range []string{"● nixos/secrets/" + host + ".yaml", "○ nixos/.sops.yaml", "○ mesh/peers.json", "○ .sops.yaml", "◆ flux/infrastructure/", "◆ flux/apps/" + host + "/sops-canary.yaml", "registered flux-" + host, "staged.",
		"◆ mesh/ssh_config", "Host " + host + " → deployer@10.1.0.97", "◆ mesh/known_hosts", "+ " + host + ",10.1.0.97"} {
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
	for _, rel := range []string{".sops.yaml", "nixos/.sops.yaml", "mesh/peers.json"} {
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
	if _, err := planMint(tmp, "sfi-prod-439bf72", &fakeTool{}, nil, false); err == nil || !strings.Contains(err.Error(), "needs the forge client") {
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

// A plain fort (no [cluster] table): host key, wireguard key and the
// deploy key (auto-rebuild's credential) are minted; the cluster identity
// — k3s token, cluster age key, fleet rule, infra recipient, canary — is
// not, and mint's gate wants only the host dir. Dismantle round-trips
// without a fleet rule to remove.
//
// The provider decides nothing here (forts#142): the plain fort on the
// hypervisor and the plain fort on a linode get the same identities, so
// the case runs once per provider table.
func TestMintPlainFort(t *testing.T) {
	for _, tc := range []struct{ name, provider, table, endpoint string }{
		{"libvirt", "libvirt", "[libvirt]\nip = \"10.0.0.96\"\n", "10.0.0.96"},
		{"linode", "linode", "[dns]\nzone = \"example.cloud\"\nname = \"mint-plain\"\n[linode]\nregion = \"us-sea\"\ntype = \"g6-nanode-1\"\n", "mint-plain.example.cloud"},
	} {
		t.Run(tc.name, func(t *testing.T) { mintPlainFort(t, tc.provider, tc.table, tc.endpoint) })
	}
}

// mintPlainFort runs the case on its own fixtures, so it runs outside an
// estate checkout too — this repo's CI is one (helpers_test.go's
// estateRoot skips there).
func mintPlainFort(t *testing.T, provider, table, endpoint string) {
	const host = "mint-plain"
	tmp := t.TempDir()
	write := func(rel, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(tmp, rel)), 0o755)
		os.WriteFile(filepath.Join(tmp, rel), []byte(content), 0o644)
	}
	const admin = "age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpq0d5e6m"
	write("estate.toml", "name = \"fixture\"\n[source]\nrepo = \"ssh://git@gitea.zen.lofi:30022/sfi/fixture-estate\"\n")
	write("nixos/.sops.yaml", "creation_rules:\n  - path_regex: secrets/.*\\.yaml$\n    age:\n      - "+admin+" # admin\n")
	write(".sops.yaml", "creation_rules:\n  - path_regex: provisioning/.*\\.yaml$\n    age:\n      - "+admin+" # admin\n")
	write("mesh/peers.json", "{\n  \"peers\": {},\n  \"clients\": {}\n}\n")
	files := []string{".sops.yaml", "nixos/.sops.yaml", "mesh/peers.json"}
	original := map[string]string{}
	for _, rel := range files {
		original[rel] = readFile(t, filepath.Join(tmp, rel))
	}
	write(manifest.Path(host), "mesh_ip = \"10.1.0.96\"\n"+table)
	tool := &fakeTool{}
	st := &fakeForgeState{}
	gitea, registered := newFakeForge(t, st, "gitea"), &st.deploy
	if _, err := planMint(tmp, host, tool, gitea, false); err == nil || !strings.Contains(err.Error(), "fort scaffold") {
		t.Fatalf("without the scaffold: want the gate, got %v", err)
	}
	// the host dir alone satisfies the gate — no fleet files wanted
	write("nixos/hosts/"+host+"/default.nix", "{ }\n")
	p, err := planMint(tmp, host, tool, gitea, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.newFile || len(p.missing) != 4 || !p.registerOn || p.canaryPath != "" || len(p.rekey) != 0 || len(p.edits) != 2 {
		t.Fatalf("plan: newFile=%v missing=%v register=%v canary=%q rekey=%v edits=%d", p.newFile, p.missing, p.registerOn, p.canaryPath, p.rekey, len(p.edits))
	}
	for _, k := range []string{keyK3sToken, keyAge} {
		if contains(p.missing, k) {
			t.Errorf("a plain fort must not mint %s", k)
		}
	}
	for _, k := range []string{keyWgPrivate, keySSHHost, keySSHHostPub, keyDeploy} {
		if !contains(p.missing, k) {
			t.Errorf("a plain fort must mint %s", k)
		}
	}
	if err := p.apply(tool, gitea); err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	p.report(&sb, true)
	rep := sb.String()
	if strings.Contains(rep, "cluster recipient") || strings.Contains(rep, "sops-canary") || !strings.Contains(rep, "plain fort") || !strings.Contains(rep, "registered flux-"+host) {
		t.Errorf("plain report:\n%s", rep)
	}
	if got := readFile(t, filepath.Join(tmp, ".sops.yaml")); got != original[".sops.yaml"] {
		t.Error("a plain fort must not touch the fleet .sops.yaml")
	}
	// The fort answers as deployer, like every fort, and the comment
	// names the provider the manifest carries.
	sc := readFile(t, filepath.Join(tmp, sshConfigPath))
	if !strings.Contains(sc, "# plain fort, "+provider+"\nHost "+host+"\n  Hostname 10.1.0.96\n  User deployer\n") {
		t.Errorf("a plain fort answers as deployer:\n%s", sc)
	}
	// The peer's endpoint is the provider's: the lab host's static LAN
	// address, the cloud host's public name.
	if !strings.Contains(readFile(t, filepath.Join(tmp, "mesh/peers.json")), `"wg_endpoint_ip": "`+endpoint+`"`) {
		t.Errorf("peer endpoint is not %s:\n%s", endpoint, readFile(t, filepath.Join(tmp, "mesh/peers.json")))
	}
	if markerBlock(readFile(t, filepath.Join(tmp, "nixos/.sops.yaml")), host) == "" {
		t.Error("nixos rule missing")
	}
	if !strings.Contains(readFile(t, filepath.Join(tmp, "mesh/peers.json")), `"`+host+`": {`) {
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
	// round trip: dismantle without a fleet rule to remove
	d, err := planDismantle(tmp, host)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.apply(&fakeTool{}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".sops.yaml", "nixos/.sops.yaml", "mesh/peers.json"} {
		if got := readFile(t, filepath.Join(tmp, rel)); got != original[rel] {
			t.Errorf("%s after mint+dismantle differs from the original", rel)
		}
	}
}

// The console door at the first provision (sfi/forts#24): a host whose
// default.nix enables sfi.ops.console gets the credential with its
// identity — the hash as one more key of the secrets file, the
// passphrase in the
// registry, the registry written first. Restore-not-rotate holds: a
// re-run mints nothing, and a hash whose passphrase is gone from the
// registry is reported, never replaced — console-password --rotate is
// the repair. Built on its own fixtures, so it runs outside an estate
// checkout too (the mirror tests above skip there).
func TestMintConsole(t *testing.T) {
	const host = "mint-console"
	tmp := t.TempDir()
	write := func(rel, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(tmp, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const admin = "age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpq0d5e6m"
	write("estate.toml", "name = \"fixture\"\n[source]\nrepo = \"ssh://git@gitea.zen.lofi:30022/sfi/fixture-estate\"\n")
	write(manifest.Path(host), "mesh_ip = \"10.1.0.95\"\n[libvirt]\nip = \"10.0.0.95\"\n")
	write("nixos/.sops.yaml", "creation_rules:\n  - path_regex: secrets/.*\\.yaml$\n    age:\n      - "+admin+" # admin\n")
	write(".sops.yaml", "creation_rules:\n  - path_regex: provisioning/.*\\.yaml$\n    age:\n      - "+admin+" # admin\n")
	write("mesh/peers.json", "{\n  \"peers\": {},\n  \"clients\": {}\n}\n")
	// The scaffold's output, reduced to the one line mint reads.
	write("nixos/hosts/"+host+"/default.nix", "{ inputs, ... }:\n{\n  imports = [ inputs.forts.nixosModules.ops-console ];\n  sfi.ops.console.enable = true;\n}\n")

	// CI has no mkpasswd and no nix; the hash's shape is mkpasswdHash's
	// contract (console_test.go).
	hashPassword = func(string) (string, error) { return "$y$j9T$fake$hash", nil }
	t.Cleanup(func() { hashPassword = mkpasswdHash })

	tool := &fakeTool{}
	gitea, _ := fakeGitea(t, nil)
	p, err := planMint(tmp, host, tool, gitea, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.console || !p.consoleMint || !p.newRegistry || p.consoleOrphan {
		t.Fatalf("plan: console=%v mint=%v newRegistry=%v orphan=%v", p.console, p.consoleMint, p.newRegistry, p.consoleOrphan)
	}
	// The plain fort's four keys and the hash, the hash last.
	if len(p.missing) != 5 || p.missing[4] != keyDeployerPasswordHash {
		t.Fatalf("missing: %v", p.missing)
	}
	var sb strings.Builder
	p.report(&sb, false)
	if !strings.Contains(sb.String(), "● "+consoleRegistry) || !strings.Contains(sb.String(), "dry run") {
		t.Fatalf("dry run: want the registry planned, got:\n%s", sb.String())
	}
	if p.consoleHash != "" {
		t.Fatal("a dry run must not hash")
	}

	if err := p.apply(tool, gitea); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(tmp, consoleRegistry)
	secrets := filepath.Join(tmp, p.secretsPath)
	// Both files encrypted once, the registry before the secrets file:
	// a failure between them leaves a passphrase nobody uses, never a
	// hash nobody can read back.
	ri, si := -1, -1
	for i, e := range tool.encrypted {
		switch e {
		case registry:
			ri = i
		case secrets:
			si = i
		}
	}
	if ri < 0 || si < 0 || ri > si {
		t.Fatalf("encrypted: %v — want the registry, then the secrets file", tool.encrypted)
	}
	reg := readFile(t, registry)
	if !strings.HasPrefix(reg, host+": ") || !strings.HasSuffix(reg, "\n") {
		t.Fatalf("registry: %q", reg)
	}
	pass := strings.TrimSpace(strings.TrimPrefix(reg, host+": "))
	if strings.Count(pass, "-") != 4 {
		t.Fatalf("registry holds %q — want the passphrase under the host name", pass)
	}
	var have map[string]string
	if err := yamlUnmarshalFile(secrets, &have); err != nil {
		t.Fatal(err)
	}
	if have[keyDeployerPasswordHash] != "$y$j9T$fake$hash" {
		t.Fatalf("secrets file holds %q under %s — the HOST holds the hash, never the passphrase", have[keyDeployerPasswordHash], keyDeployerPasswordHash)
	}
	if strings.Contains(readFile(t, secrets), pass) {
		t.Fatal("the passphrase reached the secrets file")
	}
	if !contains(tool.added, consoleRegistry) {
		t.Fatalf("added: %v — the registry is not staged", tool.added)
	}
	sb.Reset()
	p.report(&sb, true)
	rep := sb.String()
	if strings.Contains(rep, pass) || strings.Contains(rep, "$y$j9T$fake$hash") {
		t.Fatalf("the report printed a value:\n%s", rep)
	}
	if !strings.Contains(rep, "● "+consoleRegistry) || !strings.Contains(rep, "console-password "+host+" --show") || !strings.Contains(rep, keyDeployerPasswordHash) {
		t.Fatalf("report:\n%s", rep)
	}

	// re-run: both halves there, nothing minted, nothing said
	p2, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea, false)
	if err != nil {
		t.Fatal(err)
	}
	if p2.consoleMint || p2.consoleOrphan || len(p2.missing) != 0 {
		t.Fatalf("re-run: mint=%v orphan=%v missing=%v", p2.consoleMint, p2.consoleOrphan, p2.missing)
	}
	sb.Reset()
	p2.report(&sb, true)
	if strings.Contains(sb.String(), consoleRegistry) {
		t.Fatalf("re-run must not mention the registry:\n%s", sb.String())
	}

	// the orphan: the hash on the host, no passphrase behind it. Mint
	// keeps the hash — somebody may still hold the value — and names the
	// deliberate repair.
	write(consoleRegistry, "sops: {}\n")
	p3, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea, false)
	if err != nil {
		t.Fatal(err)
	}
	if p3.consoleMint || !p3.consoleOrphan || len(p3.missing) != 0 {
		t.Fatalf("orphan: mint=%v orphan=%v missing=%v", p3.consoleMint, p3.consoleOrphan, p3.missing)
	}
	sb.Reset()
	p3.report(&sb, false)
	if !strings.Contains(sb.String(), "! "+consoleRegistry) || !strings.Contains(sb.String(), "console-password "+host+" --rotate --write") {
		t.Fatalf("orphan: want the repair named, got:\n%s", sb.String())
	}

	// a second fort joins an existing registry with a sops set, not a
	// rewrite: the first fort's entry is never touched
	write(consoleRegistry, host+": ENC[z]\nsops: {}\n")
	const other = "mint-console-2"
	write(manifest.Path(other), "mesh_ip = \"10.1.0.94\"\n[libvirt]\nip = \"10.0.0.94\"\n")
	write("nixos/hosts/"+other+"/default.nix", "{\n  sfi.ops.console.enable = true;\n}\n")
	tool4 := &fakeTool{}
	p4, err := planMint(tmp, other, tool4, gitea, false)
	if err != nil {
		t.Fatal(err)
	}
	if !p4.consoleMint || p4.newRegistry {
		t.Fatalf("second fort: mint=%v newRegistry=%v", p4.consoleMint, p4.newRegistry)
	}
	if err := p4.apply(tool4, gitea); err != nil {
		t.Fatal(err)
	}
	if contains(tool4.encrypted, registry) {
		t.Fatal("an existing registry must not be re-encrypted whole")
	}
	var set bool
	for _, s := range tool4.sets {
		if s[0] == registry && s[1] == `["`+other+`"]` && strings.Count(s[2], "-") == 4 {
			set = true
		}
	}
	if !set {
		t.Fatalf("sets: %v — want the second fort's entry set in the registry", tool4.sets)
	}
	sb.Reset()
	p4.report(&sb, true)
	if !strings.Contains(sb.String(), "○ "+consoleRegistry) {
		t.Fatalf("second fort: want the appended registry, got:\n%s", sb.String())
	}

	// a host that does not enable the module: no console half at all,
	// whatever the registry says — that is `fort check`'s warning
	write("nixos/hosts/"+host+"/default.nix", "{ }\n")
	write(consoleRegistry, "sops: {}\n")
	p5, err := planMint(tmp, host, &fakeTool{encrypted: tool.encrypted}, gitea, false)
	if err != nil {
		t.Fatal(err)
	}
	if p5.console || p5.consoleMint || p5.consoleOrphan {
		t.Fatalf("opted out: console=%v mint=%v orphan=%v", p5.console, p5.consoleMint, p5.consoleOrphan)
	}
}
