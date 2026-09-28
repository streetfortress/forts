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

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"gopkg.in/yaml.v3"
)

// The agent identity bundles (#414, #532): what a dev fort ([dev],
// #413) carries so each agent account on it acts on gitea as its own
// user with credentials that are the fort's own — minted per fort,
// sops-seeded beside the host's other secrets, revoked with the fort by
// `fort destroy`. A compromised fort revokes alone.
//
// ONE BUNDLE PER ACCOUNT. The manifest's [dev] table names the accounts
// (user, gitea_user, verb, email, token_scopes) and this file holds no
// name of its own: the mint is the half that leaves this repo later, and
// a CLI with a fort's account names compiled in cannot serve a second
// fleet. Every key of the secrets file is prefixed with the Unix account
// that reads it — the same prefix dev-fort.nix gives the sops secret:
//
//	<user>-ssh-key (+ .pub)    git transport, registered on that account's
//	                           gitea user under the title agent-<host>
//	<user>-gpg-key             commit signing (armored secret key, no
//	                           passphrase), registered AND verified on the
//	                           user so gitea shows the commits Verified.
//	                           gitea keeps only the bare primary-key packet
//	                           (no uid), so destroy finds it by key id,
//	                           derived from the retired secrets file in git
//	                           history
//	<user>-gitea-token         a personal access token named agent-<host>,
//	                           tea's login and the verb's API credential,
//	                           with the account's own token_scopes
//	agent-claude-oauth-token   FLEET secret copied from
//	                           provisioning/secrets.yaml — the one-year
//	                           token `claude setup-token` prints. One per
//	                           fort, not one per account (the name predates
//	                           the accounts): billing is the subscription,
//	                           not the API, and dev-fort.nix gives every
//	                           account the same value through a group
//
// The title is agent-<host> on EVERY account, on purpose: `gotea
// agent-policy` reads the fort out of it (bundleHost, src/agent-policy.ts),
// and the gitea user already says which account it is.
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

// keyAgentClaude is the one key of the bundle that is not per account.
const keyAgentClaude = "agent-claude-oauth-token"

// The per-account keys of nixos/secrets/<host>.yaml. The Unix account is
// the prefix, so a reader of the file sees which account each key is for
// and dev-fort.nix derives the same names.
func keyAccountSSH(user string) string    { return user + "-ssh-key" }
func keyAccountSSHPub(user string) string { return user + "-ssh-key.pub" }
func keyAccountGPG(user string) string    { return user + "-gpg-key" }
func keyAccountToken(user string) string  { return user + "-gitea-token" }

// fleetSecret reads a key of provisioning/secrets.yaml; a variable so
// tests can supply one without an age key for the fleet file.
var fleetSecret = secret

// agentPlan is one account's half of a mint plan: restored where the
// secrets file has it, minted where not, and its standing on gitea.
type agentPlan struct {
	account   manifest.DevAccount
	ssh       sshKey
	gpg       gpgKey
	token     string // "" until apply mints it (shown exactly once by gitea)
	sshStatus string
	gpgStatus string
	tokStatus string

	registerSSH bool // apply must POST the ssh key
	registerGPG bool // apply must POST the gpg key (with the verification signature)
	verifyGPG   bool // registered earlier without a signature: apply verifies it
	mintToken   bool // apply must create the token
}

// planAgents plans one bundle per account the manifest names, plus the
// fort's one Claude credential. A cluster fort has none of it.
func (p *mintPlan) planAgents(take func(string) (string, bool), gitea *giteaDeployKeys) error {
	if !p.entry.IsDev() {
		return nil
	}
	if v, ok := take(keyAgentClaude); ok {
		p.claude = v
	} else {
		v, err := fleetSecret(p.root, keyAgentClaude)
		if err != nil || v == "" {
			return fmt.Errorf("no %s in provisioning/secrets.yaml — the Claude credential is a FLEET secret (billing is the subscription; the token `claude setup-token` prints on a machine logged into the plan, valid a year). Seed it once with:\n  sops --config .sops.yaml set provisioning/secrets.yaml '[\"%s\"]' '\"<token>\"'", keyAgentClaude, keyAgentClaude)
		}
		p.claude = v
	}
	for _, account := range p.entry.Accounts() {
		a, err := p.planAgent(account, take, gitea)
		if err != nil {
			// The first failure of a new account is gitea answering 404
			// for a user nobody created: mint registers a bundle and never
			// invents the user it belongs to.
			hint := ""
			if strings.Contains(err.Error(), "404") {
				hint = fmt.Sprintf(" — does the gitea user %s exist? A human creates it, sets %s on it and adds it to sfi-agentic-dev; then re-run (#532)", account.GiteaUser, account.Email)
			}
			return fmt.Errorf("%s (%s): %w%s", account.User, account.GiteaUser, err, hint)
		}
		p.agents = append(p.agents, a)
	}
	return nil
}

func (p *mintPlan) planAgent(account manifest.DevAccount, take func(string) (string, bool), gitea *giteaDeployKeys) (*agentPlan, error) {
	a := &agentPlan{account: account}
	user := account.User
	title := "agent-" + p.host
	g := gitea.as(account.GiteaUser)
	var err error

	// The ssh key, with the same public-half consistency check as the host key.
	if v, ok := take(keyAccountSSH(user)); ok {
		a.ssh, err = sshKeyFrom(v, title)
	} else {
		a.ssh, err = newSSHKey(title)
	}
	if err != nil {
		return nil, err
	}
	if pub, ok := p.have[keyAccountSSHPub(user)]; ok && strings.TrimSpace(pub) != strings.TrimSpace(a.ssh.public) {
		return nil, fmt.Errorf("%s: %s does not match %s", p.secretsPath, keyAccountSSHPub(user), keyAccountSSH(user))
	} else if !ok {
		p.missing = append(p.missing, keyAccountSSHPub(user))
	}

	if v, ok := take(keyAccountGPG(user)); ok {
		a.gpg, err = gpgKeyFrom(v, keyAccountGPG(user))
	} else {
		a.gpg, err = newGPGKey(account, title)
	}
	if err != nil {
		return nil, err
	}

	// The token: no public half to compare, so the guard is by name
	// alone, and both halves must agree — in the file and on gitea, or in
	// neither. One without the other is a stale token or a hand
	// revocation, and either is resolved deliberately.
	inFile := false
	if v, ok := take(keyAccountToken(user)); ok {
		a.token, inFile = v, true
	}

	// Standing on gitea, read-only: a mismatch refuses before anything is written.
	keys, err := g.keys()
	if err != nil {
		return nil, err
	}
	a.sshStatus, a.registerSSH, err = keyStanding(keys, title, a.ssh.public, "ssh key", p.secretsPath, keyAccountSSH(user))
	if err != nil {
		return nil, err
	}
	gpgs, err := g.gpgKeys()
	if err != nil {
		return nil, err
	}
	a.gpgStatus, a.registerGPG, a.verifyGPG = gpgStanding(gpgs, a.gpg.keyID)
	tokens, err := g.tokens()
	if err != nil {
		return nil, err
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
		return nil, fmt.Errorf("%s holds %s but no token named %s exists on %s — revoked by hand? remove it from the secrets file (sops --config nixos/.sops.yaml unset %s '[\"%s\"]') and re-run to mint a new one", p.secretsPath, keyAccountToken(user), title, account.GiteaUser, p.secretsPath, keyAccountToken(user))
	case !inFile && onGitea:
		return nil, fmt.Errorf("a token named %s exists on %s but %s has no %s — a stale token from an earlier mint; delete it deliberately (fort destroy %s revokes it, or the user's settings) and re-run", title, account.GiteaUser, p.secretsPath, keyAccountToken(user), p.host)
	default:
		a.tokStatus = "would mint (scopes " + strings.Join(account.TokenScopes, ", ") + ")"
		a.mintToken = true
	}
	return a, nil
}

// applyAgents: the tokens first (gitea shows one exactly once, so it must
// be in hand before the secrets file renders), then — after the secrets
// file is written, so a failure there leaves a re-run that restores from
// the file and registers the gap — the keys.
func (p *mintPlan) mintAgentTokens(gitea *giteaDeployKeys) error {
	title := "agent-" + p.host
	for _, a := range p.agents {
		if !a.mintToken {
			continue
		}
		tok, err := gitea.as(a.account.GiteaUser).createToken(title, a.account.TokenScopes)
		if err != nil {
			return err
		}
		a.token, a.mintToken = tok, false
		a.tokStatus = "minted " + title + " (scopes " + strings.Join(a.account.TokenScopes, ", ") + ")"
	}
	return nil
}

func (p *mintPlan) registerAgents(gitea *giteaDeployKeys) error {
	title := "agent-" + p.host
	for _, a := range p.agents {
		g := gitea.as(a.account.GiteaUser)
		if a.registerSSH {
			if err := g.addKey(title, a.ssh.public); err != nil {
				return err
			}
			a.sshStatus, a.registerSSH = "registered "+title, false
		}
		if !a.registerGPG && !a.verifyGPG {
			continue
		}
		token, err := g.gpgToken()
		if err != nil {
			return err
		}
		sig, err := a.gpg.detachSign(token)
		if err != nil {
			return err
		}
		if a.registerGPG {
			if err := g.addGPGKey(a.gpg.public, sig); err != nil {
				return err
			}
			a.gpgStatus = "registered + verified (key id " + a.gpg.keyID + ")"
		} else {
			if err := g.verifyGPGKey(a.gpg.keyID, sig); err != nil {
				return err
			}
			a.gpgStatus = "verified (key id " + a.gpg.keyID + ", was registered unverified)"
		}
		a.registerGPG, a.verifyGPG = false, false
	}
	return nil
}

// agentValue answers for every key of the bundle: the fort's Claude
// credential, and each account's own three.
func (p *mintPlan) agentValue(key string) (string, bool) {
	if key == keyAgentClaude && p.claude != "" {
		return p.claude, true
	}
	for _, a := range p.agents {
		switch user := a.account.User; key {
		case keyAccountSSH(user):
			return a.ssh.private, true
		case keyAccountSSHPub(user):
			return a.ssh.public, true
		case keyAccountGPG(user):
			return a.gpg.private, true
		case keyAccountToken(user):
			return a.token, true
		}
	}
	return "", false
}

func (p *mintPlan) reportAgents(w io.Writer) {
	if len(p.agents) == 0 {
		return
	}
	title := "agent-" + p.host
	for _, a := range p.agents {
		acc := a.account
		fmt.Fprintf(w, "· %s (%s account, %s verb) ssh key %s: %s\n", acc.GiteaUser, acc.User, acc.Verb, title, a.sshStatus)
		fmt.Fprintf(w, "· %s gpg key %s (uid comment %s): %s\n", acc.GiteaUser, a.gpg.keyID, title, a.gpgStatus)
		fmt.Fprintf(w, "· %s token %s: %s\n", acc.GiteaUser, title, a.tokStatus)
	}
	fmt.Fprintf(w, "· claude credential: %s from provisioning/secrets.yaml — fleet-wide, one per fort, outside this fort's revocation unit\n", keyAgentClaude)
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

// newGPGKey mints an ed25519 primary (signing) key with the account's
// gitea user and its email — the address that user holds, so the uid is
// active on registration — and the fort's title as the uid comment. No
// passphrase: the fort's protection is the sops file and the host.
func newGPGKey(account manifest.DevAccount, comment string) (gpgKey, error) {
	e, err := openpgp.NewEntity(account.GiteaUser, comment, account.Email, gpgConfig())
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
	return gpgKeyFrom(priv.String(), "a fresh key")
}

// gpgKeyFrom reads an armored secret key; `what` names it in an error (the
// secrets-file key it came from).
func gpgKeyFrom(armored, what string) (gpgKey, error) {
	el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil {
		return gpgKey{}, fmt.Errorf("%s: %w", what, err)
	}
	if len(el) != 1 || el[0].PrivateKey == nil {
		return gpgKey{}, fmt.Errorf("%s: want exactly one secret key, got %d", what, len(el))
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

// do sends a request as the fleet token; as another user when sudo names
// one (gitea's Sudo header: an admin's token acting as that user).
func (g *giteaDeployKeys) do(method, path string, body any, sudo string) (*http.Response, error) {
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
	if sudo != "" {
		req.Header.Set("Sudo", sudo)
	}
	return g.client.Do(req)
}

// doBasic is the token route's shape: basic auth by method, the fleet
// token as the password, the token's owner (an admin) as the user.
func (g *giteaDeployKeys) doBasic(method, path string, body any) (*http.Response, error) {
	if g.admin == "" {
		resp, err := g.do("GET", "/user", nil, "")
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

// giteaUser is the fleet client bound to one gitea user: every route
// below acts as that user, so a fort with one account per role (#532)
// says which one on every call rather than carrying a default.
type giteaUser struct {
	*giteaDeployKeys
	login string
}

func (g *giteaDeployKeys) as(login string) *giteaUser { return &giteaUser{g, login} }

func (u *giteaUser) keys() ([]giteaKey, error) {
	resp, err := u.do("GET", "/user/keys?limit=100", nil, u.login)
	if err != nil {
		return nil, err
	}
	var keys []giteaKey
	return keys, decode(resp, &keys, "listing "+u.login+"'s ssh keys")
}

func (u *giteaUser) addKey(title, pub string) error {
	resp, err := u.do("POST", "/user/keys", map[string]any{"title": title, "key": strings.TrimSpace(pub), "read_only": false}, u.login)
	if err != nil {
		return err
	}
	return decode(resp, nil, "registering ssh key "+title+" on "+u.login)
}

func (u *giteaUser) deleteKey(id int) error {
	resp, err := u.do("DELETE", fmt.Sprintf("/user/keys/%d", id), nil, u.login)
	if err != nil {
		return err
	}
	return decode(resp, nil, fmt.Sprintf("deleting ssh key id %d on %s", id, u.login))
}

func (u *giteaUser) gpgKeys() ([]giteaGPGKey, error) {
	resp, err := u.do("GET", "/user/gpg_keys?limit=100", nil, u.login)
	if err != nil {
		return nil, err
	}
	var keys []giteaGPGKey
	return keys, decode(resp, &keys, "listing "+u.login+"'s gpg keys")
}

func (u *giteaUser) gpgToken() (string, error) {
	resp, err := u.do("GET", "/user/gpg_key_token", nil, u.login)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("gpg verification token for %s: %s %s", u.login, resp.Status, strings.TrimSpace(string(b)))
	}
	return string(b), nil
}

func (u *giteaUser) addGPGKey(armoredPublic, armoredSignature string) error {
	resp, err := u.do("POST", "/user/gpg_keys", map[string]any{"armored_public_key": armoredPublic, "armored_signature": armoredSignature}, u.login)
	if err != nil {
		return err
	}
	return decode(resp, nil, "registering the gpg key on "+u.login)
}

func (u *giteaUser) verifyGPGKey(keyID, armoredSignature string) error {
	resp, err := u.do("POST", "/user/gpg_key_verify", map[string]any{"key_id": keyID, "armored_signature": armoredSignature}, u.login)
	if err != nil {
		return err
	}
	return decode(resp, nil, "verifying gpg key "+keyID+" on "+u.login)
}

func (u *giteaUser) deleteGPGKey(id int) error {
	resp, err := u.do("DELETE", fmt.Sprintf("/user/gpg_keys/%d", id), nil, u.login)
	if err != nil {
		return err
	}
	return decode(resp, nil, fmt.Sprintf("deleting gpg key id %d on %s", id, u.login))
}

func (u *giteaUser) tokens() ([]giteaToken, error) {
	resp, err := u.doBasic("GET", "/users/"+u.login+"/tokens?limit=100", nil)
	if err != nil {
		return nil, err
	}
	var toks []giteaToken
	return toks, decode(resp, &toks, "listing "+u.login+"'s tokens")
}

// createToken mints the token and returns the value gitea shows once.
// The scopes are the account's own (the manifest's token_scopes): gitea
// scopes are global per token, so the token is the revocation unit and
// the narrow one is what keeps a review session off `git push`.
func (u *giteaUser) createToken(name string, scopes []string) (string, error) {
	resp, err := u.doBasic("POST", "/users/"+u.login+"/tokens", map[string]any{"name": name, "scopes": scopes})
	if err != nil {
		return "", err
	}
	var t giteaToken
	if err := decode(resp, &t, "minting token "+name+" on "+u.login); err != nil {
		return "", err
	}
	if t.Token == "" {
		return "", fmt.Errorf("minting token %s on %s: gitea answered without the value", name, u.login)
	}
	return t.Token, nil
}

func (u *giteaUser) deleteToken(id int) error {
	resp, err := u.doBasic("DELETE", fmt.Sprintf("/users/%s/tokens/%d", u.login, id), nil)
	if err != nil {
		return err
	}
	return decode(resp, nil, fmt.Sprintf("deleting token id %d on %s", id, u.login))
}

// --- destroy's half: on every account's gitea user, everything titled
// agent-<host> goes, and the GPG key with that account's key id ("" =
// none known: the secrets file could not be read back — destroy says
// which). Not found is not an error (a cluster fort never had a bundle).

// retiredAccount is one account of a retired fort: the gitea user to
// revoke on, and the key id of the GPG key it signed with. Both come out
// of git history, because destroy runs after dismantle deleted the
// manifest and the secrets file.
type retiredAccount struct {
	unixUser  string
	giteaUser string
	gpgKeyID  string
}

// revokeAgentIdentities deletes every account's bundle. One message per
// account, joined — a partial revocation must say which half happened,
// so an error carries what was already done.
func revokeAgentIdentities(g *giteaDeployKeys, host string, accounts []retiredAccount) (string, error) {
	title := "agent-" + host
	var lines []string
	for _, account := range accounts {
		msg, err := revokeAgentIdentity(g.as(account.giteaUser), title, account.gpgKeyID)
		if err != nil {
			return strings.Join(lines, "; "), fmt.Errorf("%s: %w", account.giteaUser, err)
		}
		lines = append(lines, msg)
	}
	if len(lines) == 0 {
		return "no account to revoke (not a dev fort, or its retired manifest names none)", nil
	}
	return strings.Join(lines, "; "), nil
}

func revokeAgentIdentity(u *giteaUser, title, gpgKeyID string) (string, error) {
	var done []string
	keys, err := u.keys()
	if err != nil {
		return "", err
	}
	for _, k := range keys {
		if k.Title != title {
			continue
		}
		if err := u.deleteKey(k.ID); err != nil {
			return "", err
		}
		done = append(done, fmt.Sprintf("ssh key id %d", k.ID))
	}
	gpgs, err := u.gpgKeys()
	if err != nil {
		return "", err
	}
	for _, k := range gpgs {
		if gpgKeyID == "" || !strings.EqualFold(k.KeyID, gpgKeyID) {
			continue
		}
		if err := u.deleteGPGKey(k.ID); err != nil {
			return "", err
		}
		done = append(done, "gpg key "+k.KeyID)
	}
	toks, err := u.tokens()
	if err != nil {
		return "", err
	}
	for _, t := range toks {
		if t.Name != title {
			continue
		}
		if err := u.deleteToken(t.ID); err != nil {
			return "", err
		}
		done = append(done, fmt.Sprintf("token id %d", t.ID))
	}
	if len(done) == 0 {
		return "nothing under " + title + " on " + u.login + " (already revoked)", nil
	}
	return "revoked " + title + " on " + u.login + " (" + strings.Join(done, ", ") + ")", nil
}

// retiredAccounts recovers what a retired dev fort's accounts were.
// Destroy runs after dismantle deleted both the manifest and
// nixos/secrets/<host>.yaml, so both come from the commit that deleted
// them: the manifest names the accounts and their gitea users, and the
// secrets file holds each account's GPG key — gitea lists a GPG key as a
// bare primary-key packet (no uid, no title), so the key itself is the
// only handle.
//
// nil when the fort was never a dev fort. An error only when a file that
// should decrypt did not, or when the manifest names accounts whose keys
// cannot be read: destroy then prints the warning and leaves the keys.
func retiredAccounts(root, host string, decrypt func(path string) ([]byte, error)) ([]retiredAccount, error) {
	raw, err := retiredFile(root, manifest.Path(host))
	if err != nil || raw == nil {
		return nil, err
	}
	entry, err := manifest.Parse(host, raw)
	if err != nil {
		return nil, fmt.Errorf("the retired %s: %w", manifest.Path(host), err)
	}
	if !entry.IsDev() {
		return nil, nil
	}
	var out []retiredAccount
	for _, a := range entry.Accounts() {
		out = append(out, retiredAccount{unixUser: a.User, giteaUser: a.GiteaUser})
	}
	rel := filepath.Join("nixos/secrets", host+".yaml")
	secrets, err := retiredFile(root, rel)
	if err != nil {
		return out, err
	}
	if secrets == nil {
		return out, fmt.Errorf("no retired %s in git history — the signing keys stay; find them by uid comment agent-%s on each user and delete them by hand", rel, host)
	}
	dir, err := os.MkdirTemp("", "fort-retired-")
	if err != nil {
		return out, err
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, host+".yaml")
	if err := os.WriteFile(tmp, secrets, 0o600); err != nil {
		return out, err
	}
	plain, err := decrypt(tmp)
	if err != nil {
		return out, fmt.Errorf("decrypting the retired %s from git history: %w", rel, err)
	}
	var have map[string]string
	if err := yaml.Unmarshal(plain, &have); err != nil {
		return out, fmt.Errorf("%s (retired): %w", rel, err)
	}
	for i, account := range out {
		armored, ok := have[keyAccountGPG(account.unixUser)]
		if !ok {
			continue
		}
		k, err := gpgKeyFrom(armored, keyAccountGPG(account.unixUser))
		if err != nil {
			return out, err
		}
		out[i].gpgKeyID = k.keyID
	}
	return out, nil
}

// retiredFile is a deleted file's last content: the commit that removed
// it from origin/main still holds the version before it. nil when the
// path was never deleted there (it never existed, or it is still live).
func retiredFile(root, rel string) ([]byte, error) {
	sha, err := exec.Command("git", "-C", root, "log", "-1", "--format=%H", "--diff-filter=D", "origin/main", "--", rel).Output()
	if err != nil {
		return nil, fmt.Errorf("git log %s: %w", rel, err)
	}
	if strings.TrimSpace(string(sha)) == "" {
		return nil, nil
	}
	raw, err := exec.Command("git", "-C", root, "show", strings.TrimSpace(string(sha))+"^:"+rel).Output()
	if err != nil {
		return nil, fmt.Errorf("git show %s before its deletion: %w", rel, err)
	}
	return raw, nil
}
