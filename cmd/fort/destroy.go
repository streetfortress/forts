package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gitea.zen.lofi/sfi/forts/internal/tools"
	"github.com/linode/linodego"
)

// destroy is the second half of decommissioning (#217): everything with
// side effects outside git, reachable only once main says the host is
// retired — the merged dismantle PR is the deliberate act `protect:
// true` was guarding for. The steps are the web3 runbook
// (docs/destroy.md), in order, each pulumi step with inherited
// stdio so a human reads and confirms exactly as by hand. `pulumi
// destroy` does not run the program, so the missing manifest is no
// obstacle: the provider comes from the checkpoint. A failure
// mid-way leaves the (partly deleted, unprotected) state in the working
// tree — the right place for it — and the rerun picks up where it was.
//
// A [baremetal] host has no stack by design (#287): `--no-stack` skips
// the pulumi steps and does only what is left — the off-repo identities
// mint registered (deploy key, B2 keys). The machine itself goes by hand
// (docs/baremetal.md). The flag is a declaration, not a detection:
// with the manifest gone nothing says whether "no stack" meant hardware
// or a host never born, and only a human knows which.
func destroy(args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: fort destroy <host> [--yes] [--no-stack]")
	}
	host := args[0]
	fs := flag.NewFlagSet("destroy", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "confirm the pulumi refresh and destroy without a prompt")
	noStack := fs.Bool("no-stack", false, "a [baremetal] host: no pulumi stack to destroy — revoke the deploy key and B2 keys only")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := needsTTYOrYes("destroy", *yes); err != nil {
		return err
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	pulumiDir := filepath.Join(root, pulumiProject)

	git, err := tools.Runner("git")
	if err != nil {
		return err
	}
	if err := git("-C", root, "fetch", "-q", "origin").Run(); err != nil {
		return fmt.Errorf("git fetch origin: %w", err)
	}
	onMain, err := git("-C", root, "ls-tree", "--name-only", "origin/main", "--", manifest.Path(host)).Output()
	if err != nil {
		return fmt.Errorf("reading origin/main: %w", err)
	}
	status, err := git("-C", root, "status", "--porcelain").Output()
	if err != nil {
		return err
	}
	provider, problems := destroyGates(root, host, strings.TrimSpace(string(onMain)) != "", len(strings.TrimSpace(string(status))) == 0, *noStack)
	if *noStack {
		fmt.Printf("%s   (no stack: a [baremetal] host, the machine goes by hand)\n", host)
	} else {
		fmt.Printf("%s   (provider %s, from the checkpoint)\n", host, provider)
	}
	if len(problems) > 0 {
		for _, p := range problems {
			fmt.Printf("  ✗ %s\n", p)
		}
		return fmt.Errorf("refusing: %d gate(s) not met", len(problems))
	}
	if *noStack {
		fmt.Println("  ✓ no manifest on origin/main or in the working tree; no stack")
		revokeIdentities(root, host)
		fmt.Printf("\n== done. Nothing to commit: the host had no state. Power the machine off and wipe its disk (docs/baremetal.md).\n")
		fmt.Println("   Off-repo residue to check: the .files wireguard copy (ubuntu mesh members).")
		return nil
	}
	fmt.Println("  ✓ no manifest on origin/main or in the working tree; tree clean; stack exists")

	if err := cloudEnv(root, provider); err != nil {
		return err
	}
	ctx := context.Background()
	if provider == "linode" {
		// Linode refuses disk deletion on a running instance and the
		// provider does not shut it down for you (the web3 lesson).
		if err := linodePowerOff(ctx, host); err != nil {
			return err
		}
	}

	rerun := func(step string, err error) error {
		return fmt.Errorf("%s failed — the stack is left as is (unprotected once that step ran); fix and rerun `fort destroy %s`: %w", step, host, err)
	}
	// 1. refresh, so the destroy works from truth (anything removed by
	// hand is dropped from state instead of failing the destroy).
	fmt.Println("== pulumi refresh")
	if err := runPulumi(pulumiDir, append([]string{"refresh", "--stack", host}, confirmArgs(*yes)...)...); err != nil {
		return rerun("refresh", err)
	}
	// 2. unprotect — state-only, reversible, and only ever right here.
	fmt.Println("== pulumi state unprotect --all (state only; the destroy is next)")
	if err := runPulumi(pulumiDir, "state", "unprotect", "--all", "--stack", host, "--yes"); err != nil {
		return rerun("state unprotect", err)
	}
	// 3. destroy — you read the deletes and confirm.
	fmt.Println("== pulumi destroy")
	if err := runPulumi(pulumiDir, append([]string{"destroy", "--stack", host}, confirmArgs(*yes)...)...); err != nil {
		return rerun("destroy", err)
	}
	// 4. stack rm refuses while any resource remains — that is the
	// check that step 3 finished (a declined preview leaves them).
	fmt.Println("== pulumi stack rm")
	if err := runPulumi(pulumiDir, "stack", "rm", "--stack", host, "--yes"); err != nil {
		return rerun("stack rm (resources remain?)", err)
	}
	// The file backend keeps residue stack rm does not touch (web3 left
	// its .bak behind): the stack's history, backups, and .bak/.attrs.
	for _, rel := range stateResidue(host) {
		os.RemoveAll(filepath.Join(pulumiDir, rel))
	}
	os.Remove(filepath.Join(root, stackConfigPath(host)))

	// 5. the off-repo identities mint registered.
	revokeIdentities(root, host)

	// 6. stage the state removal; the commit + PR stay with you (git
	// history is the provenance of the destroy).
	stage(root)
	fmt.Printf("\n== done. Staged the state removal — commit it as its own PR (\"%s: destroyed\").\n", host)
	fmt.Println("   Off-repo residue to check: the .files wireguard copy (ubuntu mesh members), ~/.local/share/nixos-provisioning/" + host + " on a playbook-era hypervisor.")
	return nil
}

// revokeIdentities: the deploy key fort mint registered — the one
// off-repo identity of the flux half (the flux age identity died with
// the sops rule) — and the B2 application keys. An identity the estate
// minted for an account of its own on the fort is the estate's to revoke.
// Both warned rather than
// fatal: the minting credential is an operational input
// (docs/infra/backups.md), and a destroy must not be blocked waiting for
// it. A B2 key is scoped to its fort's prefix, so what it can still
// reach is only the backups that are about to age out — but it is live
// until someone removes it.
//
// The retired nixos/secrets/<host>.yaml is read back from git history
// once and answers for both: the GPG key ids (decrypted), and whether
// the fort ever held a B2 key at all (a plaintext key name, #432) — so a
// fort that never had one never asks for the minting key.
func revokeIdentities(root, host string) {
	if msg, err := revokeDeployKey(root, host); err != nil {
		fmt.Fprintf(os.Stderr, "warning: deploy key: %v — delete flux-%s by hand in the repo's deploy-key settings\n", err, host)
	} else {
		fmt.Println("  deploy key:", msg)
	}
	secrets, err := readRetiredSecrets(root, host)
	if err != nil {
		// Unknown, not none: the B2 half falls back to listing by name.
		fmt.Fprintf(os.Stderr, "warning: retired secrets: %v\n", err)
	}
	if msg, err := revokeB2Key(host, secrets); err != nil {
		fmt.Fprintf(os.Stderr, "warning: b2: %v — delete them by hand\n", err)
	} else {
		fmt.Println("  b2 key:", msg)
	}
}

// destroyGates: the inverse of every other verb's precondition. The
// host's manifest must be gone from origin/main (the dismantle PR
// merged) and from the working tree, the tree clean (so the state
// removal lands as its own commit), and the stack must exist — a host
// nothing raised through the CLI has none and goes by hand. With
// noStack the last gate inverts: a stack that DOES exist means the
// flag is wrong, and the pulumi path must run instead.
func destroyGates(root, host string, onMain, clean, noStack bool) (provider string, problems []string) {
	if onMain {
		problems = append(problems, "still declared on origin/main ("+manifest.Path(host)+") — merge the `fort dismantle "+host+"` PR first (this refuses to unprotect a declared host)")
	}
	if exists(root, manifest.Path(host)) {
		problems = append(problems, "still in the working tree ("+manifest.Path(host)+") — check out merged main")
	}
	if !clean {
		problems = append(problems, "working tree not clean — the state removal must land as its own commit")
	}
	state := filepath.Join(root, stackStatePath(host))
	raw, err := os.ReadFile(state)
	if noStack {
		if err == nil {
			problems = append(problems, "stack "+host+" exists in provisioning/pulumi/state — this host was raised by a backend; drop --no-stack")
		}
		return "", problems
	}
	if err != nil {
		problems = append(problems, "no stack "+host+" in provisioning/pulumi/state — never provisioned or adopted through the CLI? (a VM raised by hand goes by hand: virsh destroy + undefine on the hypervisor; a [baremetal] host never has one: fort destroy "+host+" --no-stack, docs/baremetal.md)")
		return "", problems
	}
	provider, err = providerFromState(raw)
	if err != nil {
		problems = append(problems, "checkpoint: "+err.Error())
	}
	return provider, problems
}

// providerFromState reads the provider the stack's checkpoint records
// (its pulumi:providers:<name> resource).
func providerFromState(raw []byte) (string, error) {
	var cp struct {
		Checkpoint struct {
			Latest struct {
				Resources []struct {
					Type string `json:"type"`
				} `json:"resources"`
			} `json:"latest"`
		} `json:"checkpoint"`
	}
	if err := json.Unmarshal(raw, &cp); err != nil {
		return "", err
	}
	for _, r := range cp.Checkpoint.Latest.Resources {
		if strings.HasPrefix(r.Type, "pulumi:providers:") {
			return strings.TrimPrefix(r.Type, "pulumi:providers:"), nil
		}
	}
	return "", fmt.Errorf("no pulumi:providers:* resource in the checkpoint (empty stack?)")
}

// pulumiProject is the estate's pulumi project directory, and
// stackConfigPath and stackStatePath are the two files it holds for one
// stack: the stack's config and its checkpoint under the file backend.
// `fort destroy` deletes both. `fort dismantle` only reads them, to say
// whether a stack is still there.
const pulumiProject = "provisioning/pulumi"

func stackConfigPath(host string) string {
	return filepath.Join(pulumiProject, "Pulumi."+host+".yaml")
}

func stackStatePath(host string) string {
	return filepath.Join(pulumiProject, "state/.pulumi/stacks/forts", host+".json")
}

// stateResidue: what the file backend leaves behind after stack rm.
func stateResidue(host string) []string {
	return []string{
		"state/.pulumi/stacks/forts/" + host + ".json.bak",
		"state/.pulumi/stacks/forts/" + host + ".json.bak.attrs",
		"state/.pulumi/stacks/forts/" + host + ".json.attrs",
		"state/.pulumi/history/forts/" + host,
		"state/.pulumi/backups/forts/" + host,
	}
}

// linodePowerOff shuts the instance labelled <host> down and waits for
// it, so the destroy's disk deletes go through. Absent instance = fine
// (removed by hand; refresh drops it from state).
func linodePowerOff(ctx context.Context, host string) error {
	c, err := linodego.NewClientFromEnv(nil)
	if err != nil {
		return err
	}
	list, err := c.ListInstances(ctx, linodego.NewListOptions(0, fmt.Sprintf(`{"label":%q}`, host)))
	if err != nil {
		return fmt.Errorf("listing instances: %w", err)
	}
	if len(list) == 0 {
		fmt.Println("  linode: no instance labelled", host, "(already gone — refresh will drop it)")
		return nil
	}
	inst := list[0]
	if inst.Status == linodego.InstanceOffline {
		fmt.Println("  linode: instance already offline")
		return nil
	}
	fmt.Printf("  linode: shutting %s (%d) down…\n", host, inst.ID)
	if err := c.ShutdownInstance(ctx, inst.ID); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if _, err := c.WaitForInstanceStatus(ctx, inst.ID, linodego.InstanceOffline, 300); err != nil {
		return fmt.Errorf("waiting for offline: %w", err)
	}
	fmt.Println("  linode: offline")
	return nil
}

// giteaClient is the deploy-key API as fort mint (register) and fort
// destroy (revoke) reach it: the provisioning token, the lab CA, and the
// repo — the estate's own (estate.toml `repo`), because the deploy key
// is what flux clones that repo with. The route was a constant naming
// sfi/deployments until sfi/deployments#673: the second estate's first
// mint would have put its key on the first estate's repo.
func giteaClient(root string) (*giteaDeployKeys, error) {
	est, err := estate.Load(root)
	if err != nil {
		return nil, err
	}
	api, err := est.RepoAPI()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", estate.Path, err)
	}
	base, _ := est.APIBase()
	token, err := secret(root, "gitea-token")
	if err != nil {
		return nil, fmt.Errorf("no gitea-token in provisioning/secrets.yaml: %w", err)
	}
	ca, err := os.ReadFile(filepath.Join(root, "nixos/certs/step-ca-root.pem"))
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	client := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	return &giteaDeployKeys{client: client, api: api, base: base, token: token}, nil
}

// revokeDeployKey deletes the read-only key fort mint registered (title
// flux-<host>). Not found is not an error: the key may never have
// existed (no flux cluster) or was removed by hand.
func revokeDeployKey(root, host string) (string, error) {
	g, err := giteaClient(root)
	if err != nil {
		return "", err
	}
	return revokeDeployKeyAt(g.client, g.api, g.token, host)
}

func revokeDeployKeyAt(client *http.Client, api, token, host string) (string, error) {
	do := func(method, url string) (*http.Response, error) {
		req, err := http.NewRequest(method, url, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "token "+token)
		return client.Do(req)
	}
	resp, err := do("GET", api+"/keys")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("listing deploy keys: %s", resp.Status)
	}
	var keys []struct {
		ID    int    `json:"id"`
		Title string `json:"title"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&keys); err != nil {
		return "", err
	}
	// Every key under the title (a timed-out POST that gitea recorded
	// anyway leaves duplicates; mint reports them).
	var revoked []string
	for _, k := range keys {
		if k.Title != "flux-"+host {
			continue
		}
		del, err := do("DELETE", fmt.Sprintf("%s/keys/%d", api, k.ID))
		if err != nil {
			return "", err
		}
		io.Copy(io.Discard, del.Body)
		del.Body.Close()
		if del.StatusCode/100 != 2 {
			return "", fmt.Errorf("deleting flux-%s (id %d): %s", host, k.ID, del.Status)
		}
		revoked = append(revoked, fmt.Sprint(k.ID))
	}
	if len(revoked) == 0 {
		return "flux-" + host + " not registered (nothing to revoke)", nil
	}
	return fmt.Sprintf("revoked flux-%s (id %s)", host, strings.Join(revoked, ", ")), nil
}
