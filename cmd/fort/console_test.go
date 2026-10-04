package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The console failsafe is three files, and the point of both the verb
// and the check is that no partial state passes without somebody saying
// so.
func TestConsoleFindings(t *testing.T) {
	const fort = "lab-fort"
	tmp := t.TempDir()
	hostNix := filepath.Join(tmp, "nixos/hosts", fort, "default.nix")
	secrets := filepath.Join(tmp, "nixos/secrets", fort+".yaml")
	registry := filepath.Join(tmp, consoleRegistry)
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const enabled = "{ ... }:\n{\n  imports = [ ../../modules/ops/console.nix ];\n  sfi.ops.console.enable = true;\n}\n"
	// Key names are plaintext in a sops file; the values here stand in
	// for ciphertext, which nothing in the check ever reads.
	const withHash = "wireguard-private-key: ENC[x]\ndeployer-password-hash: ENC[y]\nsops: {}\n"
	const withoutHash = "wireguard-private-key: ENC[x]\nsops: {}\n"

	one := func(t *testing.T, want string) finding {
		t.Helper()
		ff := consoleFindings(tmp, fort)
		if len(ff) != 1 || ff[0].level != want {
			t.Fatalf("want one %s finding, got %v", want, ff)
		}
		return ff[0]
	}

	// No half at all: silence. Most forts are here, and a check that
	// talks about every fort's missing failsafe teaches operators to
	// skim.
	write(hostNix, "{ }\n")
	write(secrets, withoutHash)
	write(registry, "sops: {}\n")
	if ff := consoleFindings(tmp, fort); len(ff) != 0 {
		t.Fatalf("no half: want no finding, got %v", ff)
	}

	// The module without the hash: sops-nix fails the activation on a
	// declared-but-absent key, so this is an error, not a warning.
	write(hostNix, enabled)
	if f := one(t, "error"); !strings.Contains(f.msg, keyDeployerPasswordHash) {
		t.Fatalf("want the key named, got %q", f.msg)
	}

	// The module and the hash, and no passphrase behind it: the getty
	// accepts something nobody can type.
	write(secrets, withHash)
	if f := one(t, "warning"); !strings.Contains(f.msg, "--rotate") {
		t.Fatalf("want the repair named, got %q", f.msg)
	}

	// All three halves.
	write(registry, fort+": ENC[z]\nsops: {}\n")
	one(t, "ok")

	// A credential no module reads — either half of it. A password in
	// the registry that opens nothing is the same defect as a hash on a
	// host that no getty consults.
	write(hostNix, "{ }\n")
	if f := one(t, "warning"); !strings.Contains(f.msg, keyDeployerPasswordHash) || !strings.Contains(f.msg, consoleRegistry) {
		t.Fatalf("want both halves named, got %q", f.msg)
	}
	write(secrets, withoutHash)
	if f := one(t, "warning"); strings.Contains(f.msg, keyDeployerPasswordHash) {
		t.Fatalf("the hash is gone; the warning must not name it: %q", f.msg)
	}
}

// The verb: the refusals, the restore-not-rotate default, and the two
// files one --write touches.
func TestConsolePassword(t *testing.T) {
	tmp := t.TempDir()
	for _, host := range []string{"lab-fort", "plain-fort"} {
		copyManifest(t, "testdata", tmp, host)
	}
	tool := &fakeTool{}
	secrets := filepath.Join(tmp, "nixos/secrets/lab-fort.yaml")
	hostNix := filepath.Join(tmp, "nixos/hosts/lab-fort/default.nix")
	registry := filepath.Join(tmp, consoleRegistry)
	write := func(path, content string) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// No identity yet: mint first. A plain fort is a fort (#647): it meets
	// the same gate and no refusal — deployer is ops/deployer.nix's on
	// both shapes.
	for _, host := range []string{"lab-fort", "plain-fort"} {
		if _, err := planConsolePassword(tmp, host, false); err == nil || !strings.Contains(err.Error(), "fort mint") {
			t.Fatalf("%s without the secrets file: want the mint gate, got %v", host, err)
		}
	}

	write(secrets, "wireguard-private-key: ENC[x]\nsops: {}\n")
	write(hostNix, "{ }\n")

	// CI has no mkpasswd and no nix; the hash's shape is mkpasswdHash's
	// contract, tested separately where the tool exists.
	hashPassword = func(string) (string, error) { return "$y$j9T$fake$hash", nil }
	t.Cleanup(func() { hashPassword = mkpasswdHash })

	p, err := planConsolePassword(tmp, "lab-fort", false)
	if err != nil {
		t.Fatal(err)
	}
	if p.haveHash || p.haveEntry || !p.newRegistry || p.enabled || !p.mints() {
		t.Fatalf("first run: %+v", p)
	}
	var buf bytes.Buffer
	p.report(&buf, false)
	if !strings.Contains(buf.String(), "dry run") {
		t.Fatalf("dry run: want the note, got %q", buf.String())
	}

	// The first --write creates the registry: one file written whole
	// and encrypted once, as fort mint does with a new secrets file.
	if err := p.apply(tool); err != nil {
		t.Fatal(err)
	}
	if len(tool.encrypted) != 1 || tool.encrypted[0] != registry {
		t.Fatalf("apply: want the new registry encrypted once, got %v", tool.encrypted)
	}
	if got := readFile(t, registry); got != "lab-fort: "+p.passphrase+"\n" {
		t.Fatalf("registry holds %q — want the passphrase under the host name", got)
	}
	if len(tool.sets) != 1 || tool.sets[0][0] != secrets || tool.sets[0][1] != `["`+keyDeployerPasswordHash+`"]` {
		t.Fatalf("apply: want one sops set of the hash, got %v", tool.sets)
	}
	if tool.sets[0][2] != "$y$j9T$fake$hash" {
		t.Fatalf("apply set %q — the HOST holds the hash, never the passphrase", tool.sets[0][2])
	}
	if len(tool.added) != 2 || tool.added[0] != consoleRegistry || tool.added[1] != "nixos/secrets/lab-fort.yaml" {
		t.Fatalf("apply: want both files staged, registry first, got %v", tool.added)
	}

	// --write prints no value. The registry is the one copy; a banner
	// would be a second one in scrollback and in any transcript.
	buf.Reset()
	p.report(&buf, true)
	if strings.Contains(buf.String(), p.passphrase) {
		t.Fatalf("the report printed the passphrase: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "--show") {
		t.Fatalf("--write: want the read named, got %q", buf.String())
	}

	// Second run: both halves are there, so nothing is minted.
	// Rotation is the deliberate act.
	write(secrets, "wireguard-private-key: ENC[x]\n"+keyDeployerPasswordHash+": ENC[y]\nsops: {}\n")
	write(registry, "lab-fort: ENC[z]\nsops: {}\n")
	p, err = planConsolePassword(tmp, "lab-fort", false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.haveHash || !p.haveEntry || p.newRegistry || p.mints() {
		t.Fatalf("re-run: %+v — a re-run must not rotate", p)
	}
	buf.Reset()
	p.report(&buf, true)
	if !strings.Contains(buf.String(), "--rotate") || !strings.Contains(buf.String(), "--show") {
		t.Fatalf("re-run: want the rotate and the read, got %q", buf.String())
	}

	// --show reads one fort's entry, and only that fort's.
	readConsolePassword = func(root, host string) (string, error) { return "xk4m-7rqt-9vhw-2bnd-pxs3", nil }
	t.Cleanup(func() { readConsolePassword = readConsolePasswordSops })
	buf.Reset()
	if err := p.show(&buf); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(buf.String()) != "xk4m-7rqt-9vhw-2bnd-pxs3" {
		t.Fatalf("--show printed %q", buf.String())
	}

	// A hash with no passphrase behind it: the one state this design
	// exists to prevent. --show says so, and a plain re-run still
	// refuses to replace the hash on its own.
	write(registry, "sops: {}\n")
	p, err = planConsolePassword(tmp, "lab-fort", false)
	if err != nil {
		t.Fatal(err)
	}
	if p.mints() {
		t.Fatal("an orphaned hash must still need --rotate")
	}
	if err := p.show(&buf); err == nil || !strings.Contains(err.Error(), "--rotate") {
		t.Fatalf("--show on a missing entry: want the repair named, got %v", err)
	}
	buf.Reset()
	p.report(&buf, true)
	if !strings.Contains(buf.String(), "nothing can read") {
		t.Fatalf("orphaned hash: want the report to say so, got %q", buf.String())
	}

	// --rotate replaces it.
	p, err = planConsolePassword(tmp, "lab-fort", true)
	if err != nil {
		t.Fatal(err)
	}
	if !p.mints() {
		t.Fatal("--rotate: want a mint")
	}

	// The opt-in line is read from the host's default.nix, and the
	// follow-up text turns on it.
	write(hostNix, "{\n  sfi.ops.console.enable = true;\n}\n")
	write(registry, "lab-fort: ENC[z]\nsops: {}\n")
	p, err = planConsolePassword(tmp, "lab-fort", false)
	if err != nil {
		t.Fatal(err)
	}
	if !p.enabled {
		t.Fatal("the opt-in line was not read")
	}
	buf.Reset()
	p.report(&buf, false)
	if !strings.Contains(buf.String(), "the getty reads the hash") || !strings.Contains(buf.String(), "virsh console lab-fort") {
		t.Fatalf("opted in: want the exercise note, got %q", buf.String())
	}
}

// The passphrase is typed by hand at a serial console: 100 bits from an
// alphabet with no confusable pair, in groups a person can keep place
// in.
func TestNewPassphrase(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		pass, err := newPassphrase()
		if err != nil {
			t.Fatal(err)
		}
		if seen[pass] {
			t.Fatalf("newPassphrase repeated %q", pass)
		}
		seen[pass] = true
		groups := strings.Split(pass, "-")
		if len(groups) != 5 {
			t.Fatalf("%q: want 5 groups", pass)
		}
		for _, g := range groups {
			if len(g) != 4 {
				t.Fatalf("%q: want groups of 4", pass)
			}
		}
		for _, c := range strings.ReplaceAll(pass, "-", "") {
			if !strings.ContainsRune(consoleAlphabet, c) {
				t.Fatalf("%q: %q is outside the alphabet", pass, c)
			}
		}
	}
	for _, c := range "ilou" {
		if strings.ContainsRune(consoleAlphabet, c) {
			t.Fatalf("the alphabet keeps %q — it is confusable at a console", c)
		}
	}
}

// mkpasswdHash's contract: a yescrypt hash of exactly the passphrase it
// was given. The newline the verb writes to mkpasswd's stdin must not be
// part of the password — an operator types the passphrase and nothing
// else — so the hash is compared against one made from the same salt
// with the password as an argument, where no newline exists. Skipped
// where mkpasswd is absent (the CI runner), which is also why
// hashPassword is a variable.
func TestMkpasswdHash(t *testing.T) {
	if _, err := exec.LookPath("mkpasswd"); err != nil {
		t.Skip("mkpasswd not on PATH")
	}
	const pass = "correct-horse"
	hash, err := mkpasswdHash(pass)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$y$") {
		t.Fatalf("want a yescrypt hash, got %q", hash)
	}
	// $y$<params>$<salt>$<hash>: mkpasswd -S takes the two middle fields.
	fields := strings.Split(hash, "$")
	if len(fields) != 5 {
		t.Fatalf("%q: want four $-separated fields", hash)
	}
	salt := fields[2] + "$" + fields[3]
	again, err := exec.Command("mkpasswd", "-m", "yescrypt", "-S", salt, pass).Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(again)) != hash {
		t.Fatalf("stdin hash %q != argv hash %q — the newline is part of the password", hash, strings.TrimSpace(string(again)))
	}
	other, err := exec.Command("mkpasswd", "-m", "yescrypt", "-S", salt, pass+"x").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(other)) == hash {
		t.Fatal("a different password hashed to the same value")
	}
}
