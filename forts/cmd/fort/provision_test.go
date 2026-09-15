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
