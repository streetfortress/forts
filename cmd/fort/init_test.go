package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/estate"
)

var testInitView = initView{Name: "test-estate", Repo: "ssh://git@gitea.zen.lofi:30022/sfi/test-estate", RepoGit: "ssh://git@gitea.zen.lofi:30022/sfi/test-estate.git",
	Forts: "git+https://example.invalid/forts", Admins: []string{"age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpq0d5e6m"}}

// testOperatorKey is a throwaway operator key, the device in its comment.
const testOperatorKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJDZXdVYO58sXRAwLieovSB3fHDAhMF96rjyfCx+vLuL macbook-air"

// The render: every file an estate needs before its first scaffold, in
// the shape the other verbs read — mint's anchors in the two sops rule
// files, the estate manifest that parses back, the flake that names the
// framework.
func TestRenderInit(t *testing.T) {
	files, err := renderInit(testInitView)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, f := range files {
		m[f.path] = string(f.content)
	}
	for _, p := range []string{"estate.toml", "README.md", "justfile", ".gitignore", ".sops.yaml", "nixos/.sops.yaml", "nixos/flake.nix", "nixos/certs/README.md",
		"forts/.gitkeep", "nixos/hosts/.gitkeep", "nixos/secrets/.gitkeep", "mesh/peers.json", "provisioning/README.md", "provisioning/pulumi/Pulumi.yaml", "provisioning/pulumi/state/.gitkeep",
		"flux/apps/.gitkeep", "flux/clusters/.gitkeep", "flux/infrastructure/controllers/kustomization.yaml", "flux/infrastructure/configs/kustomization.yaml", ".gitea/workflows/checks.yaml"} {
		if _, ok := m[p]; !ok {
			t.Errorf("init lacks %s", p)
		}
	}
	for _, p := range []string{"flux/infrastructure/configs/step-ca-root.pem", "flux/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml"} {
		if _, ok := m[p]; ok {
			t.Errorf("init must not write %s — the estate's", p)
		}
	}
	if strings.Contains(strings.Join(func() []string {
		var ps []string
		for p := range m {
			ps = append(ps, p)
		}
		return ps
	}(), " "), ".gotpl") {
		t.Error("a .gotpl suffix survived the render")
	}
	est, err := estate.Parse([]byte(m["estate.toml"]))
	if err != nil || est.Name != "test-estate" || len(est.Hubs) != 0 {
		t.Errorf("estate.toml: %+v %v", est, err)
	}
	// no zone named: the default derives from the name, and the comment
	// says which (forts#45)
	if est.Mesh.Zone != "" || est.ZoneName() != "test-estate.mesh" || !strings.Contains(m["estate.toml"], "# zone = \"test-estate.mesh\"") {
		t.Errorf("estate.toml zone default:\n%s", m["estate.toml"])
	}
	// mint's anchors, exactly once each
	if strings.Count(m["nixos/.sops.yaml"], "  - path_regex: secrets/.*\\.yaml$\n") != 1 || !strings.Contains(m["nixos/.sops.yaml"], "      - "+testInitView.Admins[0]) {
		t.Errorf("nixos/.sops.yaml:\n%s", m["nixos/.sops.yaml"])
	}
	if strings.Count(m[".sops.yaml"], "  - path_regex: flux/infrastructure/") != 1 || strings.Count(m[".sops.yaml"], "# fort-managed: one recipient per cluster") != 1 {
		t.Errorf(".sops.yaml:\n%s", m[".sops.yaml"])
	}
	if !strings.Contains(m["nixos/flake.nix"], `forts.url = "git+https://example.invalid/forts";`) || !strings.Contains(m["nixos/flake.nix"], "forts.nixosModules.common") {
		t.Errorf("flake:\n%s", m["nixos/flake.nix"])
	}
	// The gate is the core's two steps and no layer's: a layer's step is
	// what `fort layer add` appends (forts#144), and nothing logs into a
	// chart registry here since the contract gate left (forts#143).
	if !strings.Contains(m[".gitea/workflows/checks.yaml"], "nix run ./nixos#fort -- check --all --eval") || strings.Contains(m[".gitea/workflows/checks.yaml"], "helm") || strings.Contains(m[".gitea/workflows/checks.yaml"], "crd-diff") {
		t.Errorf("workflow:\n%s", m[".gitea/workflows/checks.yaml"])
	}
	// The cache key is a live workflow expression, not the escaping that
	// renders it: the gate restores the closure of the pin it reads from
	// nixos/flake.lock (forts#75).
	if !strings.Contains(m[".gitea/workflows/checks.yaml"], "key: nix-closure-v2-${{ hashFiles('nixos/flake.lock') }}") {
		t.Errorf("workflow cache key:\n%s", m[".gitea/workflows/checks.yaml"])
	}
	if !strings.Contains(m["provisioning/pulumi/Pulumi.yaml"], "binary: fort-pulumi") {
		t.Errorf("Pulumi.yaml:\n%s", m["provisioning/pulumi/Pulumi.yaml"])
	}
	// The justfile holds the estate's own verbs and no wrapper for the
	// CLI: a fresh estate does not vendor this source, so a `go run
	// ./cmd/fort` recipe would name a path that is not there (forts#86).
	// fetch-kubeconfig reads the rendered client config, not the manifest
	// path an older estate used.
	if !strings.Contains(m["justfile"], "\nfetch-kubeconfig host:\n") || !strings.Contains(m["justfile"], "\nupdatekeys:\n") {
		t.Errorf("justfile recipes:\n%s", m["justfile"])
	}
	if !strings.Contains(m["justfile"], "\nfort +ARGS:\n") || !strings.Contains(m["justfile"], "nix shell $installables -c fort {{ARGS}}") {
		t.Errorf("justfile lacks the fort recipe:\n%s", m["justfile"])
	}
	if strings.Contains(m["justfile"], "go run ./cmd/fort") || strings.Contains(m["justfile"], "provisioning/forts/") {
		t.Errorf("justfile:\n%s", m["justfile"])
	}
	if !strings.Contains(m["justfile"], "-F mesh/ssh_config") {
		t.Errorf("fetch-kubeconfig must read mesh/ssh_config:\n%s", m["justfile"])
	}
	// The read-only ops identity takes a layer's rules by aggregation
	// (#169): the aggregated role and its binding, and the
	// prometheus-operator kinds the hub runs.
	if ops := m["flux/infrastructure/configs/ops-readonly.yaml"]; !strings.Contains(ops, "name: ops-ro-layers-read") || !strings.Contains(ops, `sfi.ops/read: "true"`) || strings.Count(ops, "name: ops-ro-layers-read") != 3 || !strings.Contains(ops, "thanosrulers") {
		t.Error("ops-readonly.yaml lacks the aggregated layers role, its binding, or thanosrulers")
	}
	// The core pushes nowhere: a monitoring layer adds the remote_write,
	// the log shipper and the variable they read (forts#143).
	if strings.Contains(m["flux/infrastructure/configs/monitoring/prometheus-edge.yaml"], "remoteWrite") || strings.Contains(m["flux/infrastructure/controllers/kustomization.yaml"], "fluent-bit") {
		t.Error("the core must carry neither remote_write nor fluent-bit")
	}
	for p, c := range m {
		if strings.HasPrefix(p, "flux/infrastructure/") && strings.Contains(c, "${HUB_MESH_IP}") {
			t.Errorf("%s references HUB_MESH_IP, a layer's variable", p)
		}
	}
	// The two identity labels (forts#50): estate beside cluster on every
	// series the edge keeps; a monitoring layer pushes them on.
	if !strings.Contains(m["flux/infrastructure/configs/monitoring/prometheus-edge.yaml"], "  externalLabels:\n    cluster: ${CLUSTER_NAME}\n    estate: ${ESTATE_NAME}\n") {
		t.Errorf("prometheus-edge external labels:\n%s", m["flux/infrastructure/configs/monitoring/prometheus-edge.yaml"])
	}
	// hubs in another estate: the forge with its name, and a role the
	// estate names (a layer's, forts#143); the commented examples give
	// way to the real tables
	v := testInitView
	v.Hubs, v.Forge = []hubView{{Role: "forge", Estate: "ssh://git@gitea.zen.lofi:30022/sfi/infra", Fort: "sfi-fort-zenbook", Name: "gitea.zen.lofi"}, {Role: "monitoring", Estate: "ssh://git@gitea.zen.lofi:30022/sfi/infra", Fort: "luxor"}}, true
	files, _ = renderInit(v)
	for _, f := range files {
		if f.path != "estate.toml" {
			continue
		}
		got := string(f.content)
		if !strings.Contains(got, "\n[hubs.forge]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/infra\"\nfort = \"sfi-fort-zenbook\"\nname = \"gitea.zen.lofi\"") || !strings.Contains(got, "\n[hubs.monitoring]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/infra\"\nfort = \"luxor\"\n") || strings.Contains(got, "# [hubs.forge]") || strings.Contains(got, "# [hubs.monitoring]") {
			t.Errorf("estate.toml with hubs in another estate:\n%s", got)
		}
		est, err := estate.Parse(f.content)
		if err != nil || est.LocalHub("monitoring") != "" {
			t.Errorf("parses back: %+v %v", est, err)
		}
	}
	// --hub's spelling: the host first, because the URL has an @ of its own
	if role, h, err := parseHubFlag("monitoring=luxor@ssh://git@gitea.zen.lofi:30022/sfi/infra"); err != nil || role != "monitoring" || h.Estate != "ssh://git@gitea.zen.lofi:30022/sfi/infra" || h.Fort != "luxor" || h.Name != "" {
		t.Errorf("parseHubFlag: %s %+v %v", role, h, err)
	}
	for _, bad := range []string{"luxor", "monitoring=luxor", "monitoring=ssh://x/sfi/infra", "monitoring=@ssh://x/sfi/infra"} {
		if _, _, err := parseHubFlag(bad); err == nil {
			t.Errorf("parseHubFlag(%q): want an error", bad)
		}
	}
	// The operators (forts#81): no --operator-key renders the commented
	// example, and the estate names no key — the framework ships none,
	// so until the estate writes one no fort of it evaluates.
	for _, f := range files {
		if f.path == "estate.toml" {
			if est, err := estate.Parse(f.content); err != nil || len(est.Operators()) != 0 || !strings.Contains(string(f.content), "# operator_keys = [") {
				t.Errorf("estate.toml with no operator key: %+v %v\n%s", est, err, f.content)
			}
		}
	}
	// --operator-key: one line per device, with the device as the comment
	v = testInitView
	v.OperatorKeys = []string{testOperatorKey, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBL2o6nnz49u8AXHFIojZiJWJkOFgzH3hplflKDiVz/i macmini"}
	files, _ = renderInit(v)
	for _, f := range files {
		if f.path != "estate.toml" {
			continue
		}
		est, err := estate.Parse(f.content)
		ops := est.Operators()
		if err != nil || len(ops) != 2 || ops[0].Name != "macbook-air" || ops[1].Name != "macmini" || strings.Contains(string(f.content), "# operator_keys") {
			t.Errorf("estate.toml with operator keys: %+v %v\n%s", ops, err, f.content)
		}
	}
	// a zone of its own: rendered as the key, and parsed back as the zone
	v = testInitView
	v.Zone = "lab.internal"
	files, _ = renderInit(v)
	for _, f := range files {
		if f.path == "estate.toml" {
			if est, err := estate.Parse(f.content); err != nil || est.ZoneName() != "lab.internal" || !strings.Contains(string(f.content), "\nzone = \"lab.internal\"") {
				t.Errorf("estate.toml with zone: %+v %v\n%s", est, err, f.content)
			}
		}
	}
}

// hubsView renders every role in est.Hubs: the forge first, then the
// rest sorted by name (#176) — estate.Roles bounds what the core renders
// for, not which tables estate.toml carries.
func TestHubsView(t *testing.T) {
	hub := estate.Hub{Estate: "ssh://git@forge.example:22/acme/hub", Fort: "central"}
	e := estate.Estate{Hubs: map[string]estate.Hub{
		"monitoring":     hub,
		"ca":             hub,
		estate.RoleForge: {Estate: hub.Estate, Fort: hub.Fort, Name: "forge.example"},
	}}
	got := hubsView(e)
	if len(got) != 3 || got[0].Role != estate.RoleForge || got[1].Role != "ca" || got[2].Role != "monitoring" {
		t.Fatalf("hubsView order: %+v", got)
	}
	if got[0].Name != "forge.example" || got[2].Fort != "central" || got[1].Estate != hub.Estate {
		t.Errorf("hubsView fields: %+v", got)
	}
}

// The verb: renders into the git root it is run in, once.
func TestInitCmd(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Chdir(dir)
	args := []string{"--name", "test-estate", "--repo", testInitView.Repo, "--forts", testInitView.Forts, "--admin", testInitView.Admins[0], "--operator-key", testOperatorKey,
		"--hub", "forge=central@" + testInitView.Repo, "--forge-name", "forge.example",
		"--hub", "monitoring=central@" + testInitView.Repo,
		"--hub", "ca=central@" + testInitView.Repo}
	if err := initCmd(append(args, "--dry-run")); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	// A key sshd cannot read is refused before anything is rendered: the
	// account is reached over the mesh and nowhere else, so a typo would
	// be a key no fort accepts and nobody reports.
	if err := initCmd(append(args, "--dry-run", "--operator-key", "macbook-air")); err == nil {
		t.Error("--operator-key with no key: want a refusal")
	}
	if _, err := os.Stat(filepath.Join(dir, "estate.toml")); err == nil {
		t.Fatal("a dry run wrote estate.toml")
	}
	if err := initCmd(args); err != nil { // writing is the default now
		t.Fatalf("write: %v", err)
	}
	est, err := estate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ops := est.Operators(); len(ops) != 1 || ops[0].Name != "macbook-air" {
		t.Errorf("--operator-key reaches estate.toml: %+v", ops)
	}
	// #176: a non-forge --hub reaches estate.toml too — every role, the
	// forge first, then the rest sorted by name.
	if len(est.Hubs) != 3 || est.Hubs["monitoring"].Fort != "central" || est.Hubs["ca"].Estate != testInitView.Repo {
		t.Errorf("every --hub must reach estate.toml: %+v", est.Hubs)
	}
	toml, err := os.ReadFile(filepath.Join(dir, "estate.toml"))
	if err != nil {
		t.Fatal(err)
	}
	forgeAt, caAt, monAt := strings.Index(string(toml), "\n[hubs.forge]\n"), strings.Index(string(toml), "\n[hubs.ca]\n"), strings.Index(string(toml), "\n[hubs.monitoring]\n")
	if forgeAt < 0 || caAt < 0 || monAt < 0 || forgeAt > caAt || caAt > monAt {
		t.Errorf("hub table order (forge %d, ca %d, monitoring %d):\n%s", forgeAt, caAt, monAt, toml)
	}
	for _, p := range []string{"nixos/flake.nix", "mesh/peers.json", "flux/infrastructure/configs/kustomization.yaml", "nixos/hosts/.gitkeep", ".gitea/workflows/checks.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := initCmd(append(args, "--write")); err == nil || !strings.Contains(err.Error(), "already") { // --write accepted as a no-op
		t.Errorf("a second init must refuse: %v", err)
	}
	if err := initCmd([]string{"--name", "x"}); err == nil {
		t.Error("init without --repo and --admin must refuse")
	}
}

// The vendored Gateway API bundle carries its version twice: the marker
// in the kustomization.yaml header, which renovate.json's customManager
// moves, and the gateway.networking.k8s.io/bundle-version stamp on every
// object of crds.yaml, which only a re-curl of standard-install.yaml
// moves (forts#118). Renovate moves the marker alone, so this test is
// what asks for the curl — the alternative is a documented manual step
// whose failure mode is a person forgetting.
func TestGatewayAPIBundlePin(t *testing.T) {
	files, err := renderInit(testInitView)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, f := range files {
		m[f.path] = string(f.content)
	}
	const dir = "flux/infrastructure/controllers/gateway-api/"
	marker := regexp.MustCompile(`(?m)^# Current version: (v\S+)$`).FindStringSubmatch(m[dir+"kustomization.yaml"])
	if marker == nil {
		t.Fatalf("%skustomization.yaml carries no `# Current version: vX.Y.Z` line, which is the one marker renovate.json reads:\n%s",
			dir, m[dir+"kustomization.yaml"])
	}
	want := marker[1]
	// Every object of the standard channel is stamped: the 10 CRDs, and
	// the ValidatingAdmissionPolicy with its binding. The floor catches a
	// crds.yaml that is truncated or from the experimental channel.
	stamps := regexp.MustCompile(`(?m)^\s+gateway\.networking\.k8s\.io/bundle-version: (v\S+)$`).FindAllStringSubmatch(m[dir+"crds.yaml"], -1)
	if len(stamps) < 10 {
		t.Fatalf("%scrds.yaml holds %d bundle-version stamps; the standard channel stamps at least 10", dir, len(stamps))
	}
	for _, s := range stamps {
		if s[1] != want {
			t.Fatalf("%skustomization.yaml pins %s and crds.yaml is stamped %s. Fetch the bundle the marker names:\n"+
				"  curl -sL -o cmd/fort/templates/init/%scrds.yaml https://github.com/kubernetes-sigs/gateway-api/releases/download/%s/standard-install.yaml",
				dir, want, s[1], dir, want)
		}
	}
}

// The anonymous route (forts#167): an https:// repo renders an
// estate.toml that parses back onto that route, says so beside repo,
// and carries no known_hosts example — the route pins no host key. The
// verb accepts the URL and asks for no --known-hosts.
func TestRenderInitAnonymous(t *testing.T) {
	v := testInitView
	v.Repo, v.RepoGit, v.Anonymous = "https://github.com/acme/test-estate", "https://github.com/acme/test-estate.git", true
	files, err := renderInit(v)
	if err != nil {
		t.Fatal(err)
	}
	var toml string
	for _, f := range files {
		if f.path == "estate.toml" {
			toml = string(f.content)
		}
	}
	est, err := estate.Parse([]byte(toml))
	if err != nil || !est.Anonymous() || est.Source.Auth != "" || est.Source.KnownHosts != "" {
		t.Fatalf("estate.toml on the anonymous route: %+v %v\n%s", est.Source, err, toml)
	}
	if !strings.Contains(toml, `repo = "https://github.com/acme/test-estate"   # the anonymous route`) || strings.Contains(toml, "known_hosts =") {
		t.Errorf("estate.toml:\n%s", toml)
	}
	// The ssh render keeps the commented known_hosts example.
	files, err = renderInit(testInitView)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.path == "estate.toml" && !strings.Contains(string(f.content), "# known_hosts = ") {
			t.Errorf("the ssh route keeps the known_hosts example:\n%s", f.content)
		}
	}

	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Chdir(dir)
	if err := initCmd([]string{"--name", "test-estate", "--repo", v.Repo, "--forts", v.Forts, "--admin", v.Admins[0], "--operator-key", testOperatorKey, "--write"}); err != nil {
		t.Fatalf("init on the anonymous route: %v", err)
	}
	if est, err := estate.Load(dir); err != nil || !est.Anonymous() {
		t.Errorf("the written estate is on the anonymous route: %+v %v", est.Source, err)
	}
}

// The gate renders where the estate's forge runs it (forts#191): GitHub
// reads .github/workflows/ only, and needs neither the closure-cache steps
// nor GITEA_ACTIONS.
func TestRenderInitGateFollowsForge(t *testing.T) {
	gate := func(kind string) map[string]string {
		v := testInitView
		v.ForgeKind = kind
		files, err := renderInit(v)
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, f := range files {
			m[f.path] = string(f.content)
		}
		return m
	}
	for _, kind := range []string{"", "gitea"} {
		m := gate(kind)
		if _, ok := m[".gitea/workflows/checks.yaml"]; !ok {
			t.Errorf("kind %q: no .gitea gate", kind)
		}
		if _, ok := m[".github/workflows/checks.yaml"]; ok {
			t.Errorf("kind %q: a .github gate", kind)
		}
	}
	m := gate("github")
	if _, ok := m[".gitea/workflows/checks.yaml"]; ok {
		t.Error("github: a .gitea gate")
	}
	g, ok := m[".github/workflows/checks.yaml"]
	if !ok {
		t.Fatal("github: no .github gate")
	}
	if !strings.Contains(g, "nix run ./nixos#fort -- check --all --eval") || !strings.Contains(g, "- name: The gate (fort check --all --eval)") {
		t.Errorf("github gate:\n%s", g)
	}
	for _, bad := range []string{"GITEA_ACTIONS", "actions/cache", "nix-closure-cache"} {
		if strings.Contains(g, bad) {
			t.Errorf("github gate carries %q:\n%s", bad, g)
		}
	}
}

// An estate.toml written first is init's input (forts#197): from its own
// place in the checkout it is kept as authored and the rest renders
// around it; a flag beside the file overrides the file.
func TestInitFromEstateToml(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Chdir(dir)
	authored := "name = \"first\"\noperator_keys = [\"" + testOperatorKey + "\"]\n\n[source]\nrepo = \"" + testInitView.Repo + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "estate.toml"), []byte(authored), 0o644); err != nil {
		t.Fatal(err)
	}
	tail := []string{"--forts", testInitView.Forts, "--admin", testInitView.Admins[0]}
	if err := initCmd(append([]string{"estate.toml"}, tail...)); err != nil {
		t.Fatalf("init from its own estate.toml: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "estate.toml"))
	if err != nil || string(got) != authored {
		t.Errorf("the authored estate.toml must stay as written: %v\n%s", err, got)
	}
	if _, err := os.Stat(filepath.Join(dir, "nixos/flake.nix")); err != nil {
		t.Errorf("the rest of the estate must render: %v", err)
	}

	// A file elsewhere seeds a fresh checkout; a flag wins over it.
	src := filepath.Join(t.TempDir(), "seed.toml")
	if err := os.WriteFile(src, []byte(authored), 0o644); err != nil {
		t.Fatal(err)
	}
	dir2 := t.TempDir()
	if out, err := exec.Command("git", "-C", dir2, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Chdir(dir2)
	if err := initCmd(append(append([]string{}, tail...), src, "--name", "second")); err != nil {
		t.Fatalf("init from a seed file: %v", err)
	}
	est, err := estate.Load(dir2)
	if err != nil {
		t.Fatal(err)
	}
	if est.Name != "second" || est.Source.Repo != testInitView.Repo || len(est.OperatorKeys) != 1 {
		t.Errorf("flag over file, file over default: %+v", est)
	}
	if err := initCmd([]string{"a.toml", "b.toml"}); err == nil {
		t.Error("two files must refuse")
	}
}

func TestHostFromManifestPath(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.MkdirAll(filepath.Join(dir, "forts"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if h, err := hostFromManifestPath("forts/web3.toml"); err != nil || h != "web3" {
		t.Errorf("forts/web3.toml: %q, %v", h, err)
	}
	if _, err := hostFromManifestPath("other/web3.toml"); err == nil {
		t.Error("a toml outside forts/ is not a manifest")
	}
}
