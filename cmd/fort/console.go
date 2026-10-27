package main

import (
	"bytes"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gitea.zen.lofi/sfi/forts/internal/tools"
)

// `fort console-password <host>` mints the credential
// modules/ops/console.nix reads: a passphrase for `deployer` that
// only a getty accepts, so an operator on Lish or `virsh console`
// reaches `sudo nixos-rollback` after the mesh breaks (#524).
//
// It writes the credential in two halves, to two files, because the two
// readers are different:
//
//	nixos/secrets/<host>.yaml   deployer-password-hash — the HOST reads
//	                            it, so it is encrypted to the host too
//	provisioning/console-passwords.yaml
//	                            <host>: the PASSPHRASE — an OPERATOR
//	                            reads it, so the admin recipients alone
//
// WHERE THE PASSPHRASE LIVES, and why it is not the password manager.
// A credential that can delete every backup lives outside the repo, where
// no invocation can read it. This one is the opposite case. An admin age key already decrypts each
// fort's ssh host key, its wireguard key, its cluster age key and its
// deploy key (nixos/secrets/<host>.yaml). A console password for an
// account with no wheel is strictly less than any of those, so the
// registry adds no reach to a key that is already the root of trust.
// What it adds is that the value is THERE: in a checkout the operator
// already has, decryptable with no network at all. A mesh outage is
// exactly when a credential must not depend on the fleet, and "the
// password manager" — which this repo names nowhere — was a dependency
// nobody had checked.
//
// The registry is a per-host file in the shape mesh/peers.json and
// mesh/ssh_config already have: one entry per fort, mint adds it,
// `fort dismantle` withdraws it, `fort check` traces it both ways. Key
// names are plaintext in a sops file, so the check reads WHICH forts
// are listed without an age key and never a value.
//
// NOTHING IS PRINTED BY the apply. The registry is where the value lives,
// so a banner would be a second copy in terminal scrollback, in a
// multiplexer's buffer and in whatever transcript the session leaves.
// `--show` is the deliberate read, one fort at a time.
//
// HOW IT RELATES TO `fort mint`. The first credential is mint's
// (sfi/forts#24): every host shape the scaffold renders enables the
// module, and mint, which reads the host file anyway, mints the
// passphrase and the hash with the rest of the identity — the same two
// files, through the same recordPassphrase below. Mint restores rather
// than rotates, because an identity that changes invalidates everything
// encrypted to it; a hash already in the secrets file is kept, even when
// its passphrase has gone missing from the registry. A password is the
// one credential an operator rotates — after a person leaves, after a
// shared console session, after a drill — so rotation stays here, as
// `--rotate`, and `--show` is the read. A plain run without `--rotate`
// mints only where mint did not: a host that enabled the module after
// the first provision.
//
// SCOPE: every fort. ops/ops-commands.nix pins every verb's sudo rule to
// `deployer`, and the account is ops/deployer.nix's on cluster forts and
// plain forts alike (#647) — whatever an estate builds on top of a fort,
// the hatch is the fort's. This verb used to refuse a fort with no
// cluster while #524 left that question open; it no longer does.
func consolePassword(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: fort console-password <host> [--dry-run] [--rotate] [--show]")
	}
	host := args[0]
	fs := flag.NewFlagSet("console-password", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the plan and write nothing (default: apply)")
	fs.Bool("write", true, "deprecated no-op: writing is the default")
	rotate := fs.Bool("rotate", false, "replace a passphrase the fort already has — the old one stops working when the host activates the next generation")
	show := fs.Bool("show", false, "decrypt and print this fort's passphrase from the registry; writes nothing")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	write := !*dryRun
	root, err := repoRoot()
	if err != nil {
		return err
	}
	p, err := planConsolePassword(root, host, *rotate)
	if err != nil {
		return err
	}
	if *show {
		return p.show(os.Stdout)
	}
	if write && p.mints() {
		if err := tools.EnsureAll("sops", "mkpasswd", "git"); err != nil {
			return err
		}
		if err := p.apply(sopsCLI{root: root}); err != nil {
			return err
		}
	}
	p.report(os.Stdout, write)
	return nil
}

const (
	// keyDeployerPasswordHash is the key modules/ops/console.nix
	// declares and `fort check` looks for.
	keyDeployerPasswordHash = "deployer-password-hash"

	// consoleRegistry holds the other half: one plaintext passphrase per
	// fort, keyed by host name. Admin recipients only — the rule in
	// .sops.yaml says why the fort is not one of them.
	consoleRegistry = "provisioning/console-passwords.yaml"
)

// consoleModuleRE matches the opt-in line in a host's default.nix. The
// check is textual on purpose: `fort check` runs in CI with no nix and
// no age key, and the one line it must see is the one the docs tell an
// operator to write.
var consoleModuleRE = regexp.MustCompile(`sfi\.ops\.console\.enable\s*=\s*true`)

type consolePlan struct {
	root, host  string
	secretsPath string // repo-relative
	hostNix     string // repo-relative
	haveHash    bool   // the host's secrets file carries the hash
	haveEntry   bool   // the registry carries this fort's passphrase
	newRegistry bool   // the registry does not exist yet
	enabled     bool   // the host's default.nix turns the module on
	rotate      bool

	// Filled by apply, and only there: a dry run mints nothing, so
	// there is no value for a plan to hold or leak. report never prints
	// the passphrase — `--show` is the read.
	passphrase string
	hash       string
}

// mints says whether the apply has work to do. The test is the HASH, not
// the registry entry: a hash that is already on a host is never replaced
// without --rotate, even when its passphrase has gone missing from the
// registry. Replacing it would invalidate a value somebody may still
// hold, so the repair is a deliberate rotation and report says so.
func (p *consolePlan) mints() bool { return !p.haveHash || p.rotate }

// planConsolePassword reads all three halves of the failsafe and no
// secret value: the key names it looks for are plaintext in a sops
// file, the same property `fort check` relies on. So a dry run needs no
// age key. Only the apply and --show do.
func planConsolePassword(root, host string, rotate bool) (*consolePlan, error) {
	// A manifest is the gate — cluster or plain, both carry deployer.
	if _, err := manifest.Load(root, host); err != nil {
		return nil, err
	}
	p := &consolePlan{
		root:        root,
		host:        host,
		secretsPath: filepath.Join("nixos/secrets", host+".yaml"),
		hostNix:     filepath.Join("nixos/hosts", host, "default.nix"),
		rotate:      rotate,
	}
	if !exists(root, p.secretsPath) {
		return nil, fmt.Errorf("%s does not exist — the host has no identity yet; run `fort mint %s` first", p.secretsPath, host)
	}
	p.haveHash = sopsHasKey(filepath.Join(root, p.secretsPath), keyDeployerPasswordHash)
	p.newRegistry = !exists(root, consoleRegistry)
	p.haveEntry = sopsHasKey(filepath.Join(root, consoleRegistry), host)
	if nix, err := os.ReadFile(filepath.Join(root, p.hostNix)); err == nil {
		p.enabled = consoleModuleRE.Match(nix)
	}
	return p, nil
}

// apply mints the passphrase, hashes it for the host and records the
// plaintext in the registry. The passphrase reaches mkpasswd on stdin
// and sops' argv, and nothing else — sops.go says why that argv is the
// trade the repo already makes for every other value it sets.
func (p *consolePlan) apply(tool secretsTool) error {
	pass, hash, err := newConsoleCredential()
	if err != nil {
		return err
	}
	// The registry first: a failure after the host's hash is set would
	// leave a fort whose console password nobody can read, which is the
	// one state this whole design exists to prevent.
	if err := recordPassphrase(tool, p.root, p.host, pass); err != nil {
		return err
	}
	if err := tool.SetWith(filepath.Join(p.root, "nixos/.sops.yaml"), filepath.Join(p.root, p.secretsPath), `["`+keyDeployerPasswordHash+`"]`, hash); err != nil {
		return err
	}
	if err := tool.GitAdd(p.secretsPath); err != nil {
		return err
	}
	p.passphrase, p.hash = pass, hash
	return nil
}

// newConsoleCredential is the pair: a fresh passphrase and its hash for
// the host. The passphrase reaches mkpasswd on stdin and sops' argv, and
// nothing else — sops.go says why that argv is the trade the repo
// already makes for every other value it sets.
func newConsoleCredential() (pass, hash string, err error) {
	if pass, err = newPassphrase(); err != nil {
		return "", "", err
	}
	if hash, err = hashPassword(pass); err != nil {
		return "", "", err
	}
	return pass, hash, nil
}

// recordPassphrase writes the registry half: the registry is created for
// the first fort — one file written whole and encrypted once, as mint
// does with a new secrets file — and set for every later one, then
// staged. Shared by this verb and fort mint, and called BEFORE the hash
// goes onto the host in both: the other order can leave a hash nobody
// can read back. The fleet .sops.yaml's provisioning/ rule picks the
// admin recipients and not the fort.
func recordPassphrase(tool secretsTool, root, host, pass string) error {
	fleetConfig := filepath.Join(root, ".sops.yaml")
	registry := filepath.Join(root, consoleRegistry)
	if !exists(root, consoleRegistry) {
		if err := os.MkdirAll(filepath.Dir(registry), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(registry, []byte(host+": "+pass+"\n"), 0o600); err != nil {
			return err
		}
		if err := tool.EncryptWith(fleetConfig, registry); err != nil {
			os.Remove(registry)
			return err
		}
	} else if err := tool.SetWith(fleetConfig, registry, `["`+host+`"]`, pass); err != nil {
		return err
	}
	return tool.GitAdd(consoleRegistry)
}

// show prints one fort's passphrase. One fort, because a whole-file
// `sops -d` puts every fort's console password on the screen to read
// one of them.
func (p *consolePlan) show(w io.Writer) error {
	if !p.haveEntry {
		return fmt.Errorf("%s has no entry for %s — its passphrase was never recorded, or `fort dismantle` withdrew it. A hash on the host cannot be read back; mint a new passphrase with `fort console-password %s --rotate`", consoleRegistry, p.host, p.host)
	}
	pass, err := readConsolePassword(p.root, p.host)
	if err != nil {
		return err
	}
	fmt.Fprintln(w, pass)
	return nil
}

func (p *consolePlan) report(w io.Writer, written bool) {
	fmt.Fprintf(w, "console-password ⛫ %s   (the getty door to `sudo nixos-rollback`; ssh never accepts it)\n", p.host)
	switch {
	case p.haveHash && !p.rotate:
		fmt.Fprintf(w, "○ %-40s carries %s already\n", p.secretsPath, keyDeployerPasswordHash)
	case p.haveHash:
		fmt.Fprintf(w, "○ %-40s %s REPLACED (--rotate)\n", p.secretsPath, keyDeployerPasswordHash)
	default:
		fmt.Fprintf(w, "○ %-40s + %s (sops set)\n", p.secretsPath, keyDeployerPasswordHash)
	}
	switch {
	case p.newRegistry:
		fmt.Fprintf(w, "● %-40s the registry, created for %s (admin recipients only)\n", consoleRegistry, p.host)
	case p.mints():
		fmt.Fprintf(w, "○ %-40s %s: the passphrase (sops set)\n", consoleRegistry, p.host)
	case p.haveEntry:
		fmt.Fprintf(w, "○ %-40s carries %s already\n", consoleRegistry, p.host)
	default:
		fmt.Fprintf(w, "! %-40s has NO entry for %s — the hash on the host cannot be read back\n", consoleRegistry, p.host)
	}
	if p.enabled {
		fmt.Fprintf(w, "○ %-40s sfi.ops.console.enable = true — the getty reads the hash\n", p.hostNix)
	} else {
		fmt.Fprintf(w, "! %-40s does not enable the module — nothing reads the hash yet\n", p.hostNix)
	}
	if p.haveHash && !p.rotate {
		if p.haveEntry {
			fmt.Fprintf(w, "\nnothing minted. read: console-password %s --show; replace: console-password %s --rotate\n", p.host, p.host)
		} else {
			fmt.Fprintf(w, "\nnothing minted. mint a new passphrase: console-password %s --rotate\n", p.host)
		}
		p.tail(w)
		return
	}
	if !written {
		fmt.Fprintln(w, "\ndry run — nothing minted.")
		return
	}
	fmt.Fprintf(w, "\nminted. passphrase in %s only (console-password %s --show)\n", consoleRegistry, p.host)
	p.tail(w)
}

// tail: one pointer when the host has not opted in; the ! line in the
// report carries the fact, this carries the fix.
func (p *consolePlan) tail(w io.Writer) {
	if p.enabled {
		return
	}
	fmt.Fprintf(w, "next: set sfi.ops.console.enable = true in %s and PR both files together — fort check gates the pair\n", p.hostNix)
}

// readConsolePassword decrypts one entry of the registry; a variable so
// the test never execs sops, exactly as fleetSecret keeps sops out of
// the mint tests.
var readConsolePassword = readConsolePasswordSops

func readConsolePasswordSops(root, host string) (string, error) {
	cmd, err := tools.Command("sops", "--config", filepath.Join(root, ".sops.yaml"),
		"-d", "--extract", `["`+host+`"]`,
		filepath.Join(root, consoleRegistry))
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("sops -d --extract [%q] %s: %s", host, consoleRegistry, lastLine(exitDetail(err)))
	}
	return strings.TrimSpace(string(out)), nil
}

// consoleFindings is the `fort check` half (#524's "exercise it, or it
// is only a belief"). The failsafe is three files, and every partial
// state fails quietly: a module with no hash fails the activation (an
// error, and since sfi/forts#24 the state a fort is in between scaffold
// and mint), a hash with no registry entry is a door nobody can open,
// and a credential no module reads is neither. A host with no half at
// all is silent: the scaffold enables the module on every shape, so a
// host without it took the line out on purpose.
func consoleFindings(root, fort string) []finding {
	hostNix := filepath.Join("nixos/hosts", fort, "default.nix")
	secretsPath := filepath.Join("nixos/secrets", fort+".yaml")
	nix, err := os.ReadFile(filepath.Join(root, hostNix))
	enabled := err == nil && consoleModuleRE.Match(nix)
	hash := sopsHasKey(filepath.Join(root, secretsPath), keyDeployerPasswordHash)
	entry := sopsHasKey(filepath.Join(root, consoleRegistry), fort)
	switch {
	case enabled && !hash:
		return []finding{{"error", secretsPath, "sfi.ops.console.enable is set in " + hostNix + " but " + keyDeployerPasswordHash + " is absent — sops-nix fails the activation on a declared-but-absent key. Mint it: fort mint " + fort + " (fort console-password " + fort + " mints it alone)"}}
	case enabled && !entry:
		return []finding{{"warning", consoleRegistry, "no entry for " + fort + ", which carries " + keyDeployerPasswordHash + " and enables the console login — the getty accepts a passphrase nobody can read, so the failsafe opens for nobody. Mint a new one: fort console-password " + fort + " --rotate"}}
	case enabled:
		return []finding{{"ok", hostNix, "console login: the module, " + keyDeployerPasswordHash + ", and the passphrase in " + consoleRegistry}}
	case hash || entry:
		return []finding{{"warning", hostNix, "this host does not set sfi.ops.console.enable, but a console credential exists for it (" + strings.Join(consoleHalves(hash, entry), " + ") + ") — no getty reads it. Enable the module, or withdraw the credential with fort dismantle"}}
	}
	return nil
}

// consoleHalves names the halves that are present, so the warning says
// which file to look in rather than making the reader check both.
func consoleHalves(hash, entry bool) []string {
	var out []string
	if hash {
		out = append(out, keyDeployerPasswordHash)
	}
	if entry {
		out = append(out, consoleRegistry)
	}
	return out
}

// sopsHasKey reports whether an encrypted file declares a top-level key.
// Key names are plaintext in a sops file whatever the creation rule's
// encrypted_regex says, so this needs no age key — the same property
// checkSecrets relies on.
func sopsHasKey(path, key string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, key+":") {
			return true
		}
	}
	return false
}

// consoleAlphabet is Crockford base32 in lower case: the digits and the
// letters minus i, l, o and u. Exactly 32 symbols, so a 5-bit slice of
// random is unbiased, and no pair of them is confusable on the 80x25
// serial console where this password is typed by hand, once, under
// pressure.
const consoleAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"

// newPassphrase returns 20 symbols in groups of four — 100 bits, and a
// shape a person can read off one screen and type into another without
// losing their place. It is never weakened for typing comfort: the
// hyphens are separators the operator types too.
func newPassphrase() (string, error) {
	const groups, per = 5, 4
	b := make([]byte, groups*per)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	var sb strings.Builder
	for i, v := range b {
		if i > 0 && i%per == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(consoleAlphabet[v&31])
	}
	return sb.String(), nil
}

// hashPassword hashes the passphrase; a variable so the test does not
// need mkpasswd (the CI go job runs on a runner that has no whois
// package and no nix), exactly as fleetSecret keeps sops out of the
// mint tests.
var hashPassword = mkpasswdHash

// mkpasswdHash runs the password through mkpasswd's default yescrypt —
// what NixOS's own `mkpasswd` gives and what the host's PAM verifies.
// The password goes in on stdin, so it is never an argv the process
// table shows. mkpasswd is one of the declared tools (internal/tools):
// the fort package carries it — #25 found a Mac that had none — and a
// checkout run builds it from nixpkgs.
func mkpasswdHash(password string) (string, error) {
	cmd, err := tools.Command("mkpasswd", "-m", "yescrypt", "-s")
	if err != nil {
		return "", err
	}
	cmd.Stdin = strings.NewReader(password + "\n")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("mkpasswd: %s", lastLine(errb.String()))
	}
	hash := strings.TrimSpace(out.String())
	if !strings.HasPrefix(hash, "$") {
		return "", fmt.Errorf("mkpasswd returned no crypt(5) hash")
	}
	return hash, nil
}
