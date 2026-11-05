package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sample = `sources:
  legacy: /tmp/legacy.env
vars:
  pw: {sops: [../db/secrets.sops.yaml, stringData.password]}
secrets:
  secrets.sops.yaml:
    metadata: {name: app-env, namespace: app, type: kubernetes.io/basic-auth}
    keys:
      A: {generate: hex24}
      B: {literal: "x: y"}
      C: {template: "u:${pw}"}
      D: {external: "somewhere", optional: true}
      E: {env: [legacy, E]}
`

// The filename is the schema: Load reads forts.inventory.yaml and nothing
// else, an absent file is nil, and the order of the keys is the
// author's.
func TestLoad(t *testing.T) {
	dir := t.TempDir()
	if inv, err := Load(dir); inv != nil || err != nil {
		t.Fatalf("absent: %+v %v", inv, err)
	}
	if Present(dir) {
		t.Error("Present on an empty dir")
	}
	os.WriteFile(filepath.Join(dir, FileName), []byte(sample), 0o644)
	inv, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if inv.Sources["legacy"] != "/tmp/legacy.env" || len(inv.Vars) != 1 || inv.Vars[0].Name != "pw" {
		t.Errorf("sources/vars: %+v", inv)
	}
	sf := inv.Secrets[0]
	if sf.File != "secrets.sops.yaml" || sf.Name != "app-env" || sf.Namespace != "app" || sf.Type != "kubernetes.io/basic-auth" {
		t.Errorf("secret file: %+v", sf)
	}
	var names, specs []string
	for _, k := range sf.Keys {
		names = append(names, k.Name)
		specs = append(specs, k.Spec.YAML())
	}
	if got := strings.Join(names, ","); got != "A,B,C,D,E" {
		t.Errorf("key order: %s", got)
	}
	if specs[1] != `{literal: "x: y"}` || specs[3] != `{external: "somewhere", optional: true}` || specs[4] != "{env: [legacy, E]}" {
		t.Errorf("round trip: %v", specs)
	}
}

// Metadata is required, a spec has exactly one kind, and a block the
// schema does not have — a chart contract's `contract: v1` — is
// refused by name.
func TestParseRefusals(t *testing.T) {
	for name, body := range map[string]string{
		"no metadata": "secrets:\n  f.yaml:\n    keys:\n      A: {generate: hex8}\n",
		"two kinds":   "secrets:\n  f.yaml:\n    metadata: {name: a, namespace: a}\n    keys:\n      A: {generate: hex8, literal: x}\n",
		"a contract":  "contract: v1\nsecret:\n  keys:\n    A: {generate: hex8}\n",
		"not a map":   "- a\n",
	} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
