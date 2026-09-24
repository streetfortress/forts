package main

import (
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

func TestFleetTree(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) { os.MkdirAll(filepath.Join(root, rel), 0o755) }
	shared := fleetShared{infra: map[string]string{}}
	cluster := manifest.Host{}

	got := checkFleetFort(root, "a", cluster, shared)
	if len(got) != 3 || got[0].level != "error" || got[0].dir != "nixos/hosts/a" {
		t.Errorf("empty tree: %+v", got)
	}
	mk("nixos/hosts/a")
	mk("fleet/clusters/a")
	mk("fleet/apps/a")
	got = checkFleetFort(root, "a", cluster, shared)
	if len(got) == 0 || got[0].dir != "fleet/clusters/a/cluster-vars.yaml" || got[0].level != "error" {
		t.Errorf("no cluster-vars: %+v", got)
	}

	// a dev fort ([dev], #413): the host dir is the whole tree, and a
	// fleet dir that exists anyway is the finding
	dev := manifest.Host{Dev: &manifest.DevSpec{}}
	got = checkFleetFort(root, "d", dev, shared)
	if len(got) != 1 || got[0].level != "error" || got[0].dir != "nixos/hosts/d" {
		t.Errorf("dev without host dir: %+v", got)
	}
	mk("nixos/hosts/d")
	got = checkFleetFort(root, "d", dev, shared)
	if len(got) != 1 || got[0].level != "ok" {
		t.Errorf("dev with host dir: %+v", got)
	}
	mk("fleet/apps/d")
	got = checkFleetFort(root, "d", dev, shared)
	if len(got) != 1 || got[0].level != "error" || got[0].dir != "fleet/apps/d" {
		t.Errorf("dev with a stray apps tree: %+v", got)
	}

	mk("nixos/hosts/zz")
	mk("fleet/apps/old")
	orphans := orphanFindings(root, map[string]bool{"a": true, "d": true})
	if len(orphans) != 2 || orphans[0].dir != "nixos/hosts/zz" || orphans[1].dir != "fleet/apps/old" || orphans[0].level != "warning" {
		t.Errorf("orphans: %+v", orphans)
	}

	os.WriteFile(filepath.Join(root, "fleet/clusters/a/cluster-vars.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\ndata:\n  CLUSTER_NAME: a\n  MESH_IP: 10.1.0.1\n"), 0o644)
	vars, err := clusterVars(root, "a")
	if err != nil || len(vars) != 2 || !vars["MESH_IP"] {
		t.Errorf("clusterVars: %v %v", vars, err)
	}
}

func TestEnvsubstFindings(t *testing.T) {
	built := `apiVersion: v1
kind: ConfigMap
metadata:
  name: fine
data:
  a: ${CLUSTER_NAME}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: stray
data:
  a: ${CLUSTER_NAME}
  b: ${MESH_IP} and ${OTHER}
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: grafana
  annotations:
    kustomize.toolkit.fluxcd.io/substitute: disabled
data:
  a: ${DS_PROMETHEUS}
---
`
	vars := map[string]bool{"CLUSTER_NAME": true}
	got := envsubstFindings("fleet/apps/a", "a", built, vars)
	if len(got) != 1 || got[0].level != "error" || !strings.Contains(got[0].msg, "ConfigMap/stray references ${MESH_IP, OTHER}") {
		t.Errorf("findings: %+v", got)
	}
	if got := envsubstFindings("x", "a", "", vars); len(got) != 0 {
		t.Errorf("empty stream: %+v", got)
	}
}

func TestKustomizeBuild(t *testing.T) {
	if _, err := exec.LookPath("kustomize"); err != nil {
		t.Skip("kustomize not on PATH")
	}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "ok"), 0o755)
	os.WriteFile(filepath.Join(root, "ok/kustomization.yaml"), []byte("resources:\n  - cm.yaml\n"), 0o644)
	os.WriteFile(filepath.Join(root, "ok/cm.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\n"), 0o644)
	if out, err := kustomizeBuild(root, "ok"); err != nil || !strings.Contains(out, "name: x") {
		t.Errorf("ok: %v\n%s", err, out)
	}
	os.MkdirAll(filepath.Join(root, "bad"), 0o755)
	os.WriteFile(filepath.Join(root, "bad/kustomization.yaml"), []byte("resources:\n  - secrets.sops.yaml\n"), 0o644)
	if _, err := kustomizeBuild(root, "bad"); err == nil || !strings.HasPrefix(err.Error(), "kustomize build: ") {
		t.Errorf("bad: %v", err)
	}
}

// Shell parameter expansion is still a ${VAR} reference as far as flux's
// envsubst is concerned, and strict mode fails the WHOLE layer on the
// unset name. The gate used to require `}` immediately after the name,
// so a CronJob whose script said ${f##*/} passed here and then stopped
// luxor's apps kustomization with `variable not set (strict mode): "f"`
// (2026-09-16). Every form below is one the shipped script used.
func TestEnvsubstFindsShellParameterExpansion(t *testing.T) {
	built := `apiVersion: batch/v1
kind: CronJob
metadata:
  name: reconciler
spec:
  script: |
    user=${f##*/}
    host=${user#fort-}; host=${host%-pv}
    keep=${CLUSTER_NAME}
`
	got := envsubstFindings("fleet/apps/a", "a", built, map[string]bool{"CLUSTER_NAME": true})
	if len(got) != 1 || got[0].level != "error" {
		t.Fatalf("findings: %+v", got)
	}
	// Named, so the operator can see which token to exempt or rename.
	for _, want := range []string{"f", "host", "user"} {
		if !strings.Contains(got[0].msg, want) {
			t.Errorf("want %q named in %q", want, got[0].msg)
		}
	}
	// The escape hatch still works — this resource references no cluster
	// vars, so exemption is the right answer rather than renaming shell
	// locals to please a substituter that should not be reading them.
	exempt := strings.Replace(built, "  name: reconciler\n",
		"  name: reconciler\n  annotations:\n    kustomize.toolkit.fluxcd.io/substitute: disabled\n", 1)
	if got := envsubstFindings("fleet/apps/a", "a", exempt, map[string]bool{}); len(got) != 0 {
		t.Errorf("annotated resource should be exempt: %+v", got)
	}
}

// kustomize does not inherit a namespace into GENERATED resources: a
// configMapGenerator entry without `namespace:` emits a bare ConfigMap,
// the apply fails with "namespace not specified", and flux stops the
// whole layer on it — which took luxor's apps kustomization down, and
// databasus-configs with it, the day rustfs-central landed (2026-09-16).
func TestNamespaceFindings(t *testing.T) {
	built := `apiVersion: v1
kind: Namespace
metadata:
  name: rustfs-central
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: rustfs-policies-ccmc29c59f
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: fine
  namespace: rustfs-central
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: step-ca
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: safe-upgrades.gateway.networking.k8s.io
`
	got := namespaceFindings("fleet/apps/luxor", "luxor", built)
	if len(got) != 1 || got[0].level != "error" {
		t.Fatalf("exactly the generated ConfigMap should be flagged: %+v", got)
	}
	if !strings.Contains(got[0].msg, "ConfigMap/rustfs-policies-ccmc29c59f") {
		t.Errorf("finding names the wrong resource: %s", got[0].msg)
	}
	// Cluster-scoped kinds carry no namespace by definition; flagging them
	// would make the gate noise, and noise is how a gate stops being read.
	for _, kind := range []string{"Namespace", "ClusterIssuer", "ValidatingAdmissionPolicy"} {
		if !clusterScoped[kind] {
			t.Errorf("%s must be exempt", kind)
		}
	}
}

// The 1 MiB ceiling is not kustomize's — it is the API server's, and it
// arrives as a failure of the whole layer (#508). The gate measures the
// same bytes the API server measures: a ConfigMap by its values, a
// Secret by its keys and its decoded values.
func TestDataSizeFindings(t *testing.T) {
	cm := func(name string, size int) string {
		return "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: " + name +
			"\ndata:\n  big.json: " + strings.Repeat("x", size) + "\n---\n"
	}
	built := cm("small", 1024) + cm("near", dataSizeWarn+1) + cm("over", dataSizeLimit+1)
	got := dataSizeFindings("fleet/apps/luxor", built)
	if len(got) != 2 {
		t.Fatalf("the small ConfigMap must not be a finding: %+v", got)
	}
	if got[0].level != "warning" || !strings.Contains(got[0].msg, "ConfigMap/near") {
		t.Errorf("three quarters of the limit is a warning: %+v", got[0])
	}
	if got[1].level != "error" || !strings.Contains(got[1].msg, "ConfigMap/over") {
		t.Errorf("over the limit is an error: %+v", got[1])
	}
	if got := dataSizeFindings("x", ""); len(got) != 0 {
		t.Errorf("empty stream: %+v", got)
	}
	// A Secret's data is base64: 1 MiB of ciphertext is 1.33 MiB of
	// text, and the API server counts the bytes it decodes to.
	secret := "apiVersion: v1\nkind: Secret\nmetadata:\n  name: s\ndata:\n  k: " +
		base64.StdEncoding.EncodeToString([]byte(strings.Repeat("y", 800*1024))) + "\n"
	if got := dataSizeFindings("x", secret); len(got) != 1 || got[0].level != "warning" {
		t.Errorf("base64 must be counted decoded: %+v", got)
	}
}
