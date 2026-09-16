// Package b2 is a minimal Backblaze B2 native-API client, scoped to the
// application-key lifecycle `fort mint` needs — authorize, list buckets,
// create / list / delete keys — plus the three file calls `cmd/b2probe`
// uses to prove a key restriction is enforced rather than cite it.
//
// Four endpoints, so it is hand-rolled against net/http the way the gitea
// deploy-key client in `cmd/fort/mint.go` is, rather than pulling a B2 SDK
// in for them. The v4 API is what this targets: v4 is where an application
// key gained `bucketIds` as an *array*, which is why one key can cover both
// backup buckets (docs/infra/backups.md, "Minting keys").
//
// No value is printed by anything here. CreatedKey.ApplicationKey is the
// one secret this package ever holds, it comes back exactly once from
// b2_create_key, and callers are expected to put it straight into sops.
package b2

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AuthURL is the fixed entry point; every other call goes to the apiUrl
// the authorize response hands back.
const AuthURL = "https://api.backblazeb2.com/b2api/v4/b2_authorize_account"

// Capabilities the design uses by name, so callers do not spell them.
//
//	Minting  — what `provisioning/secrets.yaml` holds: it can mint and
//	           revoke fort credentials and cannot remove a backup object.
//	Writer   — what a fort's key gets: no deleteFiles, so a compromised
//	           fort cannot erase the offsite copy.
//	Pruning  — the delete-capable set, which lives only on the host that
//	           runs `restic forget --prune`.
var (
	Minting = []string{"listBuckets", "listKeys", "writeKeys", "deleteKeys", "listFiles", "readFiles", "writeFiles"}
	Writer  = []string{"listBuckets", "listFiles", "readFiles", "writeFiles"}
	Pruning = []string{"listBuckets", "listFiles", "readFiles", "writeFiles", "deleteFiles"}
)

// Client is an authorized session. Zero value is unusable; call Authorize.
type Client struct {
	HTTP      *http.Client
	AccountID string
	APIURL    string
	S3URL     string
	// Allowed is what the *authorizing* credential itself may do — the
	// restrictions B2 echoes back, which is how a caller learns whether
	// the credential it was handed is the master key or a scoped one.
	Allowed Allowed

	token string
}

// Allowed mirrors apiInfo.storageApi.allowed.
type Allowed struct {
	Capabilities []string `json:"capabilities"`
	Buckets      []struct {
		ID   string  `json:"id"`
		Name *string `json:"name"`
	} `json:"buckets"`
	NamePrefix *string `json:"namePrefix"`
}

// Key is one application key as b2_list_keys reports it. The secret half
// is absent by design: B2 returns it only from b2_create_key.
type Key struct {
	AccountID           string   `json:"accountId"`
	ApplicationKeyID    string   `json:"applicationKeyId"`
	KeyName             string   `json:"keyName"`
	Capabilities        []string `json:"capabilities"`
	ExpirationTimestamp *int64   `json:"expirationTimestamp"`
	BucketIDs           []string `json:"bucketIds"`
	NamePrefix          *string  `json:"namePrefix"`
	Options             []string `json:"options"`
}

// CreatedKey is b2_create_key's response — the only place the secret
// exists. Record ApplicationKeyID beside it: it is the S3 access key ID,
// and it is what makes the mint-time guard exact (a name alone cannot
// tell a live key from a stale one).
type CreatedKey struct {
	Key
	ApplicationKey string `json:"applicationKey"`
}

// KeySpec is a key to create. BucketIDs and NamePrefix are the whole
// point: a fort's key names both backup buckets and its own prefix.
type KeySpec struct {
	Name         string
	Capabilities []string
	BucketIDs    []string
	NamePrefix   string
	ValidFor     time.Duration
}

// Bucket is the subset of b2_list_buckets a key spec needs.
type Bucket struct {
	ID   string `json:"bucketId"`
	Name string `json:"bucketName"`
	Type string `json:"bucketType"`
}

// APIError carries B2's own error shape so callers can assert on the
// *reason* a call was refused — the probe distinguishes "this credential
// may not create keys at all" from "it may not grant that capability",
// and those are different B2 codes behind the same 401.
type APIError struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Call    string `json:"-"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %d %s: %s", e.Call, e.Status, e.Code, e.Message)
}

// Unauthorized reports whether err is B2 refusing the call on authority
// rather than on shape — 401, or the 400 `bad_request` B2 answers when a
// key spec asks for capabilities the caller does not hold.
func Unauthorized(err error) bool {
	var e *APIError
	if !errors.As(err, &e) {
		return false
	}
	return e.Status == 401 || e.Code == "unauthorized" || e.Code == "access_denied"
}

// Authorize exchanges a key pair for a session. authURL is the override
// tests use; pass "" for the real endpoint.
func Authorize(ctx context.Context, authURL, keyID, appKey string) (*Client, error) {
	if authURL == "" {
		authURL = AuthURL
	}
	c := &Client{HTTP: http.DefaultClient}
	req, err := http.NewRequestWithContext(ctx, "GET", authURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(keyID+":"+appKey)))

	var out struct {
		AccountID          string `json:"accountId"`
		AuthorizationToken string `json:"authorizationToken"`
		APIInfo            struct {
			StorageAPI struct {
				APIURL      string  `json:"apiUrl"`
				S3APIURL    string  `json:"s3ApiUrl"`
				DownloadURL string  `json:"downloadUrl"`
				Allowed     Allowed `json:"allowed"`
			} `json:"storageApi"`
		} `json:"apiInfo"`
	}
	if err := c.do(req, "b2_authorize_account", &out); err != nil {
		return nil, err
	}
	c.AccountID = out.AccountID
	c.token = out.AuthorizationToken
	c.APIURL = strings.TrimRight(out.APIInfo.StorageAPI.APIURL, "/")
	c.S3URL = out.APIInfo.StorageAPI.S3APIURL
	c.Allowed = out.APIInfo.StorageAPI.Allowed
	if c.token == "" || c.APIURL == "" {
		return nil, fmt.Errorf("b2_authorize_account: no token or apiUrl in the response (v4 shape expected)")
	}
	return c, nil
}

// Can reports whether the authorizing credential holds a capability.
func (c *Client) Can(capability string) bool {
	for _, have := range c.Allowed.Capabilities {
		if have == capability {
			return true
		}
	}
	return false
}

// Master reports whether this looks like the account master key: every
// capability and no bucket or prefix restriction. Used only to warn.
func (c *Client) Master() bool {
	return len(c.Allowed.Buckets) == 0 && c.Allowed.NamePrefix == nil &&
		c.Can("deleteBuckets") && c.Can("writeKeys") && c.Can("deleteFiles")
}

func (c *Client) ListBuckets(ctx context.Context) ([]Bucket, error) {
	body := map[string]any{"accountId": c.AccountID}
	var out struct {
		Buckets []Bucket `json:"buckets"`
	}
	err := c.call(ctx, "b2_list_buckets", body, &out)
	return out.Buckets, err
}

// ListKeys pages through every key on the account.
func (c *Client) ListKeys(ctx context.Context) ([]Key, error) {
	var all []Key
	start := ""
	for {
		body := map[string]any{"accountId": c.AccountID, "maxKeyCount": 1000}
		if start != "" {
			body["startApplicationKeyId"] = start
		}
		var out struct {
			Keys []Key   `json:"keys"`
			Next *string `json:"nextApplicationKeyId"`
		}
		if err := c.call(ctx, "b2_list_keys", body, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Keys...)
		if out.Next == nil || *out.Next == "" {
			return all, nil
		}
		start = *out.Next
	}
}

// KeyNamed returns every key carrying a name. B2 does not enforce unique
// key names, so the mint-time guard has to reason about a set: "the id I
// recorded, plus any impostor wearing the same name".
func KeyNamed(keys []Key, name string) []Key {
	var out []Key
	for _, k := range keys {
		if k.KeyName == name {
			out = append(out, k)
		}
	}
	return out
}

// CreateKey mints one. The secret in the result is returned exactly once
// by B2 and is never recoverable — persist it before doing anything else.
func (c *Client) CreateKey(ctx context.Context, spec KeySpec) (*CreatedKey, error) {
	if err := ValidName(spec.Name); err != nil {
		return nil, err
	}
	body := map[string]any{
		"accountId":    c.AccountID,
		"keyName":      spec.Name,
		"capabilities": spec.Capabilities,
	}
	if len(spec.BucketIDs) > 0 {
		body["bucketIds"] = spec.BucketIDs
	}
	if spec.NamePrefix != "" {
		body["namePrefix"] = spec.NamePrefix
	}
	if spec.ValidFor > 0 {
		body["validDurationInSeconds"] = int(spec.ValidFor.Seconds())
	}
	var out CreatedKey
	if err := c.call(ctx, "b2_create_key", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteKey(ctx context.Context, id string) (*Key, error) {
	var out Key
	err := c.call(ctx, "b2_delete_key", map[string]any{"applicationKeyId": id}, &out)
	return &out, err
}

// ValidName enforces b2_create_key's documented keyName rule up front —
// letters, digits and hyphens, 100 max — so `fort-<host>` is checked
// against it here rather than discovered at the API.
func ValidName(name string) error {
	if name == "" || len(name) > 100 {
		return fmt.Errorf("key name %q: must be 1-100 characters", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
		default:
			return fmt.Errorf("key name %q: only letters, digits and hyphens are allowed (B2 rejects %q)", name, string(r))
		}
	}
	return nil
}

func (c *Client) call(ctx context.Context, name string, body map[string]any, out any) error {
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	u, err := url.JoinPath(c.APIURL, "b2api", "v4", name)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", u, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.token)
	req.Header.Set("Content-Type", "application/json")
	return c.do(req, name, out)
}

func (c *Client) do(req *http.Request, name string, out any) error {
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if resp.StatusCode/100 != 2 {
		// B2 repeats the status in the body; unmarshal first, then pin
		// the transport's view of it so a malformed body cannot lie.
		e := &APIError{}
		if json.Unmarshal(payload, e) != nil || e.Code == "" {
			e.Code, e.Message = "unparsed", strings.TrimSpace(string(payload))
		}
		e.Status, e.Call = resp.StatusCode, name
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%s: decoding response: %w", name, err)
	}
	return nil
}

// --- the three file calls the probe needs
//
// Key lifecycle is what `fort mint` uses; these exist so a restriction can
// be proven rather than cited. "A compromised fort cannot erase the
// offsite copy" is a claim about an HTTP status, and the only honest way
// to hold it is to have seen the 401.

type UploadTarget struct {
	URL   string `json:"uploadUrl"`
	Token string `json:"authorizationToken"`
}

func (c *Client) UploadURL(ctx context.Context, bucketID string) (*UploadTarget, error) {
	var out UploadTarget
	err := c.call(ctx, "b2_get_upload_url", map[string]any{"bucketId": bucketID}, &out)
	return &out, err
}

// Upload puts one small object. Returns the file id, which is what
// DeleteFileVersion needs.
func (c *Client) Upload(ctx context.Context, t *UploadTarget, name string, data []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", t.URL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	sum := sha1.Sum(data)
	req.Header.Set("Authorization", t.Token)
	req.Header.Set("X-Bz-File-Name", url.PathEscape(name))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Bz-Content-Sha1", hex.EncodeToString(sum[:]))
	req.ContentLength = int64(len(data))
	var out struct {
		FileID string `json:"fileId"`
	}
	err = c.do(req, "b2_upload_file", &out)
	return out.FileID, err
}

func (c *Client) DeleteFileVersion(ctx context.Context, name, fileID string) error {
	return c.call(ctx, "b2_delete_file_version", map[string]any{"fileName": name, "fileId": fileID}, nil)
}

// --- lifecycle rules
//
// The design's retention instructions are lifecycle rules, and the two
// fields do opposite things: DaysFromHidingToDeleting reclaims versions
// that are already deleted (without it, B2 bills for everything `restic
// prune` believes it removed), while DaysFromUploadingToHiding expires
// live objects by age and would shred a restic repo's pack files.

type LifecycleRule struct {
	FileNamePrefix            string `json:"fileNamePrefix"`
	DaysFromHidingToDeleting  *int   `json:"daysFromHidingToDeleting"`
	DaysFromUploadingToHiding *int   `json:"daysFromUploadingToHiding"`
}

type bucketDetail struct {
	ID             string          `json:"bucketId"`
	Name           string          `json:"bucketName"`
	LifecycleRules []LifecycleRule `json:"lifecycleRules"`
}

// BucketRules reads back what a bucket's lifecycle rules actually are.
func (c *Client) BucketRules(ctx context.Context, name string) ([]LifecycleRule, error) {
	var out struct {
		Buckets []bucketDetail `json:"buckets"`
	}
	if err := c.call(ctx, "b2_list_buckets", map[string]any{"accountId": c.AccountID}, &out); err != nil {
		return nil, err
	}
	for _, b := range out.Buckets {
		if b.Name == name {
			return b.LifecycleRules, nil
		}
	}
	return nil, fmt.Errorf("no bucket named %q", name)
}

// SetBucketRules replaces a bucket's lifecycle rules wholesale.
func (c *Client) SetBucketRules(ctx context.Context, bucketID string, rules []LifecycleRule) error {
	if rules == nil {
		rules = []LifecycleRule{}
	}
	return c.call(ctx, "b2_update_bucket", map[string]any{
		"accountId": c.AccountID, "bucketId": bucketID, "lifecycleRules": rules,
	}, nil)
}

func Days(n int) *int { return &n }
