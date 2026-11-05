// Package inventory reads a directory's forts.inventory.yaml — the fort-side
// record of the Secret values the directory holds and where each value
// comes from (forts#88): named sources (sops'd dotenvs), named vars, and
// one entry per Secret file with its metadata and a value spec per key.
// `fort add` takes a chart's contract in and writes an inventory out;
// `fort seed` executes it; `fort check` gates on it. A layer that
// renders a directory of its own writes the same file.
//
// It is not a contract. A contract (internal/contract) is the chart
// author's declaration, checked at a boundary, naming no file and no
// fort. An inventory names namespaces, sops file paths and a dotenv on
// an operator's machine: one directory's, on one fort.
//
// The filename is the schema: there is no sniff for a block. Order is
// preserved everywhere (a map would sort the author's key order away
// and make diffs churn), hence the yaml.Node walking.
package inventory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// FileName is the inventory's name. The forts. qualifier pairs it with
// the chart's forts.contract.yaml across the chart/estate boundary, and
// keeps it clear of the Ansible inventory.yaml an estate repo can also
// carry (the one this project's manifest replaced, #252) and of the
// editor schema that filename attracts. An inventory is one fort's.
const FileName = "forts.inventory.yaml"

// Spec is one value's provenance. Exactly one of the kinds is set.
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
	File            string // relative to the directory
	Name, Namespace string
	Type            string // Secret type; "" = Opaque
	Keys            []NamedSpec
}

type Inventory struct {
	Sources map[string]string
	Vars    []NamedSpec
	Secrets []SecretFile
}

// Present reports whether dir carries an inventory.
func Present(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, FileName))
	return err == nil
}

// Load reads dir's inventory. An absent file is (nil, nil): the
// directory holds no Secret the fort seeds. A file that is there and
// does not parse is an error.
func Load(dir string) (*Inventory, error) {
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	inv, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	return inv, nil
}

func Parse(raw []byte) (*Inventory, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("not a mapping")
	}
	inv := &Inventory{Sources: map[string]string{}}
	for k, v := range Pairs(doc.Content[0]) {
		switch k {
		case "sources":
			for name, val := range Pairs(v) {
				inv.Sources[name] = val.Value
			}
		case "vars":
			specs, err := NamedSpecs(v, "vars")
			if err != nil {
				return nil, err
			}
			inv.Vars = specs
		case "secrets":
			for file, body := range Pairs(v) {
				sf := SecretFile{File: file}
				for fk, fv := range Pairs(body) {
					switch fk {
					case "metadata":
						for mk, mv := range Pairs(fv) {
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
						specs, err := NamedSpecs(fv, "secrets."+file+".keys")
						if err != nil {
							return nil, err
						}
						sf.Keys = specs
					}
				}
				if sf.Name == "" || sf.Namespace == "" {
					return nil, fmt.Errorf("secrets.%s: metadata.name and namespace are required", file)
				}
				inv.Secrets = append(inv.Secrets, sf)
			}
		default:
			return nil, fmt.Errorf("unknown block %q (an inventory has sources, vars and secrets)", k)
		}
	}
	return inv, nil
}

// Pairs iterates a mapping node in order. Yields nothing for a
// non-mapping.
func Pairs(n *yaml.Node) func(func(string, *yaml.Node) bool) {
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

// NamedSpecs reads a mapping of name → value spec, in order.
func NamedSpecs(n *yaml.Node, where string) ([]NamedSpec, error) {
	var out []NamedSpec
	for name, v := range Pairs(n) {
		spec, err := ParseSpec(v)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", where, name, err)
		}
		out = append(out, NamedSpec{name, spec})
	}
	return out, nil
}

// ParseSpec reads one value spec, a flow mapping like {generate: hex24}.
func ParseSpec(n *yaml.Node) (Spec, error) {
	var s Spec
	if n.Kind != yaml.MappingNode {
		return s, fmt.Errorf("value spec must be a mapping like {generate: hex24}")
	}
	kinds := 0
	for k, v := range Pairs(n) {
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
