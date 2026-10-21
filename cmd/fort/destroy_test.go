package main

import (
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const checkpoint = `{"checkpoint":{"latest":{"resources":[
 {"type":"pulumi:pulumi:Stack"},{"type":"pulumi:providers:libvirt"},{"type":"libvirt:index/domain:Domain","protect":true}]}}}`

func TestDestroyGates(t *testing.T) {
	tmp := t.TempDir()
	write := func(rel, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(tmp, rel)), 0o755)
		os.WriteFile(filepath.Join(tmp, rel), []byte(content), 0o644)
	}
	const lab = "[libvirt]\nip = \"10.0.0.1\"\n"
	write(manifest.Path("other"), lab)
	write("provisioning/pulumi/state/.pulumi/stacks/forts/gone.json", checkpoint)

	// the happy path: no manifest anywhere, clean, stack exists
	if p, problems := destroyGates(tmp, "gone", false, true, false); p != "libvirt" || len(problems) != 0 {
		t.Errorf("want libvirt + no problems, got %q %v", p, problems)
	}
	// still on main
	if _, problems := destroyGates(tmp, "gone", true, true, false); len(problems) != 1 || !strings.Contains(problems[0], "origin/main") {
		t.Errorf("main gate: %v", problems)
	}
	// still in the working tree + dirty
	write(manifest.Path("gone"), lab)
	if _, problems := destroyGates(tmp, "gone", false, false, false); len(problems) != 2 {
		t.Errorf("tree gates: %v", problems)
	}
	os.Remove(filepath.Join(tmp, manifest.Path("gone")))
	// no stack
	if p, problems := destroyGates(tmp, "never", false, true, false); p != "" || len(problems) != 1 || !strings.Contains(problems[0], "no stack") {
		t.Errorf("stack gate: %q %v", p, problems)
	}
	// --no-stack (a [baremetal] host): the stack gate inverts — none is
	// the happy path, one is a wrong flag
	if p, problems := destroyGates(tmp, "never", false, true, true); p != "" || len(problems) != 0 {
		t.Errorf("no-stack happy path: %q %v", p, problems)
	}
	if _, problems := destroyGates(tmp, "gone", false, true, true); len(problems) != 1 || !strings.Contains(problems[0], "drop --no-stack") {
		t.Errorf("no-stack with a stack: %v", problems)
	}
	if _, err := providerFromState([]byte(`{"checkpoint":{"latest":{"resources":[]}}}`)); err == nil {
		t.Error("empty checkpoint should have no provider")
	}
}

func TestRevokeDeployKey(t *testing.T) {
	st := &fakeForgeState{deploy: []deployKey{{ID: 7, Title: "flux-other"}, {ID: 9, Title: "flux-gone"}, {ID: 11, Title: "flux-gone"}}}
	f := newFakeForge(t, st, "gitea")
	msg, err := revokeDeployKeyAt(f, "gone")
	if err != nil || len(st.deploy) != 1 || st.deploy[0].ID != 7 || !strings.Contains(msg, "revoked flux-gone (id 9, 11)") {
		t.Errorf("revoke: %q %v (left %+v)", msg, err, st.deploy)
	}
	msg, err = revokeDeployKeyAt(f, "never")
	if err != nil || !strings.Contains(msg, "not registered") {
		t.Errorf("absent key: %q %v", msg, err)
	}
	bad, _ := newForge("gitea", "forge.example", "acme", "estate", "wrong", f.(*restForge).client)
	bad.(*restForge).keys = f.(*restForge).keys
	if _, err := revokeDeployKeyAt(bad, "gone"); err == nil {
		t.Error("bad token should error")
	}
}

func TestStacksInState(t *testing.T) {
	tmp := t.TempDir()
	for _, f := range []string{"a.json", "a.json.bak", "b.json", "b.json.attrs"} {
		os.MkdirAll(filepath.Join(tmp, "state/.pulumi/stacks/forts"), 0o755)
		os.WriteFile(filepath.Join(tmp, "state/.pulumi/stacks/forts", f), nil, 0o644)
	}
	if got := strings.Join(stacksInState(tmp), ","); got != "a,b" {
		t.Errorf("stacks: %s", got)
	}
}
