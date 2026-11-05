package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"gitea.zen.lofi/sfi/forts/internal/contract"
	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/inventory"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gopkg.in/yaml.v3"
)

//go:embed templates/add
var addTemplates embed.FS

// add takes a chart's contract in and writes an inventory out
// (docs/reference/contracts.md, inventory.md): the app dir — the
// OCIRepository, the HelmRelease, the kustomization entry, the
// inventory of the Secret the chart consumes — and, when the contract
// or --http names a Service, the gateway listener, the certificate and
// the route. A pure generator — new files plus appended list entries,
// never a rewrite; nothing is applied. The default lays the tree down
// for a PR and prints what to seed; --dry-run prints the marked tree.
//
// The core renders what its init templates install: the gateway stack
// and the inventory. The platform services a contract `requires` (a
// database, a cache, a bucket) are a layer's to render after this
// (sfi/fort-layers, forts#250); add names them and renders none.
func add(args []string) error {
	if len(args) < 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return fmt.Errorf("usage: fort add <fort> <oci-ref> (--version X | --follow-pr N | --follow-main) [--name app] [--hostname fqdn] [--http service:port] [--chart-dir path] [--dry-run]")
	}
	o := addOpts{fort: args[0], ref: args[1]}
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.StringVar(&o.version, "version", "", "chart version to pin (a release tag)")
	fs.StringVar(&o.followPR, "follow-pr", "", "follow a PR's per-commit preview charts instead of a tag")
	fs.BoolVar(&o.followMain, "follow-main", false, "follow main's per-commit preview charts instead of a tag")
	fs.StringVar(&o.name, "name", "", "app name (namespace, release, dir); default: chart name")
	fs.StringVar(&o.hostname, "hostname", "", "public hostname; default <app>.<fort>.<zone> — the manifest's [dns] zone, else the estate's mesh zone (estate.toml)")
	fs.StringVar(&o.http, "http", "", "the Service the fleet routes to, <service>:<port> — for a chart that ships no contract")
	fs.StringVar(&o.chartDir, "chart-dir", "", "read the chart from a local dir instead of pulling (chart authors, tests)")
	dryRun := fs.Bool("dry-run", false, "print the plan and write nothing (default: write the tree and stage it)")
	fs.Bool("write", true, "deprecated no-op: writing is the default")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	o.write = !*dryRun
	pins := 0
	for _, set := range []bool{o.version != "", o.followPR != "", o.followMain} {
		if set {
			pins++
		}
	}
	if pins > 1 || (pins == 0 && o.chartDir == "") {
		return fmt.Errorf("pin the chart with exactly one of --version, --follow-pr or --follow-main")
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	o.root = root
	p, err := planAdd(o)
	if err != nil {
		return err
	}
	if o.write {
		if err := p.apply(); err != nil {
			return err
		}
		// Author verbs stage what they generate (mint and seed do): the
		// new dirs and the appended lists.
		if err := (sopsCLI{root: o.root}).GitAdd(filepath.Join("flux/apps", o.fort)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: git add flux/apps/%s: %v\n", o.fort, err)
		}
	}
	p.report(os.Stdout, o.write)
	return nil
}

type addOpts struct {
	root, fort, ref, version, followPR, name, hostname, http, chartDir string
	followMain                                                         bool
	write                                                              bool
}

type plan struct {
	root, fort, app string
	resolved        string
	actions         []action
	externals       []contract.Key
	// noContract: the chart ships no forts.contract.yaml, so this app
	// has no key gate and the report says so.
	noContract bool
	// http: the app has a gateway listener, a certificate and a route.
	http bool
	// required: the platform services the contract requires, which a
	// layer renders after this add; unresolved: the Secret keys that
	// compose their outputs, which the same layer rewrites.
	required   []string
	unresolved []contract.Key
	// noIssuer: the estate names no [acme] email, so the shared layer
	// carries no ClusterIssuer for the certificate to name.
	noIssuer bool
}

// view is the template context; conventions live here, not in flags.
type view struct {
	Fort, App, Hostname, Zone, Issuer, ACMEDirectory, SecretName string
	// NoContract: the chart ships no contract at all, so the generated
	// comments must not credit one.
	NoContract bool
	// HasSecret: the effective contract declares at least one key, so
	// the app consumes a Secret. False is the secret-less shape — no
	// secrets.sops.yaml, no inventory, no `existingSecret`
	// value the chart never reads.
	HasSecret bool
	// CA: the estate carries nixos/certs/ca-root.pem, so the
	// OCIRepository is handed it as certSecretRef (chart.go).
	CA                               bool
	ChartURL, ChartRepo, Version, PR string
	// Main: the fort follows the chart repo's main branch through its
	// per-commit prereleases, rather than a tag or a PR.
	Main          bool
	HTTP          *contract.HTTP
	Metrics       *contract.Metrics
	Component     string
	SelectorLines []string
	ValuesYAML    string
	Keys          []inventory.NamedSpec
	// Required: the services the contract requires, "type.handle".
	Required              []string
	GatewayPatchesComment string
}

const gatewayPatchesComment = `# One JSON6902 patch per service appends its https listener to the
# Gateway (the design's one exception to "new files only": listeners
# live in a single object, and a strategic-merge patch cannot append to
# a CRD list — kustomize has no merge key for it). fort add adds a file
# here and one entry below; gateway.yaml is never edited.
`

func planAdd(o addOpts) (*plan, error) {
	entry, err := manifest.Load(o.root, o.fort)
	if err != nil {
		return nil, err
	}
	// The estate: its mesh zone is the hostname's zone when the
	// manifest names no public one, and its ACME issuer signs the cert.
	est, err := estate.Load(o.root)
	if err != nil {
		return nil, err
	}

	chartDir, resolved := o.chartDir, o.version
	if chartDir == "" {
		dir, v, err := resolveChart(o.root, chartPin{url: o.ref, version: o.version, followPR: o.followPR, followMain: o.followMain})
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(filepath.Dir(dir))
		chartDir, resolved = dir, v
	} else if resolved == "" {
		m, err := loadChartMeta(chartDir)
		if err != nil {
			return nil, err
		}
		resolved = m.Version
	}
	meta, err := loadChartMeta(chartDir)
	if err != nil {
		return nil, err
	}
	// An absent contract is a supported shape: the chart declares
	// nothing, so add plans nothing from it. A contract that is there
	// and does not load is still an error — fix it in the chart.
	c, err := contract.Load(chartDir)
	if err != nil {
		return nil, fmt.Errorf("chart %s ships a %s that does not load (%v) — fix it in the chart repo", meta.Name, contract.FileName, err)
	}
	if o.http != "" {
		if c.HTTP != nil {
			return nil, fmt.Errorf("the chart's contract already routes to %s:%d — --http is for a chart that declares no http", c.HTTP.Service, c.HTTP.Port)
		}
		h, err := parseHTTPFlag(o.http)
		if err != nil {
			return nil, err
		}
		c.HTTP = h
	}

	app := o.name
	if app == "" {
		app = meta.Name
	}
	// oci://host/owner/name[:version] → url without the version, owner/name.
	repoPath := strings.SplitN(strings.TrimPrefix(o.ref, "oci://"), ":", 2)[0]
	v := view{
		Fort: o.fort, App: app, SecretName: app + "-env",
		NoContract: !c.Present,
		CA:         exists(o.root, estateCA),
		ChartURL:   "oci://" + repoPath,
		ChartRepo:  strings.SplitN(repoPath, "/", 2)[len(strings.SplitN(repoPath, "/", 2))-1],
		Version:    resolved, PR: o.followPR, Main: o.followMain,
		HTTP: c.HTTP, Metrics: c.Metrics,
		Issuer:        "letsencrypt",
		ACMEDirectory: est.ACMEDirectory(),
		Required:      c.Required(),

		GatewayPatchesComment: gatewayPatchesComment,
	}
	// The hostname's zone: the manifest's public zone where the fort has
	// one, else the estate's mesh zone, where <app>.<fort>.<zone> is a
	// name every host on the mesh resolves to the fort (mesh-dns.nix).
	v.Zone = entry.DNS.Zone
	if v.Zone == "" {
		v.Zone = est.ZoneName()
	}
	v.Hostname = o.hostname
	if v.Hostname == "" {
		v.Hostname = app + "." + o.fort + "." + v.Zone
	}
	if c.Metrics != nil {
		v.Component = "app"
		keys := make([]string, 0, len(c.Metrics.Selector))
		for k := range c.Metrics.Selector {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			v.SelectorLines = append(v.SelectorLines, k+": "+c.Metrics.Selector[k])
			if k == "app.kubernetes.io/component" {
				v.Component = c.Metrics.Selector[k]
			}
		}
	}

	p := &plan{root: o.root, fort: o.fort, app: app, resolved: resolved, noContract: !c.Present, http: c.HTTP != nil, required: v.Required, noIssuer: c.HTTP != nil && est.ACME.Email == ""}
	apps := filepath.Join("flux/apps", o.fort)
	appDir := filepath.Join(apps, app)
	if exists(o.root, appDir) {
		return nil, fmt.Errorf("%s exists — fort add never overwrites (remove the dir first, or refine in place)", appDir)
	}
	if !exists(o.root, filepath.Join(apps, "kustomization.yaml")) {
		return nil, fmt.Errorf("%s has no flux/apps tree (fort scaffold %s first)", o.fort, o.fort)
	}
	tpl, err := parseAddTemplates()
	if err != nil {
		return nil, err
	}
	render := func(name string) []byte {
		var b bytes.Buffer
		if err := tpl.ExecuteTemplate(&b, name, v); err != nil {
			panic(fmt.Sprintf("template %s: %v", name, err))
		}
		return b.Bytes()
	}
	static := func(name string) []byte {
		b, err := addTemplates.ReadFile("templates/add/" + name)
		if err != nil {
			panic(err)
		}
		return b
	}

	// --- the inventory: the chart's keys, as the fort seeds them.
	keys, externals, unresolved, err := inventoryFromChart(c, &overlay{})
	if err != nil {
		return nil, err
	}
	v.Keys, p.externals, p.unresolved = keys, externals, unresolved
	v.HasSecret = len(v.Keys) > 0
	if v.HasSecret {
		values := map[string]any{}
		setPath(values, c.Secret.Values, v.SecretName)
		var vb bytes.Buffer
		enc := yaml.NewEncoder(&vb)
		enc.SetIndent(2)
		if err := enc.Encode(values); err != nil {
			return nil, err
		}
		enc.Close()
		v.ValuesYAML = strings.TrimRight(indent(vb.String(), "    "), "\n")
	}

	// --- gateway: instance on first http need, listener + cert after.
	var rootAdds []string // gateway, then the app
	if c.HTTP != nil {
		gwDir := filepath.Join(apps, "gateway")
		p.add(action{path: filepath.Join(gwDir, "certificate-"+app+".yaml"), mark: '●', note: v.Issuer + ", " + v.Hostname, content: render("gateway/certificate.yaml.gotpl")})
		p.add(action{path: filepath.Join(gwDir, "listener-"+app+".yaml"), mark: '●', note: "JSON6902 add /spec/listeners/- (https-" + app + ")", content: render("gateway/listener.yaml.gotpl")})
		if exists(o.root, gwDir) {
			p.add(action{path: filepath.Join(gwDir, "kustomization.yaml"), mark: '○', note: "+ certificate-" + app + ".yaml; patches: + listener-" + app + ".yaml",
				edit: func(s string) (string, error) {
					s, err := appendListItem(s, "resources", "certificate-"+app+".yaml")
					if err != nil {
						return "", err
					}
					return appendPatch(s, gatewayPatchesComment, "listener-"+app+".yaml", "gateway.networking.k8s.io", "v1", "Gateway", "homelab", "traefik"), nil
				}})
		} else {
			p.add(action{path: filepath.Join(gwDir, "gateway.yaml"), mark: '●', note: "http listener only", content: render("gateway/gateway.yaml.gotpl")})
			p.add(action{path: filepath.Join(gwDir, "redirect-https.yaml"), mark: '●', note: "catch-all http→https 301", content: static("gateway/redirect-https.yaml")})
			p.add(action{path: filepath.Join(gwDir, "kustomization.yaml"), mark: '●', note: "patches: listener-" + app + ".yaml", content: render("gateway/kustomization.yaml.gotpl")})
			rootAdds = append(rootAdds, "gateway")
		}
	}

	// --- the app dir
	p.add(action{path: filepath.Join(appDir, "namespace.yaml"), mark: '●', content: render("app/namespace.yaml.gotpl")})
	pin := "tag " + resolved
	switch {
	case o.followPR != "":
		pin = "semver filter on 0.0.0-pr." + o.followPR + ".* (now " + resolved + ")"
	case o.followMain:
		pin = "semver filter on 0.0.0-main.* (now " + resolved + ")"
	}
	p.add(action{path: filepath.Join(appDir, "repository.yaml"), mark: '●', note: "OCIRepository " + v.ChartURL + ", " + pin, content: render("app/repository.yaml.gotpl")})
	if v.CA {
		ca, err := os.ReadFile(filepath.Join(o.root, estateCA))
		if err != nil {
			return nil, err
		}
		p.add(action{path: filepath.Join(appDir, "ca-root.pem"), mark: '●', note: "copy of " + estateCA, content: ca})
	}
	p.add(action{path: filepath.Join(appDir, "release.yaml"), mark: '●', note: releaseNote(v), content: render("app/release.yaml.gotpl")})
	if c.HTTP != nil {
		p.add(action{path: filepath.Join(appDir, "route.yaml"), mark: '●', note: fmt.Sprintf("HTTPRoute %s → %s:%d", v.Hostname, c.HTTP.Service, c.HTTP.Port), content: render("app/route.yaml.gotpl")})
	}
	if c.Metrics != nil {
		p.add(action{path: filepath.Join(appDir, "monitoring.yaml"), mark: '●', note: "PodMonitor " + app + "-" + v.Component + " (rules are policy: by hand)", content: render("app/monitoring.yaml.gotpl")})
	}
	p.add(action{path: filepath.Join(appDir, "kustomization.yaml"), mark: '●', content: render("app/kustomization.yaml.gotpl")})
	if v.HasSecret {
		p.add(action{path: filepath.Join(appDir, inventory.FileName), mark: '●', note: fmt.Sprintf("the inventory: %d keys and their sources", len(v.Keys)), content: render("app/forts.inventory.yaml.gotpl")})
		p.add(action{path: filepath.Join(appDir, "secrets.sops.yaml"), mark: '◆', note: "seeded"})
	}

	rootAdds = append(rootAdds, app)
	p.add(action{path: filepath.Join(apps, "kustomization.yaml"), mark: '○', note: "+ " + strings.Join(rootAdds, ", "),
		edit: func(s string) (string, error) {
			for _, r := range rootAdds {
				var err error
				if s, err = appendListItem(s, "resources", r); err != nil {
					return "", err
				}
			}
			return s, nil
		}})
	return p, nil
}

// releaseNote is the HelmRelease's line of the report: what the values
// carry, and the retry posture.
func releaseNote(v view) string {
	switch {
	case v.HasSecret:
		return "values: " + v.SecretName + " as the Secret; retry forever — refine replicas/env in place"
	default:
		return "values: none; retry forever — refine replicas/env in place"
	}
}

// parseHTTPFlag reads --http <service>:<port> — the ingress a chart
// that ships no contract cannot declare. The contract stays the source
// of truth wherever there is one: the flag only fills the gap, and
// planAdd refuses it against a contract that declares http.
func parseHTTPFlag(s string) (*contract.HTTP, error) {
	svc, port, ok := strings.Cut(s, ":")
	if !ok || svc == "" || port == "" {
		return nil, fmt.Errorf("--http %q: write it as <service>:<port>", s)
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("--http %q: %q is not a port number", s, port)
	}
	return &contract.HTTP{Service: svc, Port: n}, nil
}

func (p *plan) add(a action) { p.actions = append(p.actions, a) }

// apply writes the plan: new files refuse to overwrite, edits are read-
// modify-write on the existing text.
func (p *plan) apply() error {
	for _, a := range p.actions {
		full := filepath.Join(p.root, a.path)
		switch {
		case a.content != nil:
			if _, err := os.Stat(full); err == nil {
				return fmt.Errorf("%s exists — fort add never overwrites", a.path)
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(full, a.content, 0o644); err != nil {
				return err
			}
		case a.edit != nil:
			raw, err := os.ReadFile(full)
			if err != nil {
				return err
			}
			out, err := a.edit(string(raw))
			if err != nil {
				return fmt.Errorf("%s: %w", a.path, err)
			}
			if err := os.WriteFile(full, []byte(out), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

// report prints the marked tree (the convention every author verb
// prints in) grouped by directory, then what the human does next.
func (p *plan) report(w io.Writer, written bool) {
	base := filepath.Join("flux/apps", p.fort)
	fmt.Fprintf(w, "%s/   (chart %s)\n", base, p.resolved)
	sort.SliceStable(p.actions, func(i, j int) bool { return p.actions[i].path < p.actions[j].path })
	lastDir := ""
	for _, a := range p.actions {
		rel := strings.TrimPrefix(a.path, base+"/")
		dir, file, _ := strings.Cut(rel, "/")
		if file == "" {
			dir, file = "", rel
		}
		if dir != "" {
			if dir != lastDir {
				mark := '○'
				if p.creates(filepath.Join(base, dir)) {
					mark = '●'
				}
				fmt.Fprintf(w, "%c %s/\n", mark, dir)
				lastDir = dir
			}
			fmt.Fprintf(w, "  %c %-28s %s\n", a.mark, file, a.note)
		} else {
			fmt.Fprintf(w, "%c %-30s %s\n", a.mark, file, a.note)
		}
	}
	fmt.Fprintln(w)
	if p.noContract {
		fmt.Fprintf(w, "note: the chart ships no %s. This app gets no key gate: `fort check`\n", contract.FileName)
		fmt.Fprintln(w, "      cannot see a chart bump that starts to need a key. The chart adopts the")
		fmt.Fprintln(w, "      gate when it ships the file — docs/reference/contracts.md.")
		if !p.http {
			fmt.Fprintln(w, "note: nothing declares an http service, so this app has no listener, no")
			fmt.Fprintln(w, "      certificate and no route. It is reachable on the mesh and the LAN only.")
			fmt.Fprintln(w, "      Add ingress with `fort add … --http <service>:<port>`.")
		}
		fmt.Fprintln(w)
	}
	if len(p.required) > 0 || len(p.unresolved) > 0 {
		if len(p.required) > 0 {
			fmt.Fprintf(w, "note: the chart requires %s — a platform service the core does not render.\n", strings.Join(p.required, ", "))
			fmt.Fprintln(w, "      A layer renders it and wires it into release.yaml (sfi/fort-layers);")
			fmt.Fprintln(w, "      until then the release waits on it.")
		}
		if len(p.unresolved) > 0 {
			verb := "composes"
			if len(p.unresolved) > 1 {
				verb = "compose"
			}
			fmt.Fprintf(w, "note: %s %s provider outputs the core does not resolve. The\n", unresolvedNote(p.unresolved), verb)
			fmt.Fprintf(w, "      layer rewrites the key to a source in %s; `fort seed` waits for it.\n", inventory.FileName)
		}
		fmt.Fprintln(w)
	}
	if p.noIssuer {
		fmt.Fprintln(w, "note: estate.toml names no [acme] email, so the shared layer carries no")
		fmt.Fprintln(w, "      ClusterIssuer `letsencrypt`. The certificate waits until one exists;")
		fmt.Fprintln(w, "      the listener serves its self-signed placeholder meanwhile.")
		fmt.Fprintln(w)
	}
	if !written {
		fmt.Fprintln(w, "dry run — nothing written.")
		return
	}
	switch {
	case p.seeds() && len(p.unresolved) > 0:
		fmt.Fprintf(w, "staged. next: the layer that renders %s rewrites the inventory, then seed the ◆ files:\n  fort seed %s\n", strings.Join(p.required, ", "), p.fort)
	case p.seeds():
		fmt.Fprintf(w, "staged. next: seed the ◆ files:\n  fort seed %s\n", p.fort)
	case p.noContract:
		fmt.Fprintln(w, "staged. nothing to seed: the chart ships no contract, so the app has no Secret.")
	default:
		fmt.Fprintln(w, "staged. nothing to seed: the chart contract declares no keys, so the app has no Secret.")
	}
	if len(p.externals) > 0 {
		fmt.Fprintln(w, "external keys — absent from the Secret until set (fort seed --set, or a sops/env ref in the inventory):")
		for _, k := range p.externals {
			opt := ""
			if k.Optional {
				opt = " (optional)"
			}
			fmt.Fprintf(w, "  %-24s %s%s\n", k.Name, k.External, opt)
		}
	}
	fmt.Fprintln(w, "then: review, commit, PR — flux applies on merge.")
}

// seeds reports whether the plan leaves a ◆ file for `fort seed`. A
// secret-less app leaves none.
func (p *plan) seeds() bool {
	for _, a := range p.actions {
		if a.mark == '◆' {
			return true
		}
	}
	return false
}

// creates reports whether the plan writes the directory's kustomization
// (i.e. the directory is new), for the tree's directory marks.
func (p *plan) creates(dir string) bool {
	for _, a := range p.actions {
		if a.path == filepath.Join(dir, "kustomization.yaml") && a.content != nil {
			return true
		}
	}
	return false
}

// parseAddTemplates registers every templates/add/<dir>/<file>.gotpl under
// its dir-qualified name (two dirs carry a kustomization.yaml.gotpl).
func parseAddTemplates() (*template.Template, error) {
	root := template.New("add").Funcs(template.FuncMap{"join": strings.Join})
	dirs, err := addTemplates.ReadDir("templates/add")
	if err != nil {
		return nil, err
	}
	for _, d := range dirs {
		files, err := addTemplates.ReadDir("templates/add/" + d.Name())
		if err != nil {
			return nil, err
		}
		for _, f := range files {
			if !strings.HasSuffix(f.Name(), ".gotpl") {
				continue
			}
			raw, err := addTemplates.ReadFile("templates/add/" + d.Name() + "/" + f.Name())
			if err != nil {
				return nil, err
			}
			if _, err := root.New(d.Name() + "/" + f.Name()).Parse(string(raw)); err != nil {
				return nil, err
			}
		}
	}
	return root, nil
}

func setPath(m map[string]any, path, val string) {
	parts := strings.Split(path, ".")
	for _, p := range parts[:len(parts)-1] {
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = val
}

func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = prefix + l
		}
	}
	return strings.Join(lines, "\n") + "\n"
}
