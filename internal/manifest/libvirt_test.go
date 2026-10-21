package manifest

import (
	"strings"
	"testing"
)

// The hypervisor and the pool are the manifest's (forts#27): the table
// decodes them, the pool has a default, and a URI of the wrong shape is
// a refusal at load time — before any verb dials a hypervisor.
func TestLibvirtHypervisor(t *testing.T) {
	const fort = `mesh_ip = "10.1.0.15"

[libvirt]
uri = "qemu+ssh://soundship/system"
ip = "10.0.0.41"
pool = "fast"
`
	h, err := Parse("lab", []byte(fort))
	if err != nil {
		t.Fatal(err)
	}
	if h.LibvirtURI() != "qemu+ssh://soundship/system" {
		t.Errorf("uri: %q", h.LibvirtURI())
	}
	if got := h.Libvirt().PoolName(); got != "fast" {
		t.Errorf("pool: %q", got)
	}
	// A table that names neither: no hypervisor, the default pool. The
	// environment is that fort's only source, and `fort check` warns.
	bare, err := Parse("lab", []byte("mesh_ip = \"10.1.0.15\"\n\n[libvirt]\nip = \"10.0.0.41\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if bare.LibvirtURI() != "" || bare.Libvirt().PoolName() != DefaultLibvirtPool {
		t.Errorf("bare table: uri %q pool %q", bare.LibvirtURI(), bare.Libvirt().PoolName())
	}
	// A URI is read, not an IP: a [linode] host has no hypervisor.
	cloudHost, err := Parse("cloud", []byte(cloud))
	if err != nil {
		t.Fatal(err)
	}
	if cloudHost.LibvirtURI() != "" {
		t.Errorf("[linode] host: uri %q", cloudHost.LibvirtURI())
	}
	for _, bad := range []string{
		`uri = "soundship"`,            // no scheme
		`uri = "qemu+ssh://soundship"`, // no connection path
		`uri = "qemu+ssh:///system"`,   // a remote transport, no host
		`uri = "qemu++ssh://s/system"`, // one driver, one transport
		`pool = "two words"`,           // not a pool name
	} {
		src := strings.Replace(fort, `uri = "qemu+ssh://soundship/system"`, bad, 1)
		src = strings.Replace(src, `pool = "fast"`, "", 1)
		if _, err := Parse("lab", []byte(src)); err == nil {
			t.Errorf("%s: want a refusal", bad)
		}
	}
	// The shapes a lab reaches: over ssh, and the local hypervisor.
	for _, good := range []string{
		"qemu+ssh://soundship/system",
		"qemu+ssh://steven@10.0.0.2:22/system?no_verify=1",
		"qemu:///system",
		"qemu+unix:///system",
		"test:///default",
	} {
		if err := ValidateLibvirtURI(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
}
