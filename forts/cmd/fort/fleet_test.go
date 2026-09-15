package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFleetTree(t *testing.T) {
	root := t.TempDir()
	mk := func(rel string) { os.MkdirAll(filepath.Join(root, rel), 0o755) }
	shared := fleetShared{infra: map[string]string{}}

	got := checkFleetFort(root, "a", shared)
	if len(got) != 3 || got[0].level != "error" || got[0].dir != "nixos/hosts/a" {
		t.Errorf("empty tree: %+v", got)
	}
	mk("nixos/hosts/a")
	mk("fleet/clusters/a")
	mk("fleet/apps/a")
	got = checkFleetFort(root, "a", shared)
	if len(got) == 0 || got[0].dir != "fleet/clusters/a/cluster-vars.yaml" || got[0].level != "error" {
		t.Errorf("no cluster-vars: %+v", got)
	}

	mk("nixos/hosts/zz")
	mk("nixos/hosts/zenbook")
	mk("fleet/apps/old")
	orphans := orphanFindings(root, map[string]bool{"a": true})
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
