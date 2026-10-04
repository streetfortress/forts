package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

// The hub: the fort that hosts what every other fort consumes — the
// central monitoring. What is particular is the ROLE; a fort registering
// with a service another fort hosts is declared in that fort's tree, and
// the per-cluster monitoring is the same shape (#288): one file per flux
// cluster under the hub's monitoring dir, rendered by scaffold, removed
// by dismantle, required by check — so nothing about a cluster lives
// outside a <host>-keyed path. Which fort is the hub is the estate's to
// say (estate.toml, `hub`); an estate with none renders no file here and
// has nobody to notice a silent fort.
func clusterMonitoringDir(hub string) string {
	return filepath.Join("flux/apps", hub, "monitoring/clusters")
}

func clusterMonitoringKustomization(hub string) string {
	return filepath.Join("flux/apps", hub, "monitoring/kustomization.yaml")
}

// clusterMonitoringPath is a cluster's file on the hub: its dead man's
// switch (metrics and logs), the backup leg its profile promises, and
// its edge Alertmanager as a Grafana datasource. A plain fort has a file
// in the same place with the pull-shaped equivalent: the hub's
// ScrapeConfig for node-exporter over the mesh, and the switch on it.
func clusterMonitoringPath(hub, host string) string {
	return filepath.Join(clusterMonitoringDir(hub), host+".yaml")
}

// The post-scaffold notes for the two shapes (also what check reports).
const (
	clusterMonitoringNote = "dead man's switch (metrics, logs), edge Alertmanager datasource"
	plainMonitoringNote   = "mesh scrape target (node-exporter), HostDown"
)

// clusterMonitoringFindings (--all): every flux cluster and every plain
// fort has its file on the hub and the hub's kustomization lists it — a
// fort with no dead man's switch is the silent failure the central layer
// exists to catch — and every file there belongs to a manifest, so a
// retired fort's rule cannot keep paging for a host that is gone.
func clusterMonitoringFindings(root, hub string, hosts map[string]manifest.Host) []finding {
	var out []finding
	if hub == "" {
		return []finding{{"ok", estate.Path, "no hub: no fort's file on a hub, and no dead man's switch for any of them — the edge Alertmanager is the only alerting"}}
	}
	kust, _ := os.ReadFile(filepath.Join(root, clusterMonitoringKustomization(hub)))
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
		rel := clusterMonitoringPath(hub, host)
		what, note := "a flux cluster with no dead man's switch on the hub", clusterMonitoringNote
		if plain {
			what, note = "a plain fort the hub does not scrape", plainMonitoringNote
		}
		switch {
		case !exists(root, rel):
			out = append(out, finding{"error", rel, "missing — " + host + " is " + what + "; `fort scaffold " + host + " --write` renders it"})
		case !strings.Contains(string(kust), "clusters/"+host+".yaml"):
			out = append(out, finding{"error", clusterMonitoringKustomization(hub), "does not list clusters/" + host + ".yaml — the file exists and flux never applies it; `fort scaffold " + host + " --write` adds the line"})
		default:
			out = append(out, finding{"ok", rel, note})
		}
	}
	entries, _ := os.ReadDir(filepath.Join(root, clusterMonitoringDir(hub)))
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".yaml")
		if _, ok := hosts[name]; !ok {
			out = append(out, finding{"error", filepath.Join(clusterMonitoringDir(hub), e.Name()), "no manifest in " + manifest.Dir + "/ — a retired fort's rules would page for a cluster that is gone; `fort dismantle` removes this file"})
		}
	}
	return out
}
