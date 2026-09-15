package main

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
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
	ClusterInit  bool
	// Cloud hosts (a cloud provider table): the linode-runtime shape —
	// no LAN block, split DNS over the mesh, zram, Lish break-glass.
	Cloud        bool
	Provider     string
	Region       string
	InstanceType string
}

// scaffoldFile is one rendered file: repo-relative path + content.
type scaffoldFile struct {
	path    string
	content []byte
	note    string
}

// scaffold renders a host's generated surface from its manifest:
// the nix host dir (default.nix in the lab or cloud shape, secrets.nix)
// and, for a flux cluster, fleet/clusters/<host>/ + the apps root
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
	files, err := renderScaffold(host, entry)
	if err != nil {
		return err
	}

	fmt.Printf("%s   (%s)\n", host, scaffoldShape(entry))
	written := 0
	for _, f := range files {
		mark, status := '●', f.note
		if exists(root, f.path) {
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
	fmt.Printf("\n%d file(s) written. next: fort mint %s --write (keys, sops rules, deploy key, canary),\n", written, host)
	if entry.Cloud() {
		fmt.Printf("then PR → merge → fort image %s → fort provision %s --image <path>\n", host, host)
	} else {
		fmt.Printf("then PR → merge → fort image %s → fort provision %s --image <path>\n", host, host)
	}
	return nil
}

func scaffoldShape(e manifest.Host) string {
	s := "lab VM"
	if e.Cloud() {
		s = e.Provider
		if l := e.Linode(); l != nil {
			s += " " + l.Region + " " + l.Type
		}
	}
	return s
}

// renderScaffold is the pure half: manifest → files.
func renderScaffold(host string, entry manifest.Host) ([]scaffoldFile, error) {
	view := hostView{
		Hostname:     host,
		MeshIP:       entry.MeshIP,
		LanPrefix:    24,
		LanGateway:   "10.0.0.1",
		LanDNS:       []string{"10.0.0.2", "8.8.8.8"},
		StateVersion: "25.11",
		ClusterInit:  true,
		Cloud:        entry.Cloud(),
		Provider:     entry.Provider,
	}
	if l := entry.Libvirt(); l != nil {
		view.IP = l.IP
		if l.LanPrefix != 0 {
			view.LanPrefix = l.LanPrefix
		}
		if l.LanGateway != "" {
			view.LanGateway = l.LanGateway
		}
		if len(l.LanDNS) > 0 {
			view.LanDNS = l.LanDNS
		}
	}
	if l := entry.Linode(); l != nil {
		view.Region, view.InstanceType = l.Region, l.Type
	}
	if entry.StateVersion != "" {
		view.StateVersion = entry.StateVersion
	}
	if entry.ClusterInit != nil {
		view.ClusterInit = *entry.ClusterInit
	}
	if view.MeshIP == "" {
		// cluster-vars.yaml's MESH_IP is mandatory (#99): every fort is a
		// mesh member — a hardened host without mesh_ip has no login path
		// at all. (manifest.Validate says so first; this covers hosts built
		// in memory.)
		return nil, fmt.Errorf("%s: mesh_ip is required — every fort is a mesh member", host)
	}

	tpl, err := template.ParseFS(scaffoldTemplates, "templates/scaffold/*.gotpl", "templates/scaffold/fleet/*.gotpl")
	if err != nil {
		return nil, err
	}
	render := func(name string) ([]byte, error) {
		var b bytes.Buffer
		err := tpl.ExecuteTemplate(&b, name, view)
		return b.Bytes(), err
	}
	hostTpl, hostNote := "host-default.nix.gotpl", "lab VM shape (qcow runtime, static LAN)"
	if view.Cloud {
		hostTpl, hostNote = "host-cloud.nix.gotpl", "cloud shape (linode runtime, split DNS over the mesh, zram)"
	}
	hostDir := filepath.Join("nixos/hosts", host)
	plan := []struct{ path, tpl, note string }{
		{filepath.Join(hostDir, "default.nix"), hostTpl, hostNote},
		{filepath.Join(hostDir, "secrets.nix"), "secrets.nix.gotpl", "sops-nix wiring (host key → age)"},
	}
	clusters := filepath.Join("fleet/clusters", host)
	plan = append(plan,
		struct{ path, tpl, note string }{filepath.Join(clusters, "flux-system/flux-operator.yaml"), "flux-operator.yaml.gotpl", ""},
		struct{ path, tpl, note string }{filepath.Join(clusters, "flux-system/flux-instance.yaml"), "flux-instance.yaml.gotpl", "sync path fleet/clusters/" + host},
		struct{ path, tpl, note string }{filepath.Join(clusters, "flux-system/kustomization.yaml"), "flux-system-kustomization.yaml.gotpl", ""},
		struct{ path, tpl, note string }{filepath.Join(clusters, "cluster-vars.yaml"), "cluster-vars.yaml.gotpl", "CLUSTER_NAME " + host + ", MESH_IP " + view.MeshIP},
		struct{ path, tpl, note string }{filepath.Join(clusters, "infrastructure.yaml"), "infrastructure.yaml.gotpl", "infra-controllers + infrastructure (shared layer)"},
		struct{ path, tpl, note string }{filepath.Join(clusters, "apps.yaml"), "apps.yaml.gotpl", "→ fleet/apps/" + host},
		struct{ path, tpl, note string }{filepath.Join("fleet/apps", host, "kustomization.yaml"), "apps-kustomization.yaml.gotpl", "root list: sops-canary (listed, absent until fort mint)"},
	)
	var out []scaffoldFile
	for _, p := range plan {
		b, err := render(p.tpl)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.tpl, err)
		}
		out = append(out, scaffoldFile{p.path, b, p.note})
	}
	return out, nil
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
			return "", fmt.Errorf("not inside the deployments repo")
		}
		dir = parent
	}
}
