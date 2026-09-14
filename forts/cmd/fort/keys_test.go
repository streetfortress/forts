package main

import (
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
)

// Golden vectors from the real tools on throwaway keys (public material
// and one never-used test identity), so the in-process generation is
// checked against ssh-keygen / ssh-to-age / wg / age-keygen rather than
// against itself.
func TestSSHToAge(t *testing.T) {
	const pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGx6GXsI2LCgCru4Jm5qAV4xKqMfWHKrmW2pVHvG6GGa vector\n"
	got, err := sshToAge(pub)
	if err != nil || got != "age14zlrlx3cmug7w4x3hh34ymkfvcpefwkmt4wstp2xynjrw9ntq9wsj5fapj" {
		t.Errorf("ssh-to-age: %q %v", got, err)
	}
	if _, err := sshToAge("ssh-rsa AAAA x"); err == nil {
		t.Error("rsa should be refused")
	}
}

func TestAgeKey(t *testing.T) {
	const id = "AGE-SECRET-KEY-104U5P2EWFVZE2WA8X3AZLTY5YQKPD782FPYSLD0FEQFJFS6EJ7VS3FZH2S" // throwaway, never used
	k, err := ageKeyFrom(id)
	if err != nil || k.recipient != "age19f4rgew5u5t5w2r75p4wxfgrt0ppkagc6udveng560fgmyp724zshumvz4" {
		t.Errorf("age-keygen -y: %q %v", k.recipient, err)
	}
	fresh, err := newAgeKey()
	if err != nil || !strings.HasPrefix(fresh.identity, "AGE-SECRET-KEY-1") || !strings.HasPrefix(fresh.recipient, "age1") {
		t.Errorf("new age key: %v %q", err, fresh.recipient)
	}
	again, _ := ageKeyFrom(fresh.identity)
	if again.recipient != fresh.recipient {
		t.Error("identity → recipient is not stable")
	}
	if _, err := ageKeyFrom("age1notanidentity"); err == nil {
		t.Error("a recipient is not an identity")
	}
}

func TestWGKey(t *testing.T) {
	// RFC 7748 §6.1: Alice's private and public keys, as `wg pubkey` derives them.
	priv, _ := hex.DecodeString("77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a")
	pub, _ := hex.DecodeString("8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a")
	k, err := wgKeyFrom(base64.StdEncoding.EncodeToString(priv))
	if err != nil || k.public != base64.StdEncoding.EncodeToString(pub) {
		t.Errorf("wg pubkey: %q %v", k.public, err)
	}
	fresh, err := newWGKey()
	if err != nil || len(fresh.private) != 44 || len(fresh.public) != 44 {
		t.Errorf("new wg key: %v", err)
	}
	if again, _ := wgKeyFrom(fresh.private); again.public != fresh.public {
		t.Error("private → public is not stable")
	}
}

func TestSSHKey(t *testing.T) {
	k, err := newSSHKey("host-x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.private, "-----BEGIN OPENSSH PRIVATE KEY-----\n") || !strings.HasSuffix(k.private, "-----END OPENSSH PRIVATE KEY-----\n") {
		t.Errorf("private key format: %.40q…", k.private)
	}
	if !strings.HasPrefix(k.public, "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI") || !strings.HasSuffix(k.public, " host-x\n") {
		t.Errorf("public key line: %q", k.public)
	}
	again, err := sshKeyFrom(k.private, "host-x")
	if err != nil || again.public != k.public {
		t.Errorf("restore from private: %v", err)
	}
	if _, err := sshToAge(k.public); err != nil {
		t.Errorf("host key → age: %v", err)
	}
}

func TestBech32(t *testing.T) {
	// BIP 173 vectors.
	if s, _ := bech32Encode("a", nil); s != "a12uel5l" {
		t.Errorf("encode: %q", s)
	}
	hrp, data, err := bech32Decode("a12uel5l")
	if err != nil || hrp != "a" || len(data) != 0 {
		t.Errorf("decode: %q %v %v", hrp, data, err)
	}
	if _, _, err := bech32Decode("a12uel5m"); err == nil {
		t.Error("bad checksum accepted")
	}
}
