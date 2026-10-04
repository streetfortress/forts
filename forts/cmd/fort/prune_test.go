package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

// The pruning key is role-scoped (#400): the holder is the host whose
// sops file names restic-prune-s3-env-b2, read offline from the plaintext
// key name, and the module half is read textually from default.nix the
// way the console check reads its opt-in line.
func TestReadPruneState(t *testing.T) {
	tmp := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(tmp, rel)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(content), 0o644)
	}
	const both = "restic-password: ENC[x]\nrestic-prune-s3-env-rustfs: ENC[x]\nrestic-prune-s3-env-b2: ENC[x]\nsops:\n    age: []\n"
	cases := map[string]struct {
		nix, secrets  string
		enabled       bool
		sweeps, holds []string
	}{
		"off": {"{ }\n", "restic-password: ENC[x]\nsops: {}\n", false, nil, nil},
		// luxor's shape: a block, enable = true, the b2 tier restated
		"block": {"{\n  sfi.pv-prune = {\n    enable = true;\n    tiers.b2.enable = true;\n    exclude = [ \"zenbook\" ];\n  };\n}\n", both, true, []string{"rustfs", "b2"}, []string{"rustfs", "b2"}},
		// the way in before the B2 key is minted: the tier switched off
		"b2 off":        {"{\n  sfi.pv-prune = {\n    enable = true;\n    dryRun = true;\n    tiers.b2.enable = false;\n  };\n}\n", both, true, []string{"rustfs"}, []string{"rustfs", "b2"}},
		"b2 off nested": {"{\n  sfi.pv-prune = {\n    enable = true;\n    tiers = {\n      b2 = { enable = false; };\n    };\n  };\n}\n", "restic-prune-s3-env-rustfs: ENC[x]\nsops: {}\n", true, []string{"rustfs"}, []string{"rustfs"}},
		"dotted":        {"{\n  sfi.pv-prune.enable = true;\n  sfi.pv-prune.tiers.rustfs.enable = false;\n}\n", both, true, []string{"b2"}, []string{"rustfs", "b2"}},
		// a tier's enable line inside the block must not read as the module's
		"only a tier line": {"{\n  sfi.pv-prune = {\n    tiers.b2.enable = true;\n  };\n}\n", "sops: {}\n", false, nil, nil},
		// a module that is written but not enabled, with the key parked
		"parked": {"{\n  sfi.pv-prune = {\n    enable = false;\n  };\n}\n", both, false, nil, []string{"rustfs", "b2"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			write("nixos/hosts/h/default.nix", tc.nix)
			write("nixos/secrets/h.yaml", tc.secrets)
			got := readPruneState(tmp, "h")
			if got.enabled != tc.enabled || strings.Join(got.sweeps, ",") != strings.Join(tc.sweeps, ",") || strings.Join(got.holds, ",") != strings.Join(tc.holds, ",") {
				t.Errorf("want enabled=%v sweeps=%v holds=%v, got %+v", tc.enabled, tc.sweeps, tc.holds, got)
			}
			if holdsPruneKey(tmp, "h") != hasKey(tc.holds, "b2") {
				t.Errorf("holdsPruneKey disagrees with holds=%v", tc.holds)
			}
		})
	}
	// No nix dir at all (a peer): not enabled, and whatever the file
	// names is held.
	os.RemoveAll(filepath.Join(tmp, "nixos/hosts/h"))
	write("nixos/secrets/h.yaml", both)
	if got := readPruneState(tmp, "h"); got.enabled || len(got.holds) != 2 {
		t.Errorf("no default.nix: %+v", got)
	}
}

// The repo-side invariant sops-nix enforces late, and the inverse that
// nothing enforced at all (#395: the key existed for four days with no
// holder and nothing noticed).
func TestPruneFindings(t *testing.T) {
	tmp := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(tmp, rel)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(content), 0o644)
	}
	one := func(t *testing.T, want string) finding {
		t.Helper()
		ff := pruneFindings(tmp, "h")
		if len(ff) != 1 || ff[0].level != want {
			t.Fatalf("want one %s finding, got %v", want, ff)
		}
		return ff[0]
	}
	const on = "{\n  sfi.pv-prune = {\n    enable = true;\n  };\n}\n"
	const b2Off = "{\n  sfi.pv-prune = {\n    enable = true;\n    tiers.b2.enable = false;\n  };\n}\n"
	const both = "restic-prune-s3-env-rustfs: ENC[x]\nrestic-prune-s3-env-b2: ENC[x]\nsops: {}\n"
	const rustfsOnly = "restic-prune-s3-env-rustfs: ENC[x]\nsops: {}\n"

	// Most forts: no module, no key, silence.
	write("nixos/hosts/h/default.nix", "{ }\n")
	write("nixos/secrets/h.yaml", "restic-password: ENC[x]\nsops: {}\n")
	if ff := pruneFindings(tmp, "h"); len(ff) != 0 {
		t.Fatalf("no half: want no finding, got %v", ff)
	}
	// luxor today: both tiers, both keys.
	write("nixos/hosts/h/default.nix", on)
	write("nixos/secrets/h.yaml", both)
	if f := one(t, "ok"); !strings.Contains(f.msg, "rustfs, b2") {
		t.Errorf("want both tiers named, got %q", f.msg)
	}
	// Forward: the tier is on and the key is absent — the activation fails.
	write("nixos/secrets/h.yaml", rustfsOnly)
	if f := one(t, "error"); !strings.Contains(f.msg, "restic-prune-s3-env-b2") || !strings.Contains(f.msg, "fort b2-key pruning --write") || f.dir != "nixos/secrets/h.yaml" {
		t.Errorf("want the key and the mint verb named at the secrets file, got %+v", f)
	}
	// The way in: the tier off until the key is minted — consistent.
	write("nixos/hosts/h/default.nix", b2Off)
	if f := one(t, "ok"); strings.Contains(f.msg, "b2") {
		t.Errorf("b2 is not swept: %q", f.msg)
	}
	// Reverse: the key is parked in a host that does not sweep the tier.
	write("nixos/secrets/h.yaml", both)
	if f := one(t, "warning"); !strings.Contains(f.msg, b2PruningKeyName) || !strings.Contains(f.msg, "does not sweep the b2 tier") || f.dir != "nixos/hosts/h/default.nix" {
		t.Errorf("want the parked key named at the host's nix, got %+v", f)
	}
	// Reverse with the module off entirely: the same warning, one per key.
	write("nixos/hosts/h/default.nix", "{ }\n")
	if ff := pruneFindings(tmp, "h"); len(ff) != 2 || ff[0].level != "warning" || ff[1].level != "warning" {
		t.Errorf("two parked keys, two warnings: %v", ff)
	}
}

// --all: exactly one declared host holds the B2 pruning key. Zero is
// the state right after fort destroy of the pruner (a warning, so the
// gate says so without blocking the destroy PR); two is an error.
func TestPruneHolderFindings(t *testing.T) {
	tmp := t.TempDir()
	write := func(rel, content string) {
		path := filepath.Join(tmp, rel)
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte(content), 0o644)
	}
	hosts := map[string]manifest.Host{"luxor": {}, "sfi-a": {}, "gone": {}}
	write("nixos/secrets/sfi-a.yaml", "restic-password: ENC[x]\nsops: {}\n")
	// a file for a host no manifest declares does not count: dismantle
	// removes the file with the manifest, and an orphaned file is the
	// orphan scan's finding
	write("nixos/secrets/stray.yaml", "restic-prune-s3-env-b2: ENC[x]\nsops: {}\n")

	ff := pruneHolderFindings(tmp, hosts)
	if len(ff) != 1 || ff[0].level != "warning" || !strings.Contains(ff[0].msg, "no B2 pruner") {
		t.Fatalf("no holder: want a warning, got %v", ff)
	}
	write("nixos/secrets/luxor.yaml", "restic-prune-s3-env-rustfs: ENC[x]\nrestic-prune-s3-env-b2: ENC[x]\nsops: {}\n")
	ff = pruneHolderFindings(tmp, hosts)
	if len(ff) != 1 || ff[0].level != "ok" || ff[0].dir != "nixos/secrets/luxor.yaml" || !strings.Contains(ff[0].msg, "luxor") {
		t.Fatalf("one holder: want ok at its file, got %v", ff)
	}
	write("nixos/secrets/gone.yaml", "restic-prune-s3-env-b2: ENC[x]\nsops: {}\n")
	ff = pruneHolderFindings(tmp, hosts)
	if len(ff) != 1 || ff[0].level != "error" || !strings.Contains(ff[0].msg, "gone, luxor") {
		t.Fatalf("two holders: want an error naming both, got %v", ff)
	}
}

// The checked-in luxor is the holder, and the textual read of its
// default.nix agrees with what the module evaluates to: both tiers on.
func TestLuxorIsThePruner(t *testing.T) {
	repo := estateRoot(t)
	s := readPruneState(repo, "luxor")
	if !s.enabled || strings.Join(s.sweeps, ",") != "rustfs,b2" || strings.Join(s.holds, ",") != "rustfs,b2" {
		t.Errorf("luxor: %+v", s)
	}
	if ff := pruneFindings(repo, "luxor"); len(ff) != 1 || ff[0].level != "ok" {
		t.Errorf("luxor findings: %v", ff)
	}
	hosts, _, err := manifest.LoadAll(repo)
	if err != nil {
		t.Fatal(err)
	}
	if ff := pruneHolderFindings(repo, hosts); len(ff) != 1 || ff[0].level != "ok" || !strings.Contains(ff[0].msg, "luxor") {
		t.Errorf("fleet holder: %v", ff)
	}
	for host := range hosts {
		if host == "luxor" {
			continue
		}
		if ff := pruneFindings(repo, host); len(ff) != 0 {
			t.Errorf("%s: %v", host, ff)
		}
	}
}

// Dismantle's repo half reports the holder as residue — it never
// reaches outside the tree, so the revoke is destroy's and the report
// says so.
func TestDismantleReportsThePruner(t *testing.T) {
	var sb strings.Builder
	p := &dismantlePlan{host: "luxor", entry: manifest.Host{Provider: "baremetal"}, pruner: true}
	p.report(&sb, false)
	rep := sb.String()
	if !strings.Contains(rep, "! luxor is the fleet's B2 pruner") || !strings.Contains(rep, "nixos/secrets/luxor.yaml names restic-prune-s3-env-b2") || !strings.Contains(rep, "fort destroy revokes "+b2PruningKeyName) {
		t.Errorf("pruner line missing:\n%s", rep)
	}
	sb.Reset()
	p.pruner = false
	p.report(&sb, false)
	if strings.Contains(sb.String(), "pruner") {
		t.Errorf("a host that is not the pruner says nothing about it:\n%s", sb.String())
	}
}
