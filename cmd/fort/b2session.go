package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"gitea.zen.lofi/sfi/forts/internal/b2"
)

// Where a B2-touching verb gets its credential. The SECRET half is
// deliberately NOT in provisioning/secrets.yaml (mintbackup.go's header):
// both hand-held keys are operational inputs that live in the password
// manager. So the verb asks for it at the moment it needs it —
//
//	B2_APP_KEY set               used as it is (scripts, CI-less reruns)
//	otherwise, on a terminal     prompted, not echoed
//	otherwise                    refused, naming which credential to export
//
// — which replaces the `read -s B2_APP_KEY; export …` dance, and with it
// the exported secret that outlived the one command it was for.
//
// The key's ID is not a secret (#432): the prompt echoes it, every
// b2_list_keys row carries it, and it grants nothing without the secret.
// So the minting key's ID lives in provisioning/secrets.yaml as
// b2-minting-key-id, seeded once, and a minting session is ONE paste
// instead of two — which also removes the one prompt that could be
// answered wrong silently (an ID from another entry fails only at the
// secret's auth step). B2_KEY_ID stays as an override; the master key's
// ID is still asked for, since nothing on file names it.

// b2Need is WHICH credential a call wants, so the prompt can ask for the
// least one that works instead of reaching for the master key every time.
type b2Need struct {
	// credential is the password-manager entry, in the operator's words.
	credential string
	// master: only the account master key will do.
	master bool
	// caps the call is about to use; checked against what B2 says the
	// supplied credential holds, so a wrong paste fails here by name and
	// not three calls later as a bare 401.
	caps []string
}

var (
	// Bootstrapping sfi-b2-key-minting-key is the one thing nothing but
	// the master key can do.
	needMaster = b2Need{credential: "ACCOUNT MASTER key", master: true}
	// Everything else: B2 does not enforce capability subsetting (#291),
	// so the minting key creates every other shape, and revokes them.
	needMinting = b2Need{credential: "sfi-b2-key-minting-key (the master key also works)", caps: []string{"listKeys", "writeKeys", "deleteKeys"}}
)

// stdinIsTTY is a var so tests can pin it either way.
var stdinIsTTY = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// errB2Skipped is an operator leaving the prompt empty: a verb for which
// the credential is optional (destroy's revoke) reports and carries on.
var errB2Skipped = errors.New("no B2 credential given")

// b2Client is the session for the life of the process, so a verb that
// plans and then writes (fort mint) asks once.
var b2Client *b2.Client

// keyB2MintingKeyID is where the minting key's ID sits in
// provisioning/secrets.yaml. Seed it once, from the password manager:
//
//	sops --config .sops.yaml set provisioning/secrets.yaml '["b2-minting-key-id"]' '"<id>"'
const keyB2MintingKeyID = "b2-minting-key-id"

// storedMintingKeyID is that ID, or "" when it is not on file (or the
// file does not decrypt here): the prompt then asks for both halves, as
// it always did, and says how to put the ID on file. A var so tests can
// pin it without an age key.
var storedMintingKeyID = func() string {
	root, err := repoRoot()
	if err != nil {
		return ""
	}
	id, err := secret(root, keyB2MintingKeyID)
	if err != nil {
		return ""
	}
	return id
}

// b2EnvSet: the secret half is in the environment, so a verb can verify
// without stopping to ask. The ID is on file for the minting key, so
// B2_APP_KEY alone is enough.
func b2EnvSet() bool { return os.Getenv("B2_APP_KEY") != "" }

// b2Session authorizes with the credential `need` describes. purpose
// finishes the sentence "<purpose> needs your …" in the prompt.
func b2Session(need b2Need, purpose string) (*b2.Client, error) {
	if b2Client != nil {
		return b2Client, nil
	}
	id, appKey := os.Getenv("B2_KEY_ID"), os.Getenv("B2_APP_KEY")
	if appKey == "" && !stdinIsTTY() {
		return nil, fmt.Errorf("B2_APP_KEY unset and no terminal to ask on — %s needs the %s. It is an operational input, not a repo secret (it can mint a key that deletes backups): export it from the password manager for this command (B2_KEY_ID too, unless the id is on file as %s), or run it interactively to be prompted", purpose, need.credential, keyB2MintingKeyID)
	}
	if id == "" && !need.master {
		id = storedMintingKeyID()
	}
	if id == "" && !stdinIsTTY() {
		return nil, fmt.Errorf("B2_KEY_ID unset and no terminal to ask on — %s needs the %s. Export B2_KEY_ID beside B2_APP_KEY for this command%s", purpose, need.credential, storeIDHint(need))
	}
	if id == "" || appKey == "" {
		var err error
		if id, appKey, err = promptB2(need, purpose, id); err != nil {
			return nil, err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, err := b2.Authorize(ctx, "", id, appKey)
	if err != nil {
		return nil, fmt.Errorf("B2 authorize: %w", err)
	}
	if need.master && !c.Master() {
		return nil, fmt.Errorf("key %s is not the account master key — %s needs the %s", id, purpose, need.credential)
	}
	for _, want := range need.caps {
		if !c.Can(want) {
			return nil, fmt.Errorf("key %s does not hold %s — %s needs the %s", id, want, purpose, need.credential)
		}
	}
	if !need.master && c.Master() {
		fmt.Fprintln(os.Stderr, "note: that was the account MASTER key; sfi-b2-key-minting-key is enough for this.")
	}
	b2Client = c
	return c, nil
}

// storeIDHint finishes a message with how to make the ID prompt go away
// — for the minting key only; the master key's ID is always typed.
func storeIDHint(need b2Need) string {
	if need.master {
		return "."
	}
	return fmt.Sprintf(", or put the id on file once so only the key is asked for:\n  sops --config .sops.yaml set provisioning/secrets.yaml '[%q]' '\"<id>\"'", keyB2MintingKeyID)
}

// promptB2 asks on the terminal. Prompts go to stderr so a verb's stdout
// stays pipeable; the key half is read without echo. A known id (from
// the environment or the file) is printed as context and not asked for.
func promptB2(need b2Need, purpose, id string) (string, string, error) {
	fmt.Fprintf(os.Stderr, "%s needs your B2 %s.\n", purpose, need.credential)
	if id != "" {
		fmt.Fprintf(os.Stderr, "  key id %s (B2_KEY_ID overrides it)\n", id)
	} else {
		fmt.Fprint(os.Stderr, "  Paste the key ID (empty to skip): ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", "", fmt.Errorf("reading the key ID: %w", err)
		}
		if id = strings.TrimSpace(line); id == "" {
			return "", "", errB2Skipped
		}
		if !need.master {
			fmt.Fprintf(os.Stderr, "  (the id is not a secret — put it on file once and only the key is asked for:\n   sops --config .sops.yaml set provisioning/secrets.yaml '[%q]' '\"%s\"')\n", keyB2MintingKeyID, id)
		}
	}
	fmt.Fprint(os.Stderr, "  Paste the key (not echoed, empty to skip): ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", "", fmt.Errorf("reading the key: %w", err)
	}
	appKey := strings.TrimSpace(string(raw))
	if appKey == "" {
		return "", "", errB2Skipped
	}
	return id, appKey, nil
}
