package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	"gitea.zen.lofi/sfi/forts/internal/estate"
)

// init is the verb that creates an estate (#385): the files a checkout
// needs before its first `fort scaffold` — what every verb assumes is
// there. estate.toml (the estate's name, git URL and hubs), the estate
// flake (sfi/forts as inputs.forts, hosts auto-discovered), the two sops
// rule files with the admin recipients, the empty mesh, the Pulumi
// project file, the shared flux infrastructure layer, the gate workflow,
// and the directories the verbs write into. A pure render, like
// scaffold: no secrets, no network, nothing overwritten. What init cannot
// write it names: the lab CA, the edge Alertmanager's webhook, the fleet
// secrets — the estate's, and the report says where each goes.
//
//go:embed all:templates/init
var initTemplates embed.FS

// initView is what the templates see.
type initView struct {
	Name, Repo, RepoGit, Mesh, Interface, Zone, Forts, Registry string
	Admins                                                      []string
	// Hubs: the roles another fort serves this estate ([hubs.<role>],
	// forts#38), in role order.
	Hubs []hubView
	// Push: the edges have a monitoring hub to push to (a fort here or in
	// another estate), so the layer carries remote_write and fluent-bit.
	// Forge: a [hubs.forge] is declared (the template's example is
	// omitted then).
	Push, Forge bool
}

// hubView is one [hubs.<role>] table as the template renders it.
type hubView struct {
	Role, Estate, Fort, Name string
}

// hubsView orders an estate's hubs for the template: the roles in the
// order estate.Roles lists them.
func hubsView(e estate.Estate) []hubView {
	var out []hubView
	for _, role := range estate.Roles {
		if h, ok := e.Hub(role); ok {
			out = append(out, hubView{Role: role, Estate: h.Estate, Fort: h.Fort, Name: h.Name})
		}
	}
	return out
}

// parseHubFlag reads one --hub value, <role>=<fort>@<estate git url>
// (monitoring=luxor@ssh://git@gitea.zen.lofi:30022/sfi/infra): the
// role, the hub fort, and the hub's estate by its URL — the one
// spelling of an estate estate.toml uses everywhere, this estate's own
// for an in-estate hub. The fort comes first because the URL carries an
// @ of its own. The forge's name is --forge-name.
func parseHubFlag(v string) (role string, h estate.Hub, err error) {
	role, rest, ok := strings.Cut(v, "=")
	if !ok {
		return "", h, fmt.Errorf("--hub %q: want <role>=<fort>@<estate git url> (monitoring=luxor@ssh://git@gitea.zen.lofi:30022/sfi/infra)", v)
	}
	fort, repo, ok := strings.Cut(rest, "@")
	if !ok || fort == "" || repo == "" {
		return "", h, fmt.Errorf("--hub %q: want <role>=<fort>@<estate git url> (monitoring=luxor@ssh://git@gitea.zen.lofi:30022/sfi/infra)", v)
	}
	return role, estate.Hub{Estate: repo, Fort: fort}, nil
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

const defaultForts = "git+https://gitea.zen.lofi/sfi/forts"

func initCmd(args []string) error {
	fl := flag.NewFlagSet("init", flag.ContinueOnError)
	name := fl.String("name", "", "the estate's name (the repo's)")
	repo := fl.String("repo", "", "the estate's git URL as flux and auto-rebuild clone it (ssh://git@<gitea>:<port>/<owner>/<name>)")
	var hubs multiFlag
	fl.Var(&hubs, "hub", "a role another fort serves this estate, <role>=<fort>@<estate git url> (monitoring=luxor@ssh://git@gitea.zen.lofi:30022/sfi/infra); repeat per role. The estate may be this one.")
	forgeName := fl.String("forge-name", "", "the forge's one name on every mesh (gitea.zen.lofi), with --hub forge=…")
	meshNet := fl.String("mesh", "", "the estate's WireGuard subnet, CIDR (10.2.0.0/24); fort check keeps every mesh address inside it")
	iface := fl.String("interface", "", "the WireGuard interface name every host gives this mesh (wg-second; at most 15 characters); default wg0")
	zone := fl.String("zone", "", "the DNS zone of the mesh: every host on it is <host>.<zone> (docs/mesh.md); default <name>"+estate.DefaultZoneSuffix)
	forts := fl.String("forts", defaultForts, "the framework's flake URL, inputs.forts of the estate flake")
	registry := fl.String("registry", "gitea.zen.lofi", "the chart registry the gate logs into")
	var admins multiFlag
	fl.Var(&admins, "admin", "an admin age recipient (age1…); repeat for each")
	write := fl.Bool("write", false, "write the files (default: print the tree)")
	fl.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: fort init --name <estate> --repo <git url> --admin <age1…> [--admin …] [--mesh <cidr>] [--interface <name>] [--zone <name>] [--hub <role>=<fort>@<estate git url> … [--forge-name <name>]] [--forts <flake url>] [--write]")
		fl.PrintDefaults()
	}
	if err := fl.Parse(args); err != nil {
		return err
	}
	if *name == "" || *repo == "" || len(admins) == 0 {
		fl.Usage()
		return fmt.Errorf("--name, --repo and at least one --admin are required")
	}
	for _, a := range admins {
		if !strings.HasPrefix(a, "age1") {
			return fmt.Errorf("--admin %q: want an age recipient (age1…)", a)
		}
	}
	est := estate.Estate{Name: *name, Repo: *repo, Mesh: *meshNet, Interface: *iface, Zone: *zone}
	for _, v := range hubs {
		role, h, err := parseHubFlag(v)
		if err != nil {
			return err
		}
		if est.Hubs == nil {
			est.Hubs = map[string]estate.Hub{}
		}
		if _, dup := est.Hubs[role]; dup {
			return fmt.Errorf("--hub %s: the role is given twice", role)
		}
		est.Hubs[role] = h
	}
	if *forgeName != "" {
		h, ok := est.Hubs[estate.RoleForge]
		if !ok {
			return fmt.Errorf("--forge-name names the forge, and no --hub forge=… declares one")
		}
		h.Name = *forgeName
		est.Hubs[estate.RoleForge] = h
	}
	if err := est.Validate(); err != nil {
		return err
	}
	root, err := repoRoot()
	if err != nil {
		return fmt.Errorf("fort init renders into the root of a git checkout — git init first (%w)", err)
	}
	if exists(root, estate.Path) {
		return fmt.Errorf("%s already has %s — this checkout is an estate; init creates one, it does not re-render", root, estate.Path)
	}
	files, err := renderInit(initView{Name: est.Name, Repo: est.Repo, RepoGit: est.RepoGit(), Hubs: hubsView(est), Mesh: est.Mesh, Interface: est.Interface, Zone: est.Zone, Push: est.Pushes(), Forge: est.Hubs[estate.RoleForge].Fort != "", Forts: *forts, Registry: *registry, Admins: admins})
	if err != nil {
		return err
	}

	fmt.Printf("%s   (estate: %s)\n", est.Name, root)
	written, infra := 0, 0
	for _, f := range files {
		if strings.HasPrefix(f.path, "flux/infrastructure/") {
			infra++
		}
		mark, status := '●', f.note
		if exists(root, f.path) {
			mark, status = '○', "exists — kept (init never overwrites)"
		} else if *write {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(root, f.path)), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(root, f.path), f.content, 0o644); err != nil {
				return err
			}
			written++
		}
		if strings.HasPrefix(f.path, "flux/infrastructure/") || filepath.Base(f.path) == ".gitkeep" {
			continue // summarized below
		}
		fmt.Printf("%c %-44s %s\n", mark, f.path, status)
	}
	fmt.Printf("● %-44s the shared flux layer: %d files (controllers + configs)\n", "flux/infrastructure/", infra)
	fmt.Printf("● %-44s forts, nixos/hosts, nixos/secrets, flux/apps, flux/clusters, provisioning/pulumi/state\n", "(the directories the verbs write into)")
	fmt.Println()
	fmt.Println("yours to provide (init cannot write them):")
	fmt.Printf("· %-44s the lab CA (lab-host.nix trusts it; cert-manager's issuer and fort add read the copy)\n", "nixos/certs/step-ca-root.pem")
	fmt.Printf("· %-44s the same file\n", "flux/infrastructure/configs/step-ca-root.pem")
	fmt.Printf("· %-44s the edge Alertmanager's webhook, sops-encrypted under the shared rule\n", "flux/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml")
	fmt.Printf("· %-44s the fleet secrets (provisioning/README.md lists the keys)\n", "provisioning/secrets.yaml")
	if !*write {
		fmt.Println("\ndry run — nothing written (init --write).")
		return nil
	}
	fmt.Printf("\n%d file(s) written. next: nix flake lock ./nixos; provide the files above; write forts/<host>.toml; fort scaffold <host> --write\n", written)
	return nil
}

// renderInit is the pure half: the view → every file under templates/init,
// a .gotpl rendered and the suffix dropped, anything else copied as is.
func renderInit(v initView) ([]scaffoldFile, error) {
	notes := map[string]string{
		"estate.toml":                     "the estate: name, repo, mesh, interface, zone, hubs",
		"README.md":                       "the estate's README: the layout, the first fort",
		".gitignore":                      "",
		".sops.yaml":                      "the flux rules: provisioning/, the shared infrastructure rule (+ fort-managed cluster recipients)",
		"nixos/.sops.yaml":                "the per-host rules: the catch-all; fort mint adds a block per fort",
		"nixos/flake.nix":                 "the estate flake: " + v.Forts + " as inputs.forts, hosts auto-discovered",
		"nixos/certs/README.md":           "where the lab CA goes",
		"mesh/peers.json":                 "empty; fort mint adds the first peer",
		"provisioning/README.md":          "what secrets.yaml holds",
		"provisioning/pulumi/Pulumi.yaml": "the project file: the program is fort-pulumi, beside fort",
		".gitea/workflows/checks.yaml":    "the gate: fort check --all + nix eval of every host",
	}
	var out []scaffoldFile
	err := fs.WalkDir(initTemplates, "templates/init", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := initTemplates.ReadFile(path)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, "templates/init/")
		if strings.HasSuffix(rel, ".gotpl") {
			rel = strings.TrimSuffix(rel, ".gotpl")
			tpl, err := template.New(rel).Parse(string(raw))
			if err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			var b bytes.Buffer
			if err := tpl.Execute(&b, v); err != nil {
				return fmt.Errorf("%s: %w", path, err)
			}
			raw = b.Bytes()
		}
		out = append(out, scaffoldFile{path: rel, content: raw, note: notes[rel]})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}
