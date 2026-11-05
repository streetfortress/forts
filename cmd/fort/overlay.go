package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/contract"
	"gitea.zen.lofi/sfi/forts/internal/inventory"
	"gopkg.in/yaml.v3"
)

// From a chart's contract to a fort's inventory (forts#88): `fort add`
// takes a contract in and writes an inventory out, and `fort seed`
// derives the same inventory on the fly for a dir that commits none.
// The overlay is the fort's say in that derivation.

// overlay is a chart-schema file committed in the fort's app dir
// (forts.contract.yaml beside the chart's pin), of which the core
// honours `sources` and `secret.keys` — a key the overlay names
// replaces the chart's spec, and a key only the overlay names is
// appended. It is not an inventory: a file with a `secrets:` block is
// one, under its own name.
type overlay struct {
	Sources  map[string]string
	Keys     map[string]inventory.Spec
	KeyOrder []string
}

func parseOverlay(raw []byte) (*overlay, error) {
	ov := &overlay{Sources: map[string]string{}, Keys: map[string]inventory.Spec{}}
	if raw == nil {
		return ov, nil
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return ov, nil
	}
	for k, v := range inventory.Pairs(doc.Content[0]) {
		switch k {
		case "secrets":
			// The pre-#88 shape: the fort-side file carrying an
			// inventory under the contract's name. The filename is the
			// schema now, so the fix is the rename, not a sniff.
			return nil, fmt.Errorf("%s carries a secrets: block — that is an inventory, not a contract; rename the file to %s (forts#88)", contract.FileName, inventory.FileName)
		case "sources":
			for name, val := range inventory.Pairs(v) {
				ov.Sources[name] = val.Value
			}
		case "secret":
			for sk, sv := range inventory.Pairs(v) {
				if sk != "keys" {
					continue
				}
				specs, err := inventory.NamedSpecs(sv, "overlay secret.keys")
				if err != nil {
					return nil, err
				}
				for _, s := range specs {
					ov.Keys[s.Name] = s.Spec
					ov.KeyOrder = append(ov.KeyOrder, s.Name)
				}
			}
		}
	}
	return ov, nil
}

// inventoryFromChart derives the app's Secret keys from the chart's
// contract and the fort's overlay: generate and external keys pass
// through, an overlay key replaces the chart's spec by name, and a key
// only the overlay names is appended (a migration superset).
//
// A `template` key composes provider outputs (`${postgres.db.password}`),
// and the core resolves no provider: a layer does (sfi/fort-layers),
// and rewrites the key's spec in the inventory to a source the seeder
// can read — a sops ref into the provider's Secret. So a template the
// overlay does not replace passes through verbatim and is returned as
// unresolved: `fort add` writes it that way and says a layer's turn
// comes next; `fort seed` refuses to seed from it.
func inventoryFromChart(c *contract.Contract, ov *overlay) (keys []inventory.NamedSpec, externals, unresolved []contract.Key, err error) {
	replaced := map[string]bool{}
	for _, k := range c.Secret.Keys {
		if s, ok := ov.Keys[k.Name]; ok {
			keys = append(keys, inventory.NamedSpec{Name: k.Name, Spec: s})
			replaced[k.Name] = true
			continue
		}
		switch {
		case k.Generate != "":
			keys = append(keys, inventory.NamedSpec{Name: k.Name, Spec: inventory.Spec{Generate: k.Generate}})
		case k.Template != "":
			keys = append(keys, inventory.NamedSpec{Name: k.Name, Spec: inventory.Spec{Template: k.Template, Optional: k.Optional}})
			unresolved = append(unresolved, k)
		case k.External != "":
			keys = append(keys, inventory.NamedSpec{Name: k.Name, Spec: inventory.Spec{External: k.External, Optional: k.Optional}})
			externals = append(externals, k)
		}
	}
	for _, name := range ov.KeyOrder {
		if !replaced[name] {
			keys = append(keys, inventory.NamedSpec{Name: name, Spec: ov.Keys[name]})
		}
	}
	return keys, externals, unresolved, nil
}

// unresolvedNote names the template keys a layer must resolve, for a
// report or an error: "DATABASE_URL (postgres.db)".
func unresolvedNote(keys []contract.Key) string {
	var parts []string
	for _, k := range keys {
		refs := outputRefs(k.Template)
		sort.Strings(refs)
		parts = append(parts, k.Name+" ("+strings.Join(uniq(refs), ", ")+")")
	}
	return strings.Join(parts, ", ")
}

func uniq(list []string) []string {
	var out []string
	for i, s := range list {
		if i == 0 || s != list[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// outputRef is a provider output a chart template composes:
// ${<type>.<handle>.<output>}.
var outputRef = regexp.MustCompile(`\$\{([a-z0-9]+\.[A-Za-z0-9_-]+)\.([a-zA-Z]+)\}`)

// outputRefs lists the `${…}` references a chart template composes.
func outputRefs(template string) []string {
	var out []string
	for _, m := range outputRef.FindAllStringSubmatch(template, -1) {
		out = append(out, m[1])
	}
	return out
}
