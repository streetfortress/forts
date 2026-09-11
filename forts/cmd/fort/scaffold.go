package main

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"text/template"

	"gitea.zen.lofi/sfi/deployments/forts/internal/inventory"
)

//go:embed templates/host-default.nix.gotpl
var hostTemplate string

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
}

func scaffold(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("scaffold needs a host name")
	}
	host := args[0]
	write := len(args) > 1 && args[1] == "--write"

	root, err := repoRoot()
	if err != nil {
		return err
	}
	entry, err := inventory.LoadHost(filepath.Join(root, "provisioning/inventory.yaml"), host)
	if err != nil {
		return err
	}

	// Defaults per create-nixos.yaml's default_vm_spec, so the two
	// generators agree while they coexist.
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

	tpl, err := template.New("host").Parse(hostTemplate)
	if err != nil {
		return err
	}

	if !write {
		return tpl.Execute(os.Stdout, view)
	}

	dest := filepath.Join(root, "nixos/hosts", host, "default.nix")
	if _, err := os.Stat(dest); err == nil {
		// create-nixos's force:no semantics: inventory seeds the host
		// once; hand refinements live in the file and are never clobbered.
		return fmt.Errorf("%s exists — scaffolding never overwrites (edit in place, or remove the file first)", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := tpl.Execute(f, view); err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", dest)
	fmt.Println("NOT yet ported from create-nixos (run it for these): host key, age recipient, sops rule + secrets, wg peer, flux cluster-init")
	return nil
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
