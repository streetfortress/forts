package main

import (
	"bytes"
	"embed"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"gitea.zen.lofi/sfi/forts/internal/contract"
	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gopkg.in/yaml.v3"
)

//go:embed templates/add
var addTemplates embed.FS

// add renders a chart's fort-side tree from its contract
// (docs/fort-configuration-contracts.md): the app dir, the gateway listener + certificate,
// provider instances on first need and tenant registration after that.
// A pure generator — new files plus appended list entries, never a
// rewrite; nothing is applied. Dry-run prints the marked tree; --write
// lays it down for a PR and prints what to seed.
func add(args []string) error {
	if len(args) < 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return fmt.Errorf("usage: fort add <fort> <oci-ref> [--version X | --follow-pr N | --follow-main] [--name app] [--hostname fqdn] [--http service:port] [--without type,...] [--chart-dir path] [--write]")
	}
	o := addOpts{fort: args[0], ref: args[1], without: map[string]bool{}}
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.StringVar(&o.version, "version", "", "chart version to pin (a release tag)")
	fs.StringVar(&o.followPR, "follow-pr", "", "follow a PR's per-commit preview charts instead of a tag")
	fs.BoolVar(&o.followMain, "follow-main", false, "follow main's per-commit preview charts instead of a tag")
	fs.StringVar(&o.name, "name", "", "app name (namespace, release, dir); default: chart name")
	fs.StringVar(&o.hostname, "hostname", "", "public hostname; default <app>.<fort>.<zone> — the estate's mesh zone (estate.toml) in the lab, the manifest's [dns] zone in the cloud")
	fs.StringVar(&o.http, "http", "", "the Service the fleet routes to, <service>:<port> — for a chart that ships no contract")
	fs.StringVar(&o.chartDir, "chart-dir", "", "read the chart from a local dir instead of pulling (chart authors, tests)")
	without := fs.String("without", "", "skip optional provider types (comma-separated)")
	fs.BoolVar(&o.write, "write", false, "write the tree (default: print the plan)")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	for _, t := range strings.Split(*without, ",") {
		if t != "" {
			o.without[t] = true
		}
	}
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
		// Author verbs stage what they generate (mint and seed do; the
		// handbook says so): the new dirs and the appended lists.
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
	without                                                            map[string]bool
	write                                                              bool
}

// action is one planned change: a new file (content), an append to an
// existing file (edit), or a seeded file the seeder will produce (neither).
type action struct {
	path    string // repo-relative
	mark    rune   // ● new, ○ appended, ◆ seeded
	note    string
	content []byte
	edit    func(string) (string, error)
}

type plan struct {
	root, fort, app string
	resolved        string
	actions         []action
	externals       []contract.Key
	// noContract: the chart ships no forts.contract.yaml, so this app
	// has no key gate and the report says so (#461).
	noContract bool
	// http: the app has a gateway listener, a certificate and a route.
	http bool
}

// view is the template context; conventions live here, not in flags.
type view struct {
	Fort, App, Hostname, Zone, Issuer, SecretName string
	Lab                                           bool
	// NoContract: the chart ships no contract at all (#461), so the
	// generated comments must not credit one.
	NoContract bool
	// HasSecret: the effective contract declares at least one key, so
	// the app consumes a Secret. False is the secret-less shape (#460)
	// — no secrets.sops.yaml, no fort-side full form, no
	// `existingSecret` value the chart never reads.
	HasSecret bool
	// Archive: the postgres instance ships base backups + WAL to the
	// store the manifest's [backups.db-physical] block names (b2, the
	// one store that takes the layer), at BackupHour. No block renders
	// no archive at all, and says so in cluster.yaml (docs/infra/
	// backups.md: "none, documented" is honest).
	Archive                          bool
	BackupHour                       int
	ChartURL, ChartRepo, Version, PR string
	// Main: the fort follows the chart repo's main branch through its
	// per-commit prereleases, rather than a tag or a PR.
	Main                                        bool
	HTTP                                        *contract.HTTP
	Metrics                                     *contract.Metrics
	Component                                   string
	SelectorLines                               []string
	Postgres                                    *pgTenant
	Valkey                                      *vkTenant
	S3                                          *s3Tenant
	ValuesYAML                                  string
	Vars                                        []ctrVar
	Keys                                        []NamedSpec
	GatewayPatchesComment, ValkeyPatchesComment string
	PostgresPatchesComment, TenantContractEntry string
	// Providers: the provider dirs forts/kustomization.yaml lists when
	// this add creates the forts/ directory (deployments#671).
	Providers []string
}

type pgTenant struct {
	Handle     string
	Extensions []string
}
type vkTenant struct {
	Handle string
	DB     int
	Key    string // the Secret key carrying this user's password
}
type s3Tenant struct {
	Handle string
	Key    string // the Secret key carrying this identity's secret key
}
type ctrVar struct{ Name, Comment, File, Path string }

const gatewayPatchesComment = `# One JSON6902 patch per service appends its https listener to the
# Gateway (the design's one exception to "new files only": listeners
# live in a single object, and a strategic-merge patch cannot append to
# a CRD list — kustomize has no merge key for it). fort add adds a file
# here and one entry below; gateway.yaml is never edited.
`
const valkeyPatchesComment = `# Tenant registration: one JSON6902 patch per consumer adds its ACL user
# to the HelmRelease values (release.yaml carries only the instance and
# its admin ` + "`default`" + ` user). fort add adds a file + an entry here.
`
const postgresPatchesComment = `# Tenant registration: one JSON6902 patch per tenant appends its login
# role to managed.roles (cluster.yaml carries the instance). fort add
# adds a file + an entry here.
`

var outputRef = regexp.MustCompile(`\$\{(postgres|valkey|s3)\.([A-Za-z0-9_-]+)\.([a-zA-Z]+)\}`)

func planAdd(o addOpts) (*plan, error) {
	entry, err := manifest.Load(o.root, o.fort)
	if err != nil {
		return nil, err
	}
	// The estate: its mesh zone is the lab hostname's zone (forts#45).
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
	// nothing, so add plans nothing from it (#461). A contract that is
	// there and does not load is still an error — fix it in the chart.
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
		Lab:        !entry.Cloud(),
		Archive:    entry.Backups.DBPhysical != nil,
		BackupHour: entry.Backups.DBPhysical.HourValue(),
		ChartURL:   "oci://" + repoPath,
		ChartRepo:  strings.SplitN(repoPath, "/", 2)[len(strings.SplitN(repoPath, "/", 2))-1],
		Version:    resolved, PR: o.followPR, Main: o.followMain,
		HTTP: c.HTTP, Metrics: c.Metrics,
		GatewayPatchesComment:  gatewayPatchesComment,
		ValkeyPatchesComment:   valkeyPatchesComment,
		PostgresPatchesComment: postgresPatchesComment,
	}
	// The hostname's zone: in the lab the estate's mesh zone, where
	// <app>.<fort>.<zone> is a name every host on the mesh resolves to
	// the fort (mesh-dns.nix); in the cloud the manifest's public zone.
	if v.Lab {
		v.Issuer, v.Zone = "step-ca", est.ZoneName()
	} else {
		v.Issuer, v.Zone = "letsencrypt", entry.DNS.Zone
		if v.Zone == "" {
			v.Zone = "streetfortress.cloud"
		}
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

	p := &plan{root: o.root, fort: o.fort, app: app, resolved: resolved, noContract: !c.Present, http: c.HTTP != nil}
	apps := filepath.Join("flux/apps", o.fort)
	appDir := filepath.Join(apps, app)
	if exists(o.root, appDir) {
		return nil, fmt.Errorf("%s exists — fort add never overwrites (fort remove first, or refine in place)", appDir)
	}
	if !exists(o.root, filepath.Join(apps, "kustomization.yaml")) {
		return nil, fmt.Errorf("%s has no flux/apps tree (fort scaffold %s --write first)", o.fort, o.fort)
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
	var rootAdds []string // gateway, then forts/, then the app
	createdGateway := false
	// fortsAdds: the provider dirs this add creates under forts/ — the
	// one directory, one namespace, every provider the fort runs
	// (deployments#671). Listed in forts/kustomization.yaml, which this
	// add renders with the directory or appends to.
	var fortsAdds []string
	fortsRoot := filepath.Join(apps, fortsDir)

	// --- providers: outputs by convention, instance on first need,
	// tenant registration otherwise.
	// The Secret entry is added after the full form is derived: only a
	// contract with keys names a Secret (#460).
	values := map[string]any{}
	providers := map[string]providerBinding{} // "type.handle" → outputs + the secret output's var
	// The postgres archive key, when this add creates the instance on a
	// fort with a db-physical block: listed with the chart's externals so
	// the report says what to mint and how to set it.
	var dbExternals []contract.Key
	for _, r := range c.Instances() {
		if o.without[r.Type] {
			if !r.Instance.Optional {
				return nil, fmt.Errorf("requires.%s.%s is not optional; --without %s refused", r.Type, r.Handle, r.Type)
			}
			continue
		}
		key := r.Type + "." + r.Handle
		switch r.Type {
		case "postgres":
			if v.Postgres != nil {
				return nil, fmt.Errorf("one postgres instance per chart in v1 (got %s and %s)", v.Postgres.Handle, r.Handle)
			}
			v.Postgres = &pgTenant{Handle: r.Handle, Extensions: r.Instance.Extensions}
			v.TenantContractEntry = tenantContractEntry(app)
			dbDir := filepath.Join(apps, providerDirs["postgres"])
			providers[key] = conventionalBinding(r.Type, r.Handle, app, 0)
			tenant := func() {
				p.add(action{path: filepath.Join(dbDir, "database-"+app+".yaml"), mark: '●', note: "Database CR" + extNote(r.Instance.Extensions), content: render("postgres/database.yaml.gotpl")})
				p.add(action{path: filepath.Join(dbDir, "role-"+app+".yaml"), mark: '●', note: "JSON6902 add /spec/managed/roles/-", content: render("postgres/role.yaml.gotpl")})
				p.add(action{path: filepath.Join(dbDir, "secrets-"+app+".sops.yaml"), mark: '◆', note: "seeded"})
			}
			switch {
			case !exists(o.root, dbDir):
				// Instance on first need: the greenfield template, with
				// this tenant already registered in its kustomization
				// and contract (the same shape a later append produces).
				if v.Archive {
					// The archive is the db-physical block's: it ships to
					// B2 through the barman-cloud plugin with the fort's
					// own key, which the report lists as the external it
					// is (mint, then seed --set), nightly at the block's
					// hour. No block renders none.
					p.add(action{path: filepath.Join(dbDir, "cluster.yaml"), mark: '●', note: "CNPG Cluster postgres: greenfield, barman archive to B2 (plugin), roles via patches", content: render("postgres/cluster.yaml.gotpl")})
					p.add(action{path: filepath.Join(dbDir, "objectstore.yaml"), mark: '●', note: "ObjectStore b2: s3://sfi-db-physical-backups/" + o.fort + ", the fort's own key", content: render("postgres/objectstore.yaml.gotpl")})
					p.add(action{path: filepath.Join(dbDir, "scheduled-backup.yaml"), mark: '●', note: fmt.Sprintf("nightly base backup via the plugin at %02d:00 UTC ([backups.db-physical] hour)", v.BackupHour), content: render("postgres/scheduled-backup.yaml.gotpl")})
					p.add(action{path: filepath.Join(dbDir, "kustomization.yaml"), mark: '●', note: "patches: role-" + app + ".yaml", content: render("postgres/kustomization.yaml.gotpl")})
					p.add(action{path: filepath.Join(dbDir, contract.FileName), mark: '●', note: "secrets-backup (the fort's B2 db-physical key, external) + secrets-" + app, content: render("postgres/forts.contract.yaml.gotpl")})
					p.add(action{path: filepath.Join(dbDir, "secrets-backup.sops.yaml"), mark: '◆', note: "seeded"})
					dbExternals = []contract.Key{
						{Name: "ACCESS_KEY_ID", External: "postgres: `fort b2-key db-physical " + o.fort + " --write`, then `fort seed " + o.fort + " postgres --set ACCESS_KEY_ID --write`"},
						{Name: "ACCESS_SECRET_KEY", External: "postgres: the same mint; `fort seed " + o.fort + " postgres --set ACCESS_SECRET_KEY --write`"},
					}
				} else {
					p.add(action{path: filepath.Join(dbDir, "cluster.yaml"), mark: '●', note: "CNPG Cluster postgres: greenfield, NO archive (no [backups.db-physical] block), roles via patches", content: render("postgres/cluster.yaml.gotpl")})
					p.add(action{path: filepath.Join(dbDir, "kustomization.yaml"), mark: '●', note: "patches: role-" + app + ".yaml", content: render("postgres/kustomization.yaml.gotpl")})
					p.add(action{path: filepath.Join(dbDir, contract.FileName), mark: '●', note: "secrets-" + app, content: render("postgres/forts.contract.yaml.gotpl")})
				}
				tenant()
				fortsAdds = append(fortsAdds, providerSubdir("postgres"))
			case exists(o.root, filepath.Join(dbDir, "database-"+app+".yaml")):
				p.add(action{path: dbDir, mark: '○', note: "untouched — tenant `" + app + "` already registered"})
			default:
				tenant()
				p.add(action{path: filepath.Join(dbDir, "kustomization.yaml"), mark: '○', note: "+ database-" + app + ".yaml, secrets-" + app + ".sops.yaml; patches: + role-" + app + ".yaml",
					edit: func(s string) (string, error) {
						s, err := appendListItem(s, "resources", "database-"+app+".yaml")
						if err != nil {
							return "", err
						}
						s, err = appendListItem(s, "resources", "secrets-"+app+".sops.yaml")
						if err != nil {
							return "", err
						}
						return appendPatch(s, postgresPatchesComment, "role-"+app+".yaml", "postgresql.cnpg.io", "v1", "Cluster", "postgres", fortsNamespace), nil
					}})
				p.add(action{path: filepath.Join(dbDir, contract.FileName), mark: '○', note: "+ secrets-" + app + ".sops.yaml (username literal, password generate)",
					edit: func(s string) (string, error) { return appendBlock(s, tenantContractEntry(app)), nil }})
			}
		case "valkey":
			if v.Valkey != nil {
				return nil, fmt.Errorf("one valkey instance per chart in v1")
			}
			vkDir := filepath.Join(apps, providerDirs["valkey"])
			db := 0
			if exists(o.root, vkDir) {
				acls, _ := filepath.Glob(filepath.Join(o.root, vkDir, "acl-*.yaml"))
				db = len(acls)
			}
			v.Valkey = &vkTenant{Handle: r.Handle, DB: db, Key: keyReferencing(c, r.Type, r.Handle)}
			providers[key] = conventionalBinding(r.Type, r.Handle, app, db)
			if exists(o.root, vkDir) {
				p.add(action{path: filepath.Join(vkDir, "acl-"+app+".yaml"), mark: '●', note: "JSON6902 add /spec/values/auth/aclUsers/" + app, content: render("valkey/acl.yaml.gotpl")})
				p.add(action{path: filepath.Join(vkDir, "kustomization.yaml"), mark: '○', note: "patches: + acl-" + app + ".yaml",
					edit: func(s string) (string, error) {
						return appendPatch(s, valkeyPatchesComment, "acl-"+app+".yaml", "helm.toolkit.fluxcd.io", "v2", "HelmRelease", "valkey", fortsNamespace), nil
					}})
				p.add(action{path: filepath.Join(vkDir, contract.FileName), mark: '○', note: "+ key " + app,
					edit: func(s string) (string, error) {
						return appendMapEntry(s, "    keys:", "      "+app+": {generate: hex24}")
					}})
				p.add(action{path: filepath.Join(vkDir, "secrets.sops.yaml"), mark: '◆', note: "+ key " + app + " (seeder add-missing-keys; until then: sops set)"})
			} else {
				for _, f := range []string{"repository.yaml", "release.yaml"} {
					p.add(action{path: filepath.Join(vkDir, f), mark: '●', content: static("valkey/" + f)})
				}
				p.add(action{path: filepath.Join(vkDir, "monitoring.yaml"), mark: '●', note: "PodMonitor + ValkeyDown (cache duty: no eviction alert)", content: static("valkey/monitoring.yaml")})
				p.add(action{path: filepath.Join(vkDir, "kustomization.yaml"), mark: '●', note: "patches: acl-" + app + ".yaml", content: render("valkey/kustomization.yaml.gotpl")})
				p.add(action{path: filepath.Join(vkDir, "acl-"+app+".yaml"), mark: '●', note: "JSON6902 add /spec/values/auth/aclUsers/" + app, content: render("valkey/acl.yaml.gotpl")})
				p.add(action{path: filepath.Join(vkDir, contract.FileName), mark: '●', note: "valkey-users: default + " + app, content: render("valkey/forts.contract.yaml.gotpl")})
				p.add(action{path: filepath.Join(vkDir, "secrets.sops.yaml"), mark: '◆', note: "seeded"})
				fortsAdds = append(fortsAdds, providerSubdir("valkey"))
			}
		case "s3":
			if v.S3 != nil {
				return nil, fmt.Errorf("one s3 instance per chart in v1")
			}
			rsDir := filepath.Join(apps, providerDirs["s3"])
			v.S3 = &s3Tenant{Handle: r.Handle, Key: keyReferencing(c, r.Type, r.Handle)}
			providers[key] = conventionalBinding(r.Type, r.Handle, app, 0)
			policy := filepath.Join(rsDir, "policies", app+".json")
			switch {
			case !exists(o.root, rsDir):
				// Instance on first need, with this tenant already
				// registered: its bucket line, its policy document, its
				// identity key — the same shape a later append produces.
				for _, f := range []string{"repository.yaml", "release.yaml", "otel-collector.yaml", "iam-cronjob.yaml"} {
					p.add(action{path: filepath.Join(rsDir, f), mark: '●', content: static("rustfs/" + f)})
				}
				p.add(action{path: filepath.Join(rsDir, "monitoring.yaml"), mark: '●', note: "PrometheusRule RustfsDown (scrape is the collector's ServiceMonitor)", content: static("rustfs/monitoring.yaml")})
				p.add(action{path: filepath.Join(rsDir, "bucket-job.yaml"), mark: '●', note: "rc bucket create " + app, content: render("rustfs/bucket-job.yaml.gotpl")})
				p.add(action{path: policy, mark: '●', note: "get/put/list/delete on bucket " + app + ", nothing else", content: render("rustfs/policy.json.gotpl")})
				p.add(action{path: filepath.Join(rsDir, "kustomization.yaml"), mark: '●', note: "configMapGenerator rustfs-policies: policies/" + app + ".json", content: render("rustfs/kustomization.yaml.gotpl")})
				p.add(action{path: filepath.Join(rsDir, contract.FileName), mark: '●', note: "rustfs-credentials (root pair) + rustfs-users: " + app, content: render("rustfs/forts.contract.yaml.gotpl")})
				p.add(action{path: filepath.Join(rsDir, "secrets.sops.yaml"), mark: '◆', note: "seeded"})
				p.add(action{path: filepath.Join(rsDir, "users.sops.yaml"), mark: '◆', note: "seeded"})
				fortsAdds = append(fortsAdds, providerSubdir("s3"))
			case exists(o.root, policy):
				p.add(action{path: rsDir, mark: '○', note: "untouched — identity `" + app + "` already registered"})
			default:
				p.add(action{path: policy, mark: '●', note: "get/put/list/delete on bucket " + app + ", nothing else", content: render("rustfs/policy.json.gotpl")})
				p.add(action{path: filepath.Join(rsDir, "bucket-job.yaml"), mark: '○', note: "+ rc bucket create " + app,
					edit: func(s string) (string, error) {
						return appendAfterLast(s, "              rc bucket create ", "              rc bucket create --ignore-existing r/"+app)
					}})
				p.add(action{path: filepath.Join(rsDir, "kustomization.yaml"), mark: '○', note: "configMapGenerator rustfs-policies: + policies/" + app + ".json",
					edit: func(s string) (string, error) {
						return appendMapEntry(s, "    files:", "      - policies/"+app+".json")
					}})
				p.add(action{path: filepath.Join(rsDir, contract.FileName), mark: '○', note: "rustfs-users: + key " + app,
					edit: func(s string) (string, error) {
						return appendMapEntryUnder(s, "  users.sops.yaml:", "    keys:", "      "+app+": {generate: hex24}")
					}})
				p.add(action{path: filepath.Join(rsDir, "users.sops.yaml"), mark: '◆', note: "+ key " + app + " (seeder add-missing-keys; until then: sops set)"})
			}
		}
		for out, path := range r.Instance.Wire {
			val, ok := providers[key].outputs[out]
			if !ok {
				return nil, fmt.Errorf("requires.%s.wire.%s: not a non-secret output of %s", key, out, r.Type)
			}
			setPath(values, path, val)
		}
	}

	// --- forts/: the one directory and the one namespace every provider
	// lives in (deployments#671). Rendered with the first provider the
	// fort needs; a later provider type is one appended line.
	if len(fortsAdds) > 0 {
		if !exists(o.root, fortsRoot) {
			v.Providers = fortsAdds
			p.add(action{path: filepath.Join(fortsRoot, "namespace.yaml"), mark: '●', note: "Namespace forts: every provider the fort runs", content: static("forts/namespace.yaml")})
			p.add(action{path: filepath.Join(fortsRoot, "kustomization.yaml"), mark: '●', note: "namespace + " + strings.Join(fortsAdds, ", "), content: render("forts/kustomization.yaml.gotpl")})
			rootAdds = append(rootAdds, fortsDir)
		} else {
			adds := fortsAdds
			p.add(action{path: filepath.Join(fortsRoot, "kustomization.yaml"), mark: '○', note: "+ " + strings.Join(adds, ", "),
				edit: func(s string) (string, error) {
					for _, r := range adds {
						var err error
						if s, err = appendListItem(s, "resources", r); err != nil {
							return "", err
						}
					}
					return s, nil
				}})
		}
	}

	// --- the fort-side contract: chart keys with provider outputs
	// resolved by convention (shared with fort seed's on-the-fly path).
	vars, keys, externals, err := fullFormFromChart(c, providers)
	if err != nil {
		return nil, err
	}
	v.Vars, v.Keys, p.externals = vars, keys, append(externals, dbExternals...)
	sort.Slice(v.Vars, func(i, j int) bool { return v.Vars[i].Name < v.Vars[j].Name })
	// The effective key set decides the shape: a chart that declares no
	// keys — and a chart whose only keys are optional templates that
	// --without dropped — gets no Secret machinery at all.
	v.HasSecret = len(v.Keys) > 0
	if v.HasSecret {
		values[c.Secret.Values] = v.SecretName
	}

	// --- release values
	if len(values) > 0 {
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
			if v.Lab {
				p.add(action{path: filepath.Join(gwDir, "cluster-issuer-step-ca.yaml"), mark: '●', note: "lab issuer (per-lab-cluster)", content: static("gateway/cluster-issuer-step-ca.yaml")})
			}
			p.add(action{path: filepath.Join(gwDir, "kustomization.yaml"), mark: '●', note: "patches: listener-" + app + ".yaml", content: render("gateway/kustomization.yaml.gotpl")})
			createdGateway = true
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
	ca, err := os.ReadFile(filepath.Join(o.root, "flux/infrastructure/configs/step-ca-root.pem"))
	if err != nil {
		return nil, err
	}
	p.add(action{path: filepath.Join(appDir, "step-ca-root.pem"), mark: '●', note: "copy of flux/infrastructure/configs/step-ca-root.pem", content: ca})
	p.add(action{path: filepath.Join(appDir, "release.yaml"), mark: '●', note: "wire: " + wireNote(c) + "; retry forever — refine replicas/env in place", content: render("app/release.yaml.gotpl")})
	if c.HTTP != nil {
		p.add(action{path: filepath.Join(appDir, "route.yaml"), mark: '●', note: fmt.Sprintf("HTTPRoute %s → %s:%d", v.Hostname, c.HTTP.Service, c.HTTP.Port), content: render("app/route.yaml.gotpl")})
	}
	if c.Metrics != nil {
		p.add(action{path: filepath.Join(appDir, "monitoring.yaml"), mark: '●', note: "PodMonitor " + app + "-" + v.Component + " (rules are policy: by hand)", content: render("app/monitoring.yaml.gotpl")})
	}
	p.add(action{path: filepath.Join(appDir, "kustomization.yaml"), mark: '●', content: render("app/kustomization.yaml.gotpl")})
	if v.HasSecret {
		p.add(action{path: filepath.Join(appDir, contract.FileName), mark: '●', note: fmt.Sprintf("pre-`fort seed` full form, %d keys", len(v.Keys)), content: render("app/forts.contract.yaml.gotpl")})
		p.add(action{path: filepath.Join(appDir, "secrets.sops.yaml"), mark: '◆', note: "seeded"})
	}

	if createdGateway {
		rootAdds = append([]string{"gateway"}, rootAdds...)
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

// parseHTTPFlag reads --http <service>:<port> — the ingress a chart
// that ships no contract cannot declare (#461). The contract stays the
// source of truth wherever there is one: the flag only fills the gap,
// and planAdd refuses it against a contract that declares http.
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

// rewriteTemplate turns a chart template (`${postgres.db.password}`,
// `${postgres.db.host}`) into the fort-side form: the type's secret
// output (`password`; `secretKey` for s3) becomes a `${<handle>_…}` var
// backed by a sops ref, non-secret outputs are inlined. A key whose provider was skipped (--without) is
// dropped if optional, an error otherwise.
func rewriteTemplate(k contract.Key, providers map[string]providerBinding, vars *[]ctrVar) (string, bool, error) {
	var fail error
	skip := false
	out := outputRef.ReplaceAllStringFunc(k.Template, func(m string) string {
		sub := outputRef.FindStringSubmatch(m)
		key, field := sub[1]+"."+sub[2], sub[3]
		b, ok := providers[key]
		if !ok {
			if k.Optional {
				skip = true
			} else {
				fail = fmt.Errorf("secret.keys.%s references %s, which is not provisioned", k.Name, key)
			}
			return m
		}
		switch field {
		case b.secretOutput:
			if !hasVar(*vars, b.passwordVar.Name) {
				*vars = append(*vars, b.passwordVar)
			}
			return "${" + b.passwordVar.Name + "}"
		case "uri":
			fail = fmt.Errorf("secret.keys.%s: the uri output is not implemented — compose it from host/port/name/user/password", k.Name)
			return m
		}
		val, ok := b.outputs[field]
		if !ok {
			fail = fmt.Errorf("secret.keys.%s: %s has no output %s", k.Name, key, field)
		}
		return val
	})
	if fail != nil {
		return "", false, fail
	}
	if strings.Contains(out, "${") && !skip {
		// What survives must be a var this rewrite introduced (the
		// provider's secret output); anything else is a reference the
		// chart wrote that nothing here resolves.
		for _, m := range regexp.MustCompile(`\$\{[^}]*\}`).FindAllString(out, -1) {
			if !hasVar(*vars, strings.TrimSuffix(strings.TrimPrefix(m, "${"), "}")) {
				return "", false, fmt.Errorf("secret.keys.%s: unresolved reference %s", k.Name, m)
			}
		}
	}
	return out, skip, nil
}

// tenantContractEntry is the provider-contract block for one tenant's
// role secret — rendered into a new instance's contract and appended to
// an existing one, so both paths leave the same file.
func tenantContractEntry(app string) string {
	return fmt.Sprintf(`  secrets-%[1]s.sops.yaml:
    metadata:
      name: postgres-%[1]s-credentials
      namespace: forts
      type: kubernetes.io/basic-auth
    keys:
      username: {literal: %[1]s}
      # %[1]s's role password; the app env mirrors it (lockstep pair).
      password: {generate: hex24}
`, app)
}

// keyReferencing names the Secret key whose template consumes the
// instance's password (the lockstep pair the provider comments name).
func keyReferencing(c *contract.Contract, typ, handle string) string {
	needle := "${" + typ + "." + handle + "." + conventionalBinding(typ, handle, "", 0).secretOutput + "}"
	for _, k := range c.Secret.Keys {
		if strings.Contains(k.Template, needle) {
			return k.Name
		}
	}
	return "(no key)"
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

// report prints the marked tree (CLAUDE.md convention) grouped by
// directory, then what the human does next.
func (p *plan) report(w io.Writer, written bool) {
	base := filepath.Join("flux/apps", p.fort)
	fmt.Fprintf(w, "%s/   (chart %s)\n", base, p.resolved)
	sort.SliceStable(p.actions, func(i, j int) bool { return p.actions[i].path < p.actions[j].path })
	lastDir, lastSub := "", ""
	for _, a := range p.actions {
		rel := strings.TrimPrefix(a.path, base+"/")
		// Grouped by the instance or app dir, one level deep: a file in a
		// subdirectory (rustfs/policies/<app>.json) lists under its
		// instance with the subpath as its name, not as a dir of its own.
		// forts/ is the exception — the one dir that holds dirs — and its
		// providers list as dirs of their own, one level further in.
		dir, file, _ := strings.Cut(rel, "/")
		if file == "" {
			dir, file = "", rel
		}
		sub := ""
		if dir == fortsDir {
			if s, f, ok := strings.Cut(file, "/"); ok {
				sub, file = s, f
			} else if a.content == nil && a.edit == nil {
				sub, file = file, "" // the dir itself: `○ untouched`
			}
		}
		if dir != "" {
			if dir != lastDir {
				mark := '○'
				if p.creates(filepath.Join(base, dir)) {
					mark = '●'
				}
				fmt.Fprintf(w, "%c %s/\n", mark, dir)
				lastDir, lastSub = dir, ""
			}
			indent := "  "
			if sub != "" {
				if sub != lastSub {
					mark := '○'
					if p.creates(filepath.Join(base, dir, sub)) {
						mark = '●'
					}
					fmt.Fprintf(w, "  %c %s/\n", mark, sub)
					lastSub = sub
				}
				indent = "    "
			}
			if file == "" {
				fmt.Fprintf(w, "%s  %s\n", indent, a.note)
				continue
			}
			fmt.Fprintf(w, "%s%c %-*s %s\n", indent, a.mark, 30-len(indent), file, a.note)
		} else {
			fmt.Fprintf(w, "%c %-30s %s\n", a.mark, file, a.note)
		}
	}
	fmt.Fprintln(w)
	if p.noContract {
		fmt.Fprintf(w, "note: the chart ships no %s. This app gets no key gate: `fort check`\n", contract.FileName)
		fmt.Fprintln(w, "      cannot see a chart bump that starts to need a key. The chart adopts the")
		fmt.Fprintln(w, "      gate when it ships the file — docs/fort-configuration-contracts.md.")
		if !p.http {
			fmt.Fprintln(w, "note: nothing declares an http service, so this app has no listener, no")
			fmt.Fprintln(w, "      certificate and no route. It is reachable on the mesh and the LAN only.")
			fmt.Fprintln(w, "      Add ingress with `fort add … --http <service>:<port>`.")
		}
		fmt.Fprintln(w)
	}
	if !written {
		fmt.Fprintln(w, "dry run — nothing written (add --write).")
		return
	}
	switch {
	case p.seeds():
		fmt.Fprintf(w, "staged. next: seed the ◆ files (dependency order is derived):\n  fort seed %s --write\n", p.fort)
	case p.noContract:
		fmt.Fprintln(w, "staged. nothing to seed: the chart ships no contract, so the app has no Secret.")
	default:
		fmt.Fprintln(w, "staged. nothing to seed: the chart contract declares no keys, so the app has no Secret.")
	}
	if len(p.externals) > 0 {
		fmt.Fprintln(w, "external keys — absent from the Secret until set (sops set, or a sops/env ref in the contract):")
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
// secret-less app on a fort that needs no provider leaves none (#460).
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
// its dir-qualified name (several dirs carry a kustomization.yaml.gotpl).
func parseAddTemplates() (*template.Template, error) {
	root := template.New("add")
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

// fleetContracts lists every fort-side contract in the tree — either
// name, one per dir, contract.Read's preference order as a glob.
func fleetContracts(root string) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range contract.Names {
		m, _ := filepath.Glob(filepath.Join(root, "flux/apps/*/*", n))
		for _, p := range m {
			if d := filepath.Dir(p); !seen[d] {
				seen[d] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

func hasVar(vars []ctrVar, name string) bool {
	for _, v := range vars {
		if v.Name == name {
			return true
		}
	}
	return false
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

func extNote(ext []string) string {
	if len(ext) == 0 {
		return ""
	}
	return " (extensions: " + strings.Join(ext, ", ") + ")"
}

func wireNote(c *contract.Contract) string {
	var parts []string
	for _, r := range c.Instances() {
		if len(r.Instance.Wire) > 0 {
			parts = append(parts, r.Type+"."+r.Handle)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
