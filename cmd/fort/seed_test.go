package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/inventory"
	"gopkg.in/yaml.v3"
)

// Every primitive of the inventory, across three dirs: a source dir a
// layer rendered (forts/postgres, forts/valkey — the path is the
// layer's choice, the file is the core's), and an app dir whose sops:
// refs point into both. Providers seed first, the order derived from
// the refs; an existing file gains its missing key by `sops set` and is
// never rewritten; a new file is written 0600, encrypted in place and
// staged; externals stay absent; the report never carries a value.
func TestSeedInventory(t *testing.T) {
	const fort = "lab-fort"
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, fort)
	apps := filepath.Join(tmp, "flux/apps", fort)
	write := func(rel, content string) { writeFile(t, apps, rel, content) }
	write("forts/valkey/forts.inventory.yaml", "secrets:\n  secrets.sops.yaml:\n    metadata: {name: valkey-users, namespace: forts}\n    keys:\n      default: {generate: hex24}\n      app: {generate: hex24}\n")
	write("forts/valkey/secrets.sops.yaml", "apiVersion: v1\nkind: Secret\nmetadata: {name: valkey-users, namespace: forts}\nstringData:\n  default: ENC[...]\nsops: {}\n")
	write("forts/postgres/forts.inventory.yaml", "secrets:\n  secrets-app.sops.yaml:\n    metadata: {name: postgres-app-credentials, namespace: forts, type: kubernetes.io/basic-auth}\n    keys:\n      username: {literal: app}\n      password: {generate: hex24}\n")
	write("app/forts.inventory.yaml", `sources:
  legacy: `+tmp+`/legacy.env
vars:
  db_password: {sops: [../forts/postgres/secrets-app.sops.yaml, stringData.password]}
secrets:
  secrets.sops.yaml:
    metadata:
      name: app-env
      namespace: app
    keys:
      SESSION: {generate: b64_32}
      DATABASE_URL: {template: "postgresql://app:${db_password}@postgres-rw:5432/app"}
      MAIL_KEY: {env: [legacy, MAIL_KEY]}
      CACHE_PASSWORD: {sops: [../forts/valkey/secrets.sops.yaml, stringData.app]}
      STRIPE_KEY: {external: "Stripe dashboard"}
      OPTIONAL_KEY: {external: "somewhere", optional: true}
`)
	tool := &fakeTool{plain: map[string]string{
		filepath.Join(apps, "forts/postgres/secrets-app.sops.yaml"): "stringData:\n  username: app\n  password: dbpw\n",
		filepath.Join(apps, "forts/valkey/secrets.sops.yaml"):       "stringData:\n  default: x\n  app: cachepw\n",
		filepath.Join(tmp, "legacy.env"):                            "# legacy\nMAIL_KEY=\"re_123\"\nOTHER=1\n",
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
	if want := []string{"postgres", "valkey", "app"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("order: want %v, got %v", want, order)
	}
	for _, p := range plans {
		if err := p.execute(o); err != nil {
			t.Fatalf("%s: %v", p.dir, err)
		}
	}
	// valkey: existed, missing `app` → one sops set, no rewrite
	if len(tool.sets) != 1 || tool.sets[0][1] != "app" || !strings.HasSuffix(tool.sets[0][0], "forts/valkey/secrets.sops.yaml") {
		t.Errorf("valkey add-missing-keys: sets = %v", tool.sets)
	}
	// forts/postgres + app: created, encrypted in place, git added
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
	for _, want := range []string{`DATABASE_URL: "postgresql://app:dbpw@postgres-rw:5432/app"`, `MAIL_KEY: "re_123"`, `CACHE_PASSWORD: "cachepw"`, "# Seeded by `fort seed`"} {
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

	// The gate agrees once the fake's `sops set` is on disk (the fake
	// records the call and writes nothing): the required external is
	// the one error, the optional one a warning, the sources are ok.
	write("forts/valkey/secrets.sops.yaml", "stringData:\n  default: ENC\n  app: ENC\n")
	findings := inventoryFindings(tmp, fort)
	if got := levelsOf(findings); got != "error,warning,ok,ok" {
		t.Errorf("check after seed: %s\n%+v", got, findings)
	}

	// --set on an external key of an existing file
	o2 := seedOpts{root: tmp, fort: fort, tool: tool, write: true, dirs: []string{"app"}, setKey: "STRIPE_KEY", setValue: "sk_test_x"}
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

// The on-the-fly path: no committed inventory, so the chart's own
// contract (the testdata fixture, at the dir's pin) gives the key set.
// A key that composes a provider output is the layer's to resolve: with
// no overlay the seed refuses and names it; an overlay that names the
// key — a sops ref into the file the layer seeded — makes the dir
// seedable, and the overlay's own key is appended.
func TestSeedFromChartContract(t *testing.T) {
	const fort = "lab-fort"
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, fort)
	apps := filepath.Join(tmp, "flux/apps", fort)
	writeFile(t, apps, "gallery/repository.yaml", "kind: OCIRepository\nspec:\n  url: oci://x/gallery\n  ref:\n    tag: \"0.2.0\"\n")
	dir := filepath.Join("flux/apps", fort, "gallery")
	o := seedOpts{root: tmp, fort: fort, tool: &fakeTool{}, chartDir: "testdata/charts/gallery", dirs: []string{"gallery"}}
	if _, err := planSeed(o); err == nil || !strings.Contains(err.Error(), "DATABASE_URL (postgres.db)") || !strings.Contains(err.Error(), "sfi/fort-layers") {
		t.Errorf("a template key with no overlay: want the layer's name in the error, got %v", err)
	}

	writeFile(t, apps, "gallery/forts.contract.yaml", "contract: v1\nsecret:\n  keys:\n    DATABASE_URL: {sops: [../forts/postgres/secrets-gallery.sops.yaml, stringData.url]}\n    ADMIN_EMAILS: {literal: ops@example}\n")
	plans, err := planSeed(o)
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || plans[0].dir != dir || !strings.HasPrefix(plans[0].source, "chart contract") || !strings.HasSuffix(plans[0].source, "+ overlay") {
		t.Fatalf("plans: %+v", plans)
	}
	got := names(plans[0].inv.Secrets[0].Keys)
	if want := []string{"APP_SECRET", "DATABASE_URL", "ADMIN_EMAILS"}; !reflect.DeepEqual(got, want) {
		t.Errorf("keys: want %v, got %v", want, got)
	}
	if s := plans[0].inv.Secrets[0].Keys[1].Spec; s.Sops == nil {
		t.Errorf("DATABASE_URL: the overlay's sops ref should replace the chart's template, got %s", s.YAML())
	}
	if sf := plans[0].inv.Secrets[0]; sf.Name != "gallery-env" || sf.Namespace != "gallery" {
		t.Errorf("Secret metadata: %+v", sf)
	}
}

// An inventory that still carries a chart's verbatim template (what
// `fort add` writes for a layered chart, before the layer rewrites it)
// is not seedable, and the error names the layer rather than a syntax
// problem.
func TestSeedRefusesUnrewrittenTemplate(t *testing.T) {
	const fort = "lab-fort"
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, fort)
	apps := filepath.Join(tmp, "flux/apps", fort)
	writeFile(t, apps, "gallery/forts.inventory.yaml", "secrets:\n  secrets.sops.yaml:\n    metadata: {name: gallery-env, namespace: gallery}\n    keys:\n      DATABASE_URL: {template: \"postgresql://x:${postgres.db.password}@h/db\"}\n")
	o := seedOpts{root: tmp, fort: fort, tool: &fakeTool{}, write: true}
	plans, err := planSeed(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := plans[0].execute(o); err == nil || !strings.Contains(err.Error(), "postgres.db") || !strings.Contains(err.Error(), "layer") {
		t.Errorf("want an error naming the provider output and the layer, got %v", err)
	}
	if exists(tmp, filepath.Join("flux/apps", fort, "gallery/secrets.sops.yaml")) {
		t.Error("a Secret was written despite the unresolved key")
	}
}

func TestReadChartPin(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "r.yaml"), []byte("kind: OCIRepository\nspec:\n  url: oci://x/y\n  ref:\n    semver: \">=0.0.0-0\"\n    semverFilter: '^0\\.0\\.0-pr\\.189\\.'\n"), 0o644)
	pin, err := readChartPin(filepath.Join(dir, "r.yaml"))
	if err != nil || pin == nil || pin.followPR != "189" || pin.url != "oci://x/y" {
		t.Errorf("follow-pr pin: %+v %v", pin, err)
	}
	os.WriteFile(filepath.Join(dir, "m.yaml"), []byte("kind: OCIRepository\nspec:\n  url: oci://x/y\n  ref:\n    semver: \">=0.0.0-0\"\n    semverFilter: '^0\\.0\\.0-main\\.'\n"), 0o644)
	pin, err = readChartPin(filepath.Join(dir, "m.yaml"))
	if err != nil || pin == nil || !pin.followMain || pin.followPR != "" {
		t.Errorf("follow-main pin: %+v %v", pin, err)
	}
	os.WriteFile(filepath.Join(dir, "t.yaml"), []byte("kind: OCIRepository\nspec:\n  url: oci://x/y\n  ref:\n    tag: \"0.1.6\"\n"), 0o644)
	pin, _ = readChartPin(filepath.Join(dir, "t.yaml"))
	if pin == nil || pin.version != "0.1.6" {
		t.Errorf("tag pin: %+v", pin)
	}
	if pin, err := readChartPin(filepath.Join(dir, "none.yaml")); pin != nil || err != nil {
		t.Errorf("no file: %+v %v", pin, err)
	}
}

// The edge cases that must not be silent: a null in a source file, a
// typo'd --set key on a multi-file dir, a key two Secrets share, a
// typo'd dir, and a non-lowercase optional flag.
func TestSeedEdgeCases(t *testing.T) {
	const fort = "lab-fort"
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, fort)
	apps := filepath.Join(tmp, "flux/apps", fort)
	write := func(rel, content string) { writeFile(t, apps, rel, content) }
	write("forts/postgres/forts.inventory.yaml", "secrets:\n  secrets-a.sops.yaml:\n    metadata: {name: a, namespace: forts}\n    keys:\n      password: {generate: hex24}\n  secrets-b.sops.yaml:\n    metadata: {name: b, namespace: forts}\n    keys:\n      password: {generate: hex24}\n")
	write("forts/postgres/secrets-a.sops.yaml", "stringData:\n  password: ENC\n")
	write("forts/postgres/secrets-b.sops.yaml", "stringData:\n  password: ENC\n")
	write("app/forts.inventory.yaml", "vars:\n  pw: {sops: [../forts/postgres/secrets-a.sops.yaml, stringData.password]}\nsecrets:\n  secrets.sops.yaml:\n    metadata: {name: app-env, namespace: app}\n    keys:\n      URL: {template: \"x://u:${pw}@h/db\"}\n")

	// null source value → error, never "<nil>" in a URL
	tool := &fakeTool{plain: map[string]string{filepath.Join(apps, "forts/postgres/secrets-a.sops.yaml"): "stringData:\n  password:\n"}}
	o := seedOpts{root: tmp, fort: fort, tool: tool, write: true, dirs: []string{"app"}}
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
	o = seedOpts{root: tmp, fort: fort, tool: tool, write: true, dirs: []string{"forts/postgres"}, setKey: "pasword", setValue: "x"}
	if _, err := planSeed(o); err == nil || !strings.Contains(err.Error(), "not a key of any Secret") {
		t.Errorf("--set typo: want an error, got %v", err)
	}

	// --set with a key two Secrets of the dir share → refused, naming
	// the qualified form; FILE/KEY then lands in that one file only
	o = seedOpts{root: tmp, fort: fort, tool: tool, write: true, dirs: []string{"forts/postgres"}, setKey: "password", setValue: "x"}
	if _, err := planSeed(o); err == nil || !strings.Contains(err.Error(), "--set secrets-a.sops.yaml/password") {
		t.Errorf("--set shared key: want the ambiguity error, got %v", err)
	}
	o.setFile = "secrets-b.sops.yaml"
	plans, err = planSeed(o)
	if err != nil {
		t.Fatalf("--set FILE/KEY: %v", err)
	}
	var targets []string
	for _, f := range plans[0].files {
		if f.set != nil {
			targets = append(targets, f.sf.File)
		}
	}
	if len(targets) != 1 || targets[0] != "secrets-b.sops.yaml" {
		t.Errorf("--set FILE/KEY: want only secrets-b.sops.yaml targeted, got %v", targets)
	}
	o.setFile = "secrets-c.sops.yaml"
	if _, err := planSeed(o); err == nil || !strings.Contains(err.Error(), "--set secrets-c.sops.yaml/password: not a key") {
		t.Errorf("--set FILE/KEY with a wrong file: want a not-a-key error, got %v", err)
	}
	for _, c := range []struct{ arg, file, key string }{{"KEY", "", "KEY"}, {"a.sops.yaml/KEY", "a.sops.yaml", "KEY"}, {"secret.key", "", "secret.key"}} {
		if f, k := splitSetArg(c.arg); f != c.file || k != c.key {
			t.Errorf("splitSetArg(%q) = %q, %q; want %q, %q", c.arg, f, k, c.file, c.key)
		}
	}

	// an explicitly named dir with nothing to seed → error
	write("gateway/kustomization.yaml", "resources: []\n")
	o = seedOpts{root: tmp, fort: fort, tool: tool, dirs: []string{"gateway"}}
	if _, err := planSeed(o); err == nil || !strings.Contains(err.Error(), "nothing to seed") {
		t.Errorf("typo'd dir: want an error, got %v", err)
	}
	// ...but auto-enumeration still skips it
	if _, err := planSeed(seedOpts{root: tmp, fort: fort, tool: tool}); err != nil {
		t.Errorf("auto-enumeration should skip gateway/: %v", err)
	}
	// a dir that escapes the fort's apps root is refused
	if _, err := planSeed(seedOpts{root: tmp, fort: fort, tool: tool, dirs: []string{"../other/app"}}); err == nil || !strings.Contains(err.Error(), "is not under") {
		t.Errorf("another fort's dir: want a refusal, got %v", err)
	}

	// optional: True / yes
	if s, err := inventory.ParseSpec(yamlNode(t, "{external: x, optional: True}")); err != nil || !s.Optional {
		t.Errorf("optional: True → %v %v", s.Optional, err)
	}
	if s, err := inventory.ParseSpec(yamlNode(t, "{external: x, optional: yes}")); err == nil && !s.Optional {
		t.Errorf("optional: yes decoded without error but false")
	}
	if _, err := inventory.ParseSpec(yamlNode(t, "{external: x, optional: maybe}")); err == nil {
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

// A <dir> is relative to the fort's apps root — an app's name, or the
// path a layer renders beneath it — unless it already is the
// repo-relative path.
func TestAppDir(t *testing.T) {
	apps := "flux/apps/f"
	for in, want := range map[string]string{
		"symbology":                      "flux/apps/f/symbology",
		"symbology/":                     "flux/apps/f/symbology",
		"./symbology":                    "flux/apps/f/symbology",
		"forts/postgres":                 "flux/apps/f/forts/postgres",
		"forts/postgres/":                "flux/apps/f/forts/postgres",
		"flux/apps/f/forts/postgres":     "flux/apps/f/forts/postgres",
		"flux/apps/f/symbology/":         "flux/apps/f/symbology",
		"flux/apps/other/forts/postgres": "flux/apps/f/flux/apps/other/forts/postgres", // the caller's prefix check rejects it
	} {
		if got := appDir(apps, in); got != want {
			t.Errorf("appDir(%q) = %q, want %q", in, got, want)
		}
	}
}

// inventoryDirs walks the whole apps root: the app dirs the core
// renders (an inventory, an overlay, or a pinned chart), and whatever a
// layer renders beneath them.
func TestInventoryDirs(t *testing.T) {
	tmp := t.TempDir()
	apps := "flux/apps/f"
	writeFile(t, tmp, apps+"/app/forts.inventory.yaml", "secrets: {}\n")
	writeFile(t, tmp, apps+"/overlaid/forts.contract.yaml", "contract: v1\n")
	writeFile(t, tmp, apps+"/pinned/repository.yaml", "kind: OCIRepository\n")
	writeFile(t, tmp, apps+"/gateway/kustomization.yaml", "resources: []\n")
	writeFile(t, tmp, apps+"/forts/postgres/forts.inventory.yaml", "secrets: {}\n")
	writeFile(t, tmp, apps+"/forts/namespace.yaml", "kind: Namespace\n")
	got, err := inventoryDirs(tmp, apps)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{apps + "/app", apps + "/forts/postgres", apps + "/overlaid", apps + "/pinned"}; !reflect.DeepEqual(got, want) {
		t.Errorf("inventoryDirs = %v, want %v", got, want)
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
	v, err := readSetValue("STRIPE_KEY", "flux/apps/x/app/secrets.sops.yaml")
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

// The secret-less shape downstream of `fort add`: the app dir carries
// no inventory and no secrets.sops.yaml, and the chart it
// pins declares no keys. `fort seed` must plan no Secret, and `fort
// check` must not demand one.
func TestSecretlessAppNeedsNoSeed(t *testing.T) {
	const fort = "lab-fort"
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, fort)
	copyTree(t, "testdata/golden/secretless/moment-tally-website", filepath.Join(tmp, "flux/apps", fort, "moment-tally-website"))

	chartDir := "testdata/charts/moment-tally-website"
	plans, err := planSeed(seedOpts{root: tmp, fort: fort, tool: &fakeTool{}, chartDir: chartDir, dirs: []string{"moment-tally-website"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 1 || len(plans[0].files) != 0 {
		t.Fatalf("want one plan with no Secret file, got %+v", plans)
	}
	var report strings.Builder
	plans[0].report(&report, false)
	if !strings.Contains(report.String(), "no Secret — the inventory declares no keys") {
		t.Errorf("seed report:\n%s", report.String())
	}
	if findings := inventoryFindings(tmp, fort); len(findings) != 0 {
		t.Errorf("check: want no finding on a secret-less app, got %+v", findings)
	}
}

// The gate over a committed inventory (forts#250): the Secret absent is
// an error that names the verb, a key the Secret lacks is an error, a
// sops ref into a file that is not there (or lacks the key) is an
// error, and a dir whose keys are all there is one ok line. An
// inventory that does not parse is an error, not a crash, and an
// inventory under the contract's name (the pre-#88 shape) is named
// with the rename as the fix.
func TestInventoryFindings(t *testing.T) {
	const fort = "lab-fort"
	tmp := t.TempDir()
	apps := filepath.Join(tmp, "flux/apps", fort)
	write := func(rel, content string) { writeFile(t, apps, rel, content) }
	write("kustomization.yaml", "resources: []\n")
	write("a/forts.inventory.yaml", "secrets:\n  secrets.sops.yaml:\n    metadata: {name: a-env, namespace: a}\n    keys:\n      X: {generate: hex8}\n")
	write("b/forts.inventory.yaml", "secrets:\n  secrets.sops.yaml:\n    metadata: {name: b-env, namespace: b}\n    keys:\n      X: {generate: hex8}\n      Y: {generate: hex8}\n")
	write("b/secrets.sops.yaml", "stringData:\n  X: ENC\n  Z: ENC\n")
	write("c/forts.inventory.yaml", "vars:\n  pw: {sops: [../forts/db/secrets.sops.yaml, stringData.password]}\nsecrets:\n  secrets.sops.yaml:\n    metadata: {name: c-env, namespace: c}\n    keys:\n      URL: {template: \"x://${pw}\"}\n      K: {sops: [../b/secrets.sops.yaml, stringData.nope]}\n")
	write("c/secrets.sops.yaml", "stringData:\n  URL: ENC\n  K: ENC\n")
	write("d/forts.inventory.yaml", "secrets:\n  secrets.sops.yaml:\n    metadata: {name: d-env, namespace: d}\n    keys:\n      X: {generate: hex8}\n")
	write("d/secrets.sops.yaml", "stringData:\n  X: ENC\n")
	write("e/forts.inventory.yaml", "secrets:\n  secrets.sops.yaml:\n    keys:\n      X: {generate: hex8}\n")
	write("f/forts.contract.yaml", "secrets:\n  secrets.sops.yaml:\n    metadata: {name: f-env, namespace: f}\n    keys:\n      X: {generate: hex8}\n")
	findings := inventoryFindings(tmp, fort)
	byDir := map[string][]string{}
	for _, f := range findings {
		byDir[filepath.Base(f.dir)] = append(byDir[filepath.Base(f.dir)], f.level+": "+f.msg)
	}
	want := map[string]string{
		"a": "error: secrets.sops.yaml: not seeded (fort seed)",
		"b": "error: secrets.sops.yaml: missing Y — fort seed adds them|warning: secrets.sops.yaml: extra key(s) not in the inventory: Z (migration superset?)",
		"c": "error: var pw: sops ref ../forts/db/secrets.sops.yaml does not exist (seed its dir first)|error: secrets.sops.yaml/K: ../b/secrets.sops.yaml has no key nope",
		"d": "ok: forts.inventory.yaml: every declared key is seeded",
		"e": "error: forts.inventory.yaml: secrets.secrets.sops.yaml: metadata.name and namespace are required",
		"f": "error: forts.contract.yaml carries a secrets: block — that is an inventory, not a contract; rename the file to forts.inventory.yaml (forts#88)",
	}
	for dir, w := range want {
		if got := strings.Join(byDir[dir], "|"); got != w {
			t.Errorf("%s:\n  want %s\n  got  %s", dir, w, got)
		}
	}
	if len(findings) != 8 {
		t.Errorf("want 8 findings, got %d: %+v", len(findings), findings)
	}
	// No apps root at all (a plain fort): nothing to say.
	if f := inventoryFindings(tmp, "other"); len(f) != 0 {
		t.Errorf("no apps root: %+v", f)
	}
}
