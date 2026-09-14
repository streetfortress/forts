package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/ssh"
)

// The identity material fort mint produces, generated here rather than
// by ssh-keygen / wg / age-keygen / ssh-to-age so that no private half
// is ever a file outside sops: each lives in memory until the host's
// secrets file is encrypted. Formats match the tools byte-for-byte
// (the host reads them back with sshd, wg and age).

// sshKey is an ed25519 OpenSSH key pair as the files ssh-keygen writes.
type sshKey struct {
	private string // "-----BEGIN OPENSSH PRIVATE KEY-----\n…" (trailing newline)
	public  string // "ssh-ed25519 AAAA… <comment>\n"
}

func newSSHKey(comment string) (sshKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return sshKey{}, err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return sshKey{}, err
	}
	return sshKey{private: string(pem.EncodeToMemory(block)), public: sshPublicLine(pub, comment)}, nil
}

// sshKeyFrom rebuilds the pair from a stored private key (the re-run
// path: the repo is the key store).
func sshKeyFrom(private, comment string) (sshKey, error) {
	raw, err := ssh.ParseRawPrivateKey([]byte(private))
	if err != nil {
		return sshKey{}, fmt.Errorf("stored ssh key: %w", err)
	}
	priv, ok := raw.(*ed25519.PrivateKey)
	if !ok {
		return sshKey{}, fmt.Errorf("stored ssh key is not ed25519")
	}
	return sshKey{private: private, public: sshPublicLine(priv.Public().(ed25519.PublicKey), comment)}, nil
}

func sshPublicLine(pub ed25519.PublicKey, comment string) string {
	k, _ := ssh.NewPublicKey(pub)
	return k.Type() + " " + base64.StdEncoding.EncodeToString(k.Marshal()) + " " + comment + "\n"
}

// sshToAge is ssh-to-age: the ed25519 public key's Edwards point mapped
// to its Montgomery u-coordinate (u = (1+y)/(1-y) mod 2^255-19) and
// encoded as an age X25519 recipient — what sops-nix uses so a host's
// ssh key doubles as its age identity.
func sshToAge(publicLine string) (string, error) {
	fields := strings.Fields(publicLine)
	if len(fields) < 2 || fields[0] != "ssh-ed25519" {
		return "", fmt.Errorf("not an ssh-ed25519 public key")
	}
	wire, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "", err
	}
	k, err := ssh.ParsePublicKey(wire)
	if err != nil {
		return "", err
	}
	pub := k.(ssh.CryptoPublicKey).CryptoPublicKey().(ed25519.PublicKey)
	// y is the little-endian encoding with the sign bit of x cleared.
	yb := make([]byte, 32)
	copy(yb, pub)
	yb[31] &= 0x7f
	for i, j := 0, len(yb)-1; i < j; i, j = i+1, j-1 {
		yb[i], yb[j] = yb[j], yb[i]
	}
	p := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 255), big.NewInt(19))
	y := new(big.Int).SetBytes(yb)
	num := new(big.Int).Add(big.NewInt(1), y)
	den := new(big.Int).Sub(big.NewInt(1), y)
	den.Mod(den, p)
	u := new(big.Int).Mul(num, den.ModInverse(den, p))
	u.Mod(u, p)
	ub := u.FillBytes(make([]byte, 32))
	for i, j := 0, len(ub)-1; i < j; i, j = i+1, j-1 {
		ub[i], ub[j] = ub[j], ub[i]
	}
	return bech32Encode("age", ub)
}

// wgKey is a WireGuard key pair as `wg genkey` / `wg pubkey` print them.
type wgKey struct{ private, public string }

func newWGKey() (wgKey, error) {
	var priv [32]byte
	if _, err := rand.Read(priv[:]); err != nil {
		return wgKey{}, err
	}
	// wg genkey clamps; X25519 would too, but the stored key is the
	// clamped one so `wg pubkey` on the host agrees byte-for-byte.
	priv[0] &= 248
	priv[31] = (priv[31] & 127) | 64
	return wgKeyFrom(base64.StdEncoding.EncodeToString(priv[:]))
}

func wgKeyFrom(private string) (wgKey, error) {
	priv, err := base64.StdEncoding.DecodeString(strings.TrimSpace(private))
	if err != nil || len(priv) != 32 {
		return wgKey{}, fmt.Errorf("stored wireguard key: not 32 base64 bytes")
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return wgKey{}, err
	}
	return wgKey{private: strings.TrimSpace(private), public: base64.StdEncoding.EncodeToString(pub)}, nil
}

// ageKey is an X25519 age identity as age-keygen prints it.
type ageKey struct{ identity, recipient string }

func newAgeKey() (ageKey, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return ageKey{}, err
	}
	id, err := bech32Encode("age-secret-key-", secret[:])
	if err != nil {
		return ageKey{}, err
	}
	return ageKeyFrom(strings.ToUpper(id))
}

func ageKeyFrom(identity string) (ageKey, error) {
	identity = strings.TrimSpace(identity)
	hrp, secret, err := bech32Decode(strings.ToLower(identity))
	if err != nil || hrp != "age-secret-key-" || len(secret) != 32 {
		return ageKey{}, fmt.Errorf("stored age key: not an AGE-SECRET-KEY-1… identity")
	}
	pub, err := curve25519.X25519(secret, curve25519.Basepoint)
	if err != nil {
		return ageKey{}, err
	}
	recipient, err := bech32Encode("age", pub)
	if err != nil {
		return ageKey{}, err
	}
	return ageKey{identity: identity, recipient: recipient}, nil
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil // = openssl rand -hex 32
}

// --- bech32 (BIP 173, the original checksum — what age uses)

const bech32Charset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"

func bech32Polymod(values []byte) uint32 {
	gen := [5]uint32{0x3b6a57b2, 0x26508e6d, 0x1ea119fa, 0x3d4233dd, 0x2a1462b3}
	chk := uint32(1)
	for _, v := range values {
		top := chk >> 25
		chk = (chk&0x1ffffff)<<5 ^ uint32(v)
		for i := 0; i < 5; i++ {
			if (top>>uint(i))&1 == 1 {
				chk ^= gen[i]
			}
		}
	}
	return chk
}

func bech32HRPExpand(hrp string) []byte {
	out := make([]byte, 0, 2*len(hrp)+1)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]>>5)
	}
	out = append(out, 0)
	for i := 0; i < len(hrp); i++ {
		out = append(out, hrp[i]&31)
	}
	return out
}

func convertBits(data []byte, from, to uint, pad bool) ([]byte, error) {
	var acc uint32
	var bits uint
	var out []byte
	maxv := uint32(1<<to - 1)
	for _, b := range data {
		acc = acc<<from | uint32(b)
		bits += from
		for bits >= to {
			bits -= to
			out = append(out, byte(acc>>bits&maxv))
		}
	}
	if pad {
		if bits > 0 {
			out = append(out, byte(acc<<(to-bits)&maxv))
		}
	} else if bits >= from || acc<<(to-bits)&maxv != 0 {
		return nil, fmt.Errorf("bech32: invalid padding")
	}
	return out, nil
}

func bech32Encode(hrp string, data []byte) (string, error) {
	values, err := convertBits(data, 8, 5, true)
	if err != nil {
		return "", err
	}
	poly := bech32Polymod(append(append(bech32HRPExpand(hrp), values...), 0, 0, 0, 0, 0, 0)) ^ 1
	var sb strings.Builder
	sb.WriteString(hrp)
	sb.WriteByte('1')
	for _, v := range values {
		sb.WriteByte(bech32Charset[v])
	}
	for i := 0; i < 6; i++ {
		sb.WriteByte(bech32Charset[(poly>>uint(5*(5-i)))&31])
	}
	return sb.String(), nil
}

func bech32Decode(s string) (string, []byte, error) {
	pos := strings.LastIndexByte(s, '1')
	if pos < 1 || pos+7 > len(s) {
		return "", nil, fmt.Errorf("bech32: malformed")
	}
	hrp := s[:pos]
	values := make([]byte, 0, len(s)-pos-1)
	for _, c := range []byte(s[pos+1:]) {
		i := strings.IndexByte(bech32Charset, c)
		if i < 0 {
			return "", nil, fmt.Errorf("bech32: bad character")
		}
		values = append(values, byte(i))
	}
	if bech32Polymod(append(bech32HRPExpand(hrp), values...)) != 1 {
		return "", nil, fmt.Errorf("bech32: bad checksum")
	}
	data, err := convertBits(values[:len(values)-6], 5, 8, false)
	if err != nil {
		return "", nil, err
	}
	return hrp, data, nil
}
