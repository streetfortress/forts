package main

import (
	"strings"
	"testing"
)

func TestAdoptable(t *testing.T) {
	step := func(op, urn, diff string) string {
		return `{"op":"` + op + `","urn":"urn:pulumi:s::forts::` + urn + `","detailedDiff":{` + diff + `}}`
	}
	ok := `{"steps":[` + step("same", "pulumi:pulumi:Stack::forts-s", "") + "," +
		step("same", "libvirt:index/volume:Volume::h-disk", "") + "," +
		step("update", "libvirt:index/domain:Domain::h", `"autostart":{"kind":"update"}`) + `]}`
	if err := adoptable(ok); err != nil {
		t.Errorf("autostart-only update should adopt: %v", err)
	}
	replace := `{"steps":[` + step("replace", "libvirt:index/domain:Domain::h", `"consoles[0].type":{"kind":"add-replace"},"autostart":{"kind":"update"}`) + `]}`
	err := adoptable(replace)
	if err == nil || !strings.Contains(err.Error(), "h: replace (autostart, consoles[0].type)") {
		t.Errorf("replace should refuse: %v", err)
	}
	memory := `{"steps":[` + step("update", "libvirt:index/domain:Domain::h", `"memory":{"kind":"update"}`) + `]}`
	if err := adoptable(memory); err == nil || !strings.Contains(err.Error(), "update of memory") {
		t.Errorf("memory update should refuse: %v", err)
	}
	if err := adoptable(`{"steps":[]}`); err == nil {
		t.Error("empty preview should refuse")
	}
	if err := adoptable(`{"steps":[` + step("create", "libvirt:index/volume:Volume::h-disk", "") + `]}`); err == nil {
		t.Error("create should refuse")
	}
}
