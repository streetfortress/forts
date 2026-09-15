package main

import (
	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Replays dismantle on a copy of the real tree's relevant files for the
// drill fort (lab) and the production fort (cloud) with a fake sops tool, and checks that exactly the
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
			for _, rel := range []string{manifest.Path(tc.host), manifest.Path("luxor"), manifest.Path("staging-k3s"), ".sops.yaml", "nixos/.sops.yaml", "wireguard/peers.json", "README.md",
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
			if exists(tmp, manifest.Path(tc.host)) {
				t.Errorf("manifest %s still exists", manifest.Path(tc.host))
			}
			for _, other := range []string{"luxor", "staging-k3s"} {
				if !exists(tmp, manifest.Path(other)) {
					t.Errorf("manifest %s was removed", other)
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
		})
	}
}

func TestDismantleEdits(t *testing.T) {
	if _, err := removeMarkerBlock("x\n", "h"); err == nil {
		t.Error("missing block should error")
	}
}
