package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/estate"
)

// fileBackend is a file backend at dir, as the tests of the gates want
// one: the repo's or the local one — the gates read both the same way.
func fileBackend(dir string) stateBackend {
	kind := estate.StateRepo
	if !strings.Contains(dir, pulumiProject) {
		kind = estate.StateLocal
	}
	return stateBackend{kind: kind, url: "file://" + dir, dir: dir}
}

// The three backends (forts#248): the repo's is the default and reads as
// before; local is a directory under the user's config dir, named after
// the estate; s3 is a URL and no directory.
func TestStateBackendFor(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
	parse := func(table string) estate.Estate {
		e, err := estate.Parse([]byte("name = \"second-estate\"\n[source]\nrepo = \"ssh://x/sfi/second-estate\"\n" + table))
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	repo, err := stateBackendFor(root, parse(""))
	if err != nil || repo.kind != estate.StateRepo || repo.dir != filepath.Join(root, "provisioning/pulumi/state") || repo.url != "file://"+repo.dir || !repo.isFile() {
		t.Errorf("repo: %+v %v", repo, err)
	}
	if repo.where() != "provisioning/pulumi/state" || !strings.Contains(repo.String(), "committed") {
		t.Errorf("repo names: %q %q", repo.where(), repo.String())
	}
	local, err := stateBackendFor(root, parse("[state]\nbackend = \"local\"\n"))
	if err != nil || local.kind != estate.StateLocal || local.dir != filepath.Join(root, "xdg/forts/states/second-estate") || local.url != "file://"+local.dir {
		t.Errorf("local: %+v %v", local, err)
	}
	if !strings.Contains(local.where(), "outside the git tree") {
		t.Errorf("local names the directory and that nothing commits it: %q", local.where())
	}
	s3, err := stateBackendFor(root, parse("[state]\nbackend = \"s3\"\nbucket = \"estate-state/second-estate\"\nendpoint = \"https://minio.lab:9000\"\nregion = \"us-east-1\"\n"))
	if err != nil || s3.kind != estate.StateS3 || s3.dir != "" || s3.isFile() || !strings.HasPrefix(s3.url, "s3://estate-state/second-estate?") {
		t.Errorf("s3: %+v %v", s3, err)
	}
	if s3.where() != s3.url {
		t.Errorf("s3 names the URL: %q", s3.where())
	}
	// no stack list without a network call on s3, and nothing to forget
	if s3.stacksOffline() != nil || s3.hasStackOffline("x") {
		t.Error("s3: an offline read must answer nothing, not block")
	}
	s3.forget("x")
	// a checkout whose estate.toml does not load reads as the repo's
	if b := offlineBackend(root); b.kind != estate.StateRepo || b.dir != repo.dir {
		t.Errorf("offlineBackend with no estate.toml: %+v", b)
	}
	os.WriteFile(filepath.Join(root, "estate.toml"), []byte("name = \"second-estate\"\n[source]\nrepo = \"ssh://x/sfi/second-estate\"\n[state]\nbackend = \"local\"\n"), 0o644)
	if b := offlineBackend(root); b.kind != estate.StateLocal || b.dir != local.dir {
		t.Errorf("offlineBackend reads [state]: %+v", b)
	}
}

// env: the backend URL always; the local directory created and warned
// about once; the bucket's credentials from the environment when set.
func TestStateBackendEnv(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PULUMI_BACKEND_URL", "file://elsewhere")
	local := fileBackend(filepath.Join(root, "local"))
	warnedLocal = false
	if err := local.env(root); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PULUMI_BACKEND_URL") != local.url {
		t.Errorf("PULUMI_BACKEND_URL: %q", os.Getenv("PULUMI_BACKEND_URL"))
	}
	if fi, err := os.Stat(local.dir); err != nil || !fi.IsDir() {
		t.Error("the local directory is created: the file backend refuses one that does not exist")
	}
	if !warnedLocal {
		t.Error("the local backend warns that nothing backs it up")
	}
	// s3 with the credentials in the environment: no sops read
	t.Setenv("AWS_ACCESS_KEY_ID", "key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	s3 := stateBackend{kind: estate.StateS3, url: "s3://b?region=r"}
	if err := s3.env(root); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("PULUMI_BACKEND_URL") != s3.url {
		t.Errorf("PULUMI_BACKEND_URL: %q", os.Getenv("PULUMI_BACKEND_URL"))
	}
	// without them, and no secrets file: the refusal names the key to set
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	err := s3.env(root)
	if err == nil || !strings.Contains(err.Error(), "state-secret-access-key") {
		t.Errorf("missing credential: want the sops key named, got %v", err)
	}
}

// The reads that replace the checkpoint file on s3.
func TestStackListAndExport(t *testing.T) {
	got, err := stacksFromList([]byte(`[{"name":"hub","current":false,"updateInProgress":false,"resourceCount":4},{"name":"edge","current":true}]`))
	if err != nil || strings.Join(got, ",") != "edge,hub" {
		t.Errorf("stack ls: %v %v", got, err)
	}
	if got, err := stacksFromList([]byte(`[]`)); err != nil || len(got) != 0 {
		t.Errorf("empty stack ls: %v %v", got, err)
	}
	if _, err := stacksFromList([]byte(`error: no stack`)); err == nil {
		t.Error("garbage must be an error")
	}
	export := `{"version":3,"deployment":{"manifest":{},"resources":[
	 {"type":"pulumi:pulumi:Stack"},{"type":"pulumi:providers:linode"},{"type":"linode:index/instance:Instance"}]}}`
	if p, err := providerFromExport([]byte(export)); err != nil || p != "linode" {
		t.Errorf("provider from export: %q %v", p, err)
	}
	if _, err := providerFromExport([]byte(`{"version":3,"deployment":{"manifest":{}}}`)); err == nil {
		t.Error("a fresh stack exports no provider")
	}
	// the file read, through the backend
	tmp := t.TempDir()
	b := fileBackend(tmp)
	os.MkdirAll(filepath.Join(tmp, ".pulumi/stacks/forts"), 0o755)
	os.WriteFile(filepath.Join(tmp, ".pulumi/stacks/forts/gone.json"), []byte(checkpoint), 0o644)
	if p, err := b.provider("unused", "gone"); err != nil || p != "libvirt" {
		t.Errorf("provider from the file: %q %v", p, err)
	}
	// forget sweeps the residue and leaves other stacks alone
	for _, rel := range stateResidue("gone") {
		os.MkdirAll(filepath.Dir(filepath.Join(tmp, rel)), 0o755)
		os.WriteFile(filepath.Join(tmp, rel), nil, 0o644)
	}
	os.WriteFile(filepath.Join(tmp, ".pulumi/stacks/forts/kept.json.bak"), nil, 0o644)
	b.forget("gone")
	for _, rel := range stateResidue("gone") {
		if _, err := os.Stat(filepath.Join(tmp, rel)); err == nil {
			t.Errorf("%s survived forget", rel)
		}
	}
	if !exists(tmp, ".pulumi/stacks/forts/kept.json.bak") {
		t.Error("forget swept another stack's residue")
	}
}
