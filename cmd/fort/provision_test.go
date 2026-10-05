package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStackGate(t *testing.T) {
	tmp := t.TempDir()
	stacks := filepath.Join(tmp, "state/.pulumi/stacks/forts")
	os.MkdirAll(stacks, 0o755)
	os.WriteFile(filepath.Join(stacks, "live.json"), []byte(checkpoint), 0o644)

	// an existing stack: never re-init, first provision or not
	for _, first := range []bool{true, false} {
		if init, err := stackGate(tmp, "live", first); init || err != nil {
			t.Errorf("live (first=%v): want no init, no error; got %v %v", first, init, err)
		}
	}
	// the first provision on a fresh host creates the stack
	if init, err := stackGate(tmp, "fresh", true); !init || err != nil {
		t.Errorf("fresh first provision: want init; got %v %v", init, err)
	}
	// a day-2 apply on a host with no stack refuses, naming the first provision
	if init, err := stackGate(tmp, "fresh", false); init || err == nil || !strings.Contains(err.Error(), "--image") {
		t.Errorf("fresh day-2: want refusal mentioning --image; got %v %v", init, err)
	}
	// a .bak alone (the residue destroy leaves) is not a stack
	os.WriteFile(filepath.Join(stacks, "gone.json.bak"), []byte(checkpoint), 0o644)
	if init, err := stackGate(tmp, "gone", true); !init || err != nil {
		t.Errorf("gone first provision: .bak is residue, want init; got %v %v", init, err)
	}
}

// The seed goes out only when the apply creates the instance (PR #654
// review): the stack export says whether one is already there.
func TestHasResourceType(t *testing.T) {
	provisioned := `{"version":3,"deployment":{"manifest":{},"resources":[
	 {"type":"pulumi:pulumi:Stack"},{"type":"pulumi:providers:linode"},
	 {"type":"linode:index/firewall:Firewall"},{"type":"linode:index/instance:Instance"},
	 {"type":"linode:index/instanceDisk:InstanceDisk"}]}}`
	if has, err := hasResourceType([]byte(provisioned), seedHostTypes["linode"]); !has || err != nil {
		t.Errorf("provisioned stack: want the instance found; got %v %v", has, err)
	}
	// a provision that failed after the firewall, before the instance:
	// still needed
	partial := `{"version":3,"deployment":{"manifest":{},"resources":[
	 {"type":"pulumi:pulumi:Stack"},{"type":"pulumi:providers:linode"},{"type":"linode:index/firewall:Firewall"}]}}`
	if has, err := hasResourceType([]byte(partial), seedHostTypes["linode"]); has || err != nil {
		t.Errorf("partial provision: want no instance; got %v %v", has, err)
	}
	// a fresh stack exports a deployment with no resources key at all
	fresh := `{"version":3,"deployment":{"manifest":{"time":"2026-10-02T00:00:00Z","magic":"x","version":"v3.263.0"}}}`
	if has, err := hasResourceType([]byte(fresh), seedHostTypes["linode"]); has || err != nil {
		t.Errorf("fresh stack: want no instance; got %v %v", has, err)
	}
	if _, err := hasResourceType([]byte("error: no stack"), seedHostTypes["linode"]); err == nil {
		t.Error("garbage must be an error, not a false")
	}
	// the lab: the domain is the host (#657); a volume alone is a provision
	// that stopped before it, and the dev fort's committed state is a
	// complete one
	lab := `{"version":3,"deployment":{"manifest":{},"resources":[
	 {"type":"pulumi:pulumi:Stack"},{"type":"pulumi:providers:libvirt"},{"type":"libvirt:index/volume:Volume"}]}}`
	if has, err := hasResourceType([]byte(lab), seedHostTypes["libvirt"]); has || err != nil {
		t.Errorf("lab partial provision: want no domain; got %v %v", has, err)
	}
	state, err := os.ReadFile("../../pulumi/state/.pulumi/stacks/forts/sfi-dev-831c637.json")
	if err != nil {
		t.Skip("no committed lab state")
	}
	var cp struct {
		Checkpoint struct {
			Latest json.RawMessage `json:"latest"`
		} `json:"checkpoint"`
	}
	if err := json.Unmarshal(state, &cp); err != nil {
		t.Fatal(err)
	}
	export := `{"version":3,"deployment":` + string(cp.Checkpoint.Latest) + `}`
	if has, err := hasResourceType([]byte(export), seedHostTypes["libvirt"]); !has || err != nil {
		t.Errorf("dev fort's state: want the domain found; got %v %v", has, err)
	}
	for _, p := range []string{"linode", "libvirt"} {
		if seedChannels[p] == "" {
			t.Errorf("provider %s: no channel name", p)
		}
	}
}
