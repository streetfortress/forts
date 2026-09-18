package main

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Parity with the committed fleet: every full-form contract parses, and
// every Secret file it describes that exists on disk carries at least
// the contract's non-external keys (key names are plaintext under the
// repo's encrypted_regex — no decryption). Extras are allowed (migration
// supersets) and logged. This is what lets the python seeder retire.
func TestFullFormParityWithFleet(t *testing.T) {
	repo, err := repoRootFrom(mustGetwd(t))
	if err != nil {
		t.Skip("not inside the deployments checkout")
	}
	contracts, _ := filepath.Glob(filepath.Join(repo, "fleet/apps/*/*/secrets.contract.yaml"))
	if len(contracts) == 0 {
		t.Fatal("no contracts found")
	}
	for _, path := range contracts {
		rel, _ := filepath.Rel(repo, path)
		raw := readFile(t, path)
		if !isFullForm([]byte(raw)) {
			t.Logf("%s: overlay form, skipped", rel)
			continue
		}
		ff, err := parseFullForm([]byte(raw))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		for _, sf := range ff.Secrets {
			file := filepath.Join(filepath.Dir(path), sf.File)
			if _, err := os.Stat(file); err != nil {
				t.Logf("%s: %s not seeded yet", rel, sf.File)
				continue
			}
			names, err := secretKeyNames(file)
			if err != nil {
				t.Errorf("%s: %v", file, err)
				continue
			}
			have := map[string]bool{}
			for _, n := range names {
				have[n] = true
			}
			want := map[string]bool{}
			for _, k := range sf.Keys {
				if k.Spec.External != "" {
					continue
				}
				want[k.Name] = true
				if !have[k.Name] {
					t.Errorf("%s: %s lacks contract key %s", rel, sf.File, k.Name)
				}
			}
			var extra []string
			for n := range have {
				if !want[n] {
					extra = append(extra, n)
				}
			}
			if len(extra) > 0 {
				sort.Strings(extra)
				t.Logf("%s: %s carries extra keys %v", rel, sf.File, extra)
			}
		}
	}
}

// fakeTool records what the seeder would do with sops, and serves
// fixture plaintext for decrypts. Nothing here is encrypted; the test
// only ever inspects key names and the recorded calls.
type fakeTool struct {
	plain     map[string]string // path → decrypted content
	encrypted []string
	sets      [][3]string
	added     []string
	unset     []string
	rekeyed   []string
}

func (f *fakeTool) Decrypt(path string) ([]byte, error) {
	if s, ok := f.plain[path]; ok {
		return []byte(s), nil
	}
	// The fake never encrypts, so a file this tool "encrypted" is still
	// plaintext on disk — a re-run reads back what the first run wrote.
	if raw, err := os.ReadFile(path); err == nil {
		for _, e := range f.encrypted {
			if e == path {
				return raw, nil
			}
		}
	}
	return nil, os.ErrNotExist
}
func (f *fakeTool) EncryptInPlace(path string) error {
	f.encrypted = append(f.encrypted, path)
	return nil
}
func (f *fakeTool) Set(path, key, value string) error {
	f.sets = append(f.sets, [3]string{path, key, value})
	return nil
}
func (f *fakeTool) EncryptWith(config, path string) error {
	f.encrypted = append(f.encrypted, path)
	return nil
}
func (f *fakeTool) SetWith(config, path, jsonPath, value string) error {
	f.sets = append(f.sets, [3]string{path, jsonPath, value})
	return nil
}
func (f *fakeTool) UnsetWith(config, path, jsonPath string) error {
	f.unset = append(f.unset, path+" "+jsonPath)
	return nil
}
func (f *fakeTool) GitAdd(path string) error { f.added = append(f.added, path); return nil }
func (f *fakeTool) UpdateKeys(path string) error {
	f.rekeyed = append(f.rekeyed, path)
	return nil
}

func TestSeedFullForm(t *testing.T) {
	const fort = "lab-fort" // testdata manifest — no real fort involved
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, fort)
	apps := filepath.Join(tmp, "fleet/apps", fort)
	write := func(rel, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(apps, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(apps, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// A provider dir (new file) and an app dir referencing it (new file
	// with every primitive), plus an existing provider secret that is
	// missing a key.
	write("valkey/secrets.contract.yaml", "secrets:\n  secrets.sops.yaml:\n    metadata: {name: valkey-users, namespace: valkey}\n    keys:\n      default: {generate: hex24}\n      app: {generate: hex24}\n")
	write("valkey/secrets.sops.yaml", "apiVersion: v1\nkind: Secret\nmetadata: {name: valkey-users, namespace: valkey}\nstringData:\n  default: ENC[...]\nsops: {}\n")
	write("sites-db/secrets.contract.yaml", "secrets:\n  secrets-app.sops.yaml:\n    metadata: {name: sites-db-app-credentials, namespace: sites-db, type: kubernetes.io/basic-auth}\n    keys:\n      username: {literal: app}\n      password: {generate: hex24}\n")
	write("app/secrets.contract.yaml", `sources:
  legacy: `+tmp+`/legacy.env
vars:
  db_password: {sops: [../sites-db/secrets-app.sops.yaml, stringData.password]}
secrets:
  secrets.sops.yaml:
    metadata:
      name: app-env
      namespace: app
    keys:
      SESSION: {generate: b64_32}
      DATABASE_URL: {template: "postgresql://app:${db_password}@sites-db-rw:5432/app"}
      MAIL_KEY: {env: [legacy, MAIL_KEY]}
      CACHE_PASSWORD: {sops: [../valkey/secrets.sops.yaml, stringData.app]}
      STRIPE_KEY: {external: "Stripe dashboard"}
      OPTIONAL_KEY: {external: "somewhere", optional: true}
`)
	tool := &fakeTool{plain: map[string]string{
		filepath.Join(apps, "sites-db/secrets-app.sops.yaml"): "stringData:\n  username: app\n  password: dbpw\n",
		filepath.Join(apps, "valkey/secrets.sops.yaml"):       "stringData:\n  default: x\n  app: cachepw\n",
		filepath.Join(tmp, "legacy.env"):                      "# legacy\nMAIL_KEY=\"re_123\"\nOTHER=1\n",
	}}
	o := seedOpts{root: tmp, fort: fort, tool: tool, write: true}
	plans, err := planSeed(o)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, p := range plans {
		order = append(order, filepath.Base(p.dir))
	}
	// providers first (the app's sops: refs point at both), alphabetical among peers
	if want := []string{"sites-db", "valkey", "app"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order: want %v, got %v", want, order)
	}
	for _, p := range plans {
		if err := p.execute(o); err != nil {
			t.Fatalf("%s: %v", p.dir, err)
		}
	}
	// valkey: existed, missing `app` → one sops set, no rewrite
	if len(tool.sets) != 1 || tool.sets[0][1] != "app" || !strings.HasSuffix(tool.sets[0][0], "valkey/secrets.sops.yaml") {
		t.Errorf("valkey add-missing-keys: sets = %v", tool.sets)
	}
	// sites-db + app: created, encrypted in place, git added
	if len(tool.encrypted) != 2 || len(tool.added) != 2 {
		t.Errorf("created files: encrypted %v added %v", tool.encrypted, tool.added)
	}
	names, err := secretKeyNames(filepath.Join(apps, "app/secrets.sops.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"CACHE_PASSWORD", "DATABASE_URL", "MAIL_KEY", "SESSION"}; !reflect.DeepEqual(names, want) {
		t.Errorf("app keys: want %v (externals absent), got %v", want, names)
	}
	raw := readFile(t, filepath.Join(apps, "app/secrets.sops.yaml"))
	for _, want := range []string{`DATABASE_URL: "postgresql://app:dbpw@sites-db-rw:5432/app"`, `MAIL_KEY: "re_123"`, `CACHE_PASSWORD: "cachepw"`, "# Seeded by `fort seed`"} {
		if !strings.Contains(raw, want) {
			t.Errorf("app secret lacks %q:\n%s", want, raw)
		}
	}
	info, _ := os.Stat(filepath.Join(apps, "app/secrets.sops.yaml"))
	if info.Mode().Perm() != 0o600 {
		t.Errorf("new file mode %o, want 0600", info.Mode().Perm())
	}
	// report never carries a value
	var sb strings.Builder
	for _, p := range plans {
		p.report(&sb, true)
	}
	for _, leak := range []string{"dbpw", "cachepw", "re_123"} {
		if strings.Contains(sb.String(), leak) {
			t.Errorf("report leaks a value: %s", leak)
		}
	}
	if !strings.Contains(sb.String(), "absent until set: STRIPE_KEY") || !strings.Contains(sb.String(), "(optional): OPTIONAL_KEY") {
		t.Errorf("report should list absent externals:\n%s", sb.String())
	}

	// --set on an external key of an existing file
	o2 := seedOpts{root: tmp, fort: fort, tool: tool, write: true, dirs: []string{filepath.Join("fleet/apps", fort, "app")}, setKey: "STRIPE_KEY", setValue: "sk_test_x"}
	plans, err = planSeed(o2)
	if err != nil {
		t.Fatal(err)
	}
	if err := plans[0].execute(o2); err != nil {
		t.Fatal(err)
	}
	last := tool.sets[len(tool.sets)-1]
	if last[1] != "STRIPE_KEY" || last[2] != "sk_test_x" {
		t.Errorf("--set: %v", last)
	}
}

// The on-the-fly path: no fort-side full form, the chart's contract
// (testdata fixture) + the fort's providers give the same key set the
// hand-written drill fort contract has (minus its migration-era
// ADMIN_EMAILS), with the same provider vars.
func TestSeedFromChartContract(t *testing.T) {
	const fort = "sfi-fort-e33e2d5" // the testdata/golden snapshot; the fort itself is gone (#348)
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, fort)
	golden := filepath.Join("testdata/golden", fort)
	for _, rel := range []string{"symbology/repository.yaml", "symbology/release.yaml", "sites-db/secrets.contract.yaml", "valkey/secrets.contract.yaml"} {
		copyFile(t, filepath.Join(golden, rel), filepath.Join(tmp, "fleet/apps", fort, rel))
	}
	o := seedOpts{root: tmp, fort: fort, tool: &fakeTool{}, chartDir: "testdata/charts/symbology", dirs: []string{filepath.Join("fleet/apps", fort, "symbology")}}
	plans, err := planSeed(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || !strings.HasPrefix(plans[0].source, "chart contract") {
		t.Fatalf("plans: %+v", plans)
	}
	got := names(plans[0].ff.Secrets[0].Keys)
	hand, err := parseFullForm([]byte(readFile(t, filepath.Join(golden, "symbology/secrets.contract.yaml"))))
	if err != nil {
		t.Fatal(err)
	}
	want := names(hand.Secrets[0].Keys)
	sort.Strings(got)
	sort.Strings(want)
	want = remove(want, "ADMIN_EMAILS")
	if !reflect.DeepEqual(got, want) {
		t.Errorf("keys: want %v, got %v", want, got)
	}
	var gotVars, wantVars []string
	for _, v := range plans[0].ff.Vars {
		gotVars = append(gotVars, v.Name+"="+v.Spec.YAML())
	}
	for _, v := range hand.Vars {
		wantVars = append(wantVars, v.Name+"="+v.Spec.YAML())
	}
	sort.Strings(gotVars)
	sort.Strings(wantVars)
	if !reflect.DeepEqual(gotVars, wantVars) {
		t.Errorf("vars: want %v, got %v", wantVars, gotVars)
	}
}

func TestReadChartPin(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "r.yaml"), []byte("kind: OCIRepository\nspec:\n  url: oci://x/y\n  ref:\n    semver: \">=0.0.0-0\"\n    semverFilter: '^0\\.0\\.0-pr\\.189\\.'\n"), 0o644)
	pin, err := readChartPin(filepath.Join(dir, "r.yaml"))
	if err != nil || pin == nil || pin.followPR != "189" || pin.url != "oci://x/y" {
		t.Errorf("follow-pr pin: %+v %v", pin, err)
	}
	os.WriteFile(filepath.Join(dir, "t.yaml"), []byte("kind: OCIRepository\nspec:\n  url: oci://x/y\n  ref:\n    tag: \"0.1.6\"\n"), 0o644)
	pin, _ = readChartPin(filepath.Join(dir, "t.yaml"))
	if pin == nil || pin.version != "0.1.6" {
		t.Errorf("tag pin: %+v", pin)
	}
}

func remove(list []string, s string) []string {
	var out []string
	for _, x := range list {
		if x != s {
			out = append(out, x)
		}
	}
	return out
}

// Review findings (ultrareview on #237): the edge cases that used to be
// silent — a null in a provider file, a positional dir after a flag,
// a typo'd --set key on a multi-file dir, a typo'd dir, and a
// non-lowercase optional flag.
func TestSeedEdgeCases(t *testing.T) {
	const fort = "lab-fort"
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, fort)
	apps := filepath.Join(tmp, "fleet/apps", fort)
	write := func(rel, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(apps, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(apps, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("sites-db/secrets.contract.yaml", "secrets:\n  secrets-a.sops.yaml:\n    metadata: {name: a, namespace: sites-db}\n    keys:\n      password: {generate: hex24}\n  secrets-b.sops.yaml:\n    metadata: {name: b, namespace: sites-db}\n    keys:\n      password: {generate: hex24}\n")
	write("sites-db/secrets-a.sops.yaml", "stringData:\n  password: ENC\n")
	write("sites-db/secrets-b.sops.yaml", "stringData:\n  password: ENC\n")
	write("app/secrets.contract.yaml", "vars:\n  pw: {sops: [../sites-db/secrets-a.sops.yaml, stringData.password]}\nsecrets:\n  secrets.sops.yaml:\n    metadata: {name: app-env, namespace: app}\n    keys:\n      URL: {template: \"x://u:${pw}@h/db\"}\n")

	// null provider value → error, never "<nil>" in a URL
	tool := &fakeTool{plain: map[string]string{filepath.Join(apps, "sites-db/secrets-a.sops.yaml"): "stringData:\n  password:\n"}}
	o := seedOpts{root: tmp, fort: fort, tool: tool, write: true, dirs: []string{filepath.Join("fleet/apps", fort, "app")}}
	plans, err := planSeed(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := plans[0].execute(o); err == nil || !strings.Contains(err.Error(), "is null") {
		t.Errorf("null sops value: want an 'is null' error, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(apps, "app/secrets.sops.yaml")); err == nil {
		t.Errorf("a Secret was written despite the null value")
	}

	// --set with a typo'd key on a two-file dir → error, not exit 0
	o = seedOpts{root: tmp, fort: fort, tool: tool, write: true, dirs: []string{filepath.Join("fleet/apps", fort, "sites-db")}, setKey: "pasword", setValue: "x"}
	if _, err := planSeed(o); err == nil || !strings.Contains(err.Error(), "not a key of any Secret") {
		t.Errorf("--set typo: want an error, got %v", err)
	}

	// an explicitly named dir with nothing to seed → error
	write("gateway/kustomization.yaml", "resources: []\n")
	o = seedOpts{root: tmp, fort: fort, tool: tool, dirs: []string{"gateway"}} // bare name = fleet/apps/<fort>/gateway
	if _, err := planSeed(o); err == nil || !strings.Contains(err.Error(), "nothing to seed") {
		t.Errorf("typo'd dir: want an error, got %v", err)
	}
	// ...but auto-enumeration still skips it
	if _, err := planSeed(seedOpts{root: tmp, fort: fort, tool: tool}); err != nil {
		t.Errorf("auto-enumeration should skip gateway/: %v", err)
	}

	// optional: True / yes
	if s, err := parseSpec(yamlNode(t, "{external: x, optional: True}")); err != nil || !s.Optional {
		t.Errorf("optional: True → %v %v", s.Optional, err)
	}
	// yaml.v3 decodes the YAML 1.1 bool set into a bool field — either
	// way the flag is honoured or rejected, never silently false.
	if s, err := parseSpec(yamlNode(t, "{external: x, optional: yes}")); err == nil && !s.Optional {
		t.Errorf("optional: yes decoded without error but false")
	}
	if _, err := parseSpec(yamlNode(t, "{external: x, optional: maybe}")); err == nil {
		t.Errorf("optional: maybe should be rejected")
	}
}

func yamlNode(t *testing.T, s string) *yaml.Node {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Content[0]
}

func TestAppDir(t *testing.T) {
	apps := "fleet/apps/f"
	for in, want := range map[string]string{
		"sites-db":                  "fleet/apps/f/sites-db",
		"sites-db/":                 "fleet/apps/f/sites-db",
		"./sites-db":                "fleet/apps/f/sites-db",
		"fleet/apps/f/sites-db":     "fleet/apps/f/sites-db",
		"fleet/apps/f/sites-db/":    "fleet/apps/f/sites-db",
		"fleet/apps/other/sites-db": "fleet/apps/other/sites-db", // caller's prefix check rejects it
	} {
		if got := appDir(apps, in); got != want {
			t.Errorf("appDir(%q) = %q, want %q", in, got, want)
		}
	}
}

// --set's value comes after the plan, and from stdin when there is no
// terminal — the scripted path, which must keep working.
func TestReadSetValueFromStdin(t *testing.T) {
	stdinIsTTY = func() bool { return false }
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
	w.WriteString("sk_test_x\n")
	w.Close()
	v, err := readSetValue("STRIPE_KEY", "fleet/apps/x/app/secrets.sops.yaml")
	if err != nil || v != "sk_test_x" {
		t.Fatalf("got %q, %v", v, err)
	}
}

func TestReadSetValueRefusesEmpty(t *testing.T) {
	stdinIsTTY = func() bool { return false }
	r, w, _ := os.Pipe()
	old := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = old })
	w.WriteString("\n")
	w.Close()
	if _, err := readSetValue("K", "f"); err == nil {
		t.Fatal("an empty value must be refused, not encrypted")
	}
}
