package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func levelsOf(fs []finding) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.level)
	}
	return strings.Join(out, ",")
}

// The served side (forts#38): every cluster fort and every plain fort
// has its file under flux/hub/<hub host>/, listed; a file with no
// manifest is an error; and the check is the same whichever estate the
// hub is in, because the files are.
func TestHubFileFindings(t *testing.T) {
	cluster := manifest.Host{MeshIP: "10.3.0.70", Cluster: &manifest.ClusterSpec{}}
	plain := manifest.Host{MeshIP: "10.3.0.71"}
	hosts := map[string]manifest.Host{"sfi-a": cluster, "sfi-p": plain}
	for name, est := range map[string]estate.Estate{
		"in-estate": {Name: "infra", Repo: "ssh://x/sfi/infra", Hubs: map[string]estate.Hub{estate.RoleMonitoring: {Estate: "https://x/sfi/infra.git", Fort: "luxor"}}},
		"remote":    {Name: "staging", Repo: "ssh://x/sfi/staging", Hubs: map[string]estate.Hub{estate.RoleMonitoring: {Estate: "ssh://git@gitea.zen.lofi:30022/sfi/infra", Fort: "luxor"}}},
	} {
		root := t.TempDir()
		writeTree(t, root, map[string]string{"flux/clusters/sfi-a/apps.yaml": "kind: Kustomization\n"})
		// nothing rendered yet: both files missing
		fs := hubFileFindings(root, est, hosts)
		if levelsOf(fs) != "error,error" || !strings.Contains(fs[0].msg, "fort scaffold sfi-a") || !strings.Contains(fs[1].msg, "plain fort the hub does not scrape") {
			t.Errorf("%s missing: %v", name, fs)
		}
		// the files exist, the kustomization lists one
		writeTree(t, root, map[string]string{
			hubFilePath("luxor", "sfi-a"): "kind: PrometheusRule\n",
			hubFilePath("luxor", "sfi-p"): "kind: ScrapeConfig\n",
			hubKustomization("luxor"):     "resources:\n  - sfi-a.yaml\n",
		})
		fs = hubFileFindings(root, est, hosts)
		if levelsOf(fs) != "ok,error" || fs[1].dir != hubKustomization("luxor") || !strings.Contains(fs[1].msg, "sfi-p.yaml") {
			t.Errorf("%s unlisted: %v", name, fs)
		}
		// both listed: two oks, the kustomization itself is not an orphan
		writeTree(t, root, map[string]string{hubKustomization("luxor"): "resources:\n  - sfi-a.yaml\n  - sfi-p.yaml\n"})
		fs = hubFileFindings(root, est, hosts)
		if levelsOf(fs) != "ok,ok" || fs[0].msg != clusterMonitoringNote || fs[1].msg != plainMonitoringNote {
			t.Errorf("%s listed: %v", name, fs)
		}
		// a file for a fort with no manifest: the retired fort's rules
		writeTree(t, root, map[string]string{hubFilePath("luxor", "sfi-gone"): "kind: PrometheusRule\n"})
		fs = hubFileFindings(root, est, hosts)
		if levelsOf(fs) != "ok,ok,error" || !strings.Contains(fs[2].msg, "fort dismantle") || fs[2].dir != hubFilePath("luxor", "sfi-gone") {
			t.Errorf("%s orphan: %v", name, fs)
		}
		// a cluster fort scaffold has not reached is skipped, not failed
		os.Remove(filepath.Join(root, hubFilePath("luxor", "sfi-gone")))
		os.RemoveAll(filepath.Join(root, "flux/clusters/sfi-a"))
		if fs := hubFileFindings(root, est, hosts); levelsOf(fs) != "ok" || fs[0].dir != hubFilePath("luxor", "sfi-p") {
			t.Errorf("%s unscaffolded cluster is skipped: %v", name, fs)
		}
	}
	// no monitoring hub: one ok that says so
	fs := hubFileFindings(t.TempDir(), estate.Estate{Name: "e", Repo: "ssh://x/sfi/e"}, hosts)
	if levelsOf(fs) != "ok" || !strings.Contains(fs[0].msg, "no [hubs.monitoring]") {
		t.Errorf("no hub: %v", fs)
	}
}

// The hub side (forts#38): one subscription per estate the hub serves
// the monitoring role, its own included; listed by serves/kustomization
// .yaml and the apps root; no subscription without an entry; a link
// named for every served estate on the host that serves it.
func TestServesFindings(t *testing.T) {
	est := estate.Estate{Name: "infra", Repo: "ssh://git@gitea.zen.lofi:30022/sfi/infra",
		Hubs: map[string]estate.Hub{
			estate.RoleForge:      {Estate: "ssh://git@gitea.zen.lofi:30022/sfi/infra", Fort: "zenbook", Name: "gitea.zen.lofi"},
			estate.RoleMonitoring: {Estate: "ssh://git@gitea.zen.lofi:30022/sfi/infra", Fort: "luxor"},
		},
		Serves: map[string][]estate.ServeEntry{
			estate.RoleMonitoring: {{Fort: "luxor", Estates: []string{"ssh://x/sfi/staging"}}},
			estate.RoleForge:      {{Fort: "zenbook", Estates: []string{"ssh://x/sfi/staging", "ssh://x/sfi/production"}}},
		}}
	if err := est.Validate(); err != nil {
		t.Fatal(err)
	}
	link := func(name, repo, address string, port int) manifest.Link {
		return manifest.Link{Name: name, Estate: repo, Address: address, Port: port, PublicKey: testWGPub}
	}
	// both hub hosts declare their links ([links.<name>], forts#55)
	hosts := map[string]manifest.Host{
		"luxor":   {MeshIP: "10.1.0.5", Cluster: &manifest.ClusterSpec{}, Links: map[string]manifest.Link{"staging": link("staging", "ssh://x/sfi/staging", "10.3.0.5", 51821)}},
		"zenbook": {MeshIP: "10.1.0.4", Cluster: &manifest.ClusterSpec{}, Links: map[string]manifest.Link{"staging": link("staging", "ssh://x/sfi/staging", "10.3.0.4", 51821), "production": link("production", "ssh://x/sfi/production", "10.4.0.4", 51822)}},
	}
	root := t.TempDir()
	// the hub's tree exists; no subscription yet
	writeTree(t, root, map[string]string{
		"flux/apps/luxor/kustomization.yaml": "resources:\n  - sops-canary.yaml\n",
	})
	fs := servesFindings(root, est, hosts)
	if levelsOf(fs) != "error,error" || fs[0].dir != servesPath("luxor", "sfi/infra") || fs[1].dir != servesPath("luxor", "sfi/staging") || !strings.Contains(fs[1].msg, "fort scaffold luxor") {
		t.Errorf("missing: %v", fs)
	}
	// the files exist; the kustomization lists one; the apps root lists
	// no serves/
	writeTree(t, root, map[string]string{
		servesPath("luxor", "sfi/infra"):   "kind: Kustomization\n",
		servesPath("luxor", "sfi/staging"): "kind: GitRepository\n",
		servesKustomization("luxor"):       "resources:\n  - infra.yaml\n",
	})
	fs = servesFindings(root, est, hosts)
	if levelsOf(fs) != "error,error" || fs[0].dir != filepath.Join("flux/apps/luxor/kustomization.yaml") || !strings.Contains(fs[1].msg, "staging.yaml") {
		t.Errorf("unlisted: %v", fs)
	}
	writeTree(t, root, map[string]string{
		servesKustomization("luxor"):         "resources:\n  - infra.yaml\n  - staging.yaml\n",
		"flux/apps/luxor/kustomization.yaml": "resources:\n  - sops-canary.yaml\n  - serves\n",
	})
	fs = servesFindings(root, est, hosts)
	if levelsOf(fs) != "ok,ok" || !strings.Contains(fs[0].msg, "this estate's own forts") || !strings.Contains(fs[1].msg, "sfi/staging: flux/hub/luxor/ of ssh://x/sfi/staging") {
		t.Errorf("listed: %v", fs)
	}
	// a subscription nobody declared: production is served the forge only
	writeTree(t, root, map[string]string{servesPath("luxor", "sfi/production"): "kind: Kustomization\n"})
	fs = servesFindings(root, est, hosts)
	if levelsOf(fs) != "ok,ok,error" || !strings.Contains(fs[2].msg, "no [[serves.monitoring]] entry names luxor") {
		t.Errorf("orphan: %v", fs)
	}
	os.Remove(filepath.Join(root, servesPath("luxor", "sfi/production")))
	// the network half: zenbook serves production the forge and has no
	// link to it; luxor serves staging and has one
	zb := hosts["zenbook"]
	zb.Links = map[string]manifest.Link{"staging": link("staging", "ssh://x/sfi/staging", "10.3.0.4", 51821)}
	hosts["zenbook"] = zb
	fs = servesFindings(root, est, hosts)
	if levelsOf(fs) != "ok,ok,warning" || fs[2].dir != manifest.Path("zenbook") || !strings.Contains(fs[2].msg, "no [links.production]") || !strings.Contains(fs[2].msg, "the role forge") || !strings.Contains(fs[2].msg, "fort link add zenbook ssh://x/sfi/production") {
		t.Errorf("no link: %v", fs)
	}
	zb.Links["production"] = link("production", "ssh://x/sfi/production", "10.4.0.4", 51822)
	hosts["zenbook"] = zb
	// the hub named in [hubs.monitoring] and in the serves table has no
	// manifest: one error from each side
	delete(hosts, "luxor")
	fs = servesFindings(root, est, hosts)
	if len(fs) < 2 || fs[0].level != "error" || !strings.Contains(fs[0].msg, manifest.Path("luxor")) || fs[1].level != "error" || !strings.Contains(fs[1].msg, "a serves table names luxor") {
		t.Errorf("hub without a manifest: %v", fs)
	}
	hosts["luxor"] = manifest.Host{MeshIP: "10.1.0.5", Cluster: &manifest.ClusterSpec{}, Links: map[string]manifest.Link{"staging": link("staging", "ssh://x/sfi/staging", "10.3.0.5", 51821)}}
	// a second monitoring hub for a second estate: its own serves/ tree
	two := est
	two.Serves = map[string][]estate.ServeEntry{estate.RoleMonitoring: {{Fort: "luxor", Estates: []string{"ssh://x/sfi/staging"}}, {Fort: "luxor-b", Estates: []string{"ssh://x/sfi/staging-b"}}}}
	hosts["luxor-b"] = manifest.Host{MeshIP: "10.1.0.6", Cluster: &manifest.ClusterSpec{}, Links: map[string]manifest.Link{"staging-b": link("staging-b", "ssh://x/sfi/staging-b", "10.5.0.6", 51821)}}
	fs = servesFindings(root, two, hosts)
	if levelsOf(fs) != "ok,ok,error" || fs[2].dir != servesPath("luxor-b", "sfi/staging-b") || !strings.Contains(fs[2].msg, "fort scaffold luxor-b") {
		t.Errorf("second hub: %v", fs)
	}
	// a served estate (no local hub, no serves tables): nothing to say
	served := estate.Estate{Name: "staging", Repo: "ssh://x/sfi/staging", Hubs: map[string]estate.Hub{estate.RoleMonitoring: {Estate: "ssh://git@gitea.zen.lofi:30022/sfi/infra", Fort: "luxor"}}}
	if fs := servesFindings(t.TempDir(), served, map[string]manifest.Host{}); len(fs) != 0 {
		t.Errorf("served estate: %v", fs)
	}
}
