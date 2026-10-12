package main

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

//go:embed templates/scaffold
var scaffoldTemplates embed.FS

// hostView is the template context for one host's generated files —
// manifest facts plus the lab defaults the create-nixos playbook used, so
// hosts scaffolded before and after it agree.
type hostView struct {
	Hostname     string
	IP           string
	MeshIP       string
	LanPrefix    int
	LanGateway   string
	LanDNS       []string
	StateVersion string
	// Datastore is [cluster] datastore (#426), resolved: the host sets
	// sfi.k3s.datastore, and modules/k3s-server.nix maps it to
	// services.k3s.clusterInit. Always rendered on a cluster host — the
	// datastore is a positive declaration, with no silent default in
	// the host file.
	Datastore string
	// PV is the manifest's [[backups.pv]] list: one restic target per
	// store, rendered as sfi.pv-backup.targets. Empty when the manifest
	// declares no pv block: the host then imports no pv-backup module
	// and declares nothing, rather than importing it disabled — a host
	// that is not an origin should read as one. The db blocks reach the
	// cluster tree through `fort add`, never the host file.
	PV []manifest.PVBackup
	// AuditLogging is [cluster] audit_logging (#626): the host sets
	// sfi.k3s.auditLog.enable. False renders nothing — a fort that does
	// not audit carries no block, as with an empty pv list above.
	AuditLogging bool
	// Cloud hosts (a cloud provider table): the linode-runtime shape —
	// no LAN block, zram, Lish break-glass.
	Cloud        bool
	Provider     string
	Region       string
	InstanceType string
	// Baremetal hosts ([baremetal]): the lab shape on a physical machine
	// — baremetal-runtime instead of qcow-runtime, the LAN block matched
	// by MAC when the manifest names one (#287).
	Baremetal bool
	MAC       string
	// RepoURL: the estate's git URL (estate.toml), the FluxInstance's
	// source and auto-rebuild's pull.
	RepoURL string
	// MeshInterface: the estate's mesh interface (estate.toml `interface`,
	// else wg0).
	MeshInterface string
	// Zone: the estate's mesh zone (estate.toml `zone`, else <name>.mesh;
	// forts#45): what every host on the mesh is named under.
	Zone string
	// Estate: the estate's name (estate.toml `name`): cluster-vars
	// ESTATE_NAME, the identity label beside cluster on every metric and
	// log line the fort emits, and the static label on what the hub
	// pulls from a plain fort (forts#50).
	Estate string
	// HubMeshIP: where the edge stack pushes (cluster-vars HUB_MESH_IP):
	// the monitoring hub's address on this mesh (hubMeshIP). "" with no
	// monitoring hub, and the layer init rendered then carries no push
	// targets.
	HubMeshIP string
	// HubHost is the monitoring hub fort's name (estate.toml
	// [hubs.monitoring] fort): the directory a fort's file renders into,
	// flux/hub/<HubHost>/. HubLocal says the hub is a fort of this
	// estate, so the subscription to that directory is this estate's own.
	HubHost  string
	HubLocal bool
	// Hub: this host IS the hub (clustermon.go), so its edge Alertmanager
	// is reached by cluster DNS rather than over the mesh.
	Hub bool
	// IsHub: this host subscribes to some estate's flux/hub/<host>/ — its
	// own estate's (Hub) or one a [[serves.monitoring]] entry names it
	// for — so the apps root gains serves/, one subscription per estate
	// (Serves).
	IsHub  bool
	Serves []serveView
	// Plain: a fort with no [cluster] table — the baseline host shape
	// (hardened, the mesh, deployer + the ops verbs, the shell; no
	// k3s/flux/backups) and no fleet surface at all.
	Plain bool
	// Links is the manifest's [links.<name>] tables (forts#55): one
	// sfi.links.<name> block per estate this host is on the mesh of,
	// and the links module imported. Empty on a host that links
	// nowhere, which renders nothing — most forts.
	Links []linkView
}

// linkView is one [links.<name>] table as links.nix.gotpl renders it:
// the four facts the module needs at build time (repo, interface, zone,
// port, address) and the names a comment reads. roles and the secret's
// name default in modules/links.nix.
type linkView struct {
	Name, Ref, Repo, Interface, Zone, Address string
	Port                                      int
}

// linkViews is the manifest's links in name order, for the templates.
func linkViews(entry manifest.Host) []linkView {
	var out []linkView
	for _, name := range entry.LinkNames() {
		l := entry.Links[name]
		out = append(out, linkView{Name: name, Ref: l.Ref(), Repo: estate.WithGit(l.Estate), Interface: l.InterfaceName(), Zone: l.ZoneName(), Address: l.Address, Port: l.Port})
	}
	return out
}

// linksImport is the import line a host with links carries, as the
// host templates spell it (nixEnsureImport matches on the module alone).
const linksImport = "inputs.forts.nixosModules.links       # on the mesh of another estate ([links.<name>] in the manifest)"

// renderLinksBlock is the sfi.links.<name> block(s) for the host's
// links, as the host templates render them — what `fort link add`
// appends to a host file that exists, and scaffold's re-run too
// (ensureLinks), so a fresh host and a hub that gained a link later
// carry the same text.
func renderLinksBlock(entry manifest.Host) (string, error) {
	tpl, err := template.ParseFS(scaffoldTemplates, "templates/scaffold/links.nix.gotpl")
	if err != nil {
		return "", err
	}
	var b bytes.Buffer
	if err := tpl.ExecuteTemplate(&b, "links.nix.gotpl", hostView{Links: linkViews(entry)}); err != nil {
		return "", err
	}
	return strings.Trim(b.String(), "\n"), nil
}

// ensureLinks is the text edit that brings an existing host file up to
// its manifest's links: the module imported, and one block per link
// the file does not set yet. Each block is rendered alone, so a hub
// with one link in the file and a new one in the manifest gains only
// the new one, under the one marker the mirror test looks for
// (sfi.links.<name>).
func ensureLinks(text string, entry manifest.Host) (string, error) {
	if len(entry.Links) == 0 {
		return text, nil
	}
	out, err := nixEnsureImport(text, linksImport)
	if err != nil {
		return "", err
	}
	for _, name := range entry.LinkNames() {
		one := entry
		one.Links = map[string]manifest.Link{name: entry.Links[name]}
		block, err := renderLinksBlock(one)
		if err != nil {
			return "", err
		}
		if out, err = nixEnsureBlock(out, "sfi.links."+name, block); err != nil {
			return "", err
		}
	}
	return out, nil
}

// serveView is one subscription the hub fort's tree carries (serve.yaml
// .gotpl): the estate it serves the monitoring role, its repo, and
// whether that estate is this one (Local: the source is flux-system's).
type serveView struct {
	Hub, Name, Estate, Repo string
	Local                   bool
}

// scaffoldFile is one rendered file: repo-relative path + content — or,
// when edit is set, an append to a file that already exists (the hub
// directory's kustomization gaining the fort's line). Appends are
// text-level so comments survive, and idempotent: a line already there
// is kept, never doubled. An edit whose file does not exist yet falls
// back to content, the whole file as the first fort renders it.
type scaffoldFile struct {
	path    string
	content []byte
	note    string
	edit    func(string) (string, error)
	// editNote, when set, is what the report says when edit changed an
	// existing file — what was added, not the fresh render's note.
	editNote string
}

// scaffold renders a host's generated surface from its manifest:
// the nix host dir (default.nix in the lab or cloud shape, secrets.nix)
// and, for a flux cluster, flux/clusters/<host>/ + the apps root
// kustomization — the files the playbook-era cluster-init laid down for
// lab VMs, one generator for lab and cloud now. Pure
// generator with force-no semantics per file: the manifest seeds the host
// once, hand refinements live in the files and are never clobbered; a
// re-run fills gaps only. The identity half (keys, sops rules, deploy
// key, canary) is `fort mint`, run after this.
func scaffold(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: fort scaffold <host> [--write]")
	}
	host := args[0]
	write := false
	for _, a := range args[1:] {
		switch a {
		case "--write":
			write = true
		default:
			return fmt.Errorf("usage: fort scaffold <host> [--write]")
		}
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	entry, err := manifest.Load(root, host)
	if err != nil {
		return err
	}
	est, err := estate.Load(root)
	if err != nil {
		return err
	}
	hubIP, err := hubMeshIP(root, est)
	if err != nil {
		return err
	}
	files, err := renderScaffold(host, entry, est, hubIP)
	if err != nil {
		return err
	}

	fmt.Printf("%s   (%s)\n", host, scaffoldShape(entry))
	written := 0
	for _, f := range files {
		mark, status := '●', f.note
		if f.edit != nil && (f.content == nil || exists(root, f.path)) {
			mark = '○'
			raw, err := os.ReadFile(filepath.Join(root, f.path))
			if err != nil {
				return err
			}
			out, err := f.edit(string(raw))
			if err != nil {
				return fmt.Errorf("%s: %w", f.path, err)
			}
			if out == string(raw) {
				status = "exists — kept (already listed)"
			} else if f.editNote != "" {
				status = f.editNote
			}
			if out != string(raw) && write {
				if err := os.WriteFile(filepath.Join(root, f.path), []byte(out), 0o644); err != nil {
					return err
				}
				written++
			}
		} else if exists(root, f.path) {
			mark, status = '○', "exists — kept (scaffolding never overwrites)"
		} else if write {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, f.path)), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(root, f.path), f.content, 0o644); err != nil {
				return err
			}
			written++
		}
		fmt.Printf("%c %-44s %s\n", mark, f.path, status)
	}
	if !write {
		fmt.Println("\ndry run — nothing written (scaffold --write).")
		return nil
	}
	if !entry.IsCluster() {
		fmt.Printf("\n%d file(s) written. next: fort mint %s --write (host keys, sops rule, deploy key — no cluster identity),\n", written, host)
	} else {
		fmt.Printf("\n%d file(s) written. next: fort mint %s --write (keys, sops rules, deploy key, canary),\n", written, host)
	}
	if entry.Realized() {
		fmt.Printf("then PR → merge → fort image %s → fort provision %s --image <path>\n", host, host)
	} else {
		fmt.Printf("then PR → merge → fort image %s → write it to the machine by hand (docs/baremetal.md)\n", host)
	}
	return nil
}

func scaffoldShape(e manifest.Host) string {
	if !e.IsCluster() {
		return "plain fort: lab VM, no cluster"
	}
	s := "lab VM"
	if e.Baremetal() != nil {
		s = "lab hardware"
	}
	if e.Cloud() {
		s = e.Provider
		if l := e.Linode(); l != nil {
			s += " " + l.Region + " " + l.Type
		}
	}
	return s + ", k3s on " + e.Datastore()
}

// hubMeshIP is where this estate's edges push: the monitoring hub's
// address on THIS mesh (forts#38). A hub that is a fort of this estate
// has it in its manifest (mesh_ip); a hub in another estate is on this
// mesh through a link, and its address is its remotes entry in
// mesh/peers.json — the one place the fact is written, by the PR that
// linked it (docs/mesh.md). "" when there is no monitoring hub and
// nothing to push to.
func hubMeshIP(root string, est estate.Estate) (string, error) {
	h, ok := est.Hub(estate.RoleMonitoring)
	if !ok {
		return "", nil
	}
	if h.Local(est) {
		hub, err := manifest.Load(root, h.Fort)
		if err != nil {
			return "", fmt.Errorf("%s [hubs.monitoring] names %q as a fort of this estate: %w", estate.Path, h.Fort, err)
		}
		return hub.MeshIP, nil
	}
	m, err := loadMesh(root)
	if err != nil {
		return "", fmt.Errorf("%s [hubs.monitoring] names %s in %s, whose address on this mesh is its remotes entry: %w", estate.Path, h.Fort, h.Ref(), err)
	}
	r, ok := m.Remotes[h.Fort]
	if !ok {
		return "", fmt.Errorf("%s [hubs.monitoring] names %s in %s, and %s has no remotes.%s — the hub reaches this mesh through a link, and its address here is that entry (docs/mesh.md, \"Linking\"); add it first", estate.Path, h.Fort, h.Ref(), peersPath, h.Fort)
	}
	return r.IP, nil
}

// renderScaffold is the pure half: the manifests → files. hubIP is the
// monitoring hub's address on this mesh (hubMeshIP), "" for an estate
// with no monitoring hub.
func renderScaffold(host string, entry manifest.Host, est estate.Estate, hubIP string) ([]scaffoldFile, error) {
	hub, hasHub := est.Hub(estate.RoleMonitoring)
	view := hostView{
		RepoURL:       est.RepoGit(),
		MeshInterface: est.InterfaceName(),
		Zone:          est.ZoneName(),
		Estate:        est.Name,
		HubMeshIP:     hubIP,
		HubHost:       hub.Fort,
		HubLocal:      hasHub && hub.Local(est),
		Hostname:      host,
		MeshIP:        entry.MeshIP,
		LanPrefix:     24,
		LanGateway:    "10.0.0.1",
		LanDNS:        []string{"10.0.0.2", "8.8.8.8"},
		StateVersion:  "25.11",
		Datastore:     entry.Datastore(),
		Cloud:         entry.Cloud(),
		Provider:      entry.Provider,
		Hub:           hasHub && hub.Local(est) && host == hub.Fort,
		Plain:         !entry.IsCluster(),
		AuditLogging:  entry.AuditLogging(),
		Links:         linkViews(entry),
	}
	// The subscriptions this fort's tree carries: its own estate's forts
	// when it is this estate's monitoring hub, then every estate a
	// [[serves.monitoring]] entry names it the hub of.
	if view.Hub {
		view.Serves = append(view.Serves, serveView{Hub: host, Name: estate.ShortName(est.Repo), Estate: est.Ref(), Repo: est.RepoGit(), Local: true})
	}
	for _, s := range est.ServedBy(host, estate.RoleMonitoring) {
		view.Serves = append(view.Serves, serveView{Hub: host, Name: s.Name(), Estate: s.Ref(), Repo: estate.WithGit(s.Estate)})
	}
	view.IsHub = len(view.Serves) > 0 && !view.Plain
	view.PV = entry.Backups.PV
	// The lab LAN block, from whichever lab table the host carries.
	lan := func(ip string, prefix int, gw string, dns []string) {
		view.IP = ip
		if prefix != 0 {
			view.LanPrefix = prefix
		}
		if gw != "" {
			view.LanGateway = gw
		}
		if len(dns) > 0 {
			view.LanDNS = dns
		}
	}
	if l := entry.Libvirt(); l != nil {
		lan(l.IP, l.LanPrefix, l.LanGateway, l.LanDNS)
	}
	if b := entry.Baremetal(); b != nil {
		lan(b.IP, b.LanPrefix, b.LanGateway, b.LanDNS)
		view.Baremetal, view.MAC = true, b.MAC
	}
	if l := entry.Linode(); l != nil {
		view.Region, view.InstanceType = l.Region, l.Type
	}
	if entry.StateVersion != "" {
		view.StateVersion = entry.StateVersion
	}
	if view.MeshIP == "" {
		// cluster-vars.yaml's MESH_IP is mandatory (#99): every fort is a
		// mesh member — a hardened host without mesh_ip has no login path
		// at all. (manifest.Validate says so first; this covers hosts built
		// in memory.)
		return nil, fmt.Errorf("%s: mesh_ip is required — every fort is a mesh member", host)
	}

	tpl, err := template.ParseFS(scaffoldTemplates, "templates/scaffold/*.gotpl", "templates/scaffold/flux/*.gotpl")
	if err != nil {
		return nil, err
	}
	render := func(name string) ([]byte, error) {
		var b bytes.Buffer
		err := tpl.ExecuteTemplate(&b, name, view)
		return b.Bytes(), err
	}
	hostTpl, hostNote := "host-default.nix.gotpl", "lab VM shape (qcow runtime, static LAN)"
	if view.Baremetal {
		hostNote = "lab hardware shape (baremetal runtime, static LAN"
		if view.MAC != "" {
			hostNote += " by MAC"
		}
		hostNote += ")"
	}
	if view.Cloud {
		hostTpl, hostNote = "host-cloud.nix.gotpl", "cloud shape (linode runtime, no LAN, zram)"
	}
	if view.Plain {
		hostTpl, hostNote = "host-plain.nix.gotpl", "plain fort shape (qcow runtime, static LAN, deployer + the ops verbs; no k3s/flux/backups)"
	}
	hostDir := filepath.Join("nixos/hosts", host)
	plan := []struct{ path, tpl, note string }{
		{filepath.Join(hostDir, "default.nix"), hostTpl, hostNote},
		{filepath.Join(hostDir, "secrets.nix"), "secrets.nix.gotpl", "sops-nix wiring (host key → age)"},
	}
	if view.Plain {
		// No fleet surface: no cluster dir, no apps root, no canary home.
		// The hub still gets the fort's file: with no edge Prometheus the
		// hub PULLS node-exporter over the mesh and arms the switch, in
		// the same <host>-keyed place a cluster's lives. Anything else the
		// estate runs on the fort is the estate's to scrape.
		var out []scaffoldFile
		for _, p := range plan {
			b, err := render(p.tpl)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p.tpl, err)
			}
			out = append(out, withLinksEdit(scaffoldFile{path: p.path, content: b, note: p.note}, p.tpl, entry))
		}
		if !hasHub {
			return out, nil
		}
		b, err := render("host-monitoring.yaml.gotpl")
		if err != nil {
			return nil, fmt.Errorf("host-monitoring.yaml.gotpl: %w", err)
		}
		k, err := render("hub-kustomization.yaml.gotpl")
		if err != nil {
			return nil, fmt.Errorf("hub-kustomization.yaml.gotpl: %w", err)
		}
		return append(out, hubFiles(hub.Fort, host, b, plainMonitoringNote, k)...), nil
	}
	clusters := filepath.Join("flux/clusters", host)
	plan = append(plan,
		struct{ path, tpl, note string }{filepath.Join(clusters, "flux-system/flux-operator.yaml"), "flux-operator.yaml.gotpl", ""},
		struct{ path, tpl, note string }{filepath.Join(clusters, "flux-system/flux-instance.yaml"), "flux-instance.yaml.gotpl", "sync path flux/clusters/" + host},
		struct{ path, tpl, note string }{filepath.Join(clusters, "flux-system/kustomization.yaml"), "flux-system-kustomization.yaml.gotpl", ""},
		struct{ path, tpl, note string }{filepath.Join(clusters, "cluster-vars.yaml"), "cluster-vars.yaml.gotpl", "CLUSTER_NAME " + host + ", ESTATE_NAME " + view.Estate + ", MESH_IP " + view.MeshIP},
		struct{ path, tpl, note string }{filepath.Join(clusters, "infrastructure.yaml"), "infrastructure.yaml.gotpl", "infra-controllers + infrastructure (shared layer)"},
		struct{ path, tpl, note string }{filepath.Join(clusters, "apps.yaml"), "apps.yaml.gotpl", "→ flux/apps/" + host},
		struct{ path, tpl, note string }{filepath.Join("flux/apps", host, "kustomization.yaml"), "apps-kustomization.yaml.gotpl", "root list: sops-canary (listed, absent until fort mint)"},
	)
	var out []scaffoldFile
	for _, p := range plan {
		b, err := render(p.tpl)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.tpl, err)
		}
		f := withLinksEdit(scaffoldFile{path: p.path, content: b, note: p.note}, p.tpl, entry)
		if p.tpl == "cluster-vars.yaml.gotpl" {
			// A cluster scaffolded before ESTATE_NAME existed (forts#50)
			// gains the line: the shared layer references it, and flux's
			// strict envsubst fails the whole layer without it. The one
			// key scaffold adds to an existing cluster-vars; the rest of
			// the file stays the estate's (HUB_MESH_IP is a hand edit,
			// docs/init.md).
			f.edit = func(s string) (string, error) {
				return ensureClusterVar(s, "ESTATE_NAME", view.Estate)
			}
			f.editNote = "+ ESTATE_NAME " + view.Estate
		}
		if view.IsHub && p.tpl == "apps-kustomization.yaml.gotpl" {
			// A hub's apps root lists serves/; a root that already
			// exists (the hub scaffolded before it served anyone) gains
			// the line, never a rewrite.
			f.note += ", serves/ (the subscriptions)"
			f.edit = func(s string) (string, error) {
				if strings.Contains(s, "- "+servesDirName+"\n") || strings.HasSuffix(s, "- "+servesDirName) {
					return s, nil
				}
				return appendListItem(s, "resources", servesDirName)
			}
		}
		out = append(out, f)
	}
	// The hub side (forts#38): one subscription per estate this host
	// serves the monitoring role, its own included — the GitRepository
	// on the served repo, the scoped service account, the Kustomization
	// on flux/hub/<this host>/ — and the kustomization that lists them.
	if view.IsHub {
		var items []string
		for _, sv := range view.Serves {
			var b bytes.Buffer
			if err := tpl.ExecuteTemplate(&b, "serve.yaml.gotpl", sv); err != nil {
				return nil, fmt.Errorf("serve.yaml.gotpl: %w", err)
			}
			note := "subscription: " + hubDir(host) + "/ of " + sv.Estate
			if sv.Local {
				note += " (this estate, through the flux-system source)"
			} else {
				note += " (" + sv.Repo + ", with this cluster's deploy key)"
			}
			out = append(out, scaffoldFile{path: servesPath(host, sv.Estate), content: b.Bytes(), note: note})
			items = append(items, sv.Name+".yaml")
		}
		k, err := render("serves-kustomization.yaml.gotpl")
		if err != nil {
			return nil, fmt.Errorf("serves-kustomization.yaml.gotpl: %w", err)
		}
		out = append(out, scaffoldFile{path: servesKustomization(host), content: k, note: "the subscriptions, one line per estate", edit: func(s string) (string, error) {
			var err error
			for _, item := range items {
				if strings.Contains(s, item) {
					continue
				}
				if s, err = appendListItem(s, "resources", item); err != nil {
					return "", err
				}
			}
			return s, nil
		}})
	}
	// The cluster's monitoring for the hub (#288): everything the central
	// layer knows about this one cluster, in one file keyed by its name,
	// plus the line that makes the hub's flux apply it. No hub, no file.
	if !hasHub {
		return out, nil
	}
	b, err := render("cluster-monitoring.yaml.gotpl")
	if err != nil {
		return nil, fmt.Errorf("cluster-monitoring.yaml.gotpl: %w", err)
	}
	k, err := render("hub-kustomization.yaml.gotpl")
	if err != nil {
		return nil, fmt.Errorf("hub-kustomization.yaml.gotpl: %w", err)
	}
	note := clusterMonitoringNote
	if len(view.PV) > 0 {
		note += ", PVBackupNotConfigured"
	}
	return append(out, hubFiles(hub.Fort, host, b, note, k)...), nil
}

// withLinksEdit: a host file whose manifest declares links is appended
// to when it exists (the links module imported, one block per link not
// yet set) — a hub is an existing host when it gains a link, and
// scaffold never overwrites a host file. A fresh render carries the
// blocks already (links.nix.gotpl); a host with no links is untouched.
func withLinksEdit(f scaffoldFile, tpl string, entry manifest.Host) scaffoldFile {
	if !strings.HasPrefix(tpl, "host-") || len(entry.Links) == 0 {
		return f
	}
	f.edit = func(s string) (string, error) { return ensureLinks(s, entry) }
	f.editNote = "+ sfi.links." + strings.Join(entry.LinkNames(), ", sfi.links.") + " (from [links] in the manifest)"
	return f
}

// hubFiles is a fort's file for the hub plus the line that makes the
// hub's flux apply it (clustermon.go) — the same pair for a cluster and
// a plain fort, different content. The kustomization is appended to
// when it exists and created from kust (the whole file, this fort's
// line in it) when this is the first fort of the estate to render one.
func hubFiles(hub, host string, content []byte, note string, kust []byte) []scaffoldFile {
	item := host + ".yaml"
	return []scaffoldFile{
		{path: hubFilePath(hub, host), content: content, note: note},
		{path: hubKustomization(hub), content: kust, note: "+ " + item, edit: func(s string) (string, error) {
			if strings.Contains(s, item) {
				return s, nil
			}
			return appendListItem(s, "resources", item)
		}},
	}
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not inside a git checkout — run fort from an estate (docs/init.md)")
		}
		dir = parent
	}
}
