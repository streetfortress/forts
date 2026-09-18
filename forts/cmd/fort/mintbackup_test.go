package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/b2"
	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

func TestS3EnvRoundTrip(t *testing.T) {
	env := s3Env("004abc", "K0secret")
	if !strings.Contains(env, "AWS_ACCESS_KEY_ID=004abc\n") || !strings.Contains(env, "AWS_SECRET_ACCESS_KEY=K0secret\n") {
		t.Fatalf("env file shape: %q", env)
	}
	// The recorded key id is read back out of the stored env file — it is
	// what makes the re-mint guard exact, since B2 never shows the secret
	// half again.
	if got := envKeyID(env); got != "004abc" {
		t.Errorf("envKeyID = %q", got)
	}
	if got := envKeyID("AWS_SECRET_ACCESS_KEY=only\n"); got != "" {
		t.Errorf("envKeyID with no id = %q", got)
	}
	if got := envKeyID(""); got != "" {
		t.Errorf("envKeyID of an unseeded secret = %q", got)
	}
}

func TestB2KeyNameIsLegal(t *testing.T) {
	// B2's keyName rule is letters, digits and hyphens. Generated fort
	// names satisfy it; a name that did not would be refused at the API
	// after the identity had already been staged.
	for _, host := range []string{"sfi-prod-439bf72", "sfi-fort-c0db879", "sfi-fort-d54d5b3"} {
		if err := b2.ValidName(b2KeyName(host)); err != nil {
			t.Errorf("%s: %v", host, err)
		}
	}
}

func TestB2Restrictions(t *testing.T) {
	prefix := "sfi-fort-abc/"
	ok := b2.Key{BucketIDs: []string{"a"}, NamePrefix: &prefix, Capabilities: b2.Writer}
	wantIDs := []string{"a"}
	if got := b2Restrictions(ok, "sfi-fort-abc", wantIDs); got != "" {
		t.Errorf("a correct key should pass: %s", got)
	}

	wrongPrefix := "someone-else/"
	cases := map[string]b2.Key{
		// The two-bucket era's grant: a PV key must not also open the db bucket.
		"is restricted to 2 bucket(s)": {BucketIDs: []string{"a", "b"}, NamePrefix: &prefix, Capabilities: b2.Writer},
		"has namePrefix":               {BucketIDs: []string{"a"}, NamePrefix: &wrongPrefix, Capabilities: b2.Writer},
		"want \"sfi-fort-abc/\"":       {BucketIDs: []string{"a"}, NamePrefix: nil, Capabilities: b2.Writer},
		// One bucket, but not THE one: B2 cannot rename a bucket, so a
		// key minted before one was replaced passes a count and fails here.
		"none of a": {BucketIDs: []string{"old"}, NamePrefix: &prefix, Capabilities: b2.Writer},
		// The one the guard exists for: a key edited in the console into
		// holding deleteFiles still carries the right name and prefix.
		"holds deleteFiles": {BucketIDs: []string{"a"}, NamePrefix: &prefix, Capabilities: b2.Pruning},
	}
	for want, k := range cases {
		got := b2Restrictions(k, "sfi-fort-abc", wantIDs)
		if !strings.Contains(got, want) {
			t.Errorf("want %q in the complaint, got %q", want, got)
		}
	}
}

func TestB2SessionRefusesWithoutTheOperationalInput(t *testing.T) {
	t.Setenv("B2_KEY_ID", "")
	t.Setenv("B2_APP_KEY", "")
	stdinIsTTY = func() bool { return false }
	_, err := b2Session(needMaster, "Minting sfi-b2-key-minting-key")
	if err == nil || !strings.Contains(err.Error(), "operational input") {
		t.Fatalf("want a refusal naming the credential's home, got %v", err)
	}
	// No terminal is the scripted case: the refusal has to say WHICH of
	// the two hand-held credentials to export.
	if !strings.Contains(err.Error(), "ACCOUNT MASTER") {
		t.Errorf("the refusal should name the credential it wants, got %v", err)
	}
}

func TestB2RolesAreTheDesignsCapabilitySets(t *testing.T) {
	minting := b2Roles["minting"]
	pruning := b2Roles["pruning"]

	// The whole reason this verb exists rather than the web console: the
	// two grants differ in exactly the way that matters, and getting
	// either wrong is silent until the day it matters.
	if hasKey(minting.caps, "deleteFiles") {
		t.Error("the minting credential must not be able to remove a backup object itself")
	}
	if !hasKey(minting.caps, "writeKeys") || !hasKey(minting.caps, "deleteKeys") {
		t.Error("the minting credential must manage fort keys (mint registers, destroy revokes)")
	}
	if !hasKey(pruning.caps, "deleteFiles") {
		t.Error("the pruning credential exists to delete; restic prune removes packs of any age")
	}
	for _, c := range pruning.caps {
		if strings.HasSuffix(c, "Keys") {
			t.Errorf("the pruning credential holds %s — it must not be able to mint its way anywhere", c)
		}
	}
	// Scoping: the pruner reaches one bucket and every fort's prefix in
	// it; the minter must see both backup buckets to resolve their ids.
	if len(pruning.buckets) != 1 || pruning.buckets[0] != b2PVBucket {
		t.Errorf("pruning buckets = %v, want just %s", pruning.buckets, b2PVBucket)
	}
	if len(minting.buckets) != 0 {
		t.Errorf("minting must not be bucket-restricted: it resolves %s and %s by name", b2PVBucket, b2PhysicalBucket)
	}
}

func TestRoleDriftNamesWhatTheConsoleGetsWrong(t *testing.T) {
	role := b2Roles["minting"]
	good := b2.Key{Capabilities: role.caps}
	if got := roleDrift(good, role, nil); got != "" {
		t.Errorf("a correct key should not drift: %s", got)
	}
	// The failure this catches: a key made in the console with a coarse
	// permission picker, holding more than the role grants.
	tooMuch := b2.Key{Capabilities: append(append([]string{}, role.caps...), "deleteFiles")}
	if got := roleDrift(tooMuch, role, nil); !strings.Contains(got, "ALSO holds deleteFiles") {
		t.Errorf("want the extra capability named, got %q", got)
	}
	tooLittle := b2.Key{Capabilities: []string{"listBuckets"}}
	if got := roleDrift(tooLittle, role, nil); !strings.Contains(got, "missing ") || !strings.Contains(got, "writeKeys") {
		t.Errorf("want the missing capability named, got %q", got)
	}
	prefix := "sfi-prod/"
	scoped := b2.Key{Capabilities: role.caps, NamePrefix: &prefix}
	if got := roleDrift(scoped, role, nil); !strings.Contains(got, "namePrefix") {
		t.Errorf("want the prefix restriction named, got %q", got)
	}
}

// Flags come AFTER the positional, as in every other verb. Go's flag
// package stops at the first non-flag argument, so parsing the whole
// slice left `--write` as a second positional and refused a correct
// command line ("fort b2-key minting --write", reported 2026-09-16).
func TestParseB2KeyArgs(t *testing.T) {
	role, write, err := parseB2KeyArgs([]string{"minting", "--write"})
	if err != nil || !write || role.name != "sfi-b2-key-minting-key" {
		t.Fatalf("minting --write: role %q write %v err %v", role.name, write, err)
	}
	if role, write, err := parseB2KeyArgs([]string{"pruning"}); err != nil || write || role.name != "sfi-b2-pruning-key" {
		t.Errorf("pruning: role %q write %v err %v", role.name, write, err)
	}
	for _, args := range [][]string{{}, {"--write"}, {"nonsense"}, {"minting", "extra"}} {
		if _, _, err := parseB2KeyArgs(args); err == nil {
			t.Errorf("%v: want a usage error", args)
		}
	}
}

// A dry run must not require the credential it is there to help you
// decide about. Found raising the first fort with a production profile:
// `fort mint <host>` refused outright with B2_KEY_ID unset, so the plan
// could not be read before exporting a key that can mint one which
// deletes backups (2026-09-16).
func TestPlanB2KeyDryRunWithoutCredentials(t *testing.T) {
	t.Setenv("B2_KEY_ID", "")
	t.Setenv("B2_APP_KEY", "")

	dry := &mintPlan{host: "sfi-fort-x", secretsPath: "nixos/secrets/sfi-fort-x.yaml"}
	if err := dry.planB2Key(); err != nil {
		t.Fatalf("a dry run should plan without the credential: %v", err)
	}
	if !dry.b2Mint || !strings.Contains(dry.b2Status, "would mint") {
		t.Errorf("want a would-mint plan, got mint=%v %q", dry.b2Mint, dry.b2Status)
	}
	if !strings.Contains(dry.b2Status, "NOT verified") {
		t.Errorf("the plan must say the grant was not checked: %q", dry.b2Status)
	}

	// --write is where it has to be fatal: apply cannot mint without it,
	// and finding out after staging the identity is worse.
	wet := &mintPlan{host: "sfi-fort-x", secretsPath: "nixos/secrets/sfi-fort-x.yaml", write: true}
	if err := wet.planB2Key(); err == nil {
		t.Error("--write without the credential must refuse up front")
	}

	// A fort that already has its key needs no credential either way:
	// nothing is being minted, so the check is skipped, not demanded.
	have := &mintPlan{host: "sfi-fort-x", write: true, resticB2: s3Env("004id", "secret")}
	if err := have.planB2Key(); err != nil {
		t.Fatalf("a re-mint with nothing to create should proceed: %v", err)
	}
	if have.b2Mint || !strings.Contains(have.b2Status, "NOT verified") {
		t.Errorf("want a skipped check, got mint=%v %q", have.b2Mint, have.b2Status)
	}
}

func TestRustfsIdentityFindings(t *testing.T) {
	root := t.TempDir()
	write := func(keys ...string) {
		dir := filepath.Join(root, filepath.Dir(rustfsUsers))
		os.MkdirAll(dir, 0o755)
		body := "apiVersion: v1\nkind: Secret\nmetadata:\n    name: rustfs-users\n    namespace: rustfs\nstringData:\n"
		for _, k := range keys {
			body += "    " + k + ": ENC[AES256_GCM,data:x,iv:y,tag:z,type:str]\n"
		}
		os.WriteFile(filepath.Join(root, rustfsUsers), []byte(body), 0o644)
	}
	backed := manifest.Host{Backups: manifest.Backups{Profile: "production"}}
	none := manifest.Host{Backups: manifest.Backups{Profile: "none"}}
	levels := func(fs []finding) string {
		var out []string
		for _, f := range fs {
			out = append(out, f.level)
		}
		return strings.Join(out, ",")
	}

	// Forward: declared, not yet minted. A warning — an unfinished raise
	// is visible and harmless.
	write("prune")
	fs := rustfsIdentityFindings(root, map[string]manifest.Host{"sfi-a": backed})
	if len(fs) != 1 || fs[0].level != "warning" || !strings.Contains(fs[0].msg, "fort-sfi-a-pv") {
		t.Errorf("forward: %v (%s)", fs, levels(fs))
	}

	// Both halves present: silent.
	write("prune", "fort-sfi-a-pv")
	if fs := rustfsIdentityFindings(root, map[string]manifest.Host{"sfi-a": backed}); len(fs) != 0 {
		t.Errorf("a matched pair should say nothing: %v", fs)
	}

	// Reverse: the fort is gone, its credential is not. THE error case —
	// a live identity scoped to a prefix whose backups are still there.
	if fs := rustfsIdentityFindings(root, map[string]manifest.Host{}); len(fs) != 1 || fs[0].level != "error" || !strings.Contains(fs[0].msg, "ORPHANED") {
		t.Errorf("reverse: %v (%s)", fs, levels(fs))
	}

	// Disagreement: the fort exists but says it backs up nowhere.
	if fs := rustfsIdentityFindings(root, map[string]manifest.Host{"sfi-a": none}); len(fs) != 1 || fs[0].level != "error" {
		t.Errorf(`profile "none" with an identity must be an error: %v`, fs)
	}

	// `prune` has its own lifecycle and is never mistaken for a fort.
	if _, ok := fortOfPVUser("prune"); ok {
		t.Error("prune must not parse as a fort identity")
	}
	if host, ok := fortOfPVUser("fort-sfi-fort-c0db879-pv"); !ok || host != "sfi-fort-c0db879" {
		t.Errorf("fortOfPVUser round trip: %q %v", host, ok)
	}
	if _, ok := fortOfPVUser("fort--pv"); ok {
		t.Error("an empty host must not parse")
	}
}
