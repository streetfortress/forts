package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

// The gate on a synthetic fort: a full-form dir that is complete, one
// missing a key, a chart-contract dir with an unset required external
// key, an unset wire value, and a provider ref into a file without the
// key. Uses the fixture manifest and the symbology contract fixture.
func TestCheckFindings(t *testing.T) {
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
	secret := func(name, ns string, keys ...string) string {
		s := "apiVersion: v1\nkind: Secret\nmetadata: {name: " + name + ", namespace: " + ns + "}\nstringData:\n"
		for _, k := range keys {
			s += "  " + k + ": ENC[AES256_GCM,data:x,type:str]\n"
		}
		return s + "sops: {}\n"
	}
	// complete full-form provider
	write("valkey/forts.contract.yaml", "secrets:\n  secrets.sops.yaml:\n    metadata: {name: valkey-users, namespace: valkey}\n    keys:\n      default: {generate: hex24}\n      symbology: {generate: hex24}\n")
	write("valkey/secrets.sops.yaml", secret("valkey-users", "valkey", "default", "symbology"))
	// sites-db: the symbology tenant secret lacks its password key
	write("sites-db/forts.contract.yaml", "secrets:\n  secrets-symbology.sops.yaml:\n    metadata: {name: c, namespace: sites-db}\n    keys:\n      username: {literal: symbology}\n      password: {generate: hex24}\n")
	write("sites-db/secrets-symbology.sops.yaml", secret("c", "sites-db", "username"))
	// symbology: chart contract (fixture), wires partly unset, Secret
	// lacks STRIPE_SECRET_KEY (required external) and RESEND_API_KEY
	// (optional), and carries an extra key
	write("symbology/repository.yaml", "kind: OCIRepository\nspec:\n  url: oci://gitea.zen.lofi/sfi/symbology\n  ref:\n    tag: \"0.1.6\"\n")
	write("symbology/release.yaml", "spec:\n  values:\n    database:\n      host: h\n      port: \"5432\"\n      name: symbology\n    cache:\n      host: h\n      port: \"6379\"\n      db: \"0\"\n      user: symbology\n")
	write("symbology/secrets.sops.yaml", secret("symbology-env", "symbology",
		"BETTER_AUTH_SECRET", "DATABASE_PASSWORD", "CACHE_PASSWORD", "STRIPE_WEBHOOK_SECRET", "OPENAI_API_KEY", "ANTHROPIC_API_KEY", "LEGACY_KEY"))

	// legacy: a full-form dir whose pinned chart ALSO declares a
	// contract — the full form lacks STRIPE_SECRET_KEY (required) and
	// RESEND_API_KEY (optional), and its release has no cache.* wires
	write("legacy/repository.yaml", "kind: OCIRepository\nspec:\n  url: oci://gitea.zen.lofi/sfi/symbology\n  ref:\n    tag: \"0.1.2\"\n")
	write("legacy/release.yaml", "spec:\n  values:\n    database: {host: h, port: \"5432\", name: legacy, user: legacy}\n")
	write("legacy/forts.contract.yaml", "secrets:\n  secrets.sops.yaml:\n    metadata: {name: legacy-env, namespace: legacy}\n    keys:\n      BETTER_AUTH_SECRET: {generate: hex32}\n      DATABASE_PASSWORD: {generate: hex32}\n      CACHE_PASSWORD: {generate: hex32}\n      STRIPE_WEBHOOK_SECRET: {generate: hex32}\n      OPENAI_API_KEY: {generate: hex32}\n      ANTHROPIC_API_KEY: {generate: hex32}\n")
	write("legacy/secrets.sops.yaml", secret("legacy-env", "legacy", "BETTER_AUTH_SECRET", "DATABASE_PASSWORD", "CACHE_PASSWORD", "STRIPE_WEBHOOK_SECRET", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"))

	o := checkOpts{root: tmp, chartDir: "testdata/charts/symbology", charts: map[string]pulledChart{}}
	findings, err := checkFort(o, fort)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.level+" "+strings.TrimPrefix(f.dir, filepath.Join("fleet/apps", fort)+"/")+": "+f.msg)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"ok valkey: forts.contract.yaml (full form)",
		"error sites-db: secrets-symbology.sops.yaml: missing password",
		"error symbology: secrets.sops.yaml: required external key(s) not set: STRIPE_SECRET_KEY",
		"warning symbology: secrets.sops.yaml: optional external key(s) unset: RESEND_API_KEY",
		"warning symbology: secrets.sops.yaml: extra key(s) not in the contract: LEGACY_KEY",
		"error symbology: var db_password: ../sites-db/secrets-symbology.sops.yaml has no key password",
		"error symbology: requires.postgres.db: release.yaml values unset: database.user (user)",
		"error legacy: chart contract declares STRIPE_SECRET_KEY — the fort-side",
		"warning legacy: chart contract's optional key(s) not carried fort-side: RESEND_API_KEY",
		"error legacy: requires.valkey.cache: release.yaml values unset: cache.db (db)",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing finding %q in:\n%s", want, joined)
		}
	}
	// valkey's wires are all set → no wire finding for it
	for _, unwanted := range []string{"symbology: requires.valkey.cache: release.yaml"} {
		if strings.Contains(joined, unwanted) {
			t.Errorf("unexpected finding mentioning %q in:\n%s", unwanted, joined)
		}
	}
	var sb strings.Builder
	if n := reportFindings(&sb, fort, findings, true); n != 6 {
		t.Errorf("errors: want 6, got %d\n%s", n, sb.String())
	}
	if !strings.Contains(sb.String(), "::error::") {
		t.Errorf("annotations missing:\n%s", sb.String())
	}
}

// The real fleet passes the gate's offline checks (full-form dirs only —
// the chart pulls are CI's job): every committed contract's Secret is
// complete and every provider ref resolves.
func TestCheckFleetOffline(t *testing.T) {
	repo, err := repoRootFrom(mustGetwd(t))
	if err != nil {
		t.Skip("not inside the deployments checkout")
	}
	dirs := fleetContracts(repo)
	for _, path := range dirs {
		raw := readFile(t, path)
		if !isFullForm([]byte(raw)) {
			continue
		}
		ff, err := parseFullForm([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		dir, _ := filepath.Rel(repo, filepath.Dir(path))
		for _, f := range append(checkSecrets(repo, dir, ff), checkVars(repo, dir, ff)...) {
			if f.level == "error" {
				t.Errorf("%s: %s", f.dir, f.msg)
			} else {
				t.Logf("%s: %s: %s", f.level, f.dir, f.msg)
			}
		}
	}
}

// The manifest gate: LoadAll's problems become findings, one per bad
// file, named by the file, sorted, the path prefix stripped from the
// message (the dir column carries it).
func TestManifestFindings(t *testing.T) {
	root := t.TempDir()
	write := func(host, content string) {
		os.MkdirAll(filepath.Join(root, manifest.Dir), 0o755)
		os.WriteFile(filepath.Join(root, manifest.Path(host)), []byte(content), 0o644)
	}
	write("d", "mesh_ip = \"10.1.0.2\"\n")
	write("a", "mesh_ip = \"10.1.0.1\"\n[backups]\nprofile = \"lab\"\n[libvirt]\nip = \"10.0.0.1\"\n")
	write("b", "[libvirt]\nip = \"10.0.0.2\"\n[linode]\nregion = \"r\"\ntype = \"t\"\n")
	hosts, problems, err := manifest.LoadAll(root)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("hosts %v err %v", hosts, err)
	}
	got := manifestFindings(problems)
	if len(got) != 2 || got[0].dir != manifest.Path("b") || !strings.HasPrefix(got[0].msg, "2 provider tables") || got[1].dir != manifest.Path("d") || !strings.HasPrefix(got[1].msg, "no provider table") || got[0].level != "error" {
		t.Errorf("findings: %+v", got)
	}
	if len(manifestFindings(nil)) != 0 {
		t.Error("no problems, no findings")
	}
}
