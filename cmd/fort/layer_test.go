package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const flakeFixture = `{
  inputs = {
    forts.url = "git+https://gitea.zen.lofi/sfi/forts";
    nixpkgs.follows = "forts/nixpkgs";
  };

  outputs = inputs@{ self, forts, nixpkgs, ... }:
    let
      layers = sys: {
      };

      fortFor = lib.genAttrs (lib.attrNames forts.packages)
        (sys: { fort = forts.packages.${sys}.fort; } // layers sys);
    in {
      packages = fortFor;
    };
}
`

const gateFixture = `name: checks
jobs:
  gate:
    steps:
      - uses: actions/checkout@v6

      - name: The gate (fort check --all)
        env:
          GITEA_ACTIONS: "true"
        run: nix run ./nixos#fort -- check --all

      - name: Eval every host with a manifest
        run: echo eval
`

func TestParseLayerRef(t *testing.T) {
	for in, want := range map[string]layerRef{
		"git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers#monitoring":      {url: "git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers", input: "fort-layers", role: "monitoring"},
		"git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers.git?ref=v1#apps": {url: "git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers.git?ref=v1", input: "fort-layers", role: "apps"},
		"path:../layers#backups": {url: "path:../layers", input: "layers", role: "backups"},
	} {
		got, err := parseLayerRef(in)
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"git+ssh://x/sfi/fort-layers", "#monitoring", "git+ssh://x/sfi/fort-layers#Monitoring", "git+ssh://x/sfi/fort-layers#-a"} {
		if _, err := parseLayerRef(bad); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func layerEstate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{flakePath: flakeFixture, gatePath: gateFixture})
	return root
}

func TestLayerAddAndRemove(t *testing.T) {
	root := layerEstate(t)
	ref, _ := parseLayerRef("git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers#monitoring")
	p, err := planLayerAdd(root, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.actions) != 2 || len(p.present) != 0 {
		t.Fatalf("plan: %d actions, %d present", len(p.actions), len(p.present))
	}
	var out bytes.Buffer
	p.report(&out, false, []string{"--central", "central"})
	if !strings.Contains(out.String(), "○ nixos/flake.nix") || !strings.Contains(out.String(), "Plan only") {
		t.Errorf("report:\n%s", out.String())
	}
	if err := p.apply(); err != nil {
		t.Fatal(err)
	}
	flake := readFile(t, filepath.Join(root, flakePath))
	for _, want := range []string{
		"    fort-layers.url = \"git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers\";\n  };",
		"      layers = sys: {\n        fort-monitoring = inputs.fort-layers.packages.${sys}.fort-monitoring;\n      };",
	} {
		if !strings.Contains(flake, want) {
			t.Errorf("flake lacks %q:\n%s", want, flake)
		}
	}
	gate := readFile(t, filepath.Join(root, gatePath))
	if !strings.Contains(gate, "run: nix run ./nixos#fort -- check --all\n\n      - name: The gate of the monitoring layer (fort monitoring check --all)\n") ||
		!strings.Contains(gate, "run: nix run ./nixos#fort-monitoring -- check --all\n\n      - name: Eval every host") {
		t.Errorf("gate:\n%s", gate)
	}

	// Re-runnable: the same add is a no-op, said so.
	p2, err := planLayerAdd(root, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.actions) != 0 || len(p2.present) != 2 {
		t.Errorf("second plan: %d actions, %d present", len(p2.actions), len(p2.present))
	}

	// A second role of the same repo reuses the input.
	ref2, _ := parseLayerRef("git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers#backups")
	p3, err := planLayerAdd(root, ref2)
	if err != nil {
		t.Fatal(err)
	}
	if err := p3.apply(); err != nil {
		t.Fatal(err)
	}
	flake = readFile(t, filepath.Join(root, flakePath))
	if strings.Count(flake, "fort-layers.url") != 1 || !strings.Contains(flake, "fort-backups = inputs.fort-layers.packages.${sys}.fort-backups;") {
		t.Errorf("flake after the second role:\n%s", flake)
	}

	// check --all lists what is installed, and flags a directory with no estate.toml.
	writeTree(t, root, map[string]string{"layers/monitoring/estate.toml": "name = \"x\"\n"})
	if err := os.MkdirAll(filepath.Join(root, "layers/backups"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Without the aggregated role in ops-readonly.yaml a layer's read
	// rule has no taker (#169): a warning that names the file.
	fs := layerFindings(root)
	if levelsOf(fs) != "ok,warning,warning" || !strings.Contains(fs[0].msg, "installed: backups, monitoring") || fs[1].dir != opsReadFile || !strings.Contains(fs[1].msg, opsAggregatedRole) || fs[2].dir != "layers/backups" {
		t.Errorf("findings: %+v", fs)
	}
	writeTree(t, root, map[string]string{opsReadFile: "kind: ClusterRole\nmetadata:\n  name: ops-ro-layers-read\naggregationRule: {}\n"})
	fs = layerFindings(root)
	if levelsOf(fs) != "ok,warning" || fs[1].dir != "layers/backups" {
		t.Errorf("findings with the aggregated role: %+v", fs)
	}

	// Remove the first role: its line and step go, the input stays for the second.
	r, err := planLayerRemove(root, "monitoring")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.apply(); err != nil {
		t.Fatal(err)
	}
	flake = readFile(t, filepath.Join(root, flakePath))
	gate = readFile(t, filepath.Join(root, gatePath))
	if strings.Contains(flake, "fort-monitoring") || !strings.Contains(flake, "fort-layers.url") || !strings.Contains(flake, "fort-backups") {
		t.Errorf("flake after remove:\n%s", flake)
	}
	if strings.Contains(gate, "monitoring layer") || !strings.Contains(gate, "run: nix run ./nixos#fort -- check --all\n\n      - name: The gate of the backups layer") {
		t.Errorf("gate after remove:\n%s", gate)
	}
	if _, err := os.Stat(filepath.Join(root, "layers/monitoring")); !os.IsNotExist(err) {
		t.Errorf("layers/monitoring still there: %v", err)
	}
	// Remove the last role: the input goes with it.
	r2, err := planLayerRemove(root, "backups")
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.apply(); err != nil {
		t.Fatal(err)
	}
	flake = readFile(t, filepath.Join(root, flakePath))
	if strings.Contains(flake, "fort-layers") || !strings.Contains(flake, "      layers = sys: {\n      };") {
		t.Errorf("flake after the last remove:\n%s", flake)
	}
	if fs := layerFindings(root); levelsOf(fs) != "ok" || !strings.Contains(fs[0].msg, "no layer installed") {
		t.Errorf("findings after remove: %+v", fs)
	}
}

func TestLayerAddRefusesAnOldFlake(t *testing.T) {
	root := layerEstate(t)
	old := strings.Replace(flakeFixture, "      layers = sys: {\n      };\n\n", "", 1)
	writeTree(t, root, map[string]string{flakePath: old})
	ref, _ := parseLayerRef("path:../layers#monitoring")
	if _, err := planLayerAdd(root, ref); err == nil || !strings.Contains(err.Error(), "rendered before layers") {
		t.Errorf("want the refusal, got %v", err)
	}
}

func TestExecLayerNamesBoth(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := execLayer("monitoring", nil)
	if err == nil || !strings.Contains(err.Error(), `unknown verb "monitoring"`) || !strings.Contains(err.Error(), "fort-monitoring") {
		t.Errorf("got %v", err)
	}
	if err := execLayer("Not_A_Name", nil); err == nil || strings.Contains(err.Error(), "fort-") {
		t.Errorf("a bad name is not a binary lookup: %v", err)
	}
}
