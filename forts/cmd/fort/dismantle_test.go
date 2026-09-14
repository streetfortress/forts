package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Replays dismantle on a copy of the real tree's relevant files for the
// drill fort (lab: no cloudHosts entry) and the production fort (cloud:
// cloudHosts entry) with a fake sops tool, and checks that exactly the
// generated surface goes and everything else survives.
func TestDismantle(t *testing.T) {
	repo, err := repoRootFrom(mustGetwd(t))
	if err != nil {
		t.Skip("not inside the deployments checkout")
	}
	for _, tc := range []struct {
		host      string
		cloud     bool
		recipient int // `# cluster:` lines in the shared rule
	}{
		{"sfi-fort-e33e2d5", false, 2}, // the stale re-preseed line too
		{"sfi-prod-439bf72", true, 1},
	} {
		t.Run(tc.host, func(t *testing.T) {
			tmp := t.TempDir()
			for _, rel := range []string{"provisioning/inventory.yaml", ".sops.yaml", "nixos/.sops.yaml", "nixos/flake.nix", "wireguard/peers.json", "README.md",
				"nixos/secrets/" + tc.host + ".yaml", "fleet/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml"} {
				copyFile(t, filepath.Join(repo, rel), filepath.Join(tmp, rel))
			}
			for _, rel := range []string{"nixos/hosts/" + tc.host, "fleet/clusters/" + tc.host, "fleet/apps/" + tc.host, "fleet/apps/staging-k3s/sites-db"} {
				copyTree(t, filepath.Join(repo, rel), filepath.Join(tmp, rel))
			}
			// a contract on another fort sourcing from this one
			write := func(rel, content string) {
				os.MkdirAll(filepath.Dir(filepath.Join(tmp, rel)), 0o755)
				os.WriteFile(filepath.Join(tmp, rel), []byte(content), 0o644)
			}
			write("fleet/apps/luxor/app/secrets.contract.yaml", "secrets:\n  secrets.sops.yaml:\n    metadata: {name: a, namespace: a}\n    keys:\n      X: {sops: [../../"+tc.host+"/sites-db/secrets-db.sops.yaml, stringData.password]}\n")

			p, err := planDismantle(tmp, tc.host)
			if err != nil {
				t.Fatal(err)
			}
			tool := &fakeTool{}
			if err := p.apply(tool); err != nil {
				t.Fatal(err)
			}
			var sb strings.Builder
			p.report(&sb, true)
			rep := sb.String()

			for _, rel := range []string{"nixos/hosts/" + tc.host, "nixos/secrets/" + tc.host + ".yaml", "fleet/clusters/" + tc.host, "fleet/apps/" + tc.host} {
				if _, err := os.Stat(filepath.Join(tmp, rel)); err == nil {
					t.Errorf("%s still exists", rel)
				}
			}
			for _, rel := range []string{"fleet/apps/staging-k3s/sites-db/cluster.yaml", "nixos/secrets/", "fleet/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml"} {
				if _, err := os.Stat(filepath.Join(tmp, rel)); err != nil {
					t.Errorf("%s was removed", rel)
				}
			}
			inv := readFile(t, filepath.Join(tmp, "provisioning/inventory.yaml"))
			if strings.Contains(inv, "        "+tc.host+":") {
				t.Errorf("inventory still has %s", tc.host)
			}
			for _, other := range []string{"        luxor:", "        staging-k3s:"} {
				if !strings.Contains(inv, other) {
					t.Errorf("inventory lost %q", other)
				}
			}
			for _, f := range []string{".sops.yaml", "nixos/.sops.yaml"} {
				s := readFile(t, filepath.Join(tmp, f))
				if strings.Contains(s, "sops rule: "+tc.host) || strings.Contains(s, "# cluster: "+tc.host) {
					t.Errorf("%s still names %s", f, tc.host)
				}
				if !strings.Contains(s, "sops rule: luxor") || !strings.Contains(s, "sops rule: staging-k3s") {
					t.Errorf("%s lost another host's block", f)
				}
			}
			if !strings.Contains(readFile(t, filepath.Join(tmp, ".sops.yaml")), "# cluster: luxor") {
				t.Errorf("shared rule lost luxor's recipient")
			}
			peers := readFile(t, filepath.Join(tmp, "wireguard/peers.json"))
			if strings.Contains(peers, `"`+tc.host+`"`) || !strings.Contains(peers, `"luxor"`) {
				t.Errorf("peers.json wrong:\n%s", peers[:200])
			}
			flake := readFile(t, filepath.Join(tmp, "nixos/flake.nix"))
			if strings.Contains(flake, `"`+tc.host+`"`) {
				t.Errorf("flake still lists %s", tc.host)
			}
			if !strings.Contains(flake, "cloudHosts = [") {
				t.Errorf("flake cloudHosts list damaged")
			}
			if len(tool.rekeyed) != 1 || !strings.HasSuffix(tool.rekeyed[0], "alertmanager-edge-config.sops.yaml") {
				t.Errorf("updatekeys: %v", tool.rekeyed)
			}
			if !strings.Contains(rep, "README.md:") {
				t.Errorf("residue should list README.md:\n%s", rep)
			}
			if !strings.Contains(rep, "fleet/apps/luxor/app/secrets.contract.yaml: secrets.sops.yaml/X") {
				t.Errorf("cross-fort ref not reported:\n%s", rep)
			}
			want := "- 2 `# cluster:"
			if tc.recipient == 1 {
				want = "- 1 `# cluster:"
			}
			if !strings.Contains(rep, want) {
				t.Errorf("recipient count: want %q in\n%s", want, rep)
			}
			if tc.cloud && !strings.Contains(rep, "cloudHosts") {
				t.Errorf("cloud host: flake edit missing:\n%s", rep)
			}
			if !tc.cloud && strings.Contains(rep, "cloudHosts") {
				t.Errorf("lab host: unexpected flake edit:\n%s", rep)
			}
		})
	}
}

func TestDismantleEdits(t *testing.T) {
	inv := "hosts:\n        # attached comment\n        a:\n          x: 1\n          # inner\n          y: 2\n        b:\n          x: 3\n"
	out, err := removeInventoryBlock(inv, "a")
	if err != nil || out != "hosts:\n        b:\n          x: 3\n" {
		t.Errorf("removeInventoryBlock: %v\n%s", err, out)
	}
	if _, err := removeInventoryBlock(inv, "zzz"); err == nil {
		t.Error("missing host should error")
	}
	flake := `      cloudHosts = [ "a" "b" ];`
	if out, _ := removeFromCloudHosts(flake, "a"); out != `      cloudHosts = [ "b" ];` {
		t.Errorf("cloudHosts: %s", out)
	}
	if out, _ := removeFromCloudHosts(`cloudHosts = [ "a" ];`, "a"); out != `cloudHosts = [ ];` {
		t.Errorf("cloudHosts last: %s", out)
	}
	if _, err := removeMarkerBlock("x\n", "h"); err == nil {
		t.Error("missing block should error")
	}
}
