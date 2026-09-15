package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lab = `mesh_ip = "10.1.0.15"
k3s_cluster_init = false

[shape]
vcpus = 2
memory = 4
disk = "80G"

[libvirt]
ip = "10.0.0.41"
`

const cloud = `mesh_ip = "10.1.0.54"

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

func TestParseRefuses(t *testing.T) {
	for _, tc := range []struct{ name, text, want string }{
		{"no provider", "mesh_ip = \"10.1.0.1\"\n", "no provider table"},
		{"two providers", lab + "\n[linode]\nregion = \"x\"\ntype = \"y\"\n", "2 provider tables (libvirt, linode)"},
		{"unknown key", "productoin = true\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key productoin"},
		{"unknown provider key", "[libvirt]\nip = \"10.0.0.1\"\nansible_host = \"x\"\n", "[libvirt]: unknown key ansible_host"},
		{"unknown table", "[hetzner]\nx = 1\n", "unknown key hetzner"},
		{"no mesh", "[libvirt]\nip = \"10.0.0.1\"\n", "mesh_ip is required"},
		{"bad mesh ip", "mesh_ip = \"lol\"\n[libvirt]\nip = \"10.0.0.1\"\n", "mesh_ip"},
		{"linode missing type", "mesh_ip = \"10.1.0.1\"\n[linode]\nregion = \"us-sea\"\n", "[linode]: region and type are required"},
		{"libvirt missing ip", "mesh_ip = \"10.1.0.1\"\n[libvirt]\nlan_prefix = 24\n", "[libvirt]: ip is required"},
		{"dns half", "mesh_ip = \"10.1.0.1\"\n[dns]\nzone = \"z\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[dns] needs both"},
		{"bad disk", "mesh_ip = \"10.1.0.1\"\n[shape]\ndisk = \"80\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[shape] disk"},
		{"wrong type", "k3s_cluster_init = \"yes\"\nmesh_ip = \"10.1.0.1\"\n[libvirt]\nip = \"10.0.0.1\"\n", "k3s_cluster_init"},
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
