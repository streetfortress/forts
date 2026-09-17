package main

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/b2"
	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

// The backup half of `fort mint` (docs/infra/backups.md). A fort whose
// manifest declares a [backups] profile carries the credentials
// `sfi.pv-backup` reads, and where each comes from is the design:
//
//	restic-password        FLEET secret, copied from provisioning/secrets.yaml.
//	                       The central pruner opens every fort's repo, so
//	                       per-host passwords would mean a keyring on
//	                       luxor. Never generated per host.
//	restic-s3-env-rustfs   built from the rustfs credentials in
//	                       provisioning/secrets.yaml (the instance's root
//	                       pair today — per-consumer rustfs IAM is the
//	                       `s3` provider's own piece of work).
//	restic-s3-env-b2       MINTED against the B2 API, production profile
//	                       only, from a credential supplied per-invocation.
//
// That last one is the shape the probe (#47/#291) settled. B2 does not
// enforce capability subsetting — a key holding writeKeys can mint one
// holding deleteFiles, and that key really deletes — so the minting
// credential is a delete-capable credential one call away and does NOT
// live in provisioning/secrets.yaml beside the others. It is an
// operational input: from the password manager, for the moment of a raise
// and no longer — prompted for, or B2_KEY_ID / B2_APP_KEY (b2session.go).
const (
	keyResticPassword = "restic-password"
	keyResticRustfs   = "restic-s3-env-rustfs"
	keyResticB2       = "restic-s3-env-b2"

	// Buckets the fort's one key covers. b2_create_key takes bucketIds as
	// an array, so a fort needs one credential rather than one per
	// bucket; the db bucket is included now because the barman plugin
	// migration will write there under the same key.
	b2PVBucket       = "sfi-pv-backups"
	b2PhysicalBucket = "sfi-db-physical-backups"
	// Logical dumps get a bucket of their own, laid out <host>/… like the
	// other two. NOT part of a host's key: databasus writes here, under
	// logicalRole's key, and a host's key stays unable to.
	b2LogicalBucket = "sfi-db-logical-backups"

	// luxor's backup-destination instance (docs/infra/providers.md).
	// S3 is a per-fort battery; what is particular is luxor's ROLE as
	// the hub, so a fort registers as a tenant of a service ANOTHER
	// fort hosts, and its identity is declared in that fort's tree.
	// mint appends, dismantle removes, `fort check` traces both ways.
	rustfsUsers = "fleet/apps/luxor/rustfs-central/users.sops.yaml"
)

// rustfsPVUser is a fort's restic identity on luxor's rustfs. The name is
// also its access key: it reads as itself in `mc admin user list`, in an
// audit, and in the repository URL that uses it.
func rustfsPVUser(host string) string { return "fort-" + host + "-pv" }

// b2KeyName is the fort's application key. Letters, digits and hyphens
// only — B2's own keyName rule, which `fort-<host>` satisfies and an
// underscore would not.
func b2KeyName(host string) string { return "fort-" + host }

// b2RestoreKeyName is the read-only key for reading <host>'s repo from
// somewhere that is not <host>. Deliberately NOT `fort-<host>`: a restore
// credential and a fort's own backup credential must never be mistaken
// for one another in `b2_list_keys`, which is where an audit looks.
func b2RestoreKeyName(host string) string { return "restore-" + host }

// s3Env is the environment file the restic units source.
func s3Env(id, secret string) string {
	return fmt.Sprintf("AWS_ACCESS_KEY_ID=%s\nAWS_SECRET_ACCESS_KEY=%s\n", id, secret)
}

// envKeyID reads AWS_ACCESS_KEY_ID back out of a stored env file. For B2
// that value IS the application key id, which is what makes the
// re-mint guard exact: a name alone cannot tell a live key from a stale
// one, and the secret half is never readable again.
func envKeyID(env string) string {
	for _, line := range strings.Split(env, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "AWS_ACCESS_KEY_ID="); ok {
			return v
		}
	}
	return ""
}

// planBackups fills the backup credentials a profile implies. Values
// that already exist in the secrets file are restored, never re-minted —
// rotation stays a deliberate act, as with every other key here.
func (p *mintPlan) planBackups(take func(string) (string, bool)) error {
	if !p.entry.Backups.Backed() {
		return nil
	}
	if v, ok := take(keyResticPassword); ok {
		p.resticPassword = v
	} else {
		v, err := secret(p.root, keyResticPassword)
		if err != nil || v == "" {
			return fmt.Errorf("no %s in provisioning/secrets.yaml — the restic repo password is a FLEET secret (the central pruner opens every fort's repo), minted once and copied into each host's file. Create it with:\n  sops --config .sops.yaml set provisioning/secrets.yaml '[\"%s\"]' \"\\\"$(openssl rand -base64 32)\\\"\"", keyResticPassword, keyResticPassword)
		}
		p.resticPassword = v
	}

	// The fort's OWN rustfs identity, not the instance root pair every
	// other consumer still shares (the audit in docs/infra/backups.md).
	// Scoped by the IAM reconciler to sfi-pv-backups/<host>/* with no
	// s3:DeleteObject, so a compromised fort can neither read another
	// fort's repo nor erase its own — the posture B2 already has, and
	// the precondition for the fleet-wide restic password being sound.
	if v, ok := take(keyResticRustfs); ok {
		p.resticRustfs = v
		p.rustfsUser = envKeyID(v)
	} else {
		if !exists(p.root, rustfsUsers) {
			return fmt.Errorf("%s does not exist — seed luxor's rustfs provider first: fort seed luxor fleet/apps/luxor/rustfs-central --write", rustfsUsers)
		}
		sec, err := newToken()
		if err != nil {
			return err
		}
		p.rustfsUser, p.rustfsSecret = rustfsPVUser(p.host), sec
		p.resticRustfs = s3Env(p.rustfsUser, sec)
	}

	if p.entry.Backups.Profile != "production" {
		return nil
	}
	if v, ok := take(keyResticB2); ok {
		p.resticB2 = v
	}
	return p.planB2Key()
}

// planB2Key is the read-only half: it reports the fort's B2 key standing
// without writing, exactly as deployKeyStatus does for gitea. The
// difference is that a B2 key has no public half to compare, so the
// comparison is the recorded key id against `b2_list_keys` — which also
// checks the RESTRICTIONS, catching a key edited in the console into
// holding deleteFiles.
func (p *mintPlan) planB2Key() error {
	recorded := envKeyID(p.resticB2)
	name := b2KeyName(p.host)

	// Without the credential in the environment, only a --write that has
	// a key to mint is worth stopping to ask for one.
	if !b2EnvSet() {
		switch {
		case recorded != "":
			// Nothing to mint, so the credential is optional: say the
			// check was skipped rather than nagging a day-2 re-mint.
			p.b2Status = fmt.Sprintf("%s recorded (id %s) — NOT verified: B2_KEY_ID / B2_APP_KEY unset", name, recorded)
			return nil
		case !p.write:
			// A dry run is where you read the plan BEFORE deciding to
			// hand over a credential that can mint a key which deletes
			// backups. Asking for it to plan inverts that.
			p.b2Status, p.b2Mint = "would mint — NOT verified (B2_KEY_ID / B2_APP_KEY unset; --write will ask for them)", true
			return nil
		}
	}
	c, err := b2Session(needMinting, "Minting "+name)
	if err != nil {
		return fmt.Errorf("minting %s needs B2 credentials: %w", name, err)
	}

	keys, err := c.ListKeys(context.Background())
	if err != nil {
		return fmt.Errorf("listing B2 keys: %w", err)
	}
	live := b2.KeyNamed(keys, name)

	switch {
	case recorded == "" && len(live) == 0:
		p.b2Status, p.b2Mint = "would mint (read-only)", true
	case recorded == "":
		// A key wearing this fort's name whose secret the repo does not
		// have. Minting a second one would leave the first live and
		// unaccounted for — the deploy key's stale-key refusal.
		return fmt.Errorf("B2 already has a key named %s (id %s) and %s holds no secret for it — the secret half is unrecoverable, so delete that key deliberately, then re-run", name, ids(live), p.secretsPath)
	case len(live) == 0:
		return fmt.Errorf("%s records B2 key %s (id %s) but no such key exists on the account — it was revoked out of band; remove the key from the secrets file to mint a fresh one", p.secretsPath, name, recorded)
	default:
		var found *b2.Key
		for i := range live {
			if live[i].ApplicationKeyID == recorded {
				found = &live[i]
			}
		}
		if found == nil {
			return fmt.Errorf("B2 key %s exists (id %s) but is not the one %s records (id %s) — delete the stale key deliberately, then re-run", name, ids(live), p.secretsPath, recorded)
		}
		// By id, not by count: B2 cannot rename a bucket, so a key minted
		// before a bucket was replaced still names two — the wrong two.
		want, err := resolveBuckets(context.Background(), c, writerRole(p.host).buckets)
		if err != nil {
			return err
		}
		if problem := b2Restrictions(*found, p.host, want); problem != "" {
			return fmt.Errorf("B2 key %s (id %s) %s — the authority drifted from what mint grants; delete it and re-run to mint a correct one", name, recorded, problem)
		}
		p.b2Status = fmt.Sprintf("%s registered (id %s, restrictions verified)", name, recorded)
	}
	return nil
}

// b2Restrictions checks a live key against what mint grants: both backup
// buckets, this fort's prefix, and no deleteFiles. Returns "" when it
// matches.
func b2Restrictions(k b2.Key, host string, wantBuckets []string) string {
	if problem := bucketDrift(k.BucketIDs, wantBuckets); problem != "" {
		return "is " + problem
	}
	want := host + "/"
	if k.NamePrefix == nil || *k.NamePrefix != want {
		return fmt.Sprintf("has namePrefix %q, want %q", deref(k.NamePrefix), want)
	}
	for _, c := range k.Capabilities {
		if c == "deleteFiles" {
			return "holds deleteFiles, which a fort's key must never have"
		}
	}
	return ""
}

// resolveBuckets maps the backup buckets' names to their ids.
func resolveBuckets(ctx context.Context, c *b2.Client, want []string) ([]string, error) {
	buckets, err := c.ListBuckets(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing B2 buckets: %w", err)
	}
	var ids []string
	for _, name := range want {
		id := ""
		for _, b := range buckets {
			if b.Name == name {
				id = b.ID
			}
		}
		if id == "" {
			return nil, fmt.Errorf("no B2 bucket named %s — create the backup buckets first (docs/infra/backups.md)", name)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// hostKeySpec is THE grant a host's backup credential gets: both backup
// buckets, its own prefix, and no deleteFiles so it cannot erase the
// offsite copy. The shape itself is writerRole's — `fort mint` creates it
// for forts, `fort b2-key writer` for hosts that are not forts, and
// b2Restrictions checks live keys against it, so it is spelled once.
func hostKeySpec(ctx context.Context, c *b2.Client, host string) (b2.KeySpec, error) {
	role := writerRole(host)
	ids, err := resolveBuckets(ctx, c, role.buckets)
	if err != nil {
		return b2.KeySpec{}, err
	}
	return role.spec(ids), nil
}

// mintB2Key is the write half, run from apply before the secrets file is
// composed — the secret comes back exactly once, so it must reach
// p.resticB2 before anything renders it.
func (p *mintPlan) mintB2Key() error {
	c, err := b2Session(needMinting, "Minting "+b2KeyName(p.host))
	if err != nil {
		return err
	}
	ctx := context.Background()
	spec, err := hostKeySpec(ctx, c, p.host)
	if err != nil {
		return err
	}

	key, err := c.CreateKey(ctx, spec)
	if err != nil {
		return fmt.Errorf("minting %s: %w", b2KeyName(p.host), err)
	}
	p.resticB2 = s3Env(key.ApplicationKeyID, key.ApplicationKey)
	p.b2Status, p.b2Mint = fmt.Sprintf("%s minted (id %s)", key.KeyName, key.ApplicationKeyID), false
	return nil
}

// bucketDrift compares a live key's bucket restriction with the ids a
// role resolves to today. "" when they are the same set.
func bucketDrift(have, want []string) string {
	if len(have) != len(want) {
		return fmt.Sprintf("restricted to %d bucket(s), want %d", len(have), len(want))
	}
	for _, id := range have {
		if !hasKey(want, id) {
			return fmt.Sprintf("restricted to bucket id %s, which is none of %s — minted before a bucket was replaced?", id, strings.Join(want, ", "))
		}
	}
	return ""
}

func ids(keys []b2.Key) string {
	var out []string
	for _, k := range keys {
		out = append(out, k.ApplicationKeyID)
	}
	return strings.Join(out, ", ")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// revokeB2Key deletes the fort's application key; `fort destroy` calls it
// beside revokeDeployKey. It takes only the host name on purpose: by
// destroy time the manifest is gone (that absence is the gate), so the
// profile cannot be consulted. Listing by name covers every case — a lab
// fort simply has no such key, and a fort whose profile changed still
// gets the key it actually has revoked.
//
// Absent credentials are reported, not fatal: a destroy must not be
// blocked on an operational input, and the caller prints what is left.
func revokeB2Key(host string) (string, error) {
	c, err := b2Session(needMinting, "Revoking "+b2KeyName(host))
	if err != nil {
		return "", fmt.Errorf("B2 key %s NOT revoked: %w", b2KeyName(host), err)
	}
	ctx := context.Background()
	keys, err := c.ListKeys(ctx)
	if err != nil {
		return "", err
	}
	live := b2.KeyNamed(keys, b2KeyName(host))
	if len(live) == 0 {
		return b2KeyName(host) + " not registered (nothing to revoke)", nil
	}
	var revoked []string
	for _, k := range live {
		if _, err := c.DeleteKey(ctx, k.ApplicationKeyID); err != nil {
			return "", fmt.Errorf("revoking %s (id %s): %w", k.KeyName, k.ApplicationKeyID, err)
		}
		revoked = append(revoked, k.ApplicationKeyID)
	}
	return fmt.Sprintf("revoked %s (id %s)", b2KeyName(host), strings.Join(revoked, ", ")), nil
}

// backupDestinations is the one-line reminder of what a profile means,
// for mint's report: the manifest says intent, and the report says where
// that lands.
func backupDestinations(profile string) string {
	switch profile {
	case "production":
		return "luxor's rustfs AND B2 (offsite does not depend on the lab)"
	case "lab":
		return "luxor's rustfs only (reproducible; not an original)"
	}
	return "nothing"
}

// secretHasKey reports whether a committed sops Secret carries a key.
// Key NAMES are plaintext under the repo's encrypted_regex, so this
// needs no age key — the same property `fort check` runs on in CI.
func secretHasKey(root, rel, key string) bool {
	names, err := secretKeyNames(filepath.Join(root, rel))
	if err != nil {
		return false
	}
	return hasKey(names, key)
}

// rustfsIdentityFindings traces BOTH ways the registration of a fort
// with luxor's rustfs-central (docs/infra/providers.md — a service one
// fort hosts for the others, so the declaration is in luxor's tree):
//
//	forward   a fort that backs up has an identity        → else a WARNING
//	reverse   an identity belongs to a fort that exists   → else an ERROR
//
// The asymmetry is deliberate. A fort that declares a profile and has no
// identity yet is an unfinished raise: visible, harmless, and cleared by
// `fort mint`. An ORPHANED identity is a live credential scoped to a
// prefix whose backups are still sitting there — that is the direction
// that earns the cross-fort write, and the one that must fail CI.
func rustfsIdentityFindings(root string, hosts map[string]manifest.Host) []finding {
	var out []finding
	if !exists(root, rustfsUsers) {
		for host, h := range hosts {
			if h.Backups.Backed() {
				return []finding{{"warn", rustfsUsers, "missing, but " + host + " backs up — seed luxor's rustfs provider: fort seed luxor fleet/apps/luxor/rustfs-central --write"}}
			}
		}
		return nil
	}
	names, err := secretKeyNames(filepath.Join(root, rustfsUsers))
	if err != nil {
		return []finding{{"error", rustfsUsers, "unreadable: " + err.Error()}}
	}

	for host, h := range hosts {
		if h.Backups.Backed() && !hasKey(names, rustfsPVUser(host)) {
			out = append(out, finding{"warning", rustfsUsers, "no identity " + rustfsPVUser(host) + " yet — " + host + " declares [backups] profile " + h.Backups.Profile + "; fort mint adds it (until then the fort cannot reach rustfs)"})
		}
	}
	for _, name := range names {
		host, ok := fortOfPVUser(name)
		if !ok {
			continue // `prune`, or an identity with its own lifecycle
		}
		if h, declared := hosts[host]; !declared {
			out = append(out, finding{"error", rustfsUsers, "ORPHANED identity " + name + ": no manifest for " + host + " — a live credential for a fort that no longer exists (fort dismantle withdraws it)"})
		} else if !h.Backups.Backed() {
			out = append(out, finding{"error", rustfsUsers, "identity " + name + " but " + host + " declares [backups] profile \"none\" — one of the two is wrong"})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].msg < out[j].msg })
	return out
}

// fortOfPVUser is rustfsPVUser's inverse.
func fortOfPVUser(name string) (string, bool) {
	host, ok := strings.CutPrefix(name, "fort-")
	if !ok {
		return "", false
	}
	host, ok = strings.CutSuffix(host, "-pv")
	if !ok || host == "" {
		return "", false
	}
	return host, true
}
