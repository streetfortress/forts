package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
	"gopkg.in/yaml.v3"
)

// The fleet half of the gate (ci/fleet-checks.py until #275): the tree
// is consistent with the manifests, every flux layer builds, and flux's
// STRICT post-build envsubst can satisfy every ${VAR} it will meet.
//
// Two red states are deliberate gates, not breakage: a staged-but-unseeded
// app tree (a kustomization lists sops files that only exist after
// `fort seed` / `fort mint`, so kustomize build fails — that red X is the
// guard working, and the PR is not mergeable until the ◆ files land) and a
// host dir with no manifest (a retired or renamed host's lingering config
// — a warning, so a dismantle in flight does not go red on its residue).

// orphanAllowlist: host dirs allowed to exist without a manifest. zenbook
// is the physical gitea host — hand-managed, predates the manifests, and
// deliberately not fort-managed until its flux adoption (Phase 3).
var orphanAllowlist = map[string]bool{"zenbook": true}

// The shared layers every cluster reconciles; built once per run and
// substitution-checked once per cluster (each carries its own vars).
var infraLayers = []string{"fleet/infrastructure/controllers", "fleet/infrastructure/configs"}

// ${VAR} tokens flux's post-build envsubst substitutes. Resources annotated
// kustomize.toolkit.fluxcd.io/substitute: disabled are exempt (the grafana
// dashboards ConfigMap carries Grafana's own ${DS_...} vars).
var varRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

const substituteDisabled = "kustomize.toolkit.fluxcd.io/substitute"

// fleetShared is the once-per-run half: the shared layers' builds (a
// failed one is a finding and an empty stream) and, under --all, the
// orphan scan.
type fleetShared struct {
	infra    map[string]string
	findings []finding
}

func checkFleetShared(root string, hosts map[string]bool, orphans bool) fleetShared {
	s := fleetShared{infra: map[string]string{}}
	for _, layer := range infraLayers {
		built, err := kustomizeBuild(root, layer)
		if err != nil {
			s.findings = append(s.findings, finding{"error", layer, err.Error()})
			continue
		}
		s.infra[layer] = built
		s.findings = append(s.findings, finding{"ok", layer, "kustomize build"})
	}
	if orphans {
		s.findings = append(s.findings, orphanFindings(root, hosts)...)
	}
	return s
}

// orphanFindings: a dir under nixos/hosts, fleet/clusters or fleet/apps
// that no manifest claims.
func orphanFindings(root string, hosts map[string]bool) []finding {
	var out []finding
	for _, parent := range []string{"nixos/hosts", "fleet/clusters", "fleet/apps"} {
		entries, err := os.ReadDir(filepath.Join(root, parent))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || hosts[e.Name()] || orphanAllowlist[e.Name()] {
				continue
			}
			out = append(out, finding{"warning", filepath.Join(parent, e.Name()), "no manifest in " + manifest.Dir + "/ — lingering config from a retired or renamed host?"})
		}
	}
	return out
}

// checkFleetFort: one fort's tree exists, its two layers build, and every
// ${VAR} in them (and in the shared layers) is a key of its cluster-vars.
func checkFleetFort(root, fort string, shared fleetShared) []finding {
	var out []finding
	for _, rel := range []string{filepath.Join("nixos/hosts", fort), filepath.Join("fleet/clusters", fort), filepath.Join("fleet/apps", fort)} {
		if !exists(root, rel) {
			out = append(out, finding{"error", rel, "missing — every fort has a manifest, a nix host dir, a flux cluster dir and an apps tree (fort scaffold renders them)"})
		}
	}
	if len(out) > 0 {
		return out
	}
	vars, err := clusterVars(root, fort)
	if err != nil {
		out = append(out, finding{"error", filepath.Join("fleet/clusters", fort, "cluster-vars.yaml"), err.Error()})
	}
	layers := []string{filepath.Join("fleet/apps", fort), filepath.Join("fleet/clusters", fort, "flux-system")}
	for _, layer := range layers {
		built, err := kustomizeBuild(root, layer)
		if err != nil {
			out = append(out, finding{"error", layer, err.Error()})
			continue
		}
		if ff := envsubstFindings(layer, fort, built, vars); len(ff) > 0 {
			out = append(out, ff...)
		} else {
			out = append(out, finding{"ok", layer, fmt.Sprintf("kustomize build + strict envsubst (%d cluster vars)", len(vars))})
		}
	}
	for _, layer := range infraLayers {
		if built, ok := shared.infra[layer]; ok {
			out = append(out, envsubstFindings(layer, fort, built, vars)...)
		}
	}
	return out
}

// clusterVars: the keys of fleet/clusters/<fort>/cluster-vars.yaml's data —
// what flux's envsubst will have.
func clusterVars(root, fort string) (map[string]bool, error) {
	raw, err := os.ReadFile(filepath.Join(root, "fleet/clusters", fort, "cluster-vars.yaml"))
	if err != nil {
		return nil, fmt.Errorf("missing (fort scaffold renders it)")
	}
	var cm struct {
		Data map[string]string `yaml:"data"`
	}
	if err := yaml.Unmarshal(raw, &cm); err != nil {
		return nil, err
	}
	vars := map[string]bool{}
	for k := range cm.Data {
		vars[k] = true
	}
	return vars, nil
}

// kustomizeBuild runs `kustomize build <rel>` and returns the stream; on
// failure the error is kustomize's last stderr line.
func kustomizeBuild(root, rel string) (string, error) {
	cmd := exec.Command("kustomize", "build", filepath.Join(root, rel))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
		return "", fmt.Errorf("kustomize build: %s", lines[len(lines)-1])
	}
	return string(out), nil
}

var docSep = regexp.MustCompile(`(?m)^---\s*$`)

// envsubstFindings: every ${VAR} in the built stream's non-exempt
// resources is a cluster-vars key — one stray token fails the whole
// layer under flux's strict substitution (#143).
func envsubstFindings(layer, fort, built string, vars map[string]bool) []finding {
	var out []finding
	for _, chunk := range docSep.Split(built, -1) {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name        string            `yaml:"name"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(chunk), &doc); err != nil {
			out = append(out, finding{"error", layer, "unparseable document in the built stream: " + err.Error()})
			continue
		}
		if doc.Metadata.Annotations[substituteDisabled] == "disabled" {
			continue
		}
		unknown := map[string]bool{}
		for _, m := range varRE.FindAllStringSubmatch(chunk, -1) {
			if !vars[m[1]] {
				unknown[m[1]] = true
			}
		}
		if len(unknown) == 0 {
			continue
		}
		names := make([]string, 0, len(unknown))
		for n := range unknown {
			names = append(names, n)
		}
		sort.Strings(names)
		out = append(out, finding{"error", layer, fmt.Sprintf("(%s): %s/%s references ${%s} not in cluster-vars — flux strict envsubst will fail the whole layer", fort, doc.Kind, doc.Metadata.Name, strings.Join(names, ", "))})
	}
	return out
}
