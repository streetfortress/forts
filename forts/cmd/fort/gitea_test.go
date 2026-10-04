package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeGiteaState is the instance as mint and destroy see it: the repo's
// deploy keys, a list that POST appends to. Auth is checked the way gitea
// does — the fleet token on every route.
type fakeGiteaState struct {
	deploy []giteaKey
}

func newFakeGitea(t *testing.T, st *fakeGiteaState) *giteaDeployKeys {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token t" {
			w.WriteHeader(401)
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/user":
			w.Write([]byte(`{"login":"admin"}`))
		case r.Method == "GET" && r.URL.Path == "/keys":
			json.NewEncoder(w).Encode(st.deploy)
		case r.Method == "POST" && r.URL.Path == "/keys":
			var k giteaKey
			json.NewDecoder(r.Body).Decode(&k)
			k.ID = len(st.deploy) + 1
			st.deploy = append(st.deploy, k)
			w.WriteHeader(201)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return &giteaDeployKeys{client: srv.Client(), api: srv.URL, base: srv.URL, token: "t"}
}
