package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The retired secrets file answers "did this fort ever hold a B2 key"
// from its plaintext key names, so destroy asks for the minting key only
// when there is something to revoke (#432: a fort with no B2 leg asked
// for it anyway, and the only way it could learn there was nothing to
// revoke was to list). No record is "unknown", not "none".
func TestRetiredSecretsSayWhetherB2WasMinted(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@t")
	git("config", "user.name", "t")
	write := func(rel, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755)
		os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644)
	}
	// The shape sops leaves: names in the clear, values and the sops
	// block opaque. The sops block is metadata, never a key name.
	write("nixos/secrets/prod.yaml", "restic-password: ENC[x]\nrestic-s3-env-rustfs: ENC[x]\nrestic-s3-env-b2: ENC[x]\nsops:\n    age: []\n")
	write("nixos/secrets/lab.yaml", "restic-password: ENC[x]\nrestic-s3-env-rustfs: ENC[x]\nsops:\n    age: []\n")
	// The fleet's pruner (#400): no B2 pv leg — no restic-s3-env-b2 —
	// but the role-scoped pruning key's env file is in its record.
	write("nixos/secrets/pruner.yaml", "restic-password: ENC[x]\nrestic-prune-s3-env-rustfs: ENC[x]\nrestic-prune-s3-env-b2: ENC[x]\nsops:\n    age: []\n")
	git("add", "-A")
	git("commit", "-q", "-m", "minted")
	git("rm", "-q", "nixos/secrets/prod.yaml", "nixos/secrets/lab.yaml", "nixos/secrets/pruner.yaml")
	git("commit", "-q", "-m", "dismantled")
	git("update-ref", "refs/remotes/origin/main", "HEAD")

	prod, err := readRetiredSecrets(root, "prod")
	if err != nil || !prod.has(keyResticB2) {
		t.Errorf("prod held a B2 key: has=%v %v", prod.has(keyResticB2), err)
	}
	if prod.has("sops") {
		t.Error("the sops block is not a key")
	}
	lab, err := readRetiredSecrets(root, "lab")
	if err != nil || lab == nil || lab.has(keyResticB2) {
		t.Errorf("lab never held one: %+v %v", lab, err)
	}
	never, err := readRetiredSecrets(root, "never")
	if err != nil || never != nil {
		t.Errorf("no file, no record: %+v %v", never, err)
	}
	if never.has(keyResticB2) {
		t.Error("a nil record holds nothing — callers gate on nil first")
	}

	// revokeB2Key on those facts, with no credential anywhere and no
	// terminal: the lab fort is answered without a session; prod and the
	// unknown case still reach for one, and are refused by name.
	t.Setenv("B2_KEY_ID", "")
	t.Setenv("B2_APP_KEY", "")
	stdinIsTTY = func() bool { return false }
	msg, err := revokeB2Key("lab", lab)
	if err != nil || !strings.Contains(msg, "none minted") || !strings.Contains(msg, "nixos/secrets/lab.yaml") {
		t.Errorf("lab: want 'none minted' naming the record, got %q %v", msg, err)
	}
	for name, s := range map[string]*retiredSecrets{"prod": prod, "unknown": nil} {
		if _, err := revokeB2Key(name, s); err == nil || !strings.Contains(err.Error(), "B2 keys NOT revoked") {
			t.Errorf("%s: a key that may exist must still be revoked (or the refusal named): %v", name, err)
		}
	}
	// The pruner's record has no fort key of its own, but it names the
	// pruning key's env file: destroy opens a session for that, and the
	// refusal lists sfi-b2-pruning-key among the keys left to delete.
	pruner, err := readRetiredSecrets(root, "pruner")
	if err != nil || !pruner.has(pruneEnvKey(pruneTierB2)) || pruner.has(keyResticB2) {
		t.Fatalf("pruner record: %+v %v", pruner, err)
	}
	if _, err := revokeB2Key("pruner", pruner); err == nil || !strings.Contains(err.Error(), b2PruningKeyName) {
		t.Errorf("the pruner's destroy must reach for the pruning key: %v", err)
	}
	if _, err := revokeB2Key("prod", prod); err != nil && strings.Contains(err.Error(), b2PruningKeyName) {
		t.Errorf("a fort that was not the pruner must not name the pruning key: %v", err)
	}
}
