package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/estate"
)

var testInitView = initView{Name: "test-estate", Repo: "ssh://git@gitea.zen.lofi:30022/sfi/test-estate", RepoGit: "ssh://git@gitea.zen.lofi:30022/sfi/test-estate.git",
	Forts: defaultForts, Registry: "gitea.zen.lofi", Admins: []string{"age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpq0d5e6m"}}

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
	for _, p := range []string{"estate.toml", "README.md", ".gitignore", ".sops.yaml", "nixos/.sops.yaml", "nixos/flake.nix", "nixos/certs/README.md",
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
	if err != nil || est.Name != "test-estate" || est.HasHub() {
		t.Errorf("estate.toml: %+v %v", est, err)
	}
	// mint's anchors, exactly once each
	if strings.Count(m["nixos/.sops.yaml"], "  - path_regex: secrets/.*\\.yaml$\n") != 1 || !strings.Contains(m["nixos/.sops.yaml"], "      - "+testInitView.Admins[0]) {
		t.Errorf("nixos/.sops.yaml:\n%s", m["nixos/.sops.yaml"])
	}
	if strings.Count(m[".sops.yaml"], "  - path_regex: flux/infrastructure/") != 1 || strings.Count(m[".sops.yaml"], "# fort-managed: one recipient per cluster") != 1 {
		t.Errorf(".sops.yaml:\n%s", m[".sops.yaml"])
	}
	if !strings.Contains(m["nixos/flake.nix"], `forts.url = "`+defaultForts+`";`) || !strings.Contains(m["nixos/flake.nix"], "forts.nixosModules.common") {
		t.Errorf("flake:\n%s", m["nixos/flake.nix"])
	}
	if !strings.Contains(m[".gitea/workflows/checks.yaml"], "${{ gitea.actor }}") || !strings.Contains(m[".gitea/workflows/checks.yaml"], "helm registry login gitea.zen.lofi") {
		t.Errorf("workflow:\n%s", m[".gitea/workflows/checks.yaml"])
	}
	if !strings.Contains(m["provisioning/pulumi/Pulumi.yaml"], "binary: fort-pulumi") {
		t.Errorf("Pulumi.yaml:\n%s", m["provisioning/pulumi/Pulumi.yaml"])
	}
	// with a hub, estate.toml names it
	v := testInitView
	v.Hub = "luxor"
	files, _ = renderInit(v)
	for _, f := range files {
		if f.path == "estate.toml" && !strings.Contains(string(f.content), "\nhub = \"luxor\"\n") {
			t.Errorf("estate.toml with hub:\n%s", f.content)
		}
	}
}

// The verb: renders into the git root it is run in, once.
func TestInitCmd(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	t.Chdir(dir)
	args := []string{"--name", "test-estate", "--repo", testInitView.Repo, "--admin", testInitView.Admins[0]}
	if err := initCmd(args); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "estate.toml")); err == nil {
		t.Fatal("a dry run wrote estate.toml")
	}
	if err := initCmd(append(args, "--write")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := estate.Load(dir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"nixos/flake.nix", "mesh/peers.json", "flux/infrastructure/configs/kustomization.yaml", "nixos/hosts/.gitkeep", ".gitea/workflows/checks.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := initCmd(append(args, "--write")); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("a second init must refuse: %v", err)
	}
	if err := initCmd([]string{"--name", "x"}); err == nil {
		t.Error("init without --repo and --admin must refuse")
	}
}
