package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStackGate(t *testing.T) {
	tmp := t.TempDir()
	stacks := filepath.Join(tmp, "state/.pulumi/stacks/forts")
	os.MkdirAll(stacks, 0o755)
	os.WriteFile(filepath.Join(stacks, "born.json"), []byte(checkpoint), 0o644)

	// an existing stack: never re-init, birth or not
	for _, birth := range []bool{true, false} {
		if init, err := stackGate(tmp, "born", birth); init || err != nil {
			t.Errorf("born (birth=%v): want no init, no error; got %v %v", birth, init, err)
		}
	}
	// a birth on a fresh host creates the stack
	if init, err := stackGate(tmp, "fresh", true); !init || err != nil {
		t.Errorf("fresh birth: want init; got %v %v", init, err)
	}
	// a day-2 apply on a host with no stack refuses, naming the birth path
	if init, err := stackGate(tmp, "fresh", false); init || err == nil || !strings.Contains(err.Error(), "--image") {
		t.Errorf("fresh day-2: want refusal mentioning --image; got %v %v", init, err)
	}
	// a .bak alone (the residue destroy leaves) is not a stack
	os.WriteFile(filepath.Join(stacks, "gone.json.bak"), []byte(checkpoint), 0o644)
	if init, err := stackGate(tmp, "gone", true); !init || err != nil {
		t.Errorf("gone birth: .bak is residue, want init; got %v %v", init, err)
	}
}

// The seed goes out only when the apply creates the instance (PR #654
// review): the stack export says whether one is already there.
func TestHasResourceType(t *testing.T) {
	born := `{"version":3,"deployment":{"manifest":{},"resources":[
	 {"type":"pulumi:pulumi:Stack"},{"type":"pulumi:providers:linode"},
	 {"type":"linode:index/firewall:Firewall"},{"type":"linode:index/instance:Instance"},
	 {"type":"linode:index/instanceDisk:InstanceDisk"}]}}`
	if has, err := hasResourceType([]byte(born), linodeInstanceType); !has || err != nil {
		t.Errorf("born stack: want the instance found; got %v %v", has, err)
	}
	// a birth that failed after the firewall, before the instance: still needed
	partial := `{"version":3,"deployment":{"manifest":{},"resources":[
	 {"type":"pulumi:pulumi:Stack"},{"type":"pulumi:providers:linode"},{"type":"linode:index/firewall:Firewall"}]}}`
	if has, err := hasResourceType([]byte(partial), linodeInstanceType); has || err != nil {
		t.Errorf("partial birth: want no instance; got %v %v", has, err)
	}
	// a fresh stack exports a deployment with no resources key at all
	fresh := `{"version":3,"deployment":{"manifest":{"time":"2026-10-02T00:00:00Z","magic":"x","version":"v3.263.0"}}}`
	if has, err := hasResourceType([]byte(fresh), linodeInstanceType); has || err != nil {
		t.Errorf("fresh stack: want no instance; got %v %v", has, err)
	}
	if _, err := hasResourceType([]byte("error: no stack"), linodeInstanceType); err == nil {
		t.Error("garbage must be an error, not a false")
	}
}
