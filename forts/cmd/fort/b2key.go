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
// the fleet's two hand-held ones, a backup credential for a host that
// has no fort manifest, and the read-only credential a restore runs
// with. None of them can be made correctly in the B2 web console.
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
// The master key is the operational input, the same B2_KEY_ID /
// B2_APP_KEY pair every B2-touching verb reads. It is needed for exactly
// this command and then goes back in the password manager.

// b2Role is one of the two credentials this mints.
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
	// kind is the word the operator typed for a host-bearing role —
	// "writer" or "reader" — and empty for the two fixed roles. It
	// selects the manifest guard, which applies to writer alone.
	kind string
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
}

// parseB2KeyArgs takes the role positionally and the flags after it —
// the shape every other verb uses (`fort mint <host> --write`). Go's flag
// package stops at the first non-flag argument, so parsing the whole
// slice would leave `--write` sitting as a second positional and report a
// usage error for a correct command line.
func parseB2KeyArgs(args []string) (b2Role, bool, error) {
	usage := fmt.Errorf("usage: fort b2-key <%s|writer <host>|reader <host>> [--write]", strings.Join(b2RoleNames(), "|"))
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return b2Role{}, false, usage
	}
	rest := args[1:]

	var role b2Role
	switch {
	case args[0] == "writer", args[0] == "reader":
		// The two roles that take an argument: the host the credential
		// backs up, or the host whose backups it may read.
		kind := args[0]
		if len(rest) < 1 || strings.HasPrefix(rest[0], "-") {
			return b2Role{}, false, fmt.Errorf("%s needs a host: fort b2-key %s <host> [--write]", kind, kind)
		}
		host := rest[0]
		rest = rest[1:]
		if kind == "writer" {
			role = writerRole(host)
		} else {
			role = readerRole(host)
		}
		// The two roles spell the name differently, so validate the one
		// the role actually chose rather than re-deriving it here.
		if err := b2.ValidName(role.name); err != nil {
			return b2Role{}, false, err
		}
	default:
		r, ok := b2Roles[args[0]]
		if !ok {
			return b2Role{}, false, fmt.Errorf("unknown role %q — one of %s, writer <host>, reader <host>", args[0], strings.Join(b2RoleNames(), ", "))
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

// writerRole is a backup credential for a host that is NOT a fort —
// zenbook today, any hand-managed mesh member later. The grant is
// hostKeySpec's, the same one `fort mint` gives a fort, so a key made
// here and a key made there are indistinguishable and the mint-time
// guard accepts either.
func writerRole(host string) b2Role {
	return b2Role{
		name:    b2KeyName(host),
		host:    host,
		kind:    "writer",
		summary: "backups for " + host + " (a host with no fort manifest)",
		caps:    b2.Writer,
		buckets: []string{b2PVBucket, b2DBBucket},
		prefix:  host + "/",
		why: "list/read/write on both backup buckets, restricted to " + host + "/, and NO\n" +
			"    deleteFiles — the same grant `fort mint` gives a fort, so a compromised\n" +
			"    host can write its own backups and erase nobody's. Use this only for a\n" +
			"    host WITHOUT a manifest: for a fort, `fort mint` creates the key and\n" +
			"    records its id, and a key made behind mint's back is one it will refuse\n" +
			"    to shadow.",
	}
}

// readerRole is the credential a RESTORE runs with. It is the one grant
// in the design that deliberately crosses a prefix boundary: every other
// key is scoped to the host holding it, which is correct for writing and
// useless for recovery. Restoring zenbook's gitea onto a drill fort means
// reading `zenbook/` from a machine whose own key may only touch
// `sfi-fort-.../`, and until this role existed there was no credential in
// the fleet that could do it — the gap the 2026-09-16 restore drill found.
//
// Read-only, and scoped to the source host rather than the whole bucket,
// because a restore reads one host's repo: the credential names whose
// data is about to be read, and cannot write a byte back into the repo it
// is reading — a restoring host must not be able to corrupt its source.
//
// Unlike writer, this role is NOT refused for a host with a fort
// manifest. Reading a fort's repo is the common case: the machine that
// restores a dead fort is, by definition, not that fort.
func readerRole(host string) b2Role {
	return b2Role{
		name:    b2RestoreKeyName(host),
		host:    host,
		kind:    "reader",
		summary: "restoring " + host + "'s backups onto another host",
		caps:    b2.Reader,
		buckets: []string{b2PVBucket, b2DBBucket},
		prefix:  host + "/",
		why: "list/read on both backup buckets, restricted to " + host + "/, and NOTHING that\n" +
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
	if role.kind == "writer" {
		root, err := repoRoot()
		if err == nil && exists(root, manifest.Path(role.host)) {
			return fmt.Errorf("%s has a fort manifest (%s) — its B2 key belongs to `fort mint %s --write`, which records the id in nixos/secrets/%s.yaml. A key made here would be one mint refuses to shadow", role.host, manifest.Path(role.host), role.host, role.host)
		}
	}

	c, err := b2Session()
	if err != nil {
		return fmt.Errorf("%w\n\nThis verb needs the ACCOUNT MASTER key, which is the one credential that\ncan create these. Export it for this command and put it back afterwards", err)
	}
	ctx := context.Background()

	var ids []string
	if len(role.buckets) > 0 {
		buckets, err := c.ListBuckets(ctx)
		if err != nil {
			return fmt.Errorf("listing buckets: %w", err)
		}
		for _, want := range role.buckets {
			id := ""
			for _, b := range buckets {
				if b.Name == want {
					id = b.ID
				}
			}
			if id == "" {
				return fmt.Errorf("no bucket named %s — create the backup buckets first (docs/infra/backups.md)", want)
			}
			ids = append(ids, id)
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

	key, err := c.CreateKey(ctx, b2.KeySpec{Name: role.name, Capabilities: role.caps, BucketIDs: ids, NamePrefix: role.prefix})
	if err != nil {
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

// followUp says where the value belongs, which differs per role: one is
// exported for a raise, the other is sops-set into luxor's host secrets.
func (r b2Role) followUp() string {
	if r.kind == "reader" {
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
	if len(k.BucketIDs) != len(wantBuckets) {
		notes = append(notes, fmt.Sprintf("restricted to %d bucket(s), want %d", len(k.BucketIDs), len(wantBuckets)))
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
