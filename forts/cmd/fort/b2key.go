package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/b2"
	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

// `fort b2-key <role>` mints the B2 credentials no other verb creates:
// the fleet's two hand-held ones, a host's per-bucket writer keys (its
// restic repo, its barman lineage, its logical dumps), and the read-only
// credential a restore runs with. None of
// them can be made correctly in the B2 web console.
//
// WHY IT EXISTS. The console's key form offers a coarse permission
// picker; the capability sets this design depends on are not expressible
// there (2026-09-16). `sfi-b2-key-minting-key` must hold writeKeys and
// NOT deleteFiles; the pruning key must hold deleteFiles and nothing
// about keys at all. Getting either wrong is silent — a minting key that
// also deletes, or a pruning key that cannot, both look fine until the
// day they matter. The capability sets already exist in internal/b2 as
// the design's three roles; this is the verb that spends them.
//
// WHY IT PRINTS A SECRET, when `fort mint` never does. Its entire job is
// to hand a value to a human for their password manager: b2_create_key
// returns the secret half exactly once, and these two credentials
// deliberately live OUTSIDE the repo (docs/infra/backups.md — a
// credential that can destroy the recovery path does not sit where every
// invocation, and eventually a CI recipient, can read it). There is
// nowhere else for the value to go. Everything else here is in service
// of that being deliberate rather than casual: dry run by default, a
// refusal when the key already exists, and one loud banner.
//
// The credential it runs with is prompted for, or read from B2_KEY_ID /
// B2_APP_KEY (b2session.go). Only `minting` needs the account master key:
// B2 does not enforce capability subsetting (#291), so the minting key
// creates every other shape and the master key stays in the password
// manager after day one.

// b2Role is one credential shape this mints.
type b2Role struct {
	name    string
	summary string
	caps    []string
	// buckets restricts the key; empty = every bucket.
	buckets []string
	// prefix restricts it to one namePrefix; empty = the whole bucket.
	prefix string
	// host is set by the roles that take one: the host this credential
	// is for (writer), or the host whose repo it may read (reader).
	host string
	// kind is the word the operator typed for a host-bearing role (a
	// b2HostRoles key) and empty for the two fixed roles. It selects the
	// manifest guard, which applies to pv alone.
	kind string
	// master: only the account master key can create this one.
	master bool
	// why explains the shape, printed in the plan so the operator can
	// check the grant before it is made.
	why string
}

var b2Roles = map[string]b2Role{
	// The credential `fort mint` uses to create per-fort keys. Bucket
	// restriction would break it: minting a fort's key resolves BOTH
	// backup buckets by name, and a bucket-restricted credential lists
	// only its own.
	"minting": {
		name:    "sfi-b2-key-minting-key",
		summary: "mints per-fort keys (fort mint)",
		caps:    b2.Minting,
		master:  true,
		why: "writeKeys + deleteKeys so `fort mint` and `fort destroy` can manage fort keys;\n" +
			"    list/read/write files so it can grant them; NO deleteFiles and no bucket\n" +
			"    writes, so the credential itself cannot remove a backup object. Note that\n" +
			"    B2 does not enforce capability subsetting (#291): this is defence in depth,\n" +
			"    not a boundary — the credential can still mint one that deletes.",
	},
	// The one credential in the fleet that deletes backups. Scoped to the
	// PV bucket because that is the only layer whose retention is
	// tool-driven; barman and the logical dumps expire by lifecycle rule
	// and want no deleting credential pointed at them at all.
	"pruning": {
		name:    "sfi-b2-pruning-key",
		summary: "restic forget --prune on luxor (sfi.pv-prune)",
		caps:    b2.Pruning,
		buckets: []string{b2PVBucket},
		why: "deleteFiles, because `restic forget --prune` removes pack files of any age\n" +
			"    and no lifecycle rule can substitute. Scoped to " + b2PVBucket + " alone and to no\n" +
			"    prefix: it must reach every fort's repo, and nothing else. It holds no key\n" +
			"    capabilities, so it cannot mint its way anywhere.",
	},
	// The listing-only credential the s3-exporter on luxor scrapes B2
	// with (#323): freshness and size of every host's backups, seen from
	// the bucket rather than from the unit that claims to have written
	// them. It is the one key that legitimately stands in a Secret for
	// good, and it can afford to because it reads nothing.
	"audit": {
		name:    "sfi-b2-audit-key",
		summary: "bucket-side backup freshness (s3-exporter on luxor)",
		caps:    b2.Auditor,
		buckets: []string{b2PVBucket, b2PhysicalBucket, b2LogicalBucket},
		why: "listBuckets + listFiles on all three backup buckets, no prefix — it must see\n" +
			"    every host — and NO readFiles: names, sizes and dates only. A leak of this\n" +
			"    key shows what backups exist and when; it cannot fetch one.",
	},
}

// parseB2KeyArgs takes the role positionally and the flags after it —
// the shape every other verb uses (`fort mint <host> --write`). Go's flag
// package stops at the first non-flag argument, so parsing the whole
// slice would leave `--write` sitting as a second positional and report a
// usage error for a correct command line.
func parseB2KeyArgs(args []string) (b2Role, bool, error) {
	usage := fmt.Errorf("usage: fort b2-key <%s|%s <host>> [--write]", strings.Join(b2RoleNames(), "|"), strings.Join(b2HostRoleNames(), " <host>|"))
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return b2Role{}, false, usage
	}
	rest := args[1:]

	var role b2Role
	switch {
	case b2HostRoles[args[0]] != nil:
		// The roles that take an argument: the host the credential backs
		// up, dumps for, or whose backups it may read.
		kind := args[0]
		if len(rest) < 1 || strings.HasPrefix(rest[0], "-") {
			return b2Role{}, false, fmt.Errorf("%s needs a host: fort b2-key %s <host> [--write]", kind, kind)
		}
		host := rest[0]
		rest = rest[1:]
		role = b2HostRoles[kind](host)
		// Each role spells the name differently, so validate the one the
		// role actually chose rather than re-deriving it here.
		if err := b2.ValidName(role.name); err != nil {
			return b2Role{}, false, err
		}
	default:
		r, ok := b2Roles[args[0]]
		if !ok {
			return b2Role{}, false, fmt.Errorf("unknown role %q — one of %s, %s <host>", args[0], strings.Join(b2RoleNames(), ", "), strings.Join(b2HostRoleNames(), " <host>, "))
		}
		role = r
	}

	fs := flag.NewFlagSet("b2-key", flag.ContinueOnError)
	write := fs.Bool("write", false, "actually create the key and print its secret once")
	if err := fs.Parse(rest); err != nil {
		return b2Role{}, false, err
	}
	if fs.NArg() != 0 {
		return b2Role{}, false, fmt.Errorf("unexpected argument %q — %v", fs.Arg(0), usage)
	}
	return role, *write, nil
}

// b2HostRoles are the roles that take a host, by the word typed. Each
// word is its key's stem — `pv` mints sfi-pv-<host> on sfi-pv-backups —
// so the command, the key in b2_list_keys and the bucket it opens read
// as one thing.
var b2HostRoles = map[string]func(host string) b2Role{
	"pv":          pvRole,
	"db-physical": physicalRole,
	"db-logical":  logicalRole,
	"restore":     restoreRole,
}

// ONE KEY PER HOST PER BUCKET. b2_create_key takes bucketIds as an array
// and a host's key briefly used that to span the PV and db buckets; it
// was split again because the holders are different trust domains. The
// PV key sits in the host's own secrets, read by root for the restic
// units. The barman key is a Kubernetes Secret beside the postgres pods.
// databasus keeps its key in its own database. And "no deleteFiles" is a
// weaker boundary than it reads: overwriting an object hides the old
// version, and the bucket's daysFromHidingToDeleting then deletes it —
// so any writer can destroy what it can reach, a week late. What a key
// can reach is therefore the whole of the boundary, and an in-cluster
// credential must not reach the restic repo.

// pvRole is a host's restic credential. For a fort it is `fort mint`'s to
// create and record — hostKeySpec builds mint's key from this same role,
// so there is one definition — and this verb is for a host with no
// manifest — the pre-fort zenbook was one; none exist today.
func pvRole(host string) b2Role {
	return b2Role{
		name:    b2KeyName(host),
		host:    host,
		kind:    "pv",
		summary: "restic PV backups for " + host,
		caps:    b2.Writer,
		buckets: []string{b2PVBucket},
		prefix:  host + "/",
		why: "list/read/write on " + b2PVBucket + " alone, restricted to " + host + "/, and NO\n" +
			"    deleteFiles: pruning is luxor's job, under the one key that deletes. For a\n" +
			"    fort, `fort mint` creates this key and records its id; a key made behind\n" +
			"    mint's back is one it will refuse to shadow, so this verb refuses a host\n" +
			"    that has a manifest.",
	}
}

// physicalRole is what a host's CNPG cluster ships barman backups with.
// It lives in the cluster, not on the host, which is the reason it is not
// the PV key. Nothing writes barman to B2 yet, so nothing mints this by
// default: a credential nobody uses is only a liability.
func physicalRole(host string) b2Role {
	return b2Role{
		name:    "sfi-db-physical-" + host,
		host:    host,
		kind:    "db-physical",
		summary: "CNPG barman backups for " + host,
		caps:    b2.Writer,
		buckets: []string{b2PhysicalBucket},
		prefix:  host + "/",
		why: "list/read/write on " + b2PhysicalBucket + " alone, restricted to " + host + "/, and\n" +
			"    NO deleteFiles — drop retentionPolicy on a B2-backed cluster; the bucket's\n" +
			"    lifecycle rule expires by age. Held in-cluster beside postgres, so it\n" +
			"    reaches the barman lineage and nothing else.",
	}
}

// logicalRole is what a databasus instance writes <host>'s logical dumps
// with: sfi-db-logical-backups/<host>/<db>/… (docs/infra/backups.md,
// "Buckets"). No deleteFiles, like every writer: dumps expire by
// lifecycle rule, so databasus's own retention must be off for this
// storage or its deletes will fail.
func logicalRole(host string) b2Role {
	return b2Role{
		name:    "sfi-db-logical-" + host,
		host:    host,
		kind:    "db-logical",
		summary: "databasus logical dumps for " + host,
		caps:    b2.Writer,
		buckets: []string{b2LogicalBucket},
		prefix:  host + "/",
		why: "list/read/write on " + b2LogicalBucket + " alone, restricted to " + host + "/, and\n" +
			"    NO deleteFiles. Held by databasus in its own database, so it reaches the\n" +
			"    dumps and nothing else. Retention is the bucket's lifecycle rule — turn\n" +
			"    databasus's own retention off for this storage, its deletes would be refused.",
	}
}

// restoreRole is the credential a RESTORE runs with. It is the one grant
// that crosses both boundaries the others keep: it reads a prefix its
// holder does not own, and it spans all three buckets — a restore reads
// the PVCs, the barman lineage and the dumps, and the uniform `<host>/`
// layout makes that one prefix. Restoring zenbook's gitea onto a drill
// fort means reading `zenbook/` from a machine whose own keys may only
// touch `sfi-fort-.../`, and until this role existed no credential in the
// fleet could — the gap the 2026-09-16 restore drill found.
//
// Read-only, and scoped to the source host rather than the whole bucket:
// the credential names whose data is about to be read, and cannot write a
// byte back — a restoring host must not be able to corrupt its source.
//
// NOT refused for a host with a fort manifest. Reading a fort's repo is
// the common case: the machine that restores a dead fort is, by
// definition, not that fort. For the same reason `fort destroy` leaves it
// alone.
func restoreRole(host string) b2Role {
	return b2Role{
		name:    b2RestoreKeyName(host),
		host:    host,
		kind:    "restore",
		summary: "restoring " + host + "'s backups onto another host",
		caps:    b2.Reader,
		buckets: []string{b2PVBucket, b2PhysicalBucket, b2LogicalBucket},
		prefix:  host + "/",
		why: "list/read on all three backup buckets, restricted to " + host + "/, and NOTHING that\n" +
			"    writes: no writeFiles, no deleteFiles, no key capabilities. This is the\n" +
			"    only credential that reads a prefix other than its holder's, so it is the\n" +
			"    one to mint late, use, and revoke — not to leave standing.",
	}
}

func b2Key(args []string) error {
	role, write, err := parseB2KeyArgs(args)
	if err != nil {
		return err
	}

	// A fort's key is mint's to create: it records the id in the host's
	// secrets file and refuses to shadow a same-named key it has no
	// secret for. Hand-minting one here would produce exactly that
	// standoff, so refuse before the call rather than after.
	if role.kind == "pv" {
		root, err := repoRoot()
		if err == nil && exists(root, manifest.Path(role.host)) {
			return fmt.Errorf("%s has a fort manifest (%s) — its PV key belongs to `fort mint %s --write`, which records the id in nixos/secrets/%s.yaml. A key made here would be one mint refuses to shadow", role.host, manifest.Path(role.host), role.host, role.host)
		}
	}

	// Only the create itself needs the master key; the plan and the drift
	// report are a key listing, which the minting key does as well.
	need := needMinting
	if role.master && write {
		need = needMaster
	}
	c, err := b2Session(need, "Minting "+role.name)
	if err != nil {
		return err
	}
	ctx := context.Background()

	var ids []string
	if len(role.buckets) > 0 {
		if ids, err = resolveBuckets(ctx, c, role.buckets); err != nil {
			return err
		}
	}

	keys, err := c.ListKeys(ctx)
	if err != nil {
		return fmt.Errorf("listing keys: %w", err)
	}

	fmt.Printf("%s   %s\n", role.name, role.summary)
	fmt.Printf("  capabilities  %s\n", strings.Join(role.caps, " "))
	if len(role.buckets) > 0 {
		fmt.Printf("  buckets       %s\n", strings.Join(role.buckets, ", "))
	} else {
		fmt.Printf("  buckets       every bucket (minting resolves both backup buckets by name)\n")
	}
	if role.prefix != "" {
		fmt.Printf("  prefix        %s\n", role.prefix)
	} else {
		fmt.Printf("  prefix        none — the whole bucket\n")
	}
	fmt.Printf("\n  %s\n\n", role.why)

	// Already there: the secret half is gone for good, so this never
	// silently mints a second one wearing the same name.
	if live := b2.KeyNamed(keys, role.name); len(live) > 0 {
		for _, k := range live {
			fmt.Printf("· exists: id %s, %d capabilities%s\n", k.ApplicationKeyID, len(k.Capabilities), bucketNote(k))
			if drift := roleDrift(k, role, ids); drift != "" {
				fmt.Printf("  ! %s\n", drift)
			}
		}
		fmt.Printf(`
Nothing minted. B2 returns an application key's secret exactly once, so a
key that exists cannot be re-read — if its value is lost, or the drift
above matters, delete it in the console deliberately and re-run.
`)
		return nil
	}

	if !write {
		fmt.Println("· would mint (read-only)")
		fmt.Println("\ndry run — nothing minted (b2-key", roleArg(role), "--write).")
		return nil
	}

	key, err := c.CreateKey(ctx, role.spec(ids))
	if err != nil {
		if b2.Unauthorized(err) && !c.Master() {
			return fmt.Errorf("creating %s: %w — B2 refused this credential; retry with the account master key", role.name, err)
		}
		return fmt.Errorf("creating %s: %w", role.name, err)
	}

	fmt.Printf(`
┌─ %s ─ THE ONLY TIME THIS VALUE IS SHOWN ─
│
│  B2_KEY_ID=%s
│  B2_APP_KEY=%s
│
└─ copy it into the password manager now. B2 cannot show it again, and
   it is deliberately not written to this repo.

`, role.name, key.ApplicationKeyID, key.ApplicationKey)
	fmt.Println(role.followUp())
	return nil
}

// spec is the role as b2_create_key takes it, given its buckets' ids.
func (r b2Role) spec(bucketIDs []string) b2.KeySpec {
	return b2.KeySpec{Name: r.name, Capabilities: r.caps, BucketIDs: bucketIDs, NamePrefix: r.prefix}
}

// followUp says where the value belongs, which differs per role: one is
// exported for a raise, the other is sops-set into luxor's host secrets.
func (r b2Role) followUp() string {
	if r.kind == "db-physical" {
		return "Next: it belongs in the cluster, as the Secret " + r.host + "'s CNPG backup\n" +
			"configuration references — seeded through that tree's secrets contract, not\n" +
			"pasted. It belongs in no host's nixos secrets: the host never uses it."
	}
	if r.kind == "db-logical" {
		f := "secrets-b2-" + r.host + ".sops.yaml"
		return "Next: it belongs in luxor's databasus-central tree, as the Secret the\n" +
			"host's Storage CR references (fleet/apps/luxor/databasus-central/\n" +
			"secrets.contract.yaml) — seeded, not pasted into a UI. That file must\n" +
			"exist first (`fort seed luxor fleet/apps/luxor/databasus-central --write`), then:\n" +
			"  fort seed luxor fleet/apps/luxor/databasus-central --set " + f + "/access-key --write\n" +
			"  fort seed luxor fleet/apps/luxor/databasus-central --set " + f + "/secret-key --write\n" +
			"FILE/KEY because every host's Secret there carries the same two key names."
	}
	if r.kind == "restore" {
		return "Next: it is an OPERATIONAL INPUT like the minting key — password manager,\n" +
			"exported as B2_KEY_ID / B2_APP_KEY for the restore that needs it, and deleted\n" +
			"in the console when that restore is done. It belongs in no host's secrets:\n" +
			"nothing runs on a schedule with it, and a standing credential that can read\n" +
			"every backup of a host is a different risk from one that writes its own."
	}
	switch r.name {
	case "sfi-b2-key-minting-key":
		return "Next: export it as B2_KEY_ID / B2_APP_KEY when running `fort mint <host> --write`\n" +
			"for a fort whose manifest says [backups] profile = \"production\". Nothing else reads it."
	case "sfi-b2-audit-key":
		return "Next: it belongs in luxor's monitoring-central tree as the Secret the\n" +
			"s3-exporter reads (fleet/apps/luxor/monitoring/secrets.contract.yaml):\n" +
			"  fort seed luxor fleet/apps/luxor/monitoring --set AWS_ACCESS_KEY_ID --write\n" +
			"  fort seed luxor fleet/apps/luxor/monitoring --set AWS_SECRET_ACCESS_KEY --write"
	case "sfi-b2-pruning-key":
		return "Next: it belongs in luxor's host secrets as the env file sfi.pv-prune sources —\n" +
			"  sops --config nixos/.sops.yaml set nixos/secrets/luxor.yaml \\\n" +
			"    '[\"restic-prune-s3-env-b2\"]' \"\\\"AWS_ACCESS_KEY_ID=<id>\\nAWS_SECRET_ACCESS_KEY=<secret>\\n\\\"\"\n" +
			"and nowhere else. It is the only credential in the fleet that deletes backups."
	default:
		return "Next: it belongs in that host's own secrets as the env file its restic\n" +
			"units source (restic-s3-env-b2), and nowhere else."
	}
}

// roleDrift reports an existing key whose grant is not what this verb
// would make — the console's coarse picker is exactly how that happens.
func roleDrift(k b2.Key, role b2Role, wantBuckets []string) string {
	var missing, extra []string
	for _, want := range role.caps {
		if !hasKey(k.Capabilities, want) {
			missing = append(missing, want)
		}
	}
	for _, have := range k.Capabilities {
		if !hasKey(role.caps, have) {
			extra = append(extra, have)
		}
	}
	var notes []string
	if len(missing) > 0 {
		notes = append(notes, "missing "+strings.Join(missing, " "))
	}
	if len(extra) > 0 {
		notes = append(notes, "ALSO holds "+strings.Join(extra, " "))
	}
	if problem := bucketDrift(k.BucketIDs, wantBuckets); problem != "" {
		notes = append(notes, problem)
	}
	have := ""
	if k.NamePrefix != nil {
		have = *k.NamePrefix
	}
	if have != role.prefix {
		want := role.prefix
		if want == "" {
			want = "none"
		}
		notes = append(notes, fmt.Sprintf("namePrefix %q, want %s", have, want))
	}
	if len(notes) == 0 {
		return ""
	}
	return "grant differs from this role: " + strings.Join(notes, "; ")
}

func bucketNote(k b2.Key) string {
	if len(k.BucketIDs) == 0 {
		return ", every bucket"
	}
	return fmt.Sprintf(", %d bucket(s)", len(k.BucketIDs))
}

func b2HostRoleNames() []string {
	names := make([]string, 0, len(b2HostRoles))
	for n := range b2HostRoles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func b2RoleNames() []string {
	names := make([]string, 0, len(b2Roles))
	for n := range b2Roles {
		names = append(names, n)
	}
	// Stable for the usage string, whatever the map iteration order and
	// however many fixed roles there come to be.
	sort.Strings(names)
	return names
}

// roleArg is the word the operator typed, recovered from the role for
// the dry-run hint.
func roleArg(role b2Role) string {
	for name, r := range b2Roles {
		if r.name == role.name {
			return name
		}
	}
	// A host-bearing role is two words, and the dry-run hint has to be
	// copy-pasteable or it is worse than nothing.
	if role.kind != "" {
		return role.kind + " " + role.host
	}
	return "<role>"
}
