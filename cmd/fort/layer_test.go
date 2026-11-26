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

      - name: The gate (fort check --all --eval)
        env:
          GITEA_ACTIONS: "true"
        run: nix run ./nixos#fort -- check --all --eval

      - name: Eval every host with a manifest
        run: echo eval
`

func TestParseLayerRef(t *testing.T) {
	for in, want := range map[string]layerRef{
		"git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers#monitoring":      {url: "git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers", input: "fort-layers", role: "monitoring"},
		"git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers.git?ref=v1#apps": {url: "git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers.git?ref=v1", input: "fort-layers", role: "apps"},
		"path:../layers#backups": {url: "path:../layers", input: "layers", role: "backups"},
		// The shorthands carry the ref as a path label (forts#279): the
		// repo is the second label there, not the last.
		"github:acme/fort-layers#monitoring":            {url: "github:acme/fort-layers", input: "fort-layers", role: "monitoring"},
		"github:acme/fort-layers/v0.0.1#monitoring":     {url: "github:acme/fort-layers/v0.0.1", input: "fort-layers", role: "monitoring"},
		"gitlab:acme/fort-layers/main#backups":          {url: "gitlab:acme/fort-layers/main", input: "fort-layers", role: "backups"},
		"sourcehut:~acme/fort-layers/v0.0.1#monitoring": {url: "sourcehut:~acme/fort-layers/v0.0.1", input: "fort-layers", role: "monitoring"},
	} {
		got, err := parseLayerRef(in)
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, bad := range []string{"git+ssh://x/sfi/fort-layers", "#monitoring", "git+ssh://x/sfi/fort-layers#Monitoring", "git+ssh://x/sfi/fort-layers#-a", "path:../v0.0.1#monitoring"} {
		if _, err := parseLayerRef(bad); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

const gatePath = ".gitea/workflows/checks.yaml"

func layerEstate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeTree(t, root, map[string]string{flakePath: flakeFixture, gatePath: gateFixture})
	return root
}

// A github-forge estate carries its gate under .github/ (forts#191); the
// layer verbs find it there.
func TestLayerAddFindsGithubGate(t *testing.T) {
	root := t.TempDir()
	gh := ".github/workflows/checks.yaml"
	writeTree(t, root, map[string]string{flakePath: flakeFixture, gh: gateFixture})
	ref, _ := parseLayerRef("git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers#monitoring")
	p, err := planLayerAdd(root, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.apply(); err != nil {
		t.Fatal(err)
	}
	if g := readFile(t, filepath.Join(root, gh)); !strings.Contains(g, "The gate of the monitoring layer") {
		t.Errorf("gate:\n%s", g)
	}
	if exists(root, gatePath) {
		t.Error("a .gitea gate appeared")
	}
}

// The gate the layer's step lands in is the one fort init renders, in
// both forge kinds. The core's step name carries the flags the step
// runs with, and those moved once (#193) while this verb still matched
// the old name (#278) — no test read a rendered gate. This one does.
func TestLayerAddOnTheRenderedEstate(t *testing.T) {
	for _, kind := range []string{"gitea", "github"} {
		v := testInitView
		v.ForgeKind = kind
		files, err := renderInit(v)
		if err != nil {
			t.Fatal(err)
		}
		tree := map[string]string{}
		for _, f := range files {
			if f.path == flakePath || strings.HasSuffix(f.path, "workflows/checks.yaml") {
				tree[f.path] = string(f.content)
			}
		}
		if len(tree) != 2 {
			t.Fatalf("%s: the render gave %d of the two files the verb edits", kind, len(tree))
		}
		root := t.TempDir()
		writeTree(t, root, tree)
		ref, err := parseLayerRef("github:acme/fort-layers/v0.0.1#monitoring")
		if err != nil {
			t.Fatal(err)
		}
		p, err := planLayerAdd(root, ref)
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if err := p.apply(); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		gate := readFile(t, filepath.Join(root, gatePathIn(root)))
		if !strings.Contains(gate, "check --all --eval\n\n      - name: "+layerGateStepName("monitoring")+"\n") {
			t.Errorf("%s: the layer's step is not after the core's:\n%s", kind, gate)
		}
		flake := readFile(t, filepath.Join(root, flakePath))
		for _, want := range []string{
			`fort-layers.url = "github:acme/fort-layers/v0.0.1";`,
			"fort-monitoring = inputs.fort-layers.packages.${sys}.fort-monitoring;",
		} {
			if !strings.Contains(flake, want) {
				t.Errorf("%s: the flake lacks %q:\n%s", kind, want, flake)
			}
		}
	}
}

// A gate with no core step refuses, and the error names what it looked
// for.
func TestLayerAddRefusesAGateWithNoCoreStep(t *testing.T) {
	root := layerEstate(t)
	writeTree(t, root, map[string]string{gatePath: strings.Replace(gateFixture, "The gate (fort check --all --eval)", "Something else", 1)})
	ref, _ := parseLayerRef("path:../layers#monitoring")
	_, err := planLayerAdd(root, ref)
	if err == nil || !strings.Contains(err.Error(), coreGateStepPrefix) {
		t.Errorf("want the refusal that names the step, got %v", err)
	}
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
	if !strings.Contains(out.String(), "○ nixos/flake.nix") || !strings.Contains(out.String(), "dry run — nothing written") {
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
	if !strings.Contains(gate, "run: nix run ./nixos#fort -- check --all --eval\n\n      - name: The gate of the monitoring layer (fort monitoring check --all)\n") ||
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
	if levelsOf(fs) != "info,warning,warning" || !strings.Contains(fs[0].msg, "backups, monitoring") || fs[1].dir != opsReadFile || !strings.Contains(fs[1].msg, opsAggregatedRole) || fs[2].dir != "layers/backups" {
		t.Errorf("findings: %+v", fs)
	}
	writeTree(t, root, map[string]string{opsReadFile: "kind: ClusterRole\nmetadata:\n  name: ops-ro-layers-read\naggregationRule: {}\n"})
	fs = layerFindings(root)
	if levelsOf(fs) != "info,warning" || fs[1].dir != "layers/backups" {
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
	if strings.Contains(gate, "monitoring layer") || !strings.Contains(gate, "run: nix run ./nixos#fort -- check --all --eval\n\n      - name: The gate of the backups layer") {
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
	if fs := layerFindings(root); levelsOf(fs) != "info" || !strings.Contains(fs[0].msg, "none") {
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

func TestLayerBinaryFallsBackToTheEstateFlake(t *testing.T) {
	root := layerEstate(t)
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	if _, ok := layerBinary(root, "monitoring"); ok {
		t.Fatal("no binary, no nix, no export: want not ok")
	}
	if err := os.WriteFile(filepath.Join(bin, "nix"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	flake := strings.Replace(flakeFixture, "      layers = sys: {\n", "      layers = sys: {\n        fort-monitoring = inputs.m.packages.${sys}.default;\n", 1)
	writeTree(t, root, map[string]string{flakePath: flake})
	argv, ok := layerBinary(root, "monitoring")
	if !ok || len(argv) != 4 || argv[1] != "run" || argv[2] != "./nixos#fort-monitoring" {
		t.Errorf("want the nix route, got %v %v", argv, ok)
	}
	if _, ok := layerBinary(root, "backups"); ok {
		t.Error("a layer the flake does not export has no route")
	}
	if err := os.WriteFile(filepath.Join(bin, "fort-monitoring"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if argv, _ := layerBinary(root, "monitoring"); len(argv) != 1 {
		t.Errorf("PATH wins, got %v", argv)
	}
}

func TestNixEnsureInputSkipsForeignBlock(t *testing.T) {
	flake := `{
  inputs = {
    forts.url = "github:sfi/forts";
    home-manager = {
      url = "github:nix-community/home-manager";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };
}
`
	got, err := nixEnsureInput(flake, "fort-layers", `fort-layers.url = "git+ssh://x/y";`)
	if err != nil {
		t.Fatal(err)
	}
	want := "      inputs.nixpkgs.follows = \"nixpkgs\";\n    };\n    fort-layers.url = \"git+ssh://x/y\";\n  };\n"
	if !strings.Contains(got, want) {
		t.Errorf("the new input is not a sibling of the foreign block:\n%s", got)
	}
	// A url inside the foreign block is not the layer's input.
	again, _ := nixEnsureInput(flake, "url", `url.url = "x";`)
	if !strings.Contains(again, "url.url") {
		t.Errorf("a nested url hid a top-level insert:\n%s", again)
	}
	dropped, err := nixDropInput(got, "fort-layers")
	if err != nil || dropped != flake {
		t.Errorf("drop did not restore the flake: %v\n%s", err, dropped)
	}
}
