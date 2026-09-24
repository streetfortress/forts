// Package contract reads a chart's forts.contract.yaml — the
// environment-agnostic declaration of what a chart needs (docs/forts/fort-configuration-contracts.md):
// the keys its existingSecret must carry and how each is produced, the
// platform services it consumes and the values paths their endpoints
// wire into, the Service the fleet routes to, the metrics port.
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
	// about secrets (#464). Every chart released before the rename
	// ships this name inside its tarball, immutably, so Read keeps
	// accepting it — permanently, until someone audits every pin in
	// the fleet and deliberately drops it.
	LegacyFileName = "secrets.contract.yaml"
)

// Names is every filename a contract may have, in preference order.
var Names = []string{FileName, LegacyFileName}

// Types is the closed provider vocabulary, in render order.
var Types = []string{"postgres", "valkey", "s3"}

type Contract struct {
	Version  string                         `yaml:"contract"`
	Secret   Secret                         `yaml:"secret"`
	Requires map[string]map[string]Instance `yaml:"requires"`
	HTTP     *HTTP                          `yaml:"http"`
	Metrics  *Metrics                       `yaml:"metrics"`

	// File is the basename Load read it from: FileName, or
	// LegacyFileName for a chart released before the rename. It is
	// what lets `fort check` nudge such a chart.
	File string `yaml:"-"`
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

type Instance struct {
	Extensions []string          `yaml:"extensions"`
	Optional   bool              `yaml:"optional"`
	Wire       map[string]string `yaml:"wire"`
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
func Load(chartDir string) (*Contract, error) {
	raw, name, err := Read(chartDir)
	if err != nil {
		return nil, err
	}
	var c Contract
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	c.File = name
	if c.Version != "v1" {
		return nil, fmt.Errorf("%s: contract: %q (want v1)", name, c.Version)
	}
	if c.Secret.Values == "" {
		c.Secret.Values = "existingSecret"
	}
	for typ := range c.Requires {
		if !contains(Types, typ) {
			return nil, fmt.Errorf("%s: requires.%s: unknown provider type (known: %v)", name, typ, Types)
		}
	}
	return &c, nil
}

// Instances returns (type, handle, instance) triples in a stable order.
func (c *Contract) Instances() []Ref {
	var out []Ref
	for _, typ := range Types {
		handles := make([]string, 0, len(c.Requires[typ]))
		for h := range c.Requires[typ] {
			handles = append(handles, h)
		}
		sort.Strings(handles)
		for _, h := range handles {
			out = append(out, Ref{typ, h, c.Requires[typ][h]})
		}
	}
	return out
}

type Ref struct {
	Type, Handle string
	Instance     Instance
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
