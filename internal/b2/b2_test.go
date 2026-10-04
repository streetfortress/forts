package b2

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// v4's authorize response nests the restrictions under
// apiInfo.storageApi.allowed — the shape that changed when keys gained
// multiple buckets. Parsing the older flat shape by mistake would leave
// Allowed empty and every capability check silently false.
const authV4 = `{
  "accountId": "abc123",
  "authorizationToken": "TOKEN",
  "apiInfo": {
    "storageApi": {
      "apiUrl": "%s",
      "s3ApiUrl": "https://s3.us-west-004.backblazeb2.com",
      "downloadUrl": "https://f004.backblazeb2.com",
      "allowed": {
        "capabilities": ["listBuckets","listKeys","writeKeys","deleteKeys","listFiles","readFiles","writeFiles"],
        "buckets": [{"id":"b1","name":"sfi-db-physical-backups"}],
        "namePrefix": "sfi-prod-439bf72/"
      }
    }
  }
}`

func serve(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, h := range routes {
		mux.HandleFunc(path, h)
	}
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func authorized(t *testing.T, routes map[string]http.HandlerFunc) *Client {
	t.Helper()
	var srv *httptest.Server
	all := map[string]http.HandlerFunc{
		"/b2api/v4/b2_authorize_account": func(w http.ResponseWriter, r *http.Request) {
			if _, _, ok := r.BasicAuth(); !ok {
				t.Error("authorize: no basic auth header")
			}
			w.Write([]byte(strings.Replace(authV4, "%s", srv.URL, 1)))
		},
	}
	for k, v := range routes {
		all[k] = v
	}
	srv = serve(t, all)
	c, err := Authorize(context.Background(), srv.URL+"/b2api/v4/b2_authorize_account", "id", "key")
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	return c
}

func TestAuthorizeReadsV4Shape(t *testing.T) {
	c := authorized(t, nil)
	if c.AccountID != "abc123" {
		t.Errorf("accountId = %q", c.AccountID)
	}
	if !c.Can("writeKeys") || c.Can("deleteFiles") {
		t.Errorf("capabilities not read from apiInfo.storageApi.allowed: %v", c.Allowed.Capabilities)
	}
	if c.Master() {
		t.Error("a bucket-restricted key without deleteFiles must not look like the master key")
	}
	if c.Allowed.NamePrefix == nil || *c.Allowed.NamePrefix != "sfi-prod-439bf72/" {
		t.Errorf("namePrefix = %v", c.Allowed.NamePrefix)
	}
}

// The array is the reason a fort needs one key and not one per bucket, so
// the request has to carry bucketIds as a list, not a scalar.
func TestCreateKeySendsBucketIDsArray(t *testing.T) {
	var got map[string]any
	c := authorized(t, map[string]http.HandlerFunc{
		"/b2api/v4/b2_create_key": func(w http.ResponseWriter, r *http.Request) {
			json.NewDecoder(r.Body).Decode(&got)
			json.NewEncoder(w).Encode(map[string]any{
				"keyName": "fort-x", "applicationKeyId": "K1", "applicationKey": "SECRET",
				"capabilities": Writer, "bucketIds": []string{"b1", "b2"}, "namePrefix": "x/",
			})
		},
	})
	k, err := c.CreateKey(context.Background(), KeySpec{
		Name: "fort-x", Capabilities: Writer,
		BucketIDs: []string{"b1", "b2"}, NamePrefix: "x/", ValidFor: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	ids, ok := got["bucketIds"].([]any)
	if !ok || len(ids) != 2 {
		t.Fatalf("bucketIds sent as %#v, want a 2-element array", got["bucketIds"])
	}
	if got["validDurationInSeconds"] != float64(3600) {
		t.Errorf("validDurationInSeconds = %v", got["validDurationInSeconds"])
	}
	if k.ApplicationKey != "SECRET" || k.ApplicationKeyID != "K1" {
		t.Errorf("the one-shot secret and its id must both survive decoding: %+v", k)
	}
}

func TestCreateKeyRejectsUnderscoresBeforeTheAPIDoes(t *testing.T) {
	c := authorized(t, nil)
	if _, err := c.CreateKey(context.Background(), KeySpec{Name: "fort_sfi_prod", Capabilities: Writer}); err == nil {
		t.Fatal("expected the keyName rule (letters, digits, hyphens) to refuse an underscore")
	}
	if err := ValidName("fort-sfi-prod-439bf72"); err != nil {
		t.Errorf("the naming convention must be a legal B2 key name: %v", err)
	}
}

func TestListKeysPages(t *testing.T) {
	page := 0
	c := authorized(t, map[string]http.HandlerFunc{
		"/b2api/v4/b2_list_keys": func(w http.ResponseWriter, r *http.Request) {
			page++
			if page == 1 {
				json.NewEncoder(w).Encode(map[string]any{
					"keys":                 []Key{{ApplicationKeyID: "K1", KeyName: "fort-a"}},
					"nextApplicationKeyId": "K2",
				})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"keys": []Key{{ApplicationKeyID: "K2", KeyName: "fort-a"}},
			})
		},
	})
	keys, err := c.ListKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("got %d keys across %d pages", len(keys), page)
	}
	// Two keys under one name is exactly the mint-time ambiguity the
	// recorded keyID resolves.
	if n := KeyNamed(keys, "fort-a"); len(n) != 2 {
		t.Errorf("KeyNamed returned %d", len(n))
	}
}

func TestUnauthorizedDistinguishesAuthorityFromShape(t *testing.T) {
	c := authorized(t, map[string]http.HandlerFunc{
		"/b2api/v4/b2_create_key": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(401)
			w.Write([]byte(`{"status":401,"code":"unauthorized","message":"not authorized with these parameters"}`))
		},
		"/b2api/v4/b2_delete_key": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(400)
			w.Write([]byte(`{"status":400,"code":"bad_bucket_id","message":"nope"}`))
		},
	})
	_, err := c.CreateKey(context.Background(), KeySpec{Name: "fort-x", Capabilities: Pruning})
	if !Unauthorized(err) {
		t.Errorf("a 401 must read as a refusal on authority: %v", err)
	}
	if _, err := c.DeleteKey(context.Background(), "K1"); Unauthorized(err) {
		t.Errorf("a 400 on shape must not read as a refusal on authority: %v", err)
	} else if !strings.Contains(err.Error(), "bad_bucket_id") {
		t.Errorf("the B2 code should survive into the error: %v", err)
	}
}
