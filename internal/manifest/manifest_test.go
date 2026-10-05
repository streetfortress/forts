package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lab = `mesh_ip = "10.1.0.15"

[cluster]

[[backups.pv]]
store = "rustfs-central"

[shape]
vcpus = 2
memory = 4
disk = "80G"

[libvirt]
ip = "10.0.0.41"
`

const cloud = `mesh_ip = "10.1.0.54"

[cluster]
datastore = "sqlite"

[backups.db-physical]
store = "b2"
hour = 4

[backups.db-logical]
store = "b2"

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
	if !h.Backups.Backed() || h.Backups.PVTo(StoreRustfsCentral) == nil || h.Backups.PVTo(StoreB2) != nil || !h.Backups.NeedsHub() || h.Backups.PV[0].Cadence() != "6h" {
		t.Errorf("lab backups: %+v", h.Backups)
	}
	if (Backups{Declared: true}).Backed() {
		t.Error(`an empty [backups] is a declaration that nothing is backed up, not a table that backs up`)
	}
	if (Backups{Declared: true}).NeedsHub() {
		t.Error("an empty [backups] needs no hub")
	}
	if h.Endpoint() != "10.0.0.41" || h.Shape.Disk != "80G" {
		t.Errorf("lab derived: endpoint %q shape %+v", h.Endpoint(), h.Shape)
	}
	// [cluster] (#426) is the shape: an empty table is a cluster on the
	// default datastore, sqlite; "etcd" is the opt-in; a plain fort has
	// no datastore at all.
	if !h.IsCluster() || h.Datastore() != "sqlite" || h.Cluster.Datastore != "" {
		t.Errorf("an empty [cluster] is a cluster on sqlite: cluster=%v datastore %q (%+v)", h.IsCluster(), h.Datastore(), h.Cluster)
	}
	if e, err := Parse("lab", []byte(strings.Replace(lab, "[cluster]\n", "[cluster]\ndatastore = \"etcd\"\n", 1))); err != nil || e.Datastore() != "etcd" {
		t.Errorf("[cluster] datastore = \"etcd\": %v %q", err, e.Datastore())
	}
	if s := (Shape{}).WithLabDefaults(); s.Vcpus != 2 || s.Memory != 4 || s.Disk != "40G" {
		t.Errorf("lab defaults: %+v", s)
	}
	// [cluster] audit_logging (#626): unset means false — a fort
	// inherits nothing by silence — and the key present means exactly
	// what it says.
	if h.AuditLogging() {
		t.Errorf("lab audits without asking: %+v", h.Cluster)
	}
	if a, err := Parse("lab", []byte(strings.Replace(lab, "[cluster]\n", "[cluster]\naudit_logging = true\n", 1))); err != nil || !a.AuditLogging() {
		t.Errorf("[cluster] audit_logging = true: %v %+v", err, a.Cluster)
	}
	if a, err := Parse("lab", []byte(strings.Replace(lab, "[cluster]\n", "[cluster]\naudit_logging = false\n", 1))); err != nil || a.AuditLogging() || a.Cluster == nil {
		t.Errorf("[cluster] audit_logging = false: %v %+v", err, a.Cluster)
	}

	c, err := Parse("sfi-prod", []byte(cloud))
	if err != nil {
		t.Fatal(err)
	}
	if c.Provider != "linode" || !c.Cloud() || c.Linode() == nil || c.Linode().Type != "g6-standard-2" || c.Libvirt() != nil {
		t.Errorf("cloud: %+v", c)
	}
	if c.Endpoint() != "sfi-prod.streetfortress.cloud" || c.Datastore() != "sqlite" {
		t.Errorf("cloud derived: endpoint %q datastore %q", c.Endpoint(), c.Datastore())
	}
	c.MeshEndpoint = "1.2.3.4"
	if c.Endpoint() != "1.2.3.4" {
		t.Error("mesh_endpoint should win")
	}
}

const metal = `mesh_ip = "10.1.0.6"

[cluster]

[[backups.pv]]
store = "b2"

[[backups.pv]]
store = "rustfs-central"
every = "weekly"

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
	if _, err := Parse("m", []byte("mesh_ip = \"10.1.0.6\"\n[cluster]\n[[backups.pv]]\nstore = \"rustfs-central\"\n[baremetal]\nip = \"10.0.0.6\"\n")); err != nil {
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
		{"linode missing type", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"rustfs-central\"\n[linode]\nregion = \"us-sea\"\n", "[linode]: region and type are required"},
		{"libvirt missing ip", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"rustfs-central\"\n[libvirt]\nlan_prefix = 24\n", "[libvirt]: ip is required"},
		// The variant the manifest exists to make un-forgettable: silence
		// is not "lab", and a typo is not a default.
		{"no backups table", "mesh_ip = \"10.1.0.1\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[backups] is required"},
		{"retired profile", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[backups]\nprofile = \"lab\"\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key backups.profile"},
		{"unknown backups key", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[backups]\nhour = 5\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key backups.hour"},
		{"pv without store", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nevery = \"6h\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[[backups.pv]] #1: store is required"},
		{"pv unknown store", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"offsite\"\n[libvirt]\nip = \"10.0.0.1\"\n", "store \"offsite\": want one of b2, rustfs-central"},
		{"pv store twice", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"b2\"\n[[backups.pv]]\nstore = \"b2\"\nevery = \"weekly\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[[backups.pv]] #2: store \"b2\" is already named"},
		{"pv bad cadence", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"b2\"\nevery = \"hourly\"\n[libvirt]\nip = \"10.0.0.1\"\n", "every \"hourly\": want one of 6h, daily, weekly"},
		{"pv unknown key", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"b2\"\nhour = 5\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key backups.pv.hour"},
		{"db-physical on the hub's rustfs", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[backups.db-physical]\nstore = \"rustfs-central\"\nhour = 4\n[libvirt]\nip = \"10.0.0.1\"\n", "[backups.db-physical]: store \"rustfs-central\" does not take this layer"},
		{"db-physical without hour", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[backups.db-physical]\nstore = \"b2\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[backups.db-physical]: hour is required"},
		{"db-physical hour 24", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[backups.db-physical]\nstore = \"b2\"\nhour = 24\n[libvirt]\nip = \"10.0.0.1\"\n", "hour 24: want 0–23"},
		{"db-logical with hour", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[backups.db-logical]\nstore = \"b2\"\nhour = 4\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key backups.db-logical.hour"},
		{"db-logical on the hub's rustfs", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[backups.db-logical]\nstore = \"rustfs-central\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[backups.db-logical]: store \"rustfs-central\" does not take this layer"},
		{"dns half", "mesh_ip = \"10.1.0.1\"\n[dns]\nzone = \"z\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[dns] needs both"},
		{"bad disk", "mesh_ip = \"10.1.0.1\"\n[shape]\ndisk = \"80\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[shape] disk"},
		// The shape table (#426) is handled like the provider tables:
		// strict keys, and the retired top-level k3s_cluster_init is an
		// unknown key like any other — the loader special-cases nothing.
		// No [cluster] is a plain fort, which has nothing to back up.
		{"plain with backups", "mesh_ip = \"10.1.0.1\"\n[[backups.pv]]\nstore = \"rustfs-central\"\n[libvirt]\nip = \"10.0.0.1\"\n", "has nothing to back up"},
		{"plain in the cloud", "mesh_ip = \"10.1.0.1\"\n[backups]\n[dns]\nzone = \"z\"\nname = \"n\"\n[linode]\nregion = \"us-sea\"\ntype = \"g6-standard-2\"\n", "is a lab VM for now"},
		{"retired dev table", "mesh_ip = \"10.1.0.1\"\n[dev]\n[backups]\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key dev"},
		{"retired k3s_cluster_init", "k3s_cluster_init = false\nmesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"rustfs-central\"\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key k3s_cluster_init"},
		{"bad datastore", "mesh_ip = \"10.1.0.1\"\n[[backups.pv]]\nstore = \"rustfs-central\"\n[cluster]\ndatastore = \"postgres\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[cluster] datastore \"postgres\": want one of sqlite, etcd"},
		{"datastore wrong type", "mesh_ip = \"10.1.0.1\"\n[[backups.pv]]\nstore = \"rustfs-central\"\n[cluster]\ndatastore = false\n[libvirt]\nip = \"10.0.0.1\"\n", "datastore"},
		// [cluster] (#626): a typo is not a default, and a plain fort has
		// no API server to audit.
		{"unknown cluster key", "mesh_ip = \"10.1.0.1\"\n[[backups.pv]]\nstore = \"rustfs-central\"\n[cluster]\naudit_loging = true\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key cluster.audit_loging"},
		{"cluster wrong type", "mesh_ip = \"10.1.0.1\"\n[[backups.pv]]\nstore = \"rustfs-central\"\n[cluster]\naudit_logging = \"yes\"\n[libvirt]\nip = \"10.0.0.1\"\n", "audit_logging"},
		{"baremetal missing ip", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"rustfs-central\"\n[baremetal]\nmac = \"f8:ce:72:02:05:07\"\n", "[baremetal]: ip is required"},
		{"baremetal bad mac", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"rustfs-central\"\n[baremetal]\nip = \"10.0.0.1\"\nmac = \"f8ce.7202.0507x\"\n", "[baremetal]: mac"},
		{"baremetal upper mac", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"rustfs-central\"\n[baremetal]\nip = \"10.0.0.1\"\nmac = \"F8:CE:72:02:05:07\"\n", "lowercase"},
		{"baremetal bad disk", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"rustfs-central\"\n[baremetal]\nip = \"10.0.0.1\"\ndisk = \"nvme0n1\"\n", "[baremetal]: disk"},
		{"baremetal with shape", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"rustfs-central\"\n[shape]\nvcpus = 4\n[baremetal]\nip = \"10.0.0.1\"\n", "[shape] is meaningless"},
		{"baremetal unknown key", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[[backups.pv]]\nstore = \"rustfs-central\"\n[baremetal]\nip = \"10.0.0.1\"\nbmc = \"x\"\n", "[baremetal]: unknown key bmc"},
	} {
		_, err := Parse("h", []byte(tc.text))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", tc.name, tc.want, err)
		}
		if err != nil && !strings.HasPrefix(err.Error(), "forts/h.toml: ") {
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

// A plain fort: no [cluster] table, no backups, a lab VM — the baseline
// host and nothing the framework names beyond it.
func TestPlainFort(t *testing.T) {
	h, err := Parse("plain-fort", []byte("mesh_ip = \"10.1.0.96\"\n[backups]\n[libvirt]\nip = \"10.0.0.96\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if h.IsCluster() || h.Datastore() != "" {
		t.Errorf("want a plain fort: cluster=%v datastore %q", h.IsCluster(), h.Datastore())
	}
	cluster, err := Parse("lab", []byte("mesh_ip = \"10.1.0.96\"\n[cluster]\n[[backups.pv]]\nstore = \"rustfs-central\"\n[libvirt]\nip = \"10.0.0.96\"\n"))
	if err != nil || !cluster.IsCluster() {
		t.Errorf("a fort with [cluster] is a cluster: %v", err)
	}
}
