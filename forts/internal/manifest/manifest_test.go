package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lab = `mesh_ip = "10.1.0.15"
k3s_cluster_init = false

[backups]
profile = "lab"

[shape]
vcpus = 2
memory = 4
disk = "80G"

[libvirt]
ip = "10.0.0.41"
`

const cloud = `mesh_ip = "10.1.0.54"

[backups]
profile = "production"

[dns]
zone = "streetfortress.cloud"
name = "sfi-prod"

[linode]
region = "us-sea"
type = "g6-standard-2"
`

func TestParse(t *testing.T) {
	h, err := Parse("lab", []byte(lab))
	if err != nil {
		t.Fatal(err)
	}
	if h.Provider != "libvirt" || h.Cloud() || h.Libvirt() == nil || h.Libvirt().IP != "10.0.0.41" || h.Linode() != nil {
		t.Errorf("lab: %+v", h)
	}
	if h.Backups.Profile != "lab" || !h.Backups.Backed() {
		t.Errorf("lab backups: %+v", h.Backups)
	}
	if (Backups{Profile: "none"}).Backed() {
		t.Error(`"none" is a declaration that nothing is backed up, not a profile that backs up`)
	}
	if h.Endpoint() != "10.0.0.41" || h.Shape.Disk != "80G" || h.ClusterInit == nil || *h.ClusterInit {
		t.Errorf("lab derived: endpoint %q shape %+v init %v", h.Endpoint(), h.Shape, h.ClusterInit)
	}
	if s := (Shape{}).WithLabDefaults(); s.Vcpus != 2 || s.Memory != 4 || s.Disk != "40G" {
		t.Errorf("lab defaults: %+v", s)
	}

	c, err := Parse("sfi-prod", []byte(cloud))
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider != "linode" || !c.Cloud() || c.Linode() == nil || c.Linode().Type != "g6-standard-2" || c.Libvirt() != nil {
		t.Errorf("cloud: %+v", c)
	}
	if c.Endpoint() != "sfi-prod.streetfortress.cloud" || c.ClusterInit != nil {
		t.Errorf("cloud derived: endpoint %q init %v", c.Endpoint(), c.ClusterInit)
	}
	c.MeshEndpoint = "1.2.3.4"
	if c.Endpoint() != "1.2.3.4" {
		t.Error("mesh_endpoint should win")
	}
}

const metal = `mesh_ip = "10.1.0.6"

[backups]
profile = "production"

[baremetal]
ip = "10.0.0.6"
mac = "f8:ce:72:02:05:07"
disk = "/dev/nvme0n1"
`

// A [baremetal] host is a lab host (LAN address, .lofi names, lab
// issuer) that no backend realizes: the verbs that would apply refuse.
func TestParseBaremetal(t *testing.T) {
	h, err := Parse("metal", []byte(metal))
	if err != nil {
		t.Fatal(err)
	}
	if h.Provider != "baremetal" || h.Cloud() || h.Realized() || h.Baremetal() == nil || h.Libvirt() != nil || h.Linode() != nil {
		t.Errorf("metal: %+v", h)
	}
	if h.Baremetal().MAC != "f8:ce:72:02:05:07" || h.Baremetal().Disk != "/dev/nvme0n1" || h.LanIP() != "10.0.0.6" || h.Endpoint() != "10.0.0.6" {
		t.Errorf("metal derived: %+v endpoint %q", h.Baremetal(), h.Endpoint())
	}
	l, _ := Parse("lab", []byte(lab))
	if !l.Realized() || l.LanIP() != "10.0.0.41" {
		t.Errorf("lab: realized %v lan %q", l.Realized(), l.LanIP())
	}
	c, _ := Parse("cloud", []byte(cloud))
	if !c.Realized() || c.LanIP() != "" {
		t.Errorf("cloud: realized %v lan %q", c.Realized(), c.LanIP())
	}
	// mac is optional: one wired NIC matches en* like a VM
	if _, err := Parse("m", []byte("mesh_ip = \"10.1.0.6\"\n[backups]\nprofile = \"lab\"\n[baremetal]\nip = \"10.0.0.6\"\n")); err != nil {
		t.Errorf("mac should be optional: %v", err)
	}
}

func TestParseRefuses(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"no provider", "mesh_ip = \"10.1.0.1\"\n", "no provider table"},
		{"two providers", lab + "\n[linode]\nregion = \"x\"\ntype = \"y\"\n", "2 provider tables (libvirt, linode)"},
		{"unknown key", "productoin = true\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key productoin"},
		{"unknown provider key", "[libvirt]\nip = \"10.0.0.1\"\nansible_host = \"x\"\n", "[libvirt]: unknown key ansible_host"},
		{"unknown table", "[hetzner]\nx = 1\n", "unknown key hetzner"},
		{"no mesh", "[libvirt]\nip = \"10.0.0.1\"\n", "mesh_ip is required"},
		{"bad mesh ip", "mesh_ip = \"lol\"\n[libvirt]\nip = \"10.0.0.1\"\n", "mesh_ip"},
		{"linode missing type", "mesh_ip = \"10.1.0.1\"\n[backups]\nprofile = \"lab\"\n[linode]\nregion = \"us-sea\"\n", "[linode]: region and type are required"},
		{"libvirt missing ip", "mesh_ip = \"10.1.0.1\"\n[backups]\nprofile = \"lab\"\n[libvirt]\nlan_prefix = 24\n", "[libvirt]: ip is required"},
		// The variant the manifest exists to make un-forgettable: silence
		// is not "lab", and a typo is not a default.
		{"no backups profile", "mesh_ip = \"10.1.0.1\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[backups] profile is required"},
		{"bad backups profile", "mesh_ip = \"10.1.0.1\"\n[backups]\nprofile = \"offsite\"\n[libvirt]\nip = \"10.0.0.1\"\n", "want one of lab, production, none"},
		{"unknown backups key", "mesh_ip = \"10.1.0.1\"\n[backups]\nprofile = \"lab\"\nhour = 5\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key"},
		{"dns half", "mesh_ip = \"10.1.0.1\"\n[dns]\nzone = \"z\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[dns] needs both"},
		{"bad disk", "mesh_ip = \"10.1.0.1\"\n[shape]\ndisk = \"80\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[shape] disk"},
		{"wrong type", "k3s_cluster_init = \"yes\"\nmesh_ip = \"10.1.0.1\"\n[libvirt]\nip = \"10.0.0.1\"\n", "k3s_cluster_init"},
		{"baremetal missing ip", "mesh_ip = \"10.1.0.1\"\n[backups]\nprofile = \"lab\"\n[baremetal]\nmac = \"f8:ce:72:02:05:07\"\n", "[baremetal]: ip is required"},
		{"baremetal bad mac", "mesh_ip = \"10.1.0.1\"\n[backups]\nprofile = \"lab\"\n[baremetal]\nip = \"10.0.0.1\"\nmac = \"f8ce.7202.0507x\"\n", "[baremetal]: mac"},
		{"baremetal upper mac", "mesh_ip = \"10.1.0.1\"\n[backups]\nprofile = \"lab\"\n[baremetal]\nip = \"10.0.0.1\"\nmac = \"F8:CE:72:02:05:07\"\n", "lowercase"},
		{"baremetal bad disk", "mesh_ip = \"10.1.0.1\"\n[backups]\nprofile = \"lab\"\n[baremetal]\nip = \"10.0.0.1\"\ndisk = \"nvme0n1\"\n", "[baremetal]: disk"},
		{"baremetal with shape", "mesh_ip = \"10.1.0.1\"\n[backups]\nprofile = \"lab\"\n[shape]\nvcpus = 4\n[baremetal]\nip = \"10.0.0.1\"\n", "[shape] is meaningless"},
		{"baremetal unknown key", "mesh_ip = \"10.1.0.1\"\n[backups]\nprofile = \"lab\"\n[baremetal]\nip = \"10.0.0.1\"\nbmc = \"x\"\n", "[baremetal]: unknown key bmc"},
	} {
		_, err := Parse("h", []byte(tc.text))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", tc.name, tc.want, err)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "provisioning/forts/h.toml: ") {
			t.Errorf("%s: error names the file: %v", tc.name, err)
		}
	}
}

func TestLoadAll(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, Dir), 0o755)
	os.WriteFile(filepath.Join(root, Path("a")), []byte(lab), 0o644)
	os.WriteFile(filepath.Join(root, Path("b")), []byte(cloud), 0o644)
	os.WriteFile(filepath.Join(root, Path("bad")), []byte("nope = true\n"), 0o644)
	os.WriteFile(filepath.Join(root, Dir, "README.md"), []byte("not a manifest"), 0o644)
	hosts, problems, err := LoadAll(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 || len(problems) != 1 || problems["bad"] == nil {
		t.Errorf("hosts %v problems %v", hosts, problems)
	}
	names, _ := Names(root)
	if strings.Join(names, ",") != "a,b,bad" {
		t.Errorf("names: %v", names)
	}
	if _, err := Load(root, "missing"); err == nil || !strings.Contains(err.Error(), "has no manifest") {
		t.Errorf("missing: %v", err)
	}
}

// [dev] (#413): a positive declaration with its own invariants — no
// backups, no cluster seed, a lab VM.
func TestDev(t *testing.T) {
	base := "mesh_ip = \"10.1.0.96\"\n[dev]\n[backups]\nprofile = \"none\"\n[libvirt]\nip = \"10.0.0.96\"\n"
	h, err := Parse("dev-fort", []byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if !h.IsDev() || h.Cluster() {
		t.Errorf("want a dev fort that is not a cluster: dev=%v cluster=%v", h.IsDev(), h.Cluster())
	}
	plain, err := Parse("lab", []byte("mesh_ip = \"10.1.0.96\"\n[backups]\nprofile = \"lab\"\n[libvirt]\nip = \"10.0.0.96\"\n"))
	if err != nil || plain.IsDev() || !plain.Cluster() {
		t.Errorf("a fort without [dev] is a cluster: %v dev=%v", err, plain.IsDev())
	}
	for name, raw := range map[string]string{
		"backups": "mesh_ip = \"10.1.0.96\"\n[dev]\n[backups]\nprofile = \"lab\"\n[libvirt]\nip = \"10.0.0.96\"\n",
		"cluster": "mesh_ip = \"10.1.0.96\"\nk3s_cluster_init = false\n[dev]\n[backups]\nprofile = \"none\"\n[libvirt]\nip = \"10.0.0.96\"\n",
		"cloud":   "mesh_ip = \"10.1.0.96\"\n[dev]\n[backups]\nprofile = \"none\"\n[dns]\nzone = \"example.cloud\"\nname = \"dev\"\n[linode]\nregion = \"us-sea\"\ntype = \"g6-standard-2\"\n",
		"unknown": "mesh_ip = \"10.1.0.96\"\n[dev]\nagent = \"sfi-agent\"\n[backups]\nprofile = \"none\"\n[libvirt]\nip = \"10.0.0.96\"\n",
	} {
		if _, err := Parse("dev-fort", []byte(raw)); err == nil {
			t.Errorf("%s: want a refusal", name)
		}
	}
}
