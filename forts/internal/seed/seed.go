// Package seed is the Go side of the seed format (#606; the decision is
// deployments#605): what travels from the repo to a fort at birth, through
// the channel its platform provides, so the bootstrap image carries no
// secret. The reader on the host is nixos/modules/seed/reader.sh, and the
// two must agree on the format — this package's test is that contract.
//
// The seed is the host's ed25519 ssh key as `fort mint` wrote it into
// nixos/secrets/<host>.yaml (ssh-host-key, then ssh-host-key.pub), the two
// files concatenated. The reader splits at the PEM trailer; everything
// after it that starts with "ssh-ed25519 " is the public line. Nothing
// else travels: the image still carries the sops file, which is in git.
package seed

import (
	"encoding/base64"
	"fmt"
	"strings"
)

// Env is how `fort provision` hands the seed to the pulumi program, per
// run, as the value the Linode instance's metadata.user_data takes
// (base64, the service's contract). An environment variable rather than
// stack config: Pulumi.<host>.yaml would be a third copy of the key, and
// the checkpoint already holds one (as a pulumi secret). The program
// reads it at creation only (ignoreChanges on the instance's metadatas),
// so a day-2 apply without it never diffs a running host.
const Env = "FORT_SEED_USER_DATA"

// LinodeUserDataMax is the Metadata service's cap on user_data: 65535
// bytes, base64-encoded.
const LinodeUserDataMax = 65535

const (
	pemHeader  = "-----BEGIN OPENSSH PRIVATE KEY-----"
	pemTrailer = "-----END OPENSSH PRIVATE KEY-----"
	pubPrefix  = "ssh-ed25519 "
)

// Compose is the seed text: the private key, then the public line, each
// ending in exactly one newline (sshd wants the trailing newline on the
// key file; the reader writes what it is given).
func Compose(private, public string) (string, error) {
	private = strings.TrimRight(private, "\n") + "\n"
	public = strings.TrimSpace(public) + "\n"
	if !strings.HasPrefix(private, pemHeader+"\n") || !strings.HasSuffix(private, "\n"+pemTrailer+"\n") {
		return "", fmt.Errorf("seed: the private key is not an OpenSSH PEM block")
	}
	if !strings.HasPrefix(public, pubPrefix) {
		return "", fmt.Errorf("seed: the public key is not an ssh-ed25519 line")
	}
	return private + public, nil
}

// UserData is Compose, base64-encoded for metadata.user_data, with the
// service's size cap asserted (a host key is a few hundred bytes; the
// check is for a wrong value, not a large one).
func UserData(private, public string) (string, error) {
	text, err := Compose(private, public)
	if err != nil {
		return "", err
	}
	enc := base64.StdEncoding.EncodeToString([]byte(text))
	if len(enc) > LinodeUserDataMax {
		return "", fmt.Errorf("seed: %d bytes encoded exceeds the Metadata service's %d-byte cap", len(enc), LinodeUserDataMax)
	}
	return enc, nil
}

// Parse mirrors the reader's split (nixos/modules/seed/reader.sh): the
// PEM block from header to trailer, then the first ssh-ed25519 line after
// the trailer. It exists so the test proves the shell and the Go agree.
func Parse(text string) (private, public string, err error) {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	start, end := -1, -1
	for i, l := range lines {
		switch {
		case l == pemHeader && start < 0:
			start = i
		case l == pemTrailer && start >= 0 && end < 0:
			end = i
		}
	}
	if start < 0 || end < 0 {
		return "", "", fmt.Errorf("seed: no OpenSSH PEM header and trailer")
	}
	private = strings.Join(lines[start:end+1], "\n") + "\n"
	for _, l := range lines[end+1:] {
		if strings.HasPrefix(l, pubPrefix) {
			return private, l + "\n", nil
		}
	}
	return "", "", fmt.Errorf("seed: no ssh-ed25519 public line after the PEM trailer")
}
