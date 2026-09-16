package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/b2"
)

// `fort b2-key <role>` mints the fleet's two hand-held B2 credentials —
// the ones no verb may create for itself, and that the B2 web console
// cannot create correctly.
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
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return b2Role{}, false, fmt.Errorf("usage: fort b2-key <%s> [--write]", strings.Join(b2RoleNames(), "|"))
	}
	role, ok := b2Roles[args[0]]
	if !ok {
		return b2Role{}, false, fmt.Errorf("unknown role %q — one of %s", args[0], strings.Join(b2RoleNames(), ", "))
	}
	fs := flag.NewFlagSet("b2-key", flag.ContinueOnError)
	write := fs.Bool("write", false, "actually create the key and print its secret once")
	if err := fs.Parse(args[1:]); err != nil {
		return b2Role{}, false, err
	}
	if fs.NArg() != 0 {
		return b2Role{}, false, fmt.Errorf("unexpected argument %q — usage: fort b2-key <%s> [--write]", fs.Arg(0), strings.Join(b2RoleNames(), "|"))
	}
	return role, *write, nil
}

func b2Key(args []string) error {
	role, write, err := parseB2KeyArgs(args)
	if err != nil {
		return err
	}

	c, err := b2Session()
	if err != nil {
		return fmt.Errorf("%w\n\nThis verb needs the ACCOUNT MASTER key, which is the one credential that\ncan create these two. Export it for this command and put it back afterwards", err)
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
	fmt.Printf("  prefix        none — the whole bucket\n")
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

	key, err := c.CreateKey(ctx, b2.KeySpec{Name: role.name, Capabilities: role.caps, BucketIDs: ids})
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
	switch r.name {
	case "sfi-b2-key-minting-key":
		return "Next: export it as B2_KEY_ID / B2_APP_KEY when running `fort mint <host> --write`\n" +
			"for a fort whose manifest says [backups] profile = \"production\". Nothing else reads it."
	default:
		return "Next: it belongs in luxor's host secrets as the env file sfi.pv-prune sources —\n" +
			"  sops --config nixos/.sops.yaml set nixos/secrets/luxor.yaml \\\n" +
			"    '[\"restic-prune-s3-env-b2\"]' \"\\\"AWS_ACCESS_KEY_ID=<id>\\nAWS_SECRET_ACCESS_KEY=<secret>\\n\\\"\"\n" +
			"and nowhere else. It is the only credential in the fleet that deletes backups."
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
	if k.NamePrefix != nil && *k.NamePrefix != "" {
		notes = append(notes, fmt.Sprintf("namePrefix %q, want none", *k.NamePrefix))
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
	// Stable for the usage string.
	if len(names) == 2 && names[0] == "pruning" {
		names[0], names[1] = names[1], names[0]
	}
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
	return "<role>"
}
