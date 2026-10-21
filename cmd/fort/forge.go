package main

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.zen.lofi/sfi/forts/internal/estate"
)

// The forge (forts#1): the host of estate.toml's `[source] repo`, and
// the one call fort makes outside the tree — the deploy key flux and
// the rebuild loop clone that repo with, registered by fort mint and
// revoked by fort destroy. Both verbs reach it through this interface
// and know nothing of the API behind it.
//
// The kind derives from the repo (estate.ForgeKind): github.com is
// github, every other host is gitea, and `[source] forge` overrides.
// Two kinds, one shape: gitea's deploy-key routes were modelled on
// GitHub's, so the two clients differ in the API root and the auth
// header and nothing else — restForge below is both, built by newForge.
//
// The token is the forge's, not the estate's (sfi/deployments#673): two
// estates on one forge share it. It lives in provisioning/secrets.yaml
// under `<kind>-token` — `gitea-token`, `github-token` — the file the
// provisioning tokens already live in, so fort init renders no new
// secret and an estate on gitea keeps the key it has. On GitHub it is a
// token that administers the repo (a fine-grained token with
// "Administration: read and write" on that repository, or a classic
// token with the `repo` scope): deploy keys hang off the repo's
// settings, and no lesser grant lists them.
type forge interface {
	// Kind is the forge's kind, for the report lines: "gitea", "github".
	Kind() string
	// List is every deploy key on the estate's repo.
	List() ([]deployKey, error)
	// Register adds a read-only deploy key under title.
	Register(title, pub string) error
	// Revoke deletes the key with that id.
	Revoke(id int) error
}

// deployKey is one deploy key as both forges report it: the id the
// revoke names, the title mint keys on (flux-<host>), the public half
// mint compares.
type deployKey struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	Key   string `json:"key"`
}

// forgeTokenKey is the key in provisioning/secrets.yaml that holds the
// forge's token: gitea-token, github-token.
func forgeTokenKey(kind string) string { return kind + "-token" }

// forgeClient is the forge of the estate at root, with its token: what
// fort mint and fort destroy call. The repo is the estate's own
// (estate.toml `[source] repo`), because the deploy key is what flux
// clones that repo with — the route was a constant naming one repo
// until sfi/deployments#673, and the second estate's first mint would
// have put its key on the first estate's.
func forgeClient(root string) (forge, error) {
	est, err := estate.Load(root)
	if err != nil {
		return nil, err
	}
	host, owner, name, err := est.Forge()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", estate.Path, err)
	}
	kind := est.ForgeKind()
	token, err := secret(root, forgeTokenKey(kind))
	if err != nil {
		return nil, fmt.Errorf("no %s in provisioning/secrets.yaml (the %s forge's token, which registers and revokes the deploy key): %w", forgeTokenKey(kind), kind, err)
	}
	// The estate's CA root, when it carries one (nixos/certs/ca-root.pem,
	// the file estate.nix trusts): a forge behind a private CA answers
	// the API under it. Added to the system roots, not in their place —
	// a lab estate whose repo is on github.com still reaches
	// api.github.com.
	client := &http.Client{Timeout: 30 * time.Second}
	if ca, err := os.ReadFile(filepath.Join(root, "nixos/certs/ca-root.pem")); err == nil {
		pool, err := x509.SystemCertPool()
		if err != nil || pool == nil {
			pool = x509.NewCertPool()
		}
		pool.AppendCertsFromPEM(ca)
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.TLSClientConfig.RootCAs = pool
		client.Transport = transport
	}
	return newForge(kind, host, owner, name, token, client)
}

// newForge is the pure half of forgeClient: the kind, the repo and the
// token to a client. The routes are the two APIs' deploy-key routes:
//
//	gitea   https://<host>/api/v1/repos/<owner>/<name>/keys   Authorization: token <t>
//	github  https://api.github.com/repos/<owner>/<name>/keys   Authorization: Bearer <t>
//
// GitHub's API is not on the repo's host, so the host is unused there;
// a GitHub Enterprise host would be https://<host>/api/v3, which no
// estate has asked for.
func newForge(kind, host, owner, name, token string, client *http.Client) (forge, error) {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	header := http.Header{}
	var keys string
	switch kind {
	case estate.ForgeGitea:
		keys = "https://" + host + "/api/v1/repos/" + owner + "/" + name + "/keys"
		header.Set("Authorization", "token "+token)
	case estate.ForgeGitHub:
		keys = "https://api.github.com/repos/" + owner + "/" + name + "/keys"
		header.Set("Authorization", "Bearer "+token)
		header.Set("Accept", "application/vnd.github+json")
		header.Set("X-GitHub-Api-Version", "2022-11-28")
	default:
		return nil, fmt.Errorf("[source] forge %q — the forge kinds fort speaks are %q and %q", kind, estate.ForgeGitea, estate.ForgeGitHub)
	}
	return &restForge{kind: kind, keys: keys, header: header, client: client}, nil
}

// restForge is the deploy-key API as both forges serve it: GET and POST
// on the keys route, DELETE on <keys>/<id>, JSON in the same field
// names. kind is for the messages; keys is the route; header is the
// auth and whatever the API wants beside it.
type restForge struct {
	kind   string
	keys   string
	header http.Header
	client *http.Client
}

func (f *restForge) Kind() string { return f.kind }

func (f *restForge) do(method, url string, body []byte) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		return nil, err
	}
	for k, v := range f.header {
		req.Header[k] = v
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return f.client.Do(req)
}

// status is the error of a response that is not the one wanted. GitHub
// answers 404, not 403, to a token that cannot see the repo, which
// reads as a wrong URL; the message says so where it applies.
func (f *restForge) status(what string, resp *http.Response) error {
	msg := fmt.Sprintf("%s on %s: %s", what, f.kind, resp.Status)
	if f.kind == estate.ForgeGitHub && resp.StatusCode == 404 {
		msg += " (GitHub answers 404 to a token that does not administer the repository, as well as to a repository that does not exist)"
	}
	return fmt.Errorf("%s", msg)
}

func (f *restForge) List() ([]deployKey, error) {
	resp, err := f.do("GET", f.keys, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, f.status("listing deploy keys", resp)
	}
	var keys []deployKey
	return keys, json.NewDecoder(resp.Body).Decode(&keys)
}

func (f *restForge) Register(title, pub string) error {
	body, _ := json.Marshal(map[string]any{"title": title, "key": strings.TrimSpace(pub), "read_only": true})
	resp, err := f.do("POST", f.keys, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return f.status("registering "+title, resp)
	}
	return nil
}

func (f *restForge) Revoke(id int) error {
	resp, err := f.do("DELETE", fmt.Sprintf("%s/%d", f.keys, id), nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return f.status(fmt.Sprintf("deleting key %d", id), resp)
	}
	return nil
}
