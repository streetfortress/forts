package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"gopkg.in/yaml.v3"
)

// fakeGiteaState is the instance as the bundle sees it: the repo's
// deploy keys, and on the sfi-agent user its ssh keys, gpg keys and
// tokens. Auth is checked the way gitea does — the fleet token on every
// route, Sudo: sfi-agent on /user/*, basic auth (admin:token) on the
// token routes — a gpg key registers verified only when the posted
// signature really signs the verification string, and the list shows
// it the way gitea does: public_key is the bare primary-key packet,
// base64, no uid (the finding of sfi/gotea#35). So the round trip mint
// performs is the one gitea performs, and destroy cannot cheat.
type fakeGiteaState struct {
	deploy  []giteaKey
	keys    []giteaKey
	gpg     []giteaGPGKey
	tokens  []giteaToken
	deleted []string
}

const fakeGPGToken = "verify-me-1234"

func newFakeGitea(t *testing.T, st *fakeGiteaState) *giteaDeployKeys {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		basicUser, basicPass, basic := r.BasicAuth()
		if strings.HasPrefix(path, "/users/sfi-agent/tokens") {
			if !basic || basicUser != "admin" || basicPass != "t" {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"message":"token routes want basic auth"}`)
				return
			}
		} else if r.Header.Get("Authorization") != "token t" {
			w.WriteHeader(401)
			return
		}
		if strings.HasPrefix(path, "/user/") && r.Header.Get("Sudo") != agentUser {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"not sudo"}`)
			return
		}
		id := func(p string) int { n, _ := strconv.Atoi(p[strings.LastIndex(p, "/")+1:]); return n }
		switch {
		case r.Method == "GET" && path == "/user":
			fmt.Fprint(w, `{"login":"admin"}`)
		case r.Method == "GET" && path == "/keys":
			json.NewEncoder(w).Encode(st.deploy)
		case r.Method == "POST" && path == "/keys":
			var k giteaKey
			json.NewDecoder(r.Body).Decode(&k)
			k.ID = len(st.deploy) + 1
			st.deploy = append(st.deploy, k)
			w.WriteHeader(201)
		case r.Method == "GET" && path == "/user/keys":
			json.NewEncoder(w).Encode(st.keys)
		case r.Method == "POST" && path == "/user/keys":
			var k giteaKey
			json.NewDecoder(r.Body).Decode(&k)
			k.ID = 100 + len(st.keys)
			st.keys = append(st.keys, k)
			w.WriteHeader(201)
		case r.Method == "DELETE" && strings.HasPrefix(path, "/user/keys/"):
			st.deleted = append(st.deleted, path)
			w.WriteHeader(204)
		case r.Method == "GET" && path == "/user/gpg_key_token":
			fmt.Fprint(w, fakeGPGToken)
		case r.Method == "GET" && path == "/user/gpg_keys":
			json.NewEncoder(w).Encode(st.gpg)
		case r.Method == "POST" && path == "/user/gpg_keys":
			var f struct{ Pub, Sig string }
			var raw map[string]string
			json.NewDecoder(r.Body).Decode(&raw)
			f.Pub, f.Sig = raw["armored_public_key"], raw["armored_signature"]
			el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(f.Pub))
			if err != nil || len(el) != 1 {
				w.WriteHeader(422)
				return
			}
			verified := false
			if f.Sig != "" {
				_, err := openpgp.CheckArmoredDetachedSignature(el, strings.NewReader(fakeGPGToken), strings.NewReader(f.Sig), nil)
				verified = err == nil
			}
			st.gpg = append(st.gpg, giteaGPGKey{ID: 200 + len(st.gpg), KeyID: el[0].PrimaryKey.KeyIdString(), PublicKey: barePacket(el[0]), Verified: verified})
			w.WriteHeader(201)
		case r.Method == "POST" && path == "/user/gpg_key_verify":
			var f struct {
				KeyID string `json:"key_id"`
				Sig   string `json:"armored_signature"`
			}
			json.NewDecoder(r.Body).Decode(&f)
			for i, k := range st.gpg {
				if k.KeyID == f.KeyID {
					// the bare packet still verifies a signature
					pk, _ := packet.Read(base64.NewDecoder(base64.StdEncoding, strings.NewReader(k.PublicKey)))
					el := openpgp.EntityList{{PrimaryKey: pk.(*packet.PublicKey)}}
					if _, err := openpgp.CheckArmoredDetachedSignature(el, strings.NewReader(fakeGPGToken), strings.NewReader(f.Sig), nil); err == nil {
						st.gpg[i].Verified = true
					}
				}
			}
			w.WriteHeader(201)
		case r.Method == "DELETE" && strings.HasPrefix(path, "/user/gpg_keys/"):
			st.deleted = append(st.deleted, path)
			w.WriteHeader(204)
		case r.Method == "GET" && path == "/users/sfi-agent/tokens":
			json.NewEncoder(w).Encode(st.tokens)
		case r.Method == "POST" && path == "/users/sfi-agent/tokens":
			var tk giteaToken
			json.NewDecoder(r.Body).Decode(&tk)
			tk.ID = 300 + len(st.tokens)
			tk.Token = "minted-" + tk.Name
			st.tokens = append(st.tokens, tk)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(tk)
			_ = id
		case r.Method == "DELETE" && strings.HasPrefix(path, "/users/sfi-agent/tokens/"):
			st.deleted = append(st.deleted, path)
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return &giteaDeployKeys{client: srv.Client(), api: srv.URL, base: srv.URL, token: "t"}
}

// barePacket is gitea's stored form of a GPG key: the primary public-key
// packet alone, base64 — 72 characters for ed25519, no armor, no uid.
func barePacket(e *openpgp.Entity) string {
	var b bytes.Buffer
	e.PrimaryKey.Serialize(&b)
	return base64.StdEncoding.EncodeToString(b.Bytes())
}

func TestGPGKeyRoundTrip(t *testing.T) {
	k, err := newGPGKey("agent-x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.private, "-----BEGIN PGP PRIVATE KEY BLOCK-----") || !strings.HasPrefix(k.public, "-----BEGIN PGP PUBLIC KEY BLOCK-----") || len(k.keyID) != 16 {
		t.Fatalf("shape: keyID=%q", k.keyID)
	}
	again, err := gpgKeyFrom(k.private)
	if err != nil || again.keyID != k.keyID || again.public != k.public {
		t.Fatalf("restore: %v keyID %s vs %s", err, again.keyID, k.keyID)
	}
	// the verification gitea performs, with the newline variants it tolerates
	sig, err := k.detachSign("tok")
	if err != nil {
		t.Fatal(err)
	}
	el, _ := openpgp.ReadArmoredKeyRing(strings.NewReader(k.public))
	if _, err := openpgp.CheckArmoredDetachedSignature(el, strings.NewReader("tok"), strings.NewReader(sig), nil); err != nil {
		t.Errorf("signature over the exact bytes must verify: %v", err)
	}
	if _, err := openpgp.CheckArmoredDetachedSignature(el, strings.NewReader("other"), strings.NewReader(sig), nil); err == nil {
		t.Error("signature must not verify another message")
	}
	if _, err := gpgKeyFrom(k.public); err == nil {
		t.Error("a public key is not the secret half")
	}
}

func TestKeyStanding(t *testing.T) {
	pub := "ssh-ed25519 AAAAtest agent-x\n"
	if s, reg, err := keyStanding(nil, "agent-x", pub, "ssh key", "s.yaml", keyAgentSSH); err != nil || !reg || s != "would register" {
		t.Errorf("absent: %q %v %v", s, reg, err)
	}
	keys := []giteaKey{{ID: 1, Title: "agent-x", Key: "ssh-ed25519 AAAAtest"}, {ID: 2, Title: "agent-y", Key: "ssh-ed25519 AAAAnope"}}
	if s, reg, err := keyStanding(keys, "agent-x", pub, "ssh key", "s.yaml", keyAgentSSH); err != nil || reg || !strings.Contains(s, "id 1, verified match") {
		t.Errorf("match: %q %v %v", s, reg, err)
	}
	keys = append(keys, giteaKey{ID: 3, Title: "agent-x", Key: "ssh-ed25519 AAAAstale"})
	if _, _, err := keyStanding(keys, "agent-x", pub, "ssh key", "s.yaml", keyAgentSSH); err == nil || !strings.Contains(err.Error(), "id 3) differs") {
		t.Errorf("stale: %v", err)
	}
	gpgs := []giteaGPGKey{{ID: 5, KeyID: "abcdef0123456789", Verified: false}}
	if s, reg, ver := gpgStanding(gpgs, "ABCDEF0123456789"); reg || !ver || !strings.Contains(s, "not verified") {
		t.Errorf("unverified: %q %v %v", s, reg, ver)
	}
	if s, reg, ver := gpgStanding(nil, "X"); !reg || ver || s != "would register + verify" {
		t.Errorf("absent gpg: %q %v %v", s, reg, ver)
	}
}

// The token guard: the file and gitea must agree.
func TestPlanAgentTokenGuards(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755)
		os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644)
	}
	write(manifest.Path("x"), "mesh_ip = \"10.1.0.99\"\n[dev]\n[backups]\nprofile = \"none\"\n[libvirt]\nip = \"10.0.0.99\"\n")
	entry, err := manifest.Load(root, "x")
	if err != nil {
		t.Fatal(err)
	}
	fleetSecret = func(root, key string) (string, error) { return "claude", nil }
	t.Cleanup(func() { fleetSecret = secret })
	plan := func(have map[string]string, st *fakeGiteaState) error {
		p := &mintPlan{root: root, host: "x", entry: entry, have: have, secretsPath: "nixos/secrets/x.yaml"}
		take := func(k string) (string, bool) { v, ok := have[k]; return v, ok }
		return p.planAgent(take, newFakeGitea(t, st))
	}
	if err := plan(map[string]string{keyAgentToken: "abc"}, &fakeGiteaState{}); err == nil || !strings.Contains(err.Error(), "revoked by hand") {
		t.Errorf("in file, not on gitea: %v", err)
	}
	if err := plan(map[string]string{}, &fakeGiteaState{tokens: []giteaToken{{ID: 1, Name: "agent-x"}}}); err == nil || !strings.Contains(err.Error(), "stale token") {
		t.Errorf("on gitea, not in file: %v", err)
	}
	fleetSecret = func(root, key string) (string, error) { return "", os.ErrNotExist }
	if err := plan(map[string]string{}, &fakeGiteaState{}); err == nil || !strings.Contains(err.Error(), "claude setup-token") {
		t.Errorf("no fleet claude token: %v", err)
	}
}

func TestRevokeAgentIdentity(t *testing.T) {
	mine, _ := newGPGKey("agent-gone")
	other, _ := newGPGKey("agent-other")
	st := &fakeGiteaState{
		keys:   []giteaKey{{ID: 7, Title: "agent-other"}, {ID: 9, Title: "agent-gone"}},
		gpg:    []giteaGPGKey{{ID: 20, KeyID: other.keyID, PublicKey: barePacket(other.entity)}, {ID: 21, KeyID: mine.keyID, PublicKey: barePacket(mine.entity)}},
		tokens: []giteaToken{{ID: 30, Name: "agent-gone"}, {ID: 31, Name: "agent-other"}},
	}
	g := newFakeGitea(t, st)
	msg, err := revokeAgentIdentity(g, "gone", strings.ToLower(mine.keyID))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(st.deleted, " "); got != "/user/keys/9 /user/gpg_keys/21 /users/sfi-agent/tokens/30" {
		t.Errorf("deleted %q", got)
	}
	if !strings.Contains(msg, "ssh key id 9") || !strings.Contains(msg, "gpg key "+mine.keyID) || !strings.Contains(msg, "token id 30") {
		t.Errorf("msg %q", msg)
	}
	// no key id known: the gpg key stays, the rest goes
	st.deleted = nil
	st.keys = append(st.keys, giteaKey{ID: 10, Title: "agent-gone"})
	if msg, err := revokeAgentIdentity(g, "gone", ""); err != nil || strings.Join(st.deleted, " ") != "/user/keys/9 /user/keys/10 /users/sfi-agent/tokens/30" || strings.Contains(msg, "gpg") {
		t.Errorf("without a key id: %q %v %v", msg, err, st.deleted)
	}
	st.deleted = nil
	if msg, err := revokeAgentIdentity(g, "never", ""); err != nil || len(st.deleted) != 0 || !strings.Contains(msg, "nothing under agent-never") {
		t.Errorf("absent: %q %v %v", msg, err, st.deleted)
	}
}

// The retired secrets file comes back from the commit that deleted it.
func TestRetiredGPGKeyID(t *testing.T) {
	k, _ := newGPGKey("agent-gone")
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
	os.MkdirAll(filepath.Join(root, "nixos/secrets"), 0o755)
	plain, _ := yaml.Marshal(map[string]string{keyAgentGPG: k.private})
	os.WriteFile(filepath.Join(root, "nixos/secrets/gone.yaml"), plain, 0o600)
	os.WriteFile(filepath.Join(root, "nixos/secrets/cluster.yaml"), []byte("k3s-token: x\n"), 0o600)
	git("add", "-A")
	git("commit", "-q", "-m", "minted")
	git("rm", "-q", "nixos/secrets/gone.yaml", "nixos/secrets/cluster.yaml")
	git("commit", "-q", "-m", "dismantled")
	// destroy reads origin/main; a local branch named so stands in
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	read := func(path string) ([]byte, error) { return os.ReadFile(path) }
	if id, err := retiredGPGKeyID(root, "gone", read); err != nil || id != k.keyID {
		t.Errorf("retired dev fort: %q %v (want %s)", id, err, k.keyID)
	}
	if id, err := retiredGPGKeyID(root, "cluster", read); err != nil || id != "" {
		t.Errorf("retired cluster fort: %q %v", id, err)
	}
	if id, err := retiredGPGKeyID(root, "never", read); err != nil || id != "" {
		t.Errorf("never existed: %q %v", id, err)
	}
	if _, err := retiredGPGKeyID(root, "gone", func(string) ([]byte, error) { return nil, os.ErrPermission }); err == nil || !strings.Contains(err.Error(), "decrypting the retired") {
		t.Errorf("decrypt failure must surface: %v", err)
	}
}
