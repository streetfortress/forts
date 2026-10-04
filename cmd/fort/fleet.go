package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
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

// orphanAllowlist: host dirs allowed to exist without a manifest. Empty
// since zenbook — the hand-managed gitea laptop that predated the
// manifests — was repaved as a fort (#338); every host has a manifest.
var orphanAllowlist = map[string]bool{}

// The shared layers every cluster reconciles; built once per run and
// substitution-checked once per cluster (each carries its own vars).
var infraLayers = []string{"flux/infrastructure/controllers", "flux/infrastructure/configs"}

// ${VAR} tokens flux's post-build envsubst substitutes. Resources annotated
// kustomize.toolkit.fluxcd.io/substitute: disabled are exempt (the grafana
// dashboards ConfigMap carries Grafana's own ${DS_...} vars).
// varRE matches what flux's envsubst will try to substitute. The
// trailing [^}]* is load-bearing: shell parameter expansion — ${f##*/},
// ${user#fort-}, ${host%-pv} — is still a ${VAR} reference to envsubst,
// and strict mode fails the whole layer on the unset name. Requiring `}`
// immediately after the name made the gate blind to exactly that, and a
// CronJob shipped through it and stopped luxor's apps layer (2026-09-16).
var varRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)[^}]*\}`)

// escapedRE matches what flux's envsubst leaves alone: `$$` is one
// literal `$`, so `$${NODE_NAME}` reaches the cluster as `${NODE_NAME}`
// for the next interpreter (fluent-bit's own env expansion, #626). The
// gate blanks these before it looks for references.
var escapedRE = regexp.MustCompile(`\$\$`)

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
		if ff := dataSizeFindings(layer, built); len(ff) > 0 {
			s.findings = append(s.findings, ff...)
		} else {
			s.findings = append(s.findings, finding{"ok", layer, "kustomize build + data sizes"})
		}
	}
	if orphans {
		s.findings = append(s.findings, orphanFindings(root, hosts)...)
	}
	return s
}

// orphanFindings: a dir under nixos/hosts, flux/clusters or flux/apps
// that no manifest claims.
func orphanFindings(root string, hosts map[string]bool) []finding {
	var out []finding
	for _, parent := range []string{"nixos/hosts", "flux/clusters", "flux/apps"} {
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
// A plain fort (no [cluster]) has a host dir and no fleet surface: the host
// dir is checked, and a fleet dir that turned up anyway is the finding.
func checkFleetFort(root, fort string, entry manifest.Host, shared fleetShared) []finding {
	var out []finding
	if !entry.IsCluster() {
		if !exists(root, filepath.Join("nixos/hosts", fort)) {
			out = append(out, finding{"error", filepath.Join("nixos/hosts", fort), "missing — a plain fort has a manifest and a nix host dir (fort scaffold renders it)"})
		}
		for _, rel := range []string{filepath.Join("flux/clusters", fort), filepath.Join("flux/apps", fort)} {
			if exists(root, rel) {
				out = append(out, finding{"error", rel, "a plain fort has no cluster — nothing reconciles this tree; remove it, or add [cluster] to the manifest"})
			}
		}
		if len(out) == 0 {
			out = append(out, finding{"ok", filepath.Join("nixos/hosts", fort), "plain fort: host dir present, no fleet surface expected"})
		}
		return out
	}
	for _, rel := range []string{filepath.Join("nixos/hosts", fort), filepath.Join("flux/clusters", fort), filepath.Join("flux/apps", fort)} {
		if !exists(root, rel) {
			out = append(out, finding{"error", rel, "missing — every fort has a manifest, a nix host dir, a flux cluster dir and an apps tree (fort scaffold renders them)"})
		}
	}
	if len(out) > 0 {
		return out
	}
	vars, err := clusterVars(root, fort)
	if err != nil {
		out = append(out, finding{"error", filepath.Join("flux/clusters", fort, "cluster-vars.yaml"), err.Error()})
	}
	layers := []string{filepath.Join("flux/apps", fort), filepath.Join("flux/clusters", fort, "flux-system")}
	for _, layer := range layers {
		built, err := kustomizeBuild(root, layer)
		if err != nil {
			out = append(out, finding{"error", layer, err.Error()})
			continue
		}
		ff := append(namespaceFindings(layer, fort, built), dataSizeFindings(layer, built)...)
		ff = append(ff, envsubstFindings(layer, fort, built, vars)...)
		if len(ff) > 0 {
			out = append(out, ff...)
		} else {
			out = append(out, finding{"ok", layer, fmt.Sprintf("kustomize build + namespaces + data sizes + strict envsubst (%d cluster vars)", len(vars))})
		}
	}
	for _, layer := range infraLayers {
		if built, ok := shared.infra[layer]; ok {
			out = append(out, namespaceFindings(layer, fort, built)...)
			out = append(out, envsubstFindings(layer, fort, built, vars)...)
		}
	}
	return out
}

// clusterVars: the keys of flux/clusters/<fort>/cluster-vars.yaml's data —
// what flux's envsubst will have.
func clusterVars(root, fort string) (map[string]bool, error) {
	raw, err := os.ReadFile(filepath.Join(root, "flux/clusters", fort, "cluster-vars.yaml"))
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

// clusterScoped are the kinds that legitimately carry no namespace.
// A deny-list rather than API discovery: the gate runs offline in CI
// against a stream, not a cluster. It errs loud — an unlisted
// cluster-scoped kind is reported and added here, which beats the
// alternative of missing a namespaced resource that has none.
var clusterScoped = map[string]bool{
	"Namespace": true, "Node": true, "PersistentVolume": true,
	"StorageClass": true, "CustomResourceDefinition": true,
	"ClusterRole": true, "ClusterRoleBinding": true,
	"ClusterIssuer": true, "IngressClass": true, "GatewayClass": true,
	"PriorityClass": true, "APIService": true,
	"ValidatingWebhookConfiguration": true, "MutatingWebhookConfiguration": true,
	"RuntimeClass": true, "CSIDriver": true, "VolumeSnapshotClass": true,
	// Gateway API's admission policies, vendored with the CRDs.
	"ValidatingAdmissionPolicy": true, "ValidatingAdmissionPolicyBinding": true,
}

// namespaceFindings: every namespaced resource in the built stream
// declares one. kustomize does NOT inherit a namespace into generated
// resources — a configMapGenerator entry without `namespace:` emits a
// bare ConfigMap, the apply fails with "namespace not specified: the
// server could not find the requested resource", and flux stops the
// WHOLE layer on it. That took luxor's apps kustomization down (and
// databasus-configs with it, as a dependency) the day rustfs-central
// landed, which is the same blast radius as the envsubst trap below and
// deserves the same gate.
func namespaceFindings(layer, fort, built string) []finding {
	var out []finding
	for _, chunk := range docSep.Split(built, -1) {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name      string `yaml:"name"`
				Namespace string `yaml:"namespace"`
			} `yaml:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(chunk), &doc); err != nil {
			continue // envsubstFindings reports the unparseable ones
		}
		if doc.Kind == "" || clusterScoped[doc.Kind] || doc.Metadata.Namespace != "" {
			continue
		}
		out = append(out, finding{"error", layer, fmt.Sprintf("(%s): %s/%s has no namespace — the apply fails with \"namespace not specified\" and stops the whole layer (a generator does not inherit one; set `namespace:` on its entry)", fort, doc.Kind, doc.Metadata.Name)})
	}
	return out
}

// dataSizeLimit is what the API server allows a ConfigMap or a Secret
// to carry: the validation sums the data values and refuses the object
// above 1 MiB (MaxSecretSize in the API server's own validation).
// dataSizeWarn is the point at which the next file of the same size
// would not fit — three quarters of the limit.
const (
	dataSizeLimit = 1 << 20
	dataSizeWarn  = dataSizeLimit * 3 / 4
)

// dataSizeFindings: no generated ConfigMap or Secret in the built stream
// passes the 1 MiB limit. The API server refuses the object, and flux
// then fails the WHOLE layer — every other resource in the directory
// stops to reconcile, and the symptom the operator sees first is "my
// dashboard did not appear" (#508). Nothing warned before that point:
// kustomize build applies no size limit of its own, and the 13 grafana
// dashboards in one ConfigMap reached 63% of the limit unremarked.
// The warning at three quarters of the limit is that notice.
func dataSizeFindings(layer, built string) []finding {
	var out []finding
	for _, chunk := range docSep.Split(built, -1) {
		if strings.TrimSpace(chunk) == "" {
			continue
		}
		var doc struct {
			Kind     string `yaml:"kind"`
			Metadata struct {
				Name string `yaml:"name"`
			} `yaml:"metadata"`
			Data       map[string]string `yaml:"data"`
			BinaryData map[string]string `yaml:"binaryData"`
			StringData map[string]string `yaml:"stringData"`
		}
		// A type error still fills the fields it could decode, and an
		// unparseable document carries no Kind and falls out below —
		// envsubstFindings is where a broken document is reported.
		_ = yaml.Unmarshal([]byte(chunk), &doc)
		if doc.Kind != "ConfigMap" && doc.Kind != "Secret" {
			continue
		}
		size := dataSize(doc.Kind, doc.Data, doc.BinaryData, doc.StringData)
		switch {
		case size > dataSizeLimit:
			out = append(out, finding{"error", layer, fmt.Sprintf("%s/%s carries %d bytes of data, over the API server's %d — the apply fails and flux stops the whole layer (split it: one ConfigMap for each file)", doc.Kind, doc.Metadata.Name, size, dataSizeLimit)})
		case size > dataSizeWarn:
			out = append(out, finding{"warning", layer, fmt.Sprintf("%s/%s carries %d bytes of data, over three quarters of the API server's %d — split it before it passes the limit and stops the whole layer", doc.Kind, doc.Metadata.Name, size, dataSizeLimit)})
		}
	}
	return out
}

// dataSize counts what the API server counts. A ConfigMap is measured
// by its values alone; binaryData holds base64, and the limit applies
// to the bytes that value decodes to. A Secret is measured by its keys
// and its values, and its data is base64 for the same reason.
func dataSize(kind string, data, binaryData, stringData map[string]string) int {
	size := 0
	if kind == "Secret" {
		for k, v := range data {
			size += len(k) + decodedLen(v)
		}
		for k, v := range stringData {
			size += len(k) + len(v)
		}
		return size
	}
	for _, v := range data {
		size += len(v)
	}
	for _, v := range binaryData {
		size += decodedLen(v)
	}
	return size
}

// decodedLen is the length of a base64 value, or the length of the
// string itself when it does not decode — a sops-encrypted value is
// ciphertext with a prefix, and counting it raw is the safe side.
func decodedLen(v string) int {
	b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(v), ""))
	if err != nil {
		return len(v)
	}
	return len(b)
}

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
		for _, m := range varRE.FindAllStringSubmatch(escapedRE.ReplaceAllString(chunk, ""), -1) {
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
