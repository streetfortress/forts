package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/estate"
)

// stateBackend is where pulumi keeps the estate's stacks (forts#248):
// the one thing the apply verbs, preview, dismantle and completion read
// instead of a path under the checkout. Three kinds. The repo's
// committed file backend (provisioning/pulumi/state) is the default and
// the solo operator's: the owning repo is where state lives, which
// dissolves the day-zero bootstrap problem (sfi/knowledge
// components/pulumi.md). A team names an S3-compatible bucket, and
// pulumi reads and writes it directly — any bucket that already exists,
// so no fort has to host the store first. An estate that wants neither
// keeps a file backend outside the git tree, under the user's config
// dir, which nothing backs up.
//
// The stack's config (Pulumi.<host>.yaml: the encryption salt, the
// image, the seed channel, the hypervisor) is the project's file on
// every kind, committed beside Pulumi.yaml; only the checkpoint moves.
type stateBackend struct {
	// kind is estate.StateRepo, StateS3 or StateLocal.
	kind string
	// url is what PULUMI_BACKEND_URL carries.
	url string
	// dir is the file backend's directory on disk; "" on s3.
	dir string
}

// stateBackendFor reads the estate's [state] table into the backend the
// verbs point pulumi at.
func stateBackendFor(root string, est estate.Estate) (stateBackend, error) {
	switch kind := est.StateBackend(); kind {
	case estate.StateRepo:
		dir := filepath.Join(root, pulumiProject, "state")
		return stateBackend{kind: kind, url: "file://" + dir, dir: dir}, nil
	case estate.StateLocal:
		dir, err := localStateDir(est.Name)
		if err != nil {
			return stateBackend{}, err
		}
		return stateBackend{kind: kind, url: "file://" + dir, dir: dir}, nil
	case estate.StateS3:
		return stateBackend{kind: kind, url: est.State.S3URL()}, nil
	default:
		return stateBackend{}, fmt.Errorf("[state] backend %q: estate.toml validation let it through", kind)
	}
}

// offlineBackend is the estate's backend for a reader that must not
// fail on the estate file and must not touch the network — completion
// and the dismantle plan. An estate.toml that does not load reads as
// the default, the repo's committed backend, as before forts#248.
func offlineBackend(root string) stateBackend {
	if est, err := estate.Load(root); err == nil {
		if b, err := stateBackendFor(root, est); err == nil {
			return b
		}
	}
	dir := filepath.Join(root, pulumiProject, "state")
	return stateBackend{kind: estate.StateRepo, url: "file://" + dir, dir: dir}
}

// localStateDir is the local backend's directory: <config dir>/forts/
// states/<estate> — ~/.config on Linux, ~/Library/Application Support
// on a Mac. The estate's name keeps two estates on one machine apart.
func localStateDir(estateName string) (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("the local state backend needs a config dir: %w", err)
	}
	return filepath.Join(cfg, "forts", "states", estateName), nil
}

// String names the backend as a message reads it.
func (b stateBackend) String() string {
	switch b.kind {
	case estate.StateS3:
		return b.url
	case estate.StateLocal:
		return b.dir + " (outside the git tree)"
	default:
		return pulumiProject + "/state (committed)"
	}
}

// where is the backend's name as the gates' refusals say it: the place a
// stack would be.
func (b stateBackend) where() string {
	if b.kind == estate.StateRepo {
		return pulumiProject + "/state"
	}
	return b.String()
}

// isFile says whether the backend is a directory this process can read:
// the repo's and the local one. On these the stack list and the
// checkpoint are file reads, cheap and offline; on s3 they are pulumi
// calls.
func (b stateBackend) isFile() bool { return b.kind != estate.StateS3 }

// warnedLocal: the local backend's warning prints once per process, not
// once per fort of a `preview --all`.
var warnedLocal bool

// env points pulumi at the backend: PULUMI_BACKEND_URL, which wins over
// the login `pulumi login` recorded in ~/.pulumi/credentials.json (a
// login from a worktree once sent every later run's state into that
// worktree, 2026-09-13), and, on s3, the bucket's credentials — explicit
// env wins; otherwise sops-decrypted from provisioning/secrets.yaml
// (state-access-key-id, state-secret-access-key), as every other
// credential a verb reads. The local backend's directory is created
// here, because the file backend refuses a directory that does not
// exist, and the warning says what the estate opted into: nothing
// commits or backs that directory up.
func (b stateBackend) env(root string) error {
	os.Setenv("PULUMI_BACKEND_URL", b.url)
	switch b.kind {
	case estate.StateLocal:
		if err := os.MkdirAll(b.dir, 0o700); err != nil {
			return fmt.Errorf("the local state backend: %w", err)
		}
		if !warnedLocal {
			warnedLocal = true
			fmt.Fprintf(os.Stderr, "warning: pulumi state lives in %s, outside the git tree — nothing backs it up; a lost directory is a lost stack, so copy it somewhere safe after every apply\n", b.dir)
		}
	case estate.StateS3:
		for env, key := range map[string]string{"AWS_ACCESS_KEY_ID": "state-access-key-id", "AWS_SECRET_ACCESS_KEY": "state-secret-access-key"} {
			if os.Getenv(env) != "" {
				continue
			}
			v, err := secret(root, key)
			if err != nil {
				return fmt.Errorf("%s not set and no %s in provisioning/secrets.yaml — the s3 state backend's credentials: sops --config .sops.yaml set provisioning/secrets.yaml '[\"%s\"]' '\"<value>\"'", env, key, key)
			}
			os.Setenv(env, v)
		}
	}
	return nil
}

// stackFile is the file backend's checkpoint for host, relative to the
// backend's directory. .bak and .attrs beside it are the backend's own
// residue, not stacks.
func stackFile(host string) string {
	return filepath.Join(".pulumi/stacks/forts", host+".json")
}

// hasStackOffline says, without touching the network, whether the
// backend holds a stack for host: the checkpoint's presence on a file
// backend; never on s3, where the answer is a bucket call (hasStack).
// For completion and the dismantle plan, which must not block.
func (b stateBackend) hasStackOffline(host string) bool {
	return b.isFile() && exists(b.dir, stackFile(host))
}

// hasStack says whether the backend holds a stack for host. env must
// have run on s3.
func (b stateBackend) hasStack(pulumiDir, host string) (bool, error) {
	if b.isFile() {
		return b.hasStackOffline(host), nil
	}
	names, err := b.stacks(pulumiDir)
	if err != nil {
		return false, err
	}
	for _, n := range names {
		if n == host {
			return true, nil
		}
	}
	return false, nil
}

// stacks lists the stacks the backend holds. env must have run on s3.
func (b stateBackend) stacks(pulumiDir string) ([]string, error) {
	if b.isFile() {
		return stacksInDir(b.dir), nil
	}
	out, err := outputPulumi(pulumiDir, "stack", "ls", "--json")
	if err != nil {
		return nil, fmt.Errorf("pulumi stack ls (the s3 state backend %s): %s", b.url, lastLine(exitDetail(err)))
	}
	return stacksFromList([]byte(out))
}

// stacksOffline is the stack list without a network call: the file
// backend's; none on s3 (completion: a <TAB> must not block on a
// bucket).
func (b stateBackend) stacksOffline() []string {
	if !b.isFile() {
		return nil
	}
	return stacksInDir(b.dir)
}

// stacksInDir lists the stacks a file backend holds
// (.pulumi/stacks/<project>/<stack>.json; .bak and .attrs are the
// backend's own residue, not stacks).
func stacksInDir(stateDir string) []string {
	matches, _ := filepath.Glob(filepath.Join(stateDir, ".pulumi/stacks/*/*.json"))
	var out []string
	for _, m := range matches {
		out = append(out, strings.TrimSuffix(filepath.Base(m), ".json"))
	}
	sort.Strings(out)
	return out
}

// stacksFromList reads `pulumi stack ls --json`: one entry per stack of
// the project, by name.
func stacksFromList(raw []byte) ([]string, error) {
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("not a stack list: %w", err)
	}
	var out []string
	for _, s := range list {
		out = append(out, s.Name)
	}
	sort.Strings(out)
	return out, nil
}

// provider reads the provider the stack's state records (its
// pulumi:providers:<name> resource): the checkpoint file on a file
// backend, `pulumi stack export` on s3. `fort destroy` has no manifest
// to ask — the dismantle PR deleted it — so this is the one source.
func (b stateBackend) provider(pulumiDir, host string) (string, error) {
	if b.isFile() {
		raw, err := os.ReadFile(filepath.Join(b.dir, stackFile(host)))
		if err != nil {
			return "", err
		}
		return providerFromState(raw)
	}
	out, err := outputPulumi(pulumiDir, "stack", "export", "--stack", host)
	if err != nil {
		return "", fmt.Errorf("pulumi stack export %s: %s", host, lastLine(exitDetail(err)))
	}
	return providerFromExport([]byte(out))
}

// forget removes what a file backend keeps after `pulumi stack rm`
// (web3 left its .bak behind): the stack's history, backups, and
// .bak/.attrs. A bucket keeps the same residue under .pulumi/history
// and .pulumi/backups; it is harmless there, and the CLI carries no S3
// client to sweep it.
func (b stateBackend) forget(host string) {
	if !b.isFile() {
		return
	}
	for _, rel := range stateResidue(host) {
		os.RemoveAll(filepath.Join(b.dir, rel))
	}
}

// stateResidue: what the file backend leaves behind after stack rm,
// relative to the backend's directory.
func stateResidue(host string) []string {
	return []string{
		".pulumi/stacks/forts/" + host + ".json.bak",
		".pulumi/stacks/forts/" + host + ".json.bak.attrs",
		".pulumi/stacks/forts/" + host + ".json.attrs",
		".pulumi/history/forts/" + host,
		".pulumi/backups/forts/" + host,
	}
}
