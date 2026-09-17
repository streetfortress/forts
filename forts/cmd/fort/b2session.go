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

	"gitea.zen.lofi/sfi/deployments/forts/internal/b2"
)

// Where a B2-touching verb gets its credential. Deliberately NOT from
// provisioning/secrets.yaml (mintbackup.go's header): both hand-held keys
// are operational inputs that live in the password manager. So the verb
// asks for one at the moment it needs it —
//
//	B2_KEY_ID / B2_APP_KEY set   used as they are (scripts, CI-less reruns)
//	otherwise, on a terminal     prompted: the id echoed, the key not
//	otherwise                    refused, naming which credential to export
//
// — which replaces the `read -s B2_APP_KEY; export …` dance, and with it
// the exported secret that outlived the one command it was for.

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

func b2EnvSet() bool { return os.Getenv("B2_KEY_ID") != "" && os.Getenv("B2_APP_KEY") != "" }

// b2Session authorizes with the credential `need` describes. purpose
// finishes the sentence "<purpose> needs your …" in the prompt.
func b2Session(need b2Need, purpose string) (*b2.Client, error) {
	if b2Client != nil {
		return b2Client, nil
	}
	id, appKey := os.Getenv("B2_KEY_ID"), os.Getenv("B2_APP_KEY")
	if id == "" || appKey == "" {
		if !stdinIsTTY() {
			return nil, fmt.Errorf("B2_KEY_ID / B2_APP_KEY unset and no terminal to ask on — %s needs the %s. It is an operational input, not a repo secret (it can mint a key that deletes backups): export the pair from the password manager for this command, or run it interactively to be prompted", purpose, need.credential)
		}
		var err error
		if id, appKey, err = promptB2(need, purpose); err != nil {
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

// promptB2 asks on the terminal. Prompts go to stderr so a verb's stdout
// stays pipeable; the key half is read without echo.
func promptB2(need b2Need, purpose string) (id, appKey string, err error) {
	fmt.Fprintf(os.Stderr, "%s needs your B2 %s.\n", purpose, need.credential)
	fmt.Fprint(os.Stderr, "  Paste the key ID (empty to skip): ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", "", fmt.Errorf("reading the key ID: %w", err)
	}
	if id = strings.TrimSpace(line); id == "" {
		return "", "", errB2Skipped
	}
	fmt.Fprint(os.Stderr, "  Paste the key (not echoed): ")
	raw, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", "", fmt.Errorf("reading the key: %w", err)
	}
	if appKey = strings.TrimSpace(string(raw)); appKey == "" {
		return "", "", errB2Skipped
	}
	return id, appKey, nil
}
