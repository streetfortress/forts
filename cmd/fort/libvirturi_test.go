package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

// The hypervisor a libvirt verb dials (forts#27): the manifest records
// it, the environment overrides it, and a fort that has neither is a
// refusal that names both.
func TestResolveLibvirtURI(t *testing.T) {
	if got, err := resolveLibvirtURI("", "qemu+ssh://soundship/system"); err != nil || got != "qemu+ssh://soundship/system" {
		t.Errorf("the manifest's: %q %v", got, err)
	}
	if _, err := resolveLibvirtURI("", ""); err == nil {
		t.Error("no manifest uri and no environment: want a refusal")
	} else if !strings.Contains(err.Error(), "LIBVIRT_DEFAULT_URI") || !strings.Contains(err.Error(), "uri") {
		t.Errorf("the refusal names neither source: %v", err)
	}
	// The escape hatch: the operator's variable wins over the manifest,
	// which is how a machine carries a credential path in the URI.
	const hatch = "qemu+ssh://me@kvm/system?keyfile=/Users/me/.ssh/id_ed25519"
	if got, _ := resolveLibvirtURI(hatch, "qemu+ssh://soundship/system"); got != hatch {
		t.Errorf("the environment must win: %q", got)
	}
	// The override is the operator's value, read once (env.go): cloudEnv
	// writes LIBVIRT_DEFAULT_URI itself, and `fort preview --all` walks
	// several forts — the second fort must get its own hypervisor.
	if got, _ := resolveLibvirtURI("", "qemu+ssh://second-hypervisor/system"); got != "qemu+ssh://second-hypervisor/system" {
		t.Errorf("a second fort on a second hypervisor: %q", got)
	}
}

// The destroy reads the hypervisor off the stack, because the dismantle
// PR deleted the manifest before it may run.
func TestRecordedLibvirtURI(t *testing.T) {
	tmp := t.TempDir()
	write := func(rel, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(tmp, rel)), 0o755)
		os.WriteFile(filepath.Join(tmp, rel), []byte(content), 0o644)
	}
	write(stackConfigPath("lab"), `encryptionsalt: v1:salt
config:
  forts:imageId: /nix/store/x-nixos.qcow2
  forts:libvirtUri: qemu+ssh://soundship/system
  forts:seed: nocloud
`)
	if got := recordedLibvirtURI(tmp, "lab"); got != "qemu+ssh://soundship/system" {
		t.Errorf("recorded: %q", got)
	}
	// A stack from before the key, and a stack that is not there at all:
	// no hypervisor, and cloudEnv names the environment.
	write(stackConfigPath("old"), "encryptionsalt: v1:salt\nconfig:\n  forts:imageId: x\n")
	if got := recordedLibvirtURI(tmp, "old"); got != "" {
		t.Errorf("a stack from before the key: %q", got)
	}
	if got := recordedLibvirtURI(tmp, "absent"); got != "" {
		t.Errorf("no stack config: %q", got)
	}
	if got := libvirtURIFromStackConfig([]byte("\tnot: [yaml")); got != "" {
		t.Errorf("unreadable config: %q", got)
	}
}

// The gate's two sentences about the hypervisor (check.go).
func TestHypervisorFindings(t *testing.T) {
	parse := func(t *testing.T, src string) manifest.Host {
		h, err := manifest.Parse("lab", []byte("mesh_ip = \"10.1.0.9\"\n"+src))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	named := parse(t, "[libvirt]\nuri = \"qemu+ssh://soundship/system\"\nip = \"10.0.0.9\"\n")
	if f := hypervisorFindings("lab", named); len(f) != 1 || f[0].level != "ok" || !strings.Contains(f[0].msg, "pool images") {
		t.Errorf("a named hypervisor: %+v", f)
	}
	bare := parse(t, "[libvirt]\nip = \"10.0.0.9\"\n")
	if f := hypervisorFindings("lab", bare); len(f) != 1 || f[0].level != "warning" {
		t.Errorf("no uri: %+v", f)
	}
	keyed := parse(t, "[libvirt]\nuri = \"qemu+ssh://s/system?sshauth=privkey&keyfile=/Users/me/.ssh/id_ed25519\"\nip = \"10.0.0.9\"\n")
	f := hypervisorFindings("lab", keyed)
	if len(f) != 2 || f[0].level != "ok" || f[1].level != "warning" || !strings.Contains(f[1].msg, "keyfile") {
		t.Errorf("a key path in the manifest: %+v", f)
	}
	// Every other provider has no hypervisor to name.
	cloud := parse(t, "[cluster]\n[linode]\nregion = \"us-sea\"\ntype = \"g6-standard-2\"\n")
	if f := hypervisorFindings("cloud", cloud); f != nil {
		t.Errorf("[linode]: %+v", f)
	}
}
