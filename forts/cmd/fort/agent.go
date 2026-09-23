package main

import (
	"bytes"
	"crypto"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"gopkg.in/yaml.v3"
)

// The agent identity bundle (#414): what a dev fort ([dev], #413)
// carries so the coding agent on it acts on gitea as `sfi-agent` with
// credentials that are the fort's own — minted per fort, sops-seeded
// beside the host's other secrets, revoked with the fort by `fort
// destroy`. A compromised fort revokes alone.
//
//	agent-ssh-key (+ .pub)     git transport, registered on the user under
//	                           the title agent-<host>
//	agent-gpg-key              commit signing (armored secret key, no
//	                           passphrase), registered AND verified on the
//	                           user so gitea shows the commits Verified.
//	                           gitea keeps only the bare primary-key packet
//	                           (no uid), so destroy finds it by key id,
//	                           derived from the retired secrets file in git
//	                           history
//	agent-gitea-token          a personal access token named agent-<host>,
//	                           tea's login and pickup's API credential
//	agent-claude-oauth-token   FLEET secret copied from
//	                           provisioning/secrets.yaml — the one-year
//	                           token `claude setup-token` prints; billing
//	                           is the subscription, not the API
//
// One credential does all of it: the fleet gitea-token mint already
// holds for the deploy key. gitea lets an admin's token act as another
// user through the Sudo header (the key routes live under /user), and
// the token route, which insists on basic auth, accepts a token as the
// basic-auth password. So nothing is prompted and no password is stored;
// the token needs write:user for the writes.
//
// GPG rather than SSH signing, though the SSH key could sign too: gitea
// marks an SSH-signed commit Verified only for a key verified through
// the web form, which the API cannot do. A GPG key is verified over the
// API by signing the string GET /user/gpg_key_token returns.

const (
	keyAgentSSH    = "agent-ssh-key"
	keyAgentSSHPub = "agent-ssh-key.pub"
	keyAgentGPG    = "agent-gpg-key"
	keyAgentToken  = "agent-gitea-token"
	keyAgentClaude = "agent-claude-oauth-token"

	agentUser  = "sfi-agent"
	agentEmail = "support@streetfortress.com"
)

// agentTokenScopes is one list, fleet-wide: gitea scopes are global per
// token, so the token is the revocation unit and the sfi-agentic-dev
// team is the permission unit. No write:user — keys are registered by
// mint, not by the agent.
var agentTokenScopes = []string{"write:repository", "write:issue", "read:organization", "read:user", "write:misc"}

// fleetSecret reads a key of provisioning/secrets.yaml; a variable so
// tests can supply one without an age key for the fleet file.
var fleetSecret = secret

// agentPlan is the bundle's half of a mint plan: restored where the
// secrets file has it, minted where not, and its standing on gitea.
type agentPlan struct {
	ssh       sshKey
	gpg       gpgKey
	token     string // "" until apply mints it (shown exactly once by gitea)
	claude    string
	sshStatus string
	gpgStatus string
	tokStatus string

	registerSSH bool // apply must POST the ssh key
	registerGPG bool // apply must POST the gpg key (with the verification signature)
	verifyGPG   bool // registered earlier without a signature: apply verifies it
	mintToken   bool // apply must create the token
}

func (p *mintPlan) planAgent(take func(string) (string, bool), gitea *giteaDeployKeys) error {
	if !p.entry.IsDev() {
		return nil
	}
	a := &agentPlan{}
	title := "agent-" + p.host
	var err error

	// The ssh key, with the same public-half consistency check as the host key.
	if v, ok := take(keyAgentSSH); ok {
		a.ssh, err = sshKeyFrom(v, title)
	} else {
		a.ssh, err = newSSHKey(title)
	}
	if err != nil {
		return err
	}
	if pub, ok := p.have[keyAgentSSHPub]; ok && strings.TrimSpace(pub) != strings.TrimSpace(a.ssh.public) {
		return fmt.Errorf("%s: %s does not match %s", p.secretsPath, keyAgentSSHPub, keyAgentSSH)
	} else if !ok {
		p.missing = append(p.missing, keyAgentSSHPub)
	}

	if v, ok := take(keyAgentGPG); ok {
		a.gpg, err = gpgKeyFrom(v)
	} else {
		a.gpg, err = newGPGKey(title)
	}
	if err != nil {
		return err
	}

	// The token: no public half to compare, so the guard is by name
	// alone, and both halves must agree — in the file and on gitea, or in
	// neither. One without the other is a stale token or a hand
	// revocation, and either is resolved deliberately.
	inFile := false
	if v, ok := take(keyAgentToken); ok {
		a.token, inFile = v, true
	}
	if v, ok := take(keyAgentClaude); ok {
		a.claude = v
	} else {
		v, err := fleetSecret(p.root, keyAgentClaude)
		if err != nil || v == "" {
			return fmt.Errorf("no %s in provisioning/secrets.yaml — the Claude credential is a FLEET secret (billing is the subscription; the token `claude setup-token` prints on a machine logged into the plan, valid a year). Seed it once with:\n  sops --config .sops.yaml set provisioning/secrets.yaml '[\"%s\"]' '\"<token>\"'", keyAgentClaude, keyAgentClaude)
		}
		a.claude = v
	}

	// Standing on gitea, read-only: a mismatch refuses before anything is written.
	keys, err := gitea.userKeys()
	if err != nil {
		return err
	}
	a.sshStatus, a.registerSSH, err = keyStanding(keys, title, a.ssh.public, "ssh key", p.secretsPath, keyAgentSSH)
	if err != nil {
		return err
	}
	gpgs, err := gitea.userGPGKeys()
	if err != nil {
		return err
	}
	a.gpgStatus, a.registerGPG, a.verifyGPG = gpgStanding(gpgs, a.gpg.keyID)
	tokens, err := gitea.agentTokens()
	if err != nil {
		return err
	}
	onGitea := false
	for _, t := range tokens {
		if t.Name == title {
			onGitea = true
		}
	}
	switch {
	case inFile && onGitea:
		a.tokStatus = "registered (name-only match: gitea never shows a token twice)"
	case inFile && !onGitea:
		return fmt.Errorf("%s holds %s but no token named %s exists on %s — revoked by hand? remove it from the secrets file (sops --config nixos/.sops.yaml unset %s '[\"%s\"]') and re-run to mint a new one", p.secretsPath, keyAgentToken, title, agentUser, p.secretsPath, keyAgentToken)
	case !inFile && onGitea:
		return fmt.Errorf("a token named %s exists on %s but %s has no %s — a stale token from an earlier mint; delete it deliberately (fort destroy %s revokes it, or the user's settings) and re-run", title, agentUser, p.secretsPath, keyAgentToken, p.host)
	default:
		a.tokStatus = "would mint (scopes " + strings.Join(agentTokenScopes, ", ") + ")"
		a.mintToken = true
	}
	p.agent = a
	return nil
}

// applyAgent: the token first (gitea shows it exactly once, so it must
// be in hand before the secrets file renders), then — after the secrets
// file is written, so a failure here leaves a re-run that restores from
// the file and registers the gap — the keys.
func (p *mintPlan) mintAgentToken(gitea *giteaDeployKeys) error {
	tok, err := gitea.createAgentToken("agent-" + p.host)
	if err != nil {
		return err
	}
	p.agent.token, p.agent.mintToken = tok, false
	p.agent.tokStatus = "minted agent-" + p.host + " (scopes " + strings.Join(agentTokenScopes, ", ") + ")"
	return nil
}

func (p *mintPlan) registerAgent(gitea *giteaDeployKeys) error {
	a := p.agent
	if a.registerSSH {
		if err := gitea.addUserKey("agent-"+p.host, a.ssh.public); err != nil {
			return err
		}
		a.sshStatus, a.registerSSH = "registered agent-"+p.host, false
	}
	if a.registerGPG || a.verifyGPG {
		token, err := gitea.gpgToken()
		if err != nil {
			return err
		}
		sig, err := a.gpg.detachSign(token)
		if err != nil {
			return err
		}
		if a.registerGPG {
			if err := gitea.addGPGKey(a.gpg.public, sig); err != nil {
				return err
			}
			a.gpgStatus = "registered + verified (key id " + a.gpg.keyID + ")"
		} else {
			if err := gitea.verifyGPGKey(a.gpg.keyID, sig); err != nil {
				return err
			}
			a.gpgStatus = "verified (key id " + a.gpg.keyID + ", was registered unverified)"
		}
		a.registerGPG, a.verifyGPG = false, false
	}
	return nil
}

func (p *mintPlan) agentValue(key string) (string, bool) {
	if p.agent == nil {
		return "", false
	}
	switch key {
	case keyAgentSSH:
		return p.agent.ssh.private, true
	case keyAgentSSHPub:
		return p.agent.ssh.public, true
	case keyAgentGPG:
		return p.agent.gpg.private, true
	case keyAgentToken:
		return p.agent.token, true
	case keyAgentClaude:
		return p.agent.claude, true
	}
	return "", false
}

func (p *mintPlan) reportAgent(w io.Writer) {
	a := p.agent
	if a == nil {
		return
	}
	fmt.Fprintf(w, "· %s ssh key agent-%s: %s\n", agentUser, p.host, a.sshStatus)
	fmt.Fprintf(w, "· %s gpg key %s (uid comment agent-%s): %s\n", agentUser, a.gpg.keyID, p.host, a.gpgStatus)
	fmt.Fprintf(w, "· %s token agent-%s: %s\n", agentUser, p.host, a.tokStatus)
	fmt.Fprintf(w, "· claude credential: %s from provisioning/secrets.yaml — fleet-wide, outside this fort's revocation unit\n", keyAgentClaude)
}

// keyStanding is deployKeyStatus's rule for any titled key list: every
// key under the title must match the local public half (a timed-out
// POST that gitea recorded anyway leaves duplicates; a stale one among
// them is a foreign key with our name on it), absent means register.
func keyStanding(keys []giteaKey, title, pub, what, secretsPath, secretKey string) (status string, register bool, err error) {
	local := strings.Join(strings.Fields(pub)[:2], " ")
	var ids, stale []string
	for _, k := range keys {
		if k.Title != title {
			continue
		}
		ids = append(ids, fmt.Sprint(k.ID))
		if !strings.HasPrefix(k.Key, local) {
			stale = append(stale, fmt.Sprint(k.ID))
		}
	}
	switch {
	case len(stale) > 0:
		return "", false, fmt.Errorf("%s %s on gitea (id %s) differs from the one in %s (%s) — delete the stale key deliberately, then re-run", what, title, strings.Join(stale, ", "), secretsPath, secretKey)
	case len(ids) > 1:
		return fmt.Sprintf("registered %d× (ids %s — duplicates, all verified match; fort destroy revokes every one)", len(ids), strings.Join(ids, ", ")), false, nil
	case len(ids) == 1:
		return fmt.Sprintf("registered (id %s, verified match)", ids[0]), false, nil
	}
	return "would register", true, nil
}

func gpgStanding(keys []giteaGPGKey, keyID string) (status string, register, verify bool) {
	for _, k := range keys {
		if !strings.EqualFold(k.KeyID, keyID) {
			continue
		}
		if k.Verified {
			return fmt.Sprintf("registered (id %d, verified)", k.ID), false, false
		}
		return fmt.Sprintf("registered (id %d) but not verified — would verify", k.ID), false, true
	}
	return "would register + verify", true, false
}

// --- the GPG key (go-crypto; nothing shells out to gnupg here)

type gpgKey struct {
	private, public string // armored
	keyID           string // long key id, upper-case hex: what gitea lists and git's user.signingkey can name
	entity          *openpgp.Entity
}

func gpgConfig() *packet.Config {
	return &packet.Config{Algorithm: packet.PubKeyAlgoEdDSA, DefaultHash: crypto.SHA256}
}

// newGPGKey mints an ed25519 primary (signing) key with the agent's
// name and email — the user's activated address, so the uid is active
// on registration — and the fort's title as the uid comment. No
// passphrase: the fort's protection is the sops file and the host.
func newGPGKey(comment string) (gpgKey, error) {
	e, err := openpgp.NewEntity(agentUser, comment, agentEmail, gpgConfig())
	if err != nil {
		return gpgKey{}, err
	}
	var priv bytes.Buffer
	w, err := armor.Encode(&priv, openpgp.PrivateKeyType, nil)
	if err != nil {
		return gpgKey{}, err
	}
	if err := e.SerializePrivate(w, nil); err != nil {
		return gpgKey{}, err
	}
	w.Close()
	return gpgKeyFrom(priv.String())
}

func gpgKeyFrom(armored string) (gpgKey, error) {
	el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil {
		return gpgKey{}, fmt.Errorf("%s: %w", keyAgentGPG, err)
	}
	if len(el) != 1 || el[0].PrivateKey == nil {
		return gpgKey{}, fmt.Errorf("%s: want exactly one secret key, got %d", keyAgentGPG, len(el))
	}
	e := el[0]
	var pub bytes.Buffer
	w, err := armor.Encode(&pub, openpgp.PublicKeyType, nil)
	if err != nil {
		return gpgKey{}, err
	}
	if err := e.Serialize(w); err != nil {
		return gpgKey{}, err
	}
	w.Close()
	return gpgKey{private: strings.TrimSpace(armored) + "\n", public: pub.String(), keyID: e.PrimaryKey.KeyIdString(), entity: e}, nil
}

// detachSign signs gitea's verification string the way gitea checks it
// (an armored detached signature over the exact bytes).
func (k gpgKey) detachSign(message string) (string, error) {
	var sig bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&sig, k.entity, strings.NewReader(message), gpgConfig()); err != nil {
		return "", err
	}
	return sig.String(), nil
}

// --- the user half of the gitea client (giteaDeployKeys carries the
// token, the CA and the base URL; the deploy-key routes are mint.go's)

type giteaGPGKey struct {
	ID        int    `json:"id"`
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	Verified  bool   `json:"verified"`
}

type giteaToken struct {
	ID     int      `json:"id"`
	Name   string   `json:"name"`
	Token  string   `json:"sha1"` // the value, on create only
	Scopes []string `json:"scopes"`
}

// do sends a request as the fleet token; as the agent when sudo is set
// (gitea's Sudo header: an admin's token acting as another user).
func (g *giteaDeployKeys) do(method, path string, body any, sudo bool) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, g.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "token "+g.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sudo {
		req.Header.Set("Sudo", agentUser)
	}
	return g.client.Do(req)
}

// doBasic is the token route's shape: basic auth by method, the fleet
// token as the password, the token's owner (an admin) as the user.
func (g *giteaDeployKeys) doBasic(method, path string, body any) (*http.Response, error) {
	if g.admin == "" {
		resp, err := g.do("GET", "/user", nil, false)
		if err != nil {
			return nil, err
		}
		var u struct {
			Login string `json:"login"`
		}
		err = decode(resp, &u, "reading the fleet token's user")
		if err != nil {
			return nil, err
		}
		g.admin = u.Login
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, g.base+path, rd)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(g.admin, g.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return g.client.Do(req)
}

// decode reads a JSON body or turns a non-2xx into an error carrying
// gitea's message — the scope errors name the missing scope, which is
// the whole diagnosis when the fleet token predates write:user.
func decode(resp *http.Response, into any, what string) error {
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		var msg struct {
			Message string `json:"message"`
		}
		json.NewDecoder(resp.Body).Decode(&msg)
		hint := ""
		if strings.Contains(msg.Message, "write:user") {
			hint = " — the fleet gitea-token in provisioning/secrets.yaml needs write:user (re-issue it with that scope added; #414)"
		}
		return fmt.Errorf("%s: %s %s%s", what, resp.Status, msg.Message, hint)
	}
	if into == nil {
		io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

func (g *giteaDeployKeys) userKeys() ([]giteaKey, error) {
	resp, err := g.do("GET", "/user/keys?limit=100", nil, true)
	if err != nil {
		return nil, err
	}
	var keys []giteaKey
	return keys, decode(resp, &keys, "listing "+agentUser+"'s ssh keys")
}

func (g *giteaDeployKeys) addUserKey(title, pub string) error {
	resp, err := g.do("POST", "/user/keys", map[string]any{"title": title, "key": strings.TrimSpace(pub), "read_only": false}, true)
	if err != nil {
		return err
	}
	return decode(resp, nil, "registering ssh key "+title+" on "+agentUser)
}

func (g *giteaDeployKeys) userGPGKeys() ([]giteaGPGKey, error) {
	resp, err := g.do("GET", "/user/gpg_keys?limit=100", nil, true)
	if err != nil {
		return nil, err
	}
	var keys []giteaGPGKey
	return keys, decode(resp, &keys, "listing "+agentUser+"'s gpg keys")
}

func (g *giteaDeployKeys) gpgToken() (string, error) {
	resp, err := g.do("GET", "/user/gpg_key_token", nil, true)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("gpg verification token: %s %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

func (g *giteaDeployKeys) addGPGKey(armoredPublic, armoredSignature string) error {
	resp, err := g.do("POST", "/user/gpg_keys", map[string]any{"armored_public_key": armoredPublic, "armored_signature": armoredSignature}, true)
	if err != nil {
		return err
	}
	return decode(resp, nil, "registering the gpg key on "+agentUser)
}

func (g *giteaDeployKeys) verifyGPGKey(keyID, armoredSignature string) error {
	resp, err := g.do("POST", "/user/gpg_key_verify", map[string]any{"key_id": keyID, "armored_signature": armoredSignature}, true)
	if err != nil {
		return err
	}
	return decode(resp, nil, "verifying gpg key "+keyID+" on "+agentUser)
}

func (g *giteaDeployKeys) agentTokens() ([]giteaToken, error) {
	resp, err := g.doBasic("GET", "/users/"+agentUser+"/tokens?limit=100", nil)
	if err != nil {
		return nil, err
	}
	var toks []giteaToken
	return toks, decode(resp, &toks, "listing "+agentUser+"'s tokens")
}

func (g *giteaDeployKeys) createAgentToken(name string) (string, error) {
	resp, err := g.doBasic("POST", "/users/"+agentUser+"/tokens", map[string]any{"name": name, "scopes": agentTokenScopes})
	if err != nil {
		return "", err
	}
	var t giteaToken
	if err := decode(resp, &t, "minting token "+name+" on "+agentUser); err != nil {
		return "", err
	}
	if t.Token == "" {
		return "", fmt.Errorf("minting token %s: gitea answered without the value", name)
	}
	return t.Token, nil
}

// --- destroy's half: everything titled agent-<host> on the user goes,
// and the GPG key with the given key id ("" = none known: not a dev
// fort, or its secrets file could not be read back — destroy says
// which). Not found is not an error (a cluster fort never had a bundle).

func revokeAgentIdentity(g *giteaDeployKeys, host, gpgKeyID string) (string, error) {
	title := "agent-" + host
	var done []string
	keys, err := g.userKeys()
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if k.Title != title {
			continue
		}
		resp, err := g.do("DELETE", fmt.Sprintf("/user/keys/%d", k.ID), nil, true)
		if err != nil {
			return "", err
		}
		if err := decode(resp, nil, fmt.Sprintf("deleting ssh key %s (id %d)", title, k.ID)); err != nil {
			return "", err
		}
		done = append(done, fmt.Sprintf("ssh key id %d", k.ID))
	}
	gpgs, err := g.userGPGKeys()
	if err != nil {
		return "", err
	}
	for _, k := range gpgs {
		if gpgKeyID == "" || !strings.EqualFold(k.KeyID, gpgKeyID) {
			continue
		}
		resp, err := g.do("DELETE", fmt.Sprintf("/user/gpg_keys/%d", k.ID), nil, true)
		if err != nil {
			return "", err
		}
		if err := decode(resp, nil, fmt.Sprintf("deleting gpg key %s (id %d)", k.KeyID, k.ID)); err != nil {
			return "", err
		}
		done = append(done, "gpg key "+k.KeyID)
	}
	toks, err := g.agentTokens()
	if err != nil {
		return "", err
	}
	for _, t := range toks {
		if t.Name != title {
			continue
		}
		resp, err := g.doBasic("DELETE", fmt.Sprintf("/users/%s/tokens/%d", agentUser, t.ID), nil)
		if err != nil {
			return "", err
		}
		if err := decode(resp, nil, fmt.Sprintf("deleting token %s (id %d)", title, t.ID)); err != nil {
			return "", err
		}
		done = append(done, fmt.Sprintf("token id %d", t.ID))
	}
	if len(done) == 0 {
		return "nothing under " + title + " on " + agentUser + " (not a dev fort, or already revoked)", nil
	}
	return "revoked " + title + " on " + agentUser + " (" + strings.Join(done, ", ") + ")", nil
}

// retiredGPGKeyID recovers the key id of a retired fort's signing key.
// Destroy runs after dismantle deleted nixos/secrets/<host>.yaml, and
// gitea's list shows a GPG key as a bare primary-key packet — no uid, no
// title — so the only handle is the key itself: the commit that deleted
// the file still holds it, encrypted to the admin key, and the id
// derives from it. "" when the file never existed, or held no
// agent-gpg-key (a cluster fort); an error only when a file that should
// decrypt did not.
func retiredGPGKeyID(root, host string, decrypt func(path string) ([]byte, error)) (string, error) {
	rel := filepath.Join("nixos/secrets", host+".yaml")
	sha, err := exec.Command("git", "-C", root, "log", "-1", "--format=%H", "--diff-filter=D", "origin/main", "--", rel).Output()
	if err != nil {
		return "", fmt.Errorf("git log %s: %w", rel, err)
	}
	if strings.TrimSpace(string(sha)) == "" {
		return "", nil
	}
	raw, err := exec.Command("git", "-C", root, "show", strings.TrimSpace(string(sha))+"^:"+rel).Output()
	if err != nil {
		return "", fmt.Errorf("git show %s before its deletion: %w", rel, err)
	}
	dir, err := os.MkdirTemp("", "fort-retired-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, host+".yaml")
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return "", err
	}
	plain, err := decrypt(tmp)
	if err != nil {
		return "", fmt.Errorf("decrypting the retired %s from git history: %w", rel, err)
	}
	var have map[string]string
	if err := yaml.Unmarshal(plain, &have); err != nil {
		return "", fmt.Errorf("%s (retired): %w", rel, err)
	}
	armored, ok := have[keyAgentGPG]
	if !ok {
		return "", nil
	}
	k, err := gpgKeyFrom(armored)
	if err != nil {
		return "", err
	}
	return k.keyID, nil
}
