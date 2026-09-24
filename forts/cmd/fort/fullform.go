package main

import (
	"fmt"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/contract"
	"gopkg.in/yaml.v3"
)

// The fort-side seeding contract in its FULL FORM — the schema
// `fort seed` executes and every committed
// fleet/apps/<fort>/<dir>/forts.contract.yaml uses today: named
// sources (sops'd dotenvs), named vars, and one entry per Secret file
// with its metadata and keyed value specs. `fort add` renders it for a
// chart from the chart's own contract; `fort seed` executes it, or
// derives it on the fly from the chart's contract + overlay when the
// fort-side file is absent or an overlay (docs/forts/fort-configuration-contracts.md).
//
// Order is preserved everywhere (a map would sort the author's key
// order away and make diffs churn), hence the yaml.Node walking.

type Spec struct {
	Literal  *string
	Generate string
	Template string
	External string
	Env      []string // [source, KEY]
	Sops     []string // [relative/file.yaml, dotted.key]
	Optional bool
}

type NamedSpec struct {
	Name string
	Spec Spec
}

type SecretFile struct {
	File            string // relative to the app dir
	Name, Namespace string
	Type            string // Secret type; "" = Opaque
	Keys            []NamedSpec
}

type FullForm struct {
	Sources map[string]string
	Vars    []NamedSpec
	Secrets []SecretFile
}

// isFullForm: a fort-side file with a top-level `secrets:` block is the
// full form; one with `contract: v1` and no `secrets:` is an overlay.
func isFullForm(raw []byte) bool {
	var probe struct {
		Secrets map[string]any `yaml:"secrets"`
	}
	return yaml.Unmarshal(raw, &probe) == nil && probe.Secrets != nil
}

func parseFullForm(raw []byte) (*FullForm, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("not a mapping")
	}
	ff := &FullForm{Sources: map[string]string{}}
	for k, v := range pairs(doc.Content[0]) {
		switch k {
		case "sources":
			for name, val := range pairs(v) {
				ff.Sources[name] = val.Value
			}
		case "vars":
			specs, err := namedSpecs(v, "vars")
			if err != nil {
				return nil, err
			}
			ff.Vars = specs
		case "secrets":
			for file, body := range pairs(v) {
				sf := SecretFile{File: file}
				for fk, fv := range pairs(body) {
					switch fk {
					case "metadata":
						for mk, mv := range pairs(fv) {
							switch mk {
							case "name":
								sf.Name = mv.Value
							case "namespace":
								sf.Namespace = mv.Value
							case "type":
								sf.Type = mv.Value
							}
						}
					case "keys":
						specs, err := namedSpecs(fv, "secrets."+file+".keys")
						if err != nil {
							return nil, err
						}
						sf.Keys = specs
					}
				}
				if sf.Name == "" || sf.Namespace == "" {
					return nil, fmt.Errorf("secrets.%s: metadata.name and namespace are required", file)
				}
				ff.Secrets = append(ff.Secrets, sf)
			}
		}
	}
	return ff, nil
}

// pairs iterates a mapping node in order. Returns nil for non-mappings.
func pairs(n *yaml.Node) func(func(string, *yaml.Node) bool) {
	return func(yield func(string, *yaml.Node) bool) {
		if n == nil || n.Kind != yaml.MappingNode {
			return
		}
		for i := 0; i+1 < len(n.Content); i += 2 {
			if !yield(n.Content[i].Value, n.Content[i+1]) {
				return
			}
		}
	}
}

func namedSpecs(n *yaml.Node, where string) ([]NamedSpec, error) {
	var out []NamedSpec
	for name, v := range pairs(n) {
		spec, err := parseSpec(v)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", where, name, err)
		}
		out = append(out, NamedSpec{name, spec})
	}
	return out, nil
}

func parseSpec(n *yaml.Node) (Spec, error) {
	var s Spec
	if n.Kind != yaml.MappingNode {
		return s, fmt.Errorf("value spec must be a mapping like {generate: hex24}")
	}
	kinds := 0
	for k, v := range pairs(n) {
		switch k {
		case "literal":
			val := v.Value
			s.Literal = &val
			kinds++
		case "generate":
			s.Generate = v.Value
			kinds++
		case "template":
			s.Template = v.Value
			kinds++
		case "external":
			s.External = v.Value
			kinds++
		case "env", "sops":
			var list []string
			if err := v.Decode(&list); err != nil || len(list) != 2 {
				return s, fmt.Errorf("%s wants [source, key]", k)
			}
			if k == "env" {
				s.Env = list
			} else {
				s.Sops = list
			}
			kinds++
		case "optional":
			if err := v.Decode(&s.Optional); err != nil {
				return s, fmt.Errorf("optional: %q is not a YAML bool", v.Value)
			}
		default:
			return s, fmt.Errorf("unknown primitive %q", k)
		}
	}
	if kinds != 1 {
		return s, fmt.Errorf("exactly one of literal/generate/template/external/env/sops")
	}
	return s, nil
}

// YAML renders the spec in the flow style the committed files use.
func (s Spec) YAML() string {
	var body string
	switch {
	case s.Literal != nil:
		body = "literal: " + yamlScalar(*s.Literal)
	case s.Generate != "":
		body = "generate: " + s.Generate
	case s.Template != "":
		body = fmt.Sprintf("template: %q", s.Template)
	case s.External != "":
		body = fmt.Sprintf("external: %q", s.External)
	case s.Env != nil:
		body = fmt.Sprintf("env: [%s, %s]", s.Env[0], s.Env[1])
	case s.Sops != nil:
		body = fmt.Sprintf("sops: [%s, %s]", s.Sops[0], s.Sops[1])
	}
	if s.Optional {
		body += ", optional: true"
	}
	return "{" + body + "}"
}

func yamlScalar(v string) string {
	if v == "" || strings.ContainsAny(v, ":#{}[],&*!|>'\"%@`") || strings.TrimSpace(v) != v || v == "true" || v == "false" {
		return fmt.Sprintf("%q", v)
	}
	return v
}

// providerBinding is what the fort resolves for one required instance:
// its non-secret outputs and the var that carries its password.
type providerBinding struct {
	outputs     map[string]string
	passwordVar ctrVar
}

// providerDirs: the shared instance each provider type lives in on a
// fort (docs/forts/fort-configuration-contracts.md conventions).
var providerDirs = map[string]string{"postgres": "sites-db", "valkey": "valkey", "s3": "rustfs"}

// conventionalBinding is THE place the per-fort conventions for a
// tenant live — add renders them into the tree, seed and check resolve
// against them (the HelmRelease's wire values override once written).
// db is the valkey logical db index add assigns (count of tenants).
func conventionalBinding(typ, handle, app string, db int) providerBinding {
	switch typ {
	case "postgres":
		return providerBinding{
			outputs:     map[string]string{"host": "sites-db-rw.sites-db.svc.cluster.local", "port": "5432", "name": app, "user": app},
			passwordVar: ctrVar{handle + "_password", "requires.postgres." + handle + " → the sites-db tenant role (lockstep pair).", "../sites-db/secrets-" + app + ".sops.yaml", "stringData.password"},
		}
	case "valkey":
		return providerBinding{
			outputs:     map[string]string{"host": "valkey.valkey.svc.cluster.local", "port": "6379", "db": fmt.Sprint(db), "user": app},
			passwordVar: ctrVar{handle + "_password", "requires.valkey." + handle + " → the valkey-users ACL entry (lockstep pair).", "../valkey/secrets.sops.yaml", "stringData." + app},
		}
	}
	return providerBinding{}
}

// fullFormFromChart derives the app's seeding contract from the chart's
// contract: provider passwords become `${<handle>_password}` vars over
// the conventional sops refs, non-secret outputs are inlined, generate
// and external keys pass through. A key whose provider is missing is
// dropped if optional, an error otherwise.
func fullFormFromChart(c *contract.Contract, providers map[string]providerBinding) (vars []ctrVar, keys []NamedSpec, externals []contract.Key, err error) {
	// `optional` on a generate/template key only matters here — a
	// template whose provider is absent is dropped — so the derived
	// full form carries it only on external keys, where the seeder and
	// the gate still need it.
	for _, k := range c.Secret.Keys {
		switch {
		case k.Generate != "":
			keys = append(keys, NamedSpec{k.Name, Spec{Generate: k.Generate}})
		case k.Template != "":
			text, skip, err := rewriteTemplate(k, providers, &vars)
			if err != nil {
				return nil, nil, nil, err
			}
			if skip {
				continue
			}
			keys = append(keys, NamedSpec{k.Name, Spec{Template: text}})
		case k.External != "":
			keys = append(keys, NamedSpec{k.Name, Spec{External: k.External, Optional: k.Optional}})
			externals = append(externals, k)
		}
	}
	return vars, keys, externals, nil
}
