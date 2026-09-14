package main

import (
	"net/http"
	"net/http/httptest"
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
	inv := func(hosts ...string) string {
		s := "all:\n  children:\n    nixos:\n      hosts:\n"
		for _, h := range hosts {
			s += "        " + h + ":\n          provider: libvirt\n"
		}
		return s
	}
	write("provisioning/inventory.yaml", inv("other"))
	write("forts/pulumi/state/.pulumi/stacks/forts/gone.json", checkpoint)

	// the happy path: absent everywhere, clean, stack exists
	if p, problems := destroyGates(tmp, "gone", []byte(inv("other")), true); p != "libvirt" || len(problems) != 0 {
		t.Errorf("want libvirt + no problems, got %q %v", p, problems)
	}
	// still on main
	if _, problems := destroyGates(tmp, "gone", []byte(inv("other", "gone")), true); len(problems) != 1 || !strings.Contains(problems[0], "origin/main") {
		t.Errorf("main gate: %v", problems)
	}
	// still in the working tree + dirty
	write("provisioning/inventory.yaml", inv("other", "gone"))
	if _, problems := destroyGates(tmp, "gone", []byte(inv("other")), false); len(problems) != 2 {
		t.Errorf("tree gates: %v", problems)
	}
	write("provisioning/inventory.yaml", inv("other"))
	// no stack
	if p, problems := destroyGates(tmp, "never", []byte(inv("other")), true); p != "" || len(problems) != 1 || !strings.Contains(problems[0], "no stack") {
		t.Errorf("stack gate: %q %v", p, problems)
	}
	if _, err := providerFromState([]byte(`{"checkpoint":{"latest":{"resources":[]}}}`)); err == nil {
		t.Error("empty checkpoint should have no provider")
	}
}

func TestRevokeDeployKey(t *testing.T) {
	var deleted string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token t" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/keys":
			w.Write([]byte(`[{"id":7,"title":"flux-other"},{"id":9,"title":"flux-gone"},{"id":11,"title":"flux-gone"}]`))
		case r.Method == "DELETE":
			deleted += r.URL.Path
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	msg, err := revokeDeployKeyAt(srv.Client(), srv.URL, "t", "gone")
	if err != nil || deleted != "/keys/9/keys/11" || !strings.Contains(msg, "revoked flux-gone (id 9, 11)") {
		t.Errorf("revoke: %q %v (deleted %q)", msg, err, deleted)
	}
	msg, err = revokeDeployKeyAt(srv.Client(), srv.URL, "t", "never")
	if err != nil || !strings.Contains(msg, "not registered") {
		t.Errorf("absent key: %q %v", msg, err)
	}
	if _, err := revokeDeployKeyAt(srv.Client(), srv.URL, "wrong", "gone"); err == nil {
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
