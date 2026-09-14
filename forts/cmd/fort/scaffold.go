package main

import (
	"bytes"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"gitea.zen.lofi/sfi/deployments/forts/internal/inventory"
)

//go:embed templates/scaffold
var scaffoldTemplates embed.FS

// hostView is the template context for one host's generated files —
// inventory facts plus the create-nixos defaults, so the two generators
// agree while they coexist.
type hostView struct {
	Hostname     string
	IP           string
	Production   bool
	FluxCluster  bool
	HasWg        bool
	WgVpnIP      string
	LanPrefix    int
	LanGateway   string
	LanDNS       []string
	StateVersion string
	ClusterInit  bool
	// Cloud hosts (inventory `provider`): the linode-runtime shape —
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

// scaffold renders a host's generated surface from its inventory entry:
// the nix host dir (default.nix in the lab or cloud shape, secrets.nix)
// and, for a flux cluster, fleet/clusters/<host>/ + the apps root
// kustomization — the same files create-nixos's cluster-init lays down
// for lab VMs (its force:no templates then find them present). Pure
// generator with force-no semantics per file: inventory seeds the host
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
	entry, err := inventory.LoadHost(filepath.Join(root, "provisioning/inventory.yaml"), host)
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
	switch {
	case entry.Cloud():
		fmt.Printf("then PR → merge → nix build ./nixos#%s-linode → inject-host-key.sh → fort provision %s --image <path>\n", host, host)
	case entry.Provider == "libvirt":
		fmt.Printf("then PR → merge → nix build ./nixos#%s-qcow → inject-host-key.sh → fort provision %s --image <path>\n", host, host)
	default:
		fmt.Printf("then PR → merge → just create-nixos %s\n", host)
	}
	return nil
}

func scaffoldShape(e inventory.Host) string {
	s := "lab VM"
	if e.Cloud() {
		s = e.Provider + " " + e.Region + " " + e.InstanceType
	}
	if e.FluxCluster {
		s += ", flux cluster"
	}
	if e.Production {
		s += ", production profile"
	}
	return s
}

// renderScaffold is the pure half: inventory entry → files.
func renderScaffold(host string, entry inventory.Host) ([]scaffoldFile, error) {
	view := hostView{
		Hostname:     host,
		IP:           entry.AnsibleHost,
		Production:   entry.Production,
		FluxCluster:  entry.FluxCluster,
		HasWg:        entry.WgVpnIP != "",
		WgVpnIP:      entry.WgVpnIP,
		LanPrefix:    24,
		LanGateway:   "10.0.0.1",
		LanDNS:       []string{"10.0.0.2", "8.8.8.8"},
		StateVersion: "25.11",
		ClusterInit:  true,
		Cloud:        entry.Cloud(),
		Provider:     entry.Provider,
		Region:       entry.Region,
		InstanceType: entry.InstanceType,
	}
	if entry.LanPrefix != nil {
		view.LanPrefix = *entry.LanPrefix
	}
	if entry.LanGateway != "" {
		view.LanGateway = entry.LanGateway
	}
	if len(entry.LanDNS) > 0 {
		view.LanDNS = entry.LanDNS
	}
	if entry.StateVersion != "" {
		view.StateVersion = entry.StateVersion
	}
	if entry.ClusterInit != nil {
		view.ClusterInit = *entry.ClusterInit
	}
	if view.FluxCluster && !view.HasWg {
		// cluster-vars.yaml's MESH_IP is mandatory (#99): every flux
		// cluster is a mesh member — a production host without wg_vpn_ip
		// has no login path at all.
		return nil, fmt.Errorf("%s: flux_cluster without wg_vpn_ip — every flux cluster is a mesh member", host)
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
	if view.FluxCluster {
		clusters := filepath.Join("fleet/clusters", host)
		plan = append(plan,
			struct{ path, tpl, note string }{filepath.Join(clusters, "flux-system/flux-operator.yaml"), "flux-operator.yaml.gotpl", ""},
			struct{ path, tpl, note string }{filepath.Join(clusters, "flux-system/flux-instance.yaml"), "flux-instance.yaml.gotpl", "sync path fleet/clusters/" + host},
			struct{ path, tpl, note string }{filepath.Join(clusters, "flux-system/kustomization.yaml"), "flux-system-kustomization.yaml.gotpl", ""},
			struct{ path, tpl, note string }{filepath.Join(clusters, "cluster-vars.yaml"), "cluster-vars.yaml.gotpl", "CLUSTER_NAME " + host + ", MESH_IP " + view.WgVpnIP},
			struct{ path, tpl, note string }{filepath.Join(clusters, "infrastructure.yaml"), "infrastructure.yaml.gotpl", "infra-controllers + infrastructure (shared layer)"},
			struct{ path, tpl, note string }{filepath.Join(clusters, "apps.yaml"), "apps.yaml.gotpl", "→ fleet/apps/" + host},
			struct{ path, tpl, note string }{filepath.Join("fleet/apps", host, "kustomization.yaml"), "apps-kustomization.yaml.gotpl", "root list: sops-canary (listed, absent until fort mint)"},
		)
	}
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
