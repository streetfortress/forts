package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const lab = `mesh_ip = "10.1.0.15"

[cluster]

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

[baremetal]
ip = "10.0.0.6"
mac = "f8:ce:72:02:05:07"
disk = "/dev/nvme0n1"
`

// A [baremetal] host is a lab host (LAN address, mesh-zone names, lab
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
	if _, err := Parse("m", []byte("mesh_ip = \"10.1.0.6\"\n[cluster]\n[baremetal]\nip = \"10.0.0.6\"\n")); err != nil {
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
		// mesh_endpoint is rendered onto a WireGuard Endpoint line, of
		// every peer of every estate this host links to, unescaped (#99).
		{"endpoint injects a section", "mesh_ip = \"10.1.0.1\"\nmesh_endpoint = \"10.0.0.1\\n[Interface]\\nPostUp = id\"\n[libvirt]\nip = \"10.0.0.1\"\n", "mesh_endpoint"},
		{"endpoint with a port", "mesh_ip = \"10.1.0.1\"\nmesh_endpoint = \"hub.lan:51820\"\n[libvirt]\nip = \"10.0.0.1\"\n", "mesh_endpoint"},
		{"linode missing type", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[linode]\nregion = \"us-sea\"\n", "[linode]: region and type are required"},
		// [backups] is the backups layer's table now (forts#143): the refusal names where it went.
		{"backups moved", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[backups]\n[libvirt]\nip = \"10.0.0.1\"\n", "layers/backups/forts/h.toml"},
		{"libvirt missing ip", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[libvirt]\nlan_prefix = 24\n", "[libvirt]: ip is required"},
		{"dns half", "mesh_ip = \"10.1.0.1\"\n[dns]\nzone = \"z\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[dns] needs both"},
		{"bad disk", "mesh_ip = \"10.1.0.1\"\n[shape]\ndisk = \"80\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[shape] disk"},
		// The shape table (#426) is handled like the provider tables:
		// strict keys, and the retired top-level k3s_cluster_init is an
		// unknown key like any other — the loader special-cases nothing.
		{"retired dev table", "mesh_ip = \"10.1.0.1\"\n[dev]\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key dev"},
		{"retired k3s_cluster_init", "k3s_cluster_init = false\nmesh_ip = \"10.1.0.1\"\n[cluster]\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key k3s_cluster_init"},
		{"bad datastore", "mesh_ip = \"10.1.0.1\"\n[cluster]\ndatastore = \"postgres\"\n[libvirt]\nip = \"10.0.0.1\"\n", "[cluster] datastore \"postgres\": want one of sqlite, etcd"},
		{"datastore wrong type", "mesh_ip = \"10.1.0.1\"\n[cluster]\ndatastore = false\n[libvirt]\nip = \"10.0.0.1\"\n", "datastore"},
		// [cluster] (#626): a typo is not a default, and a plain fort has
		// no API server to audit.
		{"unknown cluster key", "mesh_ip = \"10.1.0.1\"\n[cluster]\naudit_loging = true\n[libvirt]\nip = \"10.0.0.1\"\n", "unknown key cluster.audit_loging"},
		{"cluster wrong type", "mesh_ip = \"10.1.0.1\"\n[cluster]\naudit_logging = \"yes\"\n[libvirt]\nip = \"10.0.0.1\"\n", "audit_logging"},
		{"baremetal missing ip", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[baremetal]\nmac = \"f8:ce:72:02:05:07\"\n", "[baremetal]: ip is required"},
		{"baremetal bad mac", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[baremetal]\nip = \"10.0.0.1\"\nmac = \"f8ce.7202.0507x\"\n", "[baremetal]: mac"},
		{"baremetal upper mac", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[baremetal]\nip = \"10.0.0.1\"\nmac = \"F8:CE:72:02:05:07\"\n", "lowercase"},
		{"baremetal bad disk", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[baremetal]\nip = \"10.0.0.1\"\ndisk = \"nvme0n1\"\n", "[baremetal]: disk"},
		{"baremetal with shape", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[shape]\nvcpus = 4\n[baremetal]\nip = \"10.0.0.1\"\n", "[shape] is meaningless"},
		{"baremetal unknown key", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[baremetal]\nip = \"10.0.0.1\"\nbmc = \"x\"\n", "[baremetal]: unknown key bmc"},
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

// A plain fort is a provider decision and a cluster decision, and the
// two do not depend on each other (forts#142): no [cluster] table on
// any of the three provider tables parses. The refusal that made a
// plain fort a lab VM is gone.
func TestParsePlainOnEveryProvider(t *testing.T) {
	for _, tc := range []struct{ name, text, provider string }{
		{"libvirt", "mesh_ip = \"10.1.0.1\"\n[libvirt]\nip = \"10.0.0.1\"\n", "libvirt"},
		{"linode", "mesh_ip = \"10.1.0.1\"\n[dns]\nzone = \"z\"\nname = \"n\"\n[linode]\nregion = \"us-sea\"\ntype = \"g6-nanode-1\"\n", "linode"},
		{"baremetal", "mesh_ip = \"10.1.0.1\"\n[baremetal]\nip = \"10.0.0.1\"\n", "baremetal"},
	} {
		h, err := Parse("h", []byte(tc.text))
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if h.Provider != tc.provider || h.IsCluster() || h.Datastore() != "" || h.AuditLogging() {
			t.Errorf("%s: %+v", tc.name, h)
		}
	}
	// The provider table still decides the backend and the runtime: a
	// plain linode is realized, a plain machine on the LAN is not.
	cloud, _ := Parse("h", []byte("mesh_ip = \"10.1.0.1\"\n[linode]\nregion = \"us-sea\"\ntype = \"g6-nanode-1\"\n"))
	metal, _ := Parse("h", []byte("mesh_ip = \"10.1.0.1\"\n[baremetal]\nip = \"10.0.0.1\"\n"))
	if !cloud.Cloud() || !cloud.Realized() || metal.Cloud() || metal.Realized() || metal.LanIP() != "10.0.0.1" {
		t.Errorf("cloud %+v metal %+v", cloud, metal)
	}
}

func TestLoadAll(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, Dir), 0o755)
	os.WriteFile(filepath.Join(root, Path("a")), []byte(lab), 0o644)
	os.WriteFile(filepath.Join(root, Path("b")), []byte(cloud), 0o644)
	os.WriteFile(filepath.Join(root, Path("bad")), []byte("nope = true\n"), 0o644)
	// A file whose name is not a host name is a problem, whatever it
	// holds: the name is a DNS label everywhere it lands (#100).
	os.WriteFile(filepath.Join(root, Path("Not_A_Host")), []byte(lab), 0o644)
	os.WriteFile(filepath.Join(root, Dir, "README.md"), []byte("not a manifest"), 0o644)
	hosts, problems, err := LoadAll(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 2 || len(problems) != 2 || problems["bad"] == nil || problems["Not_A_Host"] == nil || !strings.Contains(problems["Not_A_Host"].Error(), "not a host name") {
		t.Errorf("hosts %v problems %v", hosts, problems)
	}
	names, _ := Names(root)
	if strings.Join(names, ",") != "Not_A_Host,a,b,bad" {
		t.Errorf("names: %v", names)
	}
	if _, err := Load(root, "missing"); err == nil || !strings.Contains(err.Error(), "has no manifest") {
		t.Errorf("missing: %v", err)
	}
	for name, want := range map[string]bool{"a": true, "sfi-prod-439bf72": true, "h0st": true, "-a": false, "a-": false, "A": false, "a_b": false, "a.b": false, "": false, strings.Repeat("a", 64): false} {
		if got := ValidHostName(name); got != want {
			t.Errorf("ValidHostName(%q) = %v", name, got)
		}
	}
}

// A plain fort: no [cluster] table, a lab VM — the baseline
// host and nothing the framework names beyond it.
func TestPlainFort(t *testing.T) {
	h, err := Parse("plain-fort", []byte("mesh_ip = \"10.1.0.96\"\n[libvirt]\nip = \"10.0.0.96\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if h.IsCluster() || h.Datastore() != "" {
		t.Errorf("want a plain fort: cluster=%v datastore %q", h.IsCluster(), h.Datastore())
	}
	cluster, err := Parse("lab", []byte("mesh_ip = \"10.1.0.96\"\n[cluster]\n[libvirt]\nip = \"10.0.0.96\"\n"))
	if err != nil || !cluster.IsCluster() {
		t.Errorf("a fort with [cluster] is a cluster: %v", err)
	}
}

// [links.<name>] (forts#55): a hub on another estate's mesh. Named by
// the estate's last label, every fact well-formed, nothing shared
// between two links, and the public key is a real one.
func TestLinks(t *testing.T) {
	const pub = "9n6EdeMmYeyeiDQRIotqWqwDkNUgqMyBZIkHQj1cMUI="
	link := "\n[links.staging]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/staging\"\naddress = \"10.2.0.254\"\nport = 51821\ninterface = \"wg-staging\"\nzone = \"staging.mesh\"\npublic_key = \"" + pub + "\"\n"
	h, err := Parse("hub", []byte(lab+link))
	if err != nil {
		t.Fatal(err)
	}
	l, ok := h.LinkTo("https://gitea.zen.lofi/sfi/staging.git")
	if !ok || l.Name != "staging" || l.Address != "10.2.0.254" || l.Port != 51821 || l.PublicKey != pub || l.Ref() != "sfi/staging" {
		t.Errorf("link: %+v %v", l, ok)
	}
	if _, ok := h.LinkTo("ssh://git@gitea.zen.lofi:30022/sfi/production"); ok {
		t.Error("LinkTo found a link that is not there")
	}
	if names := h.LinkNames(); len(names) != 1 || names[0] != "staging" {
		t.Errorf("names: %v", names)
	}
	// The defaults modules/links.nix gives: wg-<name>, <name>.mesh.
	bare := "\n[links.staging]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/staging\"\naddress = \"10.2.0.254\"\nport = 51821\npublic_key = \"" + pub + "\"\n"
	if h, err := Parse("hub", []byte(lab+bare)); err != nil || h.Links["staging"].InterfaceName() != "wg-staging" || h.Links["staging"].ZoneName() != "staging.mesh" {
		t.Errorf("defaults: %v %+v", err, h.Links["staging"])
	}
	second := "\n[links.production]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/production\"\naddress = \"10.3.0.254\"\nport = 51822\npublic_key = \"" + pub + "\"\n"
	if _, err := Parse("hub", []byte(lab+link+second)); err != nil {
		t.Errorf("two links: %v", err)
	}
	for name, tc := range map[string]struct{ text, want string }{
		"wrong name":   {strings.Replace(link, "[links.staging]", "[links.stage]", 1), "last label is staging"},
		"bad estate":   {strings.Replace(link, "ssh://git@gitea.zen.lofi:30022/sfi/staging", "staging", 1), "estate"},
		"bad address":  {strings.Replace(link, "10.2.0.254", "nope", 1), "address"},
		"mesh port":    {strings.Replace(link, "51821", "51820", 1), "own mesh port"},
		"bad port":     {strings.Replace(link, "51821", "70000", 1), "port"},
		"bad iface":    {strings.Replace(link, "wg-staging", "wg staging", 1), "interface"},
		"long iface":   {strings.Replace(link, "wg-staging", "wg-staging-estate-x", 1), "interface"},
		"bad zone":     {strings.Replace(link, "staging.mesh", "Staging.Mesh", 1), "zone"},
		"bad key":      {strings.Replace(link, pub, "not-a-key", 1), "public_key"},
		"short key":    {strings.Replace(link, pub, "c3RhZ2luZw==", 1), "public_key"},
		"no key":       {strings.Replace(link, "public_key = \""+pub+"\"\n", "", 1), "public_key"},
		"shared port":  {link + strings.Replace(second, "51822", "51821", 1), "also [links.production]'s"},
		"shared iface": {link + strings.Replace(second, "port = 51822", "port = 51822\ninterface = \"wg-staging\"", 1), "also [links.production]'s"},
		"shared zone":  {link + strings.Replace(second, "port = 51822", "port = 51822\nzone = \"staging.mesh\"", 1), "also [links.production]'s"},
		"unknown key":  {strings.Replace(link, "port = 51821", "port = 51821\nroles = [\"forge\"]", 1), "unknown key"},
	} {
		if _, err := Parse("hub", []byte(lab+tc.text)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
}
