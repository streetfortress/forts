package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

// The hub is a relationship between two estates (forts#38). A served
// estate renders what it asks of its monitoring hub — the dead man's
// switch for each fort, the log analog, the backup leg a pv block
// promises, the edge Alertmanager as a Grafana datasource; for a plain
// fort the hub's scrape of it — into flux/hub/<hub host>/ of ITS OWN
// tree, one file per fort, rendered by scaffold, removed by dismantle,
// required by check. The hub subscribes to that path with flux: one
// GitRepository on the served estate's repo, one Kustomization that
// applies the directory under a service account scoped to
// monitoring-central (fort scaffold <fort> renders it from the hub
// estate's [[serves.monitoring]] entry). An in-estate hub reads the same directory
// through its own flux source. Nothing about a fort is written into
// another fort's tree, and nothing is written across repos: the hub's
// tree holds one subscription per served estate, which is a function of
// estates, not of forts.
func hubDir(hub string) string {
	return filepath.Join("flux/hub", hub)
}

func hubKustomization(hub string) string {
	return filepath.Join(hubDir(hub), "kustomization.yaml")
}

// hubFilePath is a fort's file for the hub: its dead man's switch
// (metrics and logs), the backup leg its pv blocks promise, and its edge
// Alertmanager as a Grafana datasource. A plain fort has a file in the
// same place with the pull-shaped equivalent: the hub's ScrapeConfig for
// node-exporter over the mesh, and the switch on it.
func hubFilePath(hub, host string) string {
	return filepath.Join(hubDir(hub), host+".yaml")
}

// The post-scaffold notes for the two shapes (also what check reports).
const (
	clusterMonitoringNote = "dead man's switch (metrics, logs), edge Alertmanager datasource"
	plainMonitoringNote   = "mesh scrape target (node-exporter), HostDown"
)

// servesDir is where the hub fort's tree carries its subscriptions: one
// file per estate it serves the monitoring role (and one for its own
// estate), listed by the directory's kustomization, which the apps root
// lists in turn.
const servesDirName = "serves"

func servesDir(hub string) string {
	return filepath.Join("flux/apps", hub, servesDirName)
}

func servesKustomization(hub string) string {
	return filepath.Join(servesDir(hub), "kustomization.yaml")
}

// servesPath is the subscription to one estate, named by the estate's
// short name: serves/staging.yaml subscribes to sfi/staging. The estate
// may be given as its URL or as <owner>/<name>.
func servesPath(hub, estateRef string) string {
	return filepath.Join(servesDir(hub), estate.ShortName(estateRef)+".yaml")
}

// hubFileFindings (--all): the served side. Every flux cluster and every
// plain fort has its file under flux/hub/<hub host>/ and the directory's
// kustomization lists it — a fort with no dead man's switch is the
// silent failure the central layer exists to catch — and every file
// there belongs to a manifest, so a retired fort's rule cannot keep
// paging for a host that is gone. The hub may be a fort of this estate
// or of another; the files are the same, and so is the check.
func hubFileFindings(root string, est estate.Estate, hosts map[string]manifest.Host) []finding {
	var out []finding
	h, ok := est.Hub(estate.RoleMonitoring)
	if !ok {
		return []finding{{"ok", estate.Path, "no [hubs.monitoring]: no fort's file for a hub, and no dead man's switch for any of them — the edge Alertmanager is the only alerting"}}
	}
	hub := h.Fort
	kust, _ := os.ReadFile(filepath.Join(root, hubKustomization(hub)))
	names := make([]string, 0, len(hosts))
	for name := range hosts {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, host := range names {
		plain := !hosts[host].IsCluster()
		if !plain && !exists(root, filepath.Join("flux/clusters", host)) {
			continue
		}
		rel := hubFilePath(hub, host)
		what, note := "a flux cluster with no dead man's switch for the hub", clusterMonitoringNote
		if plain {
			what, note = "a plain fort the hub does not scrape", plainMonitoringNote
		}
		switch {
		case !exists(root, rel):
			out = append(out, finding{"error", rel, "missing — " + host + " is " + what + "; `fort scaffold " + host + " --write` renders it"})
		case !strings.Contains(string(kust), host+".yaml"):
			out = append(out, finding{"error", hubKustomization(hub), "does not list " + host + ".yaml — the file exists and the hub's flux never applies it; `fort scaffold " + host + " --write` adds the line"})
		default:
			out = append(out, finding{"ok", rel, note})
		}
	}
	entries, _ := os.ReadDir(filepath.Join(root, hubDir(hub)))
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".yaml")
		if e.Name() == "kustomization.yaml" {
			continue
		}
		if _, ok := hosts[name]; !ok {
			out = append(out, finding{"error", filepath.Join(hubDir(hub), e.Name()), "no manifest in " + manifest.Dir + "/ — a retired fort's rules would page for a cluster that is gone; `fort dismantle` removes this file"})
		}
	}
	return out
}

// servesFindings (--all): the hub side. Every fort a serves table names
// has a manifest here. For every [[serves.monitoring]] entry, the
// fort's tree holds the subscription to each estate it lists
// (serves/<estate>.yaml), the directory's kustomization lists it, and
// the apps root lists the directory; the estate's own monitoring hub
// holds its own estate's subscription the same way. A subscription with
// no entry is an error, because prune on a Kustomization nobody declared
// is a hub applying an estate nobody asked it to. For every (role, fort,
// estate), the fort's manifest declares a link to that estate
// ([links.<name>], docs/links.md) — a warning: the network half of the
// relationship is its own verb (fort link add), and the link's
// reconciler refuses on its own when the two sides disagree.
func servesFindings(root string, est estate.Estate, hosts map[string]manifest.Host) []finding {
	var out []finding
	own := est.LocalHub(estate.RoleMonitoring)
	if own != "" {
		if _, ok := hosts[own]; !ok {
			out = append(out, finding{"error", estate.Path, "[hubs.monitoring] names " + own + " as a fort of this estate, and " + manifest.Path(own) + " does not exist"})
		}
	}
	for _, fort := range est.ServingForts() {
		if _, ok := hosts[fort]; !ok {
			out = append(out, finding{"error", estate.Path, "a serves table names " + fort + ", and " + manifest.Path(fort) + " does not exist — a hub is a fort of this estate"})
		}
	}
	// The subscriptions each fort owes, by fort.
	type want struct{ ref, why string }
	wants := map[string][]want{}
	if own != "" && exists(root, filepath.Join("flux/apps", own)) {
		wants[own] = append(wants[own], want{est.Ref(), "this estate's own forts: " + hubDir(own) + "/ through the flux-system source"})
	}
	for _, s := range est.Servings() {
		if s.Role != estate.RoleMonitoring {
			continue
		}
		wants[s.Fort] = append(wants[s.Fort], want{s.Ref(), "the forts of " + s.Ref() + ": " + hubDir(s.Fort) + "/ of " + s.Estate})
	}
	forts := make([]string, 0, len(wants))
	for f := range wants {
		forts = append(forts, f)
	}
	sort.Strings(forts)
	for _, hub := range forts {
		kust, _ := os.ReadFile(filepath.Join(root, servesKustomization(hub)))
		apps, _ := os.ReadFile(filepath.Join(root, "flux/apps", hub, "kustomization.yaml"))
		known := map[string]bool{}
		for _, w := range wants[hub] {
			rel := servesPath(hub, w.ref)
			item := estate.ShortName(w.ref) + ".yaml"
			known[item] = true
			switch {
			case !exists(root, rel):
				out = append(out, finding{"error", rel, "missing — no subscription to " + w.ref + "; `fort scaffold " + hub + " --write` renders it"})
			case !strings.Contains(string(kust), item):
				out = append(out, finding{"error", servesKustomization(hub), "does not list " + item + " — the subscription exists and flux never applies it; `fort scaffold " + hub + " --write` adds the line"})
			case !strings.Contains(string(apps), servesDirName):
				out = append(out, finding{"error", filepath.Join("flux/apps", hub, "kustomization.yaml"), "does not list " + servesDirName + "/ — the subscriptions exist and flux never applies them; `fort scaffold " + hub + " --write` adds the line"})
			default:
				out = append(out, finding{"ok", rel, w.why})
			}
		}
		entries, _ := os.ReadDir(filepath.Join(root, servesDir(hub)))
		for _, e := range entries {
			if e.Name() == "kustomization.yaml" || known[e.Name()] {
				continue
			}
			out = append(out, finding{"error", filepath.Join(servesDir(hub), e.Name()), "no [[serves.monitoring]] entry names " + hub + " the hub of this estate — a subscription nobody declared; remove the file, or add the entry to " + estate.Path})
		}
	}
	// The network half: a link per served estate, on the fort that
	// serves it.
	linked := map[string]bool{}
	for _, s := range est.Servings() {
		key := s.Fort + " " + s.Name()
		if linked[key] {
			continue
		}
		linked[key] = true
		h, ok := hosts[s.Fort]
		if !ok {
			continue // no manifest: the error above
		}
		if _, ok := h.LinkTo(s.Estate); !ok {
			out = append(out, finding{"warning", manifest.Path(s.Fort), fmt.Sprintf("no [links.%s] — %s serves %s the role %s, and a hub reaches the forts it serves over a link to their mesh: `fort link add %s %s --write` (docs/links.md)", s.Name(), s.Fort, s.Ref(), s.Role, s.Fort, s.Estate)})
		}
	}
	return out
}

// hubBuildFindings (--all): the directory the hub subscribes to builds.
// The served estate's own clusters never apply flux/hub/<hub>/, so
// nothing else in this gate would build it — and a file there that does
// not build is a subscription that fails on the hub, in another
// estate's flux, where this estate's PR cannot see it. Not run when the
// directory does not exist yet (no fort scaffolded).
func hubBuildFindings(root string, est estate.Estate) []finding {
	h, ok := est.Hub(estate.RoleMonitoring)
	if !ok || !exists(root, hubKustomization(h.Fort)) {
		return nil
	}
	if _, err := kustomizeBuild(root, hubDir(h.Fort)); err != nil {
		return []finding{{"error", hubDir(h.Fort), err.Error() + " — the hub's subscription would fail on this directory"}}
	}
	return []finding{{"ok", hubDir(h.Fort), "kustomize build (what the hub's subscription applies)"}}
}
