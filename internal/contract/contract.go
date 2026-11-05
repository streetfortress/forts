// Package contract reads a chart's forts.contract.yaml — the
// environment-agnostic declaration of what a chart needs
// (docs/reference/contracts.md): the keys its existingSecret must carry
// and how each is produced, the Service the fleet routes to, the
// metrics port.
//
// The core reads the `secret`, `http` and `metrics` blocks (forts#250):
// `fort add` renders the route, the listener, the certificate and the
// inventory from them, `fort seed` produces the Secret, `fort
// check` gates on it. The `requires` block — the platform services a
// chart consumes — is the apps layer's (sfi/fort-layers): the core
// parses it only far enough to say that a layer must resolve it.
package contract

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

const (
	// FileName is the contract's name.
	FileName = "forts.contract.yaml"
	// LegacyFileName is what it was called when the schema was only
	// about secrets. Every chart released before the rename ships this
	// name inside its tarball, immutably, so Read keeps accepting it.
	LegacyFileName = "secrets.contract.yaml"
)

// Names is every filename a contract may have, in preference order.
var Names = []string{FileName, LegacyFileName}

type Contract struct {
	Version string   `yaml:"contract"`
	Secret  Secret   `yaml:"secret"`
	HTTP    *HTTP    `yaml:"http"`
	Metrics *Metrics `yaml:"metrics"`
	// Requires names the platform services the chart consumes, by type
	// and handle. The core renders none of them: a layer does, and the
	// core's verbs only read this far to name what the layer must
	// resolve (an instance's Optional says whether the app runs without
	// it).
	Requires map[string]map[string]Instance `yaml:"requires"`
	// File is the basename Load read it from: FileName, or
	// LegacyFileName for a chart released before the rename.
	File string `yaml:"-"`
	// Present is false when the chart ships no contract at all, and
	// Load returned the empty v1 contract. A caller that must tell "the
	// chart declares nothing" from "the chart declares this" reads this
	// field; a caller that only reads the blocks does not have to,
	// because every block of the empty contract is empty.
	Present bool `yaml:"-"`
}

type Secret struct {
	Values string `yaml:"values"`
	Keys   Keys   `yaml:"keys"`
}

// Key is one existingSecret entry. Exactly one of Generate / Template /
// External is set (the chart-side linter's job; here we take the first).
type Key struct {
	Name     string
	Generate string `yaml:"generate"`
	Template string `yaml:"template"`
	External string `yaml:"external"`
	Optional bool   `yaml:"optional"`
}

// Keys keeps the chart author's order (a map would sort it away).
type Keys []Key

func (k *Keys) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("secret.keys: expected a mapping")
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		var key Key
		if err := n.Content[i+1].Decode(&key); err != nil {
			return fmt.Errorf("secret.keys.%s: %w", n.Content[i].Value, err)
		}
		key.Name = n.Content[i].Value
		if (key.Generate != "") == (key.Template != "") && key.External == "" ||
			key.External != "" && (key.Generate != "" || key.Template != "") {
			return fmt.Errorf("secret.keys.%s: exactly one of generate/template/external", key.Name)
		}
		*k = append(*k, key)
	}
	return nil
}

// Instance is one required platform service, as far as the core reads
// it: whether the app runs without it.
type Instance struct {
	Optional bool `yaml:"optional"`
}

type HTTP struct {
	Service string `yaml:"service"`
	Port    int    `yaml:"port"`
}

type Metrics struct {
	Port     string            `yaml:"port"`
	Selector map[string]string `yaml:"selector"`
}

// Read returns dir's contract, the basename it came from, and any
// error. FileName wins; LegacyFileName is the fallback. When neither
// is there the error is the current name's os.ErrNotExist — callers
// test it with os.IsNotExist, and the name in it is the one to write.
func Read(dir string) ([]byte, string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, FileName))
	if err == nil {
		return raw, FileName, nil
	}
	if !os.IsNotExist(err) {
		return nil, "", err
	}
	if legacy, lerr := os.ReadFile(filepath.Join(dir, LegacyFileName)); lerr == nil {
		return legacy, LegacyFileName, nil
	}
	return nil, "", err
}

// Present reports whether dir carries a contract under either name.
func Present(dir string) bool {
	_, _, err := Read(dir)
	return err == nil
}

// Load reads dir's contract under either name (see Read).
//
// An absent contract is not an error. A chart that ships no contract
// declares nothing — no keys, no route — so Load returns the empty v1
// contract with Present false. A contract that IS there and does not
// parse, or does not say `contract: v1`, is still an error: absent and
// malformed are different facts.
func Load(chartDir string) (*Contract, error) {
	raw, name, err := Read(chartDir)
	if os.IsNotExist(err) {
		return &Contract{Version: "v1"}, nil
	}
	if err != nil {
		return nil, err
	}
	var c Contract
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	c.File, c.Present = name, true
	if c.Version != "v1" {
		return nil, fmt.Errorf("%s: contract: %q (want v1)", name, c.Version)
	}
	// The default values path is only meaningful when the chart
	// consumes a Secret. A contract with no keys declares no Secret,
	// and `fort add` must not write an `existingSecret` value that the
	// chart never reads.
	if c.Secret.Values == "" && len(c.Secret.Keys) > 0 {
		c.Secret.Values = "existingSecret"
	}
	return &c, nil
}

// Required lists the required services as "type.handle", sorted — what
// the report names when the core cannot resolve them. Optional ones are
// left out: the app runs without them.
func (c *Contract) Required() []string {
	var out []string
	for typ, handles := range c.Requires {
		for h, inst := range handles {
			if !inst.Optional {
				out = append(out, typ+"."+h)
			}
		}
	}
	sort.Strings(out)
	return out
}
