package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/estate"
)

// fakeForgeState is the forge as mint and destroy see it: the repo's
// deploy keys, a list that POST appends to and DELETE removes from.
type fakeForgeState struct {
	deploy []deployKey
}

// newFakeForge serves the deploy-key routes the way the kind's API
// does — the auth header each one wants on every route, 404 for the
// rest — and returns the client that speaks to it.
func newFakeForge(t *testing.T, st *fakeForgeState, kind string) forge {
	auth := "token t"
	if kind == estate.ForgeGitHub {
		auth = "Bearer t"
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != auth {
			w.WriteHeader(401)
			return
		}
		if kind == estate.ForgeGitHub && r.Header.Get("Accept") != "application/vnd.github+json" {
			w.WriteHeader(415)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/keys":
			json.NewEncoder(w).Encode(st.deploy)
		case r.Method == "POST" && r.URL.Path == "/keys":
			var k deployKey
			json.NewDecoder(r.Body).Decode(&k)
			k.ID = len(st.deploy) + 1
			st.deploy = append(st.deploy, k)
			w.WriteHeader(201)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/keys/"):
			id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/keys/"))
			var kept []deployKey
			for _, k := range st.deploy {
				if k.ID != id {
					kept = append(kept, k)
				}
			}
			if len(kept) == len(st.deploy) {
				w.WriteHeader(404)
				return
			}
			st.deploy = kept
			w.WriteHeader(204)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	f, err := newForge(kind, "forge.example", "acme", "estate", "t", srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	f.(*restForge).keys = srv.URL + "/keys"
	return f
}

// The two kinds, one shape (forts#1): the route and the auth header are
// what differ, and nothing else.
func TestNewForge(t *testing.T) {
	f, err := newForge(estate.ForgeGitea, "forge.example:30022", "acme", "estate", "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	g := f.(*restForge)
	if g.Kind() != "gitea" || g.keys != "https://forge.example:30022/api/v1/repos/acme/estate/keys" || g.header.Get("Authorization") != "token secret" {
		t.Errorf("gitea: %+v", g)
	}
	f, err = newForge(estate.ForgeGitHub, "github.com", "acme", "estate", "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	h := f.(*restForge)
	if h.Kind() != "github" || h.keys != "https://api.github.com/repos/acme/estate/keys" || h.header.Get("Authorization") != "Bearer secret" || h.header.Get("Accept") != "application/vnd.github+json" || h.header.Get("X-GitHub-Api-Version") == "" {
		t.Errorf("github: %+v", h)
	}
	if _, err := newForge("sourcehut", "sr.ht", "acme", "estate", "secret", nil); err == nil || !strings.Contains(err.Error(), "sourcehut") {
		t.Errorf("unknown kind: %v", err)
	}
	if forgeTokenKey("github") != "github-token" || forgeTokenKey("gitea") != "gitea-token" {
		t.Error("the token is <kind>-token in provisioning/secrets.yaml")
	}
}

// The whole lifecycle of a deploy key on each kind, through the fake
// that checks the kind's auth: absent, registered, verified, revoked —
// mint's read, mint's write, destroy's revoke.
func TestForgeLifecycle(t *testing.T) {
	const pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJDZXdVYO58sXRAwLieovSB3fHDAhMF96rjyfCx+vLuL flux-cloud-fort"
	for _, kind := range []string{estate.ForgeGitea, estate.ForgeGitHub} {
		st := &fakeForgeState{deploy: []deployKey{{ID: 1, Title: "flux-other", Key: "ssh-ed25519 AAAAother"}}}
		f := newFakeForge(t, st, kind)
		status, register, err := deployKeyStatus(f, "cloud-fort", pub)
		if err != nil || !register || status != "would register" {
			t.Fatalf("%s absent: %q %v %v", kind, status, register, err)
		}
		if status, err := registerDeployKey(f, "cloud-fort", pub); err != nil || status != "registered flux-cloud-fort (read-only)" {
			t.Fatalf("%s register: %q %v", kind, status, err)
		}
		if len(st.deploy) != 2 || st.deploy[1].Title != "flux-cloud-fort" || !strings.HasPrefix(st.deploy[1].Key, "ssh-ed25519 AAAAC3") {
			t.Fatalf("%s registered: %+v", kind, st.deploy)
		}
		status, register, err = deployKeyStatus(f, "cloud-fort", pub)
		if err != nil || register || status != "registered (id 2, verified match)" {
			t.Fatalf("%s present: %q %v %v", kind, status, register, err)
		}
		msg, err := revokeDeployKeyAt(f, "cloud-fort")
		if err != nil || msg != "revoked flux-cloud-fort (id 2)" || len(st.deploy) != 1 {
			t.Fatalf("%s revoke: %q %v %+v", kind, msg, err, st.deploy)
		}
		if msg, err := revokeDeployKeyAt(f, "cloud-fort"); err != nil || !strings.Contains(msg, "not registered") {
			t.Fatalf("%s revoke again: %q %v", kind, msg, err)
		}
		// A wrong token is the kind's refusal, named by the kind.
		bad, _ := newForge(kind, "forge.example", "acme", "estate", "wrong", f.(*restForge).client)
		bad.(*restForge).keys = f.(*restForge).keys
		if _, _, err := deployKeyStatus(bad, "cloud-fort", pub); err == nil || !strings.Contains(err.Error(), kind) {
			t.Errorf("%s bad token: %v", kind, err)
		}
	}
}

// GitHub answers 404 to a token that cannot administer the repository;
// the message says so, because the status alone reads as a wrong URL.
func TestGitHubNotFoundHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	t.Cleanup(srv.Close)
	f, _ := newForge(estate.ForgeGitHub, "github.com", "acme", "estate", "t", srv.Client())
	f.(*restForge).keys = srv.URL + "/keys"
	if _, err := f.List(); err == nil || !strings.Contains(err.Error(), "does not administer") {
		t.Errorf("github 404: %v", err)
	}
	g, _ := newForge(estate.ForgeGitea, "forge.example", "acme", "estate", "t", srv.Client())
	g.(*restForge).keys = srv.URL + "/keys"
	if _, err := g.List(); err == nil || strings.Contains(err.Error(), "administer") {
		t.Errorf("gitea 404 carries no github hint: %v", err)
	}
}
