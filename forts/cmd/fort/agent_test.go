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
	"regexp"
	"sort"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"gopkg.in/yaml.v3"
)

// fakeGiteaState is the instance as the bundles see it: the repo's deploy
// keys, and PER AGENT USER its ssh keys, gpg keys and tokens (#532 gives
// a fort one user per account). Auth is checked the way gitea does — the
// fleet token on every route, Sudo: <user> on /user/*, basic auth
// (admin:token) on the token routes — a gpg key registers verified only
// when the posted signature really signs the verification string, and the
// list shows it the way gitea does: public_key is the bare primary-key
// packet, base64, no uid (the finding of sfi/gotea#35). So the round trip
// mint performs is the one gitea performs, and destroy cannot cheat.
type fakeGiteaState struct {
	deploy  []giteaKey
	users   map[string]*fakeUser
	deleted []string // "<login> <path>", in call order
}

type fakeUser struct {
	keys   []giteaKey
	gpg    []giteaGPGKey
	tokens []giteaToken
}

func (st *fakeGiteaState) user(login string) *fakeUser {
	if st.users == nil {
		st.users = map[string]*fakeUser{}
	}
	if st.users[login] == nil {
		st.users[login] = &fakeUser{}
	}
	return st.users[login]
}

const fakeGPGToken = "verify-me-1234"

func newFakeGitea(t *testing.T, st *fakeGiteaState) *giteaDeployKeys {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		basicUser, basicPass, basic := r.BasicAuth()
		// The token routes name their user in the path; the key routes
		// name it in the Sudo header.
		login := r.Header.Get("Sudo")
		if rest, ok := strings.CutPrefix(path, "/users/"); ok {
			login = rest[:strings.Index(rest, "/")]
			if !basic || basicUser != "admin" || basicPass != "t" {
				w.WriteHeader(403)
				fmt.Fprint(w, `{"message":"token routes want basic auth"}`)
				return
			}
			path = "/users/" + strings.TrimPrefix(rest[len(login):], "/")
		} else if r.Header.Get("Authorization") != "token t" {
			w.WriteHeader(401)
			return
		}
		if strings.HasPrefix(path, "/user/") && login == "" {
			w.WriteHeader(403)
			fmt.Fprint(w, `{"message":"not sudo"}`)
			return
		}
		u := st.user(login)
		gone := func() {
			st.deleted = append(st.deleted, login+" "+r.URL.Path)
			w.WriteHeader(204)
		}
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
			json.NewEncoder(w).Encode(u.keys)
		case r.Method == "POST" && path == "/user/keys":
			var k giteaKey
			json.NewDecoder(r.Body).Decode(&k)
			k.ID = 100 + len(u.keys)
			u.keys = append(u.keys, k)
			w.WriteHeader(201)
		case r.Method == "DELETE" && strings.HasPrefix(path, "/user/keys/"):
			gone()
		case r.Method == "GET" && path == "/user/gpg_key_token":
			fmt.Fprint(w, fakeGPGToken)
		case r.Method == "GET" && path == "/user/gpg_keys":
			json.NewEncoder(w).Encode(u.gpg)
		case r.Method == "POST" && path == "/user/gpg_keys":
			var raw map[string]string
			json.NewDecoder(r.Body).Decode(&raw)
			pub, sig := raw["armored_public_key"], raw["armored_signature"]
			el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(pub))
			if err != nil || len(el) != 1 {
				w.WriteHeader(422)
				return
			}
			verified := false
			if sig != "" {
				_, err := openpgp.CheckArmoredDetachedSignature(el, strings.NewReader(fakeGPGToken), strings.NewReader(sig), nil)
				verified = err == nil
			}
			u.gpg = append(u.gpg, giteaGPGKey{ID: 200 + len(u.gpg), KeyID: el[0].PrimaryKey.KeyIdString(), PublicKey: barePacket(el[0]), Verified: verified})
			w.WriteHeader(201)
		case r.Method == "POST" && path == "/user/gpg_key_verify":
			var f struct {
				KeyID string `json:"key_id"`
				Sig   string `json:"armored_signature"`
			}
			json.NewDecoder(r.Body).Decode(&f)
			for i, k := range u.gpg {
				if k.KeyID == f.KeyID {
					// the bare packet still verifies a signature
					pk, _ := packet.Read(base64.NewDecoder(base64.StdEncoding, strings.NewReader(k.PublicKey)))
					el := openpgp.EntityList{{PrimaryKey: pk.(*packet.PublicKey)}}
					if _, err := openpgp.CheckArmoredDetachedSignature(el, strings.NewReader(fakeGPGToken), strings.NewReader(f.Sig), nil); err == nil {
						u.gpg[i].Verified = true
					}
				}
			}
			w.WriteHeader(201)
		case r.Method == "DELETE" && strings.HasPrefix(path, "/user/gpg_keys/"):
			gone()
		case r.Method == "GET" && path == "/users/tokens":
			json.NewEncoder(w).Encode(u.tokens)
		case r.Method == "POST" && path == "/users/tokens":
			var tk giteaToken
			json.NewDecoder(r.Body).Decode(&tk)
			tk.ID = 300 + len(u.tokens)
			tk.Token = "minted-" + login + "-" + tk.Name
			u.tokens = append(u.tokens, tk)
			w.WriteHeader(201)
			json.NewEncoder(w).Encode(tk)
		case r.Method == "DELETE" && strings.HasPrefix(path, "/users/tokens/"):
			gone()
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

// testAccount is a [dev] account as the manifest would give it.
func testAccount(user, verb, giteaUser string, scopes ...string) manifest.DevAccount {
	if len(scopes) == 0 {
		scopes = []string{"write:issue"}
	}
	return manifest.DevAccount{User: user, Verb: verb, GiteaUser: giteaUser, Email: user + "@streetfortress.com", TokenScopes: scopes}
}

// devManifest is a dev fort's manifest with the accounts named.
func devManifest(meshIP, lanIP string, accounts ...manifest.DevAccount) string {
	b := &strings.Builder{}
	fmt.Fprintf(b, "mesh_ip = %q\n[dev]\n", meshIP)
	for i, a := range accounts {
		fmt.Fprintf(b, "\n[[dev.accounts]]\nuser = %q\nverb = %q\ngitea_user = %q\nemail = %q\n", a.User, a.Verb, a.GiteaUser, a.Email)
		if i > 0 {
			fmt.Fprintf(b, "ssh_alias = %q\n", "sfi-"+a.User)
		}
		fmt.Fprintf(b, "token_scopes = [%q]\n", a.TokenScopes[0])
	}
	fmt.Fprintf(b, "\n[backups]\nprofile = \"none\"\n[libvirt]\nip = %q\n", lanIP)
	return b.String()
}

func TestGPGKeyRoundTrip(t *testing.T) {
	k, err := newGPGKey(testAccount("agent", "pickup", "sfi-agent"), "agent-x")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.private, "-----BEGIN PGP PRIVATE KEY BLOCK-----") || !strings.HasPrefix(k.public, "-----BEGIN PGP PUBLIC KEY BLOCK-----") || len(k.keyID) != 16 {
		t.Fatalf("shape: keyID=%q", k.keyID)
	}
	again, err := gpgKeyFrom(k.private, "agent-gpg-key")
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
	if _, err := gpgKeyFrom(k.public, "agent-gpg-key"); err == nil {
		t.Error("a public key is not the secret half")
	}
	// The uid is the account's gitea user and the address that user holds
	// — gitea marks a uid active only for an address it knows.
	uid := ""
	for name := range again.entity.Identities {
		uid = name
	}
	if !strings.Contains(uid, "sfi-agent") || !strings.Contains(uid, "agent@streetfortress.com") || !strings.Contains(uid, "agent-x") {
		t.Errorf("uid %q: want the gitea user, the fort title and the account's address", uid)
	}
}

func TestKeyStanding(t *testing.T) {
	pub := "ssh-ed25519 AAAAtest agent-x\n"
	key := keyAccountSSH("agent")
	if s, reg, err := keyStanding(nil, "agent-x", pub, "ssh key", "s.yaml", key); err != nil || !reg || s != "would register" {
		t.Errorf("absent: %q %v %v", s, reg, err)
	}
	keys := []giteaKey{{ID: 1, Title: "agent-x", Key: "ssh-ed25519 AAAAtest"}, {ID: 2, Title: "agent-y", Key: "ssh-ed25519 AAAAnope"}}
	if s, reg, err := keyStanding(keys, "agent-x", pub, "ssh key", "s.yaml", key); err != nil || reg || !strings.Contains(s, "id 1, verified match") {
		t.Errorf("match: %q %v %v", s, reg, err)
	}
	keys = append(keys, giteaKey{ID: 3, Title: "agent-x", Key: "ssh-ed25519 AAAAstale"})
	if _, _, err := keyStanding(keys, "agent-x", pub, "ssh key", "s.yaml", key); err == nil || !strings.Contains(err.Error(), "id 3) differs") {
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

// The keys of the secrets file are the Unix accounts (#532), so nothing
// in the Go code names an account.
func TestAccountKeyNames(t *testing.T) {
	for _, want := range []string{"analyst-ssh-key", "analyst-ssh-key.pub", "analyst-gpg-key", "analyst-gitea-token"} {
		got := map[string]string{
			"analyst-ssh-key":     keyAccountSSH("analyst"),
			"analyst-ssh-key.pub": keyAccountSSHPub("analyst"),
			"analyst-gpg-key":     keyAccountGPG("analyst"),
			"analyst-gitea-token": keyAccountToken("analyst"),
		}[want]
		if got != want {
			t.Errorf("want %s, got %s", want, got)
		}
	}
}

// One bundle per account, each on its own gitea user, each with its own
// scopes — and one Claude credential for the fort (#532).
func TestPlanAgentsPerAccount(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, manifest.Dir), 0o755)
	accounts := []manifest.DevAccount{
		testAccount("agent", "pickup", "sfi-agent", "write:repository", "write:issue"),
		testAccount("analyst", "review", "sfi-analyst", "write:issue"),
	}
	os.WriteFile(filepath.Join(root, manifest.Path("x")), []byte(devManifest("10.1.0.99", "10.0.0.99", accounts...)), 0o644)
	entry, err := manifest.Load(root, "x")
	if err != nil {
		t.Fatal(err)
	}
	fleetSecret = func(root, key string) (string, error) { return "claude", nil }
	t.Cleanup(func() { fleetSecret = secret })

	p := &mintPlan{root: root, host: "x", entry: entry, have: map[string]string{}, secretsPath: "nixos/secrets/x.yaml"}
	// planMint's take: a key the file does not hold is one apply adds.
	take := func(k string) (string, bool) {
		v, ok := p.have[k]
		if !ok {
			p.missing = append(p.missing, k)
		}
		return v, ok
	}
	st := &fakeGiteaState{}
	g := newFakeGitea(t, st)
	if err := p.planAgents(take, g); err != nil {
		t.Fatal(err)
	}
	if len(p.agents) != 2 || p.claude != "claude" {
		t.Fatalf("plans %d, claude %q", len(p.agents), p.claude)
	}
	if !strings.Contains(p.agents[0].tokStatus, "write:repository") || strings.Contains(p.agents[1].tokStatus, "write:repository") {
		t.Errorf("scopes per account: %q / %q", p.agents[0].tokStatus, p.agents[1].tokStatus)
	}
	// Every key of every account, plus the fort's one Claude credential.
	for _, key := range []string{"agent-ssh-key", "agent-ssh-key.pub", "agent-gpg-key", "agent-gitea-token",
		"analyst-ssh-key", "analyst-ssh-key.pub", "analyst-gpg-key", "analyst-gitea-token", keyAgentClaude} {
		if v, ok := p.agentValue(key); !ok && key != "agent-gitea-token" && key != "analyst-gitea-token" {
			t.Errorf("%s: no value (%q)", key, v)
		}
		if !hasKey(p.missing, key) {
			t.Errorf("%s: not in the new file's key list %v", key, p.missing)
		}
	}
	// apply: the tokens, then the keys — on the right user each time.
	if err := p.mintAgentTokens(g); err != nil {
		t.Fatal(err)
	}
	if err := p.registerAgents(g); err != nil {
		t.Fatal(err)
	}
	for _, a := range p.agents {
		login := a.account.GiteaUser
		u := st.user(login)
		if len(u.keys) != 1 || u.keys[0].Title != "agent-x" {
			t.Errorf("%s ssh keys: %+v", login, u.keys)
		}
		if len(u.gpg) != 1 || !u.gpg[0].Verified {
			t.Errorf("%s gpg keys: %+v", login, u.gpg)
		}
		if len(u.tokens) != 1 || u.tokens[0].Name != "agent-x" || a.token != "minted-"+login+"-agent-x" {
			t.Errorf("%s token: %+v (%q)", login, u.tokens, a.token)
		}
		if got := strings.Join(u.tokens[0].Scopes, ","); got != strings.Join(a.account.TokenScopes, ",") {
			t.Errorf("%s scopes on gitea: %q", login, got)
		}
	}
	var report bytes.Buffer
	p.reportAgents(&report)
	for _, want := range []string{"sfi-agent (agent account, pickup verb)", "sfi-analyst (analyst account, review verb)", "agent-claude-oauth-token"} {
		if !strings.Contains(report.String(), want) {
			t.Errorf("report misses %q:\n%s", want, report.String())
		}
	}
}

// The token guard, per account: the file and gitea must agree.
func TestPlanAgentTokenGuards(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, manifest.Dir), 0o755)
	os.WriteFile(filepath.Join(root, manifest.Path("x")),
		[]byte(devManifest("10.1.0.99", "10.0.0.99", testAccount("agent", "pickup", "sfi-agent"))), 0o644)
	entry, err := manifest.Load(root, "x")
	if err != nil {
		t.Fatal(err)
	}
	fleetSecret = func(root, key string) (string, error) { return "claude", nil }
	t.Cleanup(func() { fleetSecret = secret })
	plan := func(have map[string]string, st *fakeGiteaState) error {
		p := &mintPlan{root: root, host: "x", entry: entry, have: have, secretsPath: "nixos/secrets/x.yaml"}
		take := func(k string) (string, bool) { v, ok := have[k]; return v, ok }
		return p.planAgents(take, newFakeGitea(t, st))
	}
	if err := plan(map[string]string{keyAccountToken("agent"): "abc"}, &fakeGiteaState{}); err == nil || !strings.Contains(err.Error(), "revoked by hand") {
		t.Errorf("in file, not on gitea: %v", err)
	}
	onGitea := &fakeGiteaState{}
	onGitea.user("sfi-agent").tokens = []giteaToken{{ID: 1, Name: "agent-x"}}
	if err := plan(map[string]string{}, onGitea); err == nil || !strings.Contains(err.Error(), "stale token") {
		t.Errorf("on gitea, not in file: %v", err)
	}
	fleetSecret = func(root, key string) (string, error) { return "", os.ErrNotExist }
	if err := plan(map[string]string{}, &fakeGiteaState{}); err == nil || !strings.Contains(err.Error(), "claude setup-token") {
		t.Errorf("no fleet claude token: %v", err)
	}
}

// destroy: every account's bundle goes, on its own user, and a foreign
// title or key id stays.
func TestRevokeAgentIdentities(t *testing.T) {
	mine, _ := newGPGKey(testAccount("agent", "pickup", "sfi-agent"), "agent-gone")
	theirs, _ := newGPGKey(testAccount("analyst", "review", "sfi-analyst"), "agent-gone")
	other, _ := newGPGKey(testAccount("agent", "pickup", "sfi-agent"), "agent-other")
	st := &fakeGiteaState{}
	st.users = map[string]*fakeUser{
		"sfi-agent": {
			keys:   []giteaKey{{ID: 7, Title: "agent-other"}, {ID: 9, Title: "agent-gone"}},
			gpg:    []giteaGPGKey{{ID: 20, KeyID: other.keyID, PublicKey: barePacket(other.entity)}, {ID: 21, KeyID: mine.keyID, PublicKey: barePacket(mine.entity)}},
			tokens: []giteaToken{{ID: 30, Name: "agent-gone"}, {ID: 31, Name: "agent-other"}},
		},
		"sfi-analyst": {
			keys:   []giteaKey{{ID: 8, Title: "agent-gone"}},
			gpg:    []giteaGPGKey{{ID: 22, KeyID: theirs.keyID, PublicKey: barePacket(theirs.entity)}},
			tokens: []giteaToken{{ID: 32, Name: "agent-gone"}},
		},
	}
	g := newFakeGitea(t, st)
	accounts := []retiredAccount{
		{unixUser: "agent", giteaUser: "sfi-agent", gpgKeyID: strings.ToLower(mine.keyID)},
		{unixUser: "analyst", giteaUser: "sfi-analyst", gpgKeyID: theirs.keyID},
	}
	msg, err := revokeAgentIdentities(g, "gone", accounts)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"sfi-agent /user/keys/9", "sfi-agent /user/gpg_keys/21", "sfi-agent /users/sfi-agent/tokens/30",
		"sfi-analyst /user/keys/8", "sfi-analyst /user/gpg_keys/22", "sfi-analyst /users/sfi-analyst/tokens/32",
	}
	got := append([]string{}, st.deleted...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("deleted %q\nwant    %q", got, want)
	}
	for _, s := range []string{"revoked agent-gone on sfi-agent", "revoked agent-gone on sfi-analyst", "ssh key id 8", "token id 32"} {
		if !strings.Contains(msg, s) {
			t.Errorf("msg %q misses %q", msg, s)
		}
	}
	// no key id known: the gpg key stays, the rest goes
	st.deleted = nil
	st.user("sfi-agent").keys = append(st.user("sfi-agent").keys, giteaKey{ID: 10, Title: "agent-gone"})
	msg, err = revokeAgentIdentities(g, "gone", []retiredAccount{{unixUser: "agent", giteaUser: "sfi-agent"}})
	if err != nil || strings.Contains(msg, "gpg") {
		t.Errorf("without a key id: %q %v", msg, err)
	}
	if strings.Join(st.deleted, " ") != "sfi-agent /user/keys/9 sfi-agent /user/keys/10 sfi-agent /users/sfi-agent/tokens/30" {
		t.Errorf("without a key id, deleted %q", st.deleted)
	}
	st.deleted = nil
	if msg, err := revokeAgentIdentities(g, "never", []retiredAccount{{unixUser: "agent", giteaUser: "sfi-agent"}}); err != nil || len(st.deleted) != 0 || !strings.Contains(msg, "nothing under agent-never") {
		t.Errorf("absent: %q %v %v", msg, err, st.deleted)
	}
	if msg, err := revokeAgentIdentities(g, "gone", nil); err != nil || !strings.Contains(msg, "no account to revoke") {
		t.Errorf("no accounts: %q %v", msg, err)
	}
}

// The retired fort's accounts come back from the commit that deleted its
// manifest, and their key ids from the commit that deleted its secrets.
func TestRetiredAccounts(t *testing.T) {
	agent, _ := newGPGKey(testAccount("agent", "pickup", "sfi-agent"), "agent-gone")
	analyst, _ := newGPGKey(testAccount("analyst", "review", "sfi-analyst"), "agent-gone")
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
	write(manifest.Path("gone"), devManifest("10.1.0.96", "10.0.0.96",
		testAccount("agent", "pickup", "sfi-agent"), testAccount("analyst", "review", "sfi-analyst")))
	write(manifest.Path("cluster"), "mesh_ip = \"10.1.0.97\"\n[cluster]\n[backups]\nprofile = \"none\"\n[libvirt]\nip = \"10.0.0.97\"\n")
	plain, _ := yaml.Marshal(map[string]string{
		keyAccountGPG("agent"):   agent.private,
		keyAccountGPG("analyst"): analyst.private,
	})
	write("nixos/secrets/gone.yaml", string(plain))
	write("nixos/secrets/cluster.yaml", "k3s-token: x\n")
	git("add", "-A")
	git("commit", "-q", "-m", "minted")
	git("rm", "-q", manifest.Path("gone"), manifest.Path("cluster"), "nixos/secrets/gone.yaml", "nixos/secrets/cluster.yaml")
	git("commit", "-q", "-m", "dismantled")
	// destroy reads origin/main; a local branch named so stands in
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	read := func(path string) ([]byte, error) { return os.ReadFile(path) }
	retired := func(host string) *retiredSecrets {
		s, err := readRetiredSecrets(root, host)
		if err != nil {
			t.Fatalf("retired secrets of %s: %v", host, err)
		}
		return s
	}

	got, err := retiredAccounts(root, "gone", retired("gone"), read)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (retiredAccount{"agent", "sfi-agent", agent.keyID}) || got[1] != (retiredAccount{"analyst", "sfi-analyst", analyst.keyID}) {
		t.Errorf("retired dev fort: %+v", got)
	}
	if got, err := retiredAccounts(root, "cluster", retired("cluster"), read); err != nil || got != nil {
		t.Errorf("retired cluster fort: %+v %v", got, err)
	}
	if s := retired("never"); s != nil {
		t.Errorf("a fort that never existed has no retired secrets: %+v", s)
	}
	if got, err := retiredAccounts(root, "never", nil, read); err != nil || got != nil {
		t.Errorf("never existed: %+v %v", got, err)
	}
	// A secrets file that should decrypt and does not: the accounts still
	// come back (the ssh keys and tokens are revocable by title), and the
	// error says the signing keys stay.
	got, err = retiredAccounts(root, "gone", retired("gone"), func(string) ([]byte, error) { return nil, os.ErrPermission })
	if err == nil || !strings.Contains(err.Error(), "decrypting the retired") {
		t.Errorf("decrypt failure must surface: %v", err)
	}
	if len(got) != 2 || got[0].gpgKeyID != "" {
		t.Errorf("accounts without their key ids: %+v", got)
	}
	// A dev fort whose secrets file is not in history at all: the same,
	// with the error naming the file that is missing.
	got, err = retiredAccounts(root, "gone", nil, read)
	if err == nil || !strings.Contains(err.Error(), "no retired nixos/secrets/gone.yaml") {
		t.Errorf("a missing secrets file must surface: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("accounts still come back: %+v", got)
	}
}

// The retired secrets file answers "did this fort ever hold a B2 key"
// from its plaintext key names, so destroy asks for the minting key only
// when there is something to revoke (#432: a profile "none" fort asked
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
	// The fleet's pruner (#400): profile "none" — no restic-s3-env-b2 —
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

// The nix half of the accounts (#532) must name exactly what the
// manifests name. Nix cannot read provisioning/forts — it is outside the
// flake's root (nixos/), and a flake sees nothing above it — so the list
// is written twice: `accounts` in nixos/modules/dev-fort.nix (the Unix
// accounts, their homes, their verbs, their sops secrets) and [dev] in
// each dev fort's manifest (the bundles `fort mint` registers). This is
// what keeps the two equal, field by field. A mismatch is a fort whose
// git identity, GPG uid or token belongs to another role.
func TestDevAccountsMatchDevFortModule(t *testing.T) {
	repo, err := repoRootFrom(mustGetwd(t))
	if err != nil {
		t.Skip("not inside the deployments checkout")
	}
	const module = "nixos/modules/dev-fort.nix"
	raw, err := os.ReadFile(filepath.Join(repo, module))
	if err != nil {
		t.Fatal(err)
	}
	// The module writes one account per line, fields in this order.
	re := regexp.MustCompile(`\{ user = "([a-z0-9-]+)"; verb = "([a-z]+)"; giteaUser = "([A-Za-z0-9-]+)"; email = "([^"]+)"; \}`)
	var inNix []string
	for _, m := range re.FindAllStringSubmatch(string(raw), -1) {
		inNix = append(inNix, strings.Join(m[1:], " "))
	}
	if len(inNix) == 0 {
		t.Fatalf("%s: no account matched %s — keep each account on one line, fields in that order", module, re)
	}
	hosts, problems, err := manifest.LoadAll(repo)
	if err != nil || len(problems) > 0 {
		t.Fatalf("manifests: %v %v", err, problems)
	}
	for host, entry := range hosts {
		if !entry.IsDev() {
			continue
		}
		var inManifest []string
		for _, a := range entry.Accounts() {
			inManifest = append(inManifest, strings.Join([]string{a.User, a.Verb, a.GiteaUser, a.Email}, " "))
		}
		if strings.Join(inManifest, ", ") != strings.Join(inNix, ", ") {
			t.Errorf("%s names\n  %s\nand %s names\n  %s\n— the two lists are one contract (user, verb, gitea user, email)",
				manifest.Path(host), strings.Join(inManifest, "\n  "), module, strings.Join(inNix, "\n  "))
		}
	}
}
