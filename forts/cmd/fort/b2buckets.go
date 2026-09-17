package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/b2"
)

// `fort b2-buckets` reconciles the account's backup buckets with the
// declaration below: each one exists, is private, and carries exactly the
// lifecycle rules docs/infra/backups.md gives it.
//
// WHY IT EXISTS. The B2 console offers lifecycle as three presets ("keep
// all versions", "keep only the last version", "keep prior versions for N
// days") over two raw fields that sound alike and do opposite things:
//
//	daysFromHidingToDeleting   reclaims versions that are ALREADY deleted.
//	                           Without it B2 bills forever for everything
//	                           `restic prune` believes it removed.
//	daysFromUploadingToHiding  expires LIVE objects by age. It is how an
//	                           append-only layer gets retention without a
//	                           deleting credential — and it shreds a
//	                           restic repo, whose old packs are still
//	                           referenced by new snapshots.
//
// Which preset sets which is not something to work out per bucket in a
// web form. A bucket made there with the defaults has NO rules, which is
// safe and unbounded; this verb is what gives it the right ones.
//
// Rules are replaced wholesale and each bucket gets ONE rule with an
// empty prefix: B2 accepts overlapping rules without saying which governs
// (#291), so the partition is ours to keep, and one rule cannot overlap.

const (
	// The undo window: how long a deleted or overwritten object stays
	// recoverable as a hidden version. The top of the doc's 1–7d range,
	// because on the restic bucket this is the only thing standing
	// between a bad prune and the data.
	b2HiddenDays = 7
	// The B2 tier's window for the append-only layers (the "90d+" in the
	// Buckets table).
	b2DBRetentionDays = 90
)

type bucketDecl struct {
	name  string
	rules []b2.LifecycleRule
	why   string
}

var b2Buckets = []bucketDecl{
	{
		name:  b2PVBucket,
		rules: []b2.LifecycleRule{{DaysFromHidingToDeleting: b2.Days(b2HiddenDays)}},
		why: "restic repos. Retention is `restic forget --prune` and nothing else, so NO\n" +
			"    age-based expiry — that would remove packs live snapshots reference. Hidden\n" +
			"    versions go after " + fmt.Sprint(b2HiddenDays) + "d, or prune never saves a byte.",
	},
	{
		name:  b2PhysicalBucket,
		rules: []b2.LifecycleRule{{DaysFromUploadingToHiding: b2.Days(b2DBRetentionDays), DaysFromHidingToDeleting: b2.Days(b2HiddenDays)}},
		why: "barman lineage, append-only from the fort's side: no key here deletes, so age\n" +
			"    IS the retention. Safe while base backups are nightly and the window is\n" +
			"    " + fmt.Sprint(b2DBRetentionDays) + "d; the WAL-expires-with-its-base assumption is the restore drill's to test.",
	},
	{
		name:  b2LogicalBucket,
		rules: []b2.LifecycleRule{{DaysFromUploadingToHiding: b2.Days(b2DBRetentionDays), DaysFromHidingToDeleting: b2.Days(b2HiddenDays)}},
		why: "databasus dumps, each one self-contained, written by keys without deleteFiles:\n" +
			"    age is the retention here too. A host whose dumps STOP loses its last one\n" +
			"    after " + fmt.Sprint(b2DBRetentionDays) + "d — that is an alert's job to catch, not this rule's.",
	},
}

// bucketAction is what one declared bucket needs. create and fix are
// mutually exclusive; both empty means it already matches.
type bucketAction struct {
	decl   bucketDecl
	live   *b2.Bucket
	create bool
	// drift names each way the live bucket differs, for the plan.
	drift []string
}

func (a bucketAction) changes() bool { return a.create || len(a.drift) > 0 }

// planBuckets compares the declaration with the account. The second
// result is every bucket the declaration does not know — reported, never
// touched: app content and the probe's scratch bucket live here too.
func planBuckets(live []b2.Bucket, want []bucketDecl) ([]bucketAction, []b2.Bucket) {
	var out []bucketAction
	known := map[string]bool{}
	for _, d := range want {
		known[d.name] = true
		a := bucketAction{decl: d}
		for i := range live {
			if live[i].Name == d.name {
				a.live = &live[i]
			}
		}
		switch {
		case a.live == nil:
			a.create = true
		default:
			if a.live.Type != b2.Private {
				a.drift = append(a.drift, fmt.Sprintf("bucketType %s, want %s — a backup bucket is never public", a.live.Type, b2.Private))
			}
			if have, wantRules := rulesText(a.live.LifecycleRules), rulesText(d.rules); have != wantRules {
				a.drift = append(a.drift, fmt.Sprintf("lifecycle %s, want %s", have, wantRules))
			}
		}
		out = append(out, a)
	}
	var strangers []b2.Bucket
	for _, b := range live {
		if !known[b.Name] {
			strangers = append(strangers, b)
		}
	}
	return out, strangers
}

// rulesText renders rules in the API's own field names — the point of the
// verb is that these, not the console's presets, are what gets read.
func rulesText(rules []b2.LifecycleRule) string {
	if len(rules) == 0 {
		return "none (keep every version forever)"
	}
	var parts []string
	for _, r := range rules {
		prefix := r.FileNamePrefix
		if prefix == "" {
			prefix = "*"
		}
		parts = append(parts, fmt.Sprintf("[%s daysFromUploadingToHiding=%s daysFromHidingToDeleting=%s]", prefix, days(r.DaysFromUploadingToHiding), days(r.DaysFromHidingToDeleting)))
	}
	return strings.Join(parts, " ")
}

func days(n *int) string {
	if n == nil {
		return "unset"
	}
	return fmt.Sprint(*n)
}

func b2BucketsCmd(args []string) error {
	fs := flag.NewFlagSet("b2-buckets", flag.ContinueOnError)
	write := fs.Bool("write", false, "create missing buckets and set type + lifecycle rules")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected argument %q — usage: fort b2-buckets [--write]", fs.Arg(0))
	}

	// The plan is a bucket listing, which the minting key does; changing
	// a bucket takes writeBuckets, which it deliberately does not hold.
	need := needMinting
	if *write {
		need = needMaster
	}
	c, err := b2Session(need, "Reconciling the backup buckets")
	if err != nil {
		return err
	}
	ctx := context.Background()
	live, err := c.ListBuckets(ctx)
	if err != nil {
		return fmt.Errorf("listing buckets: %w", err)
	}
	actions, strangers := planBuckets(live, b2Buckets)

	pending := 0
	for _, a := range actions {
		fmt.Printf("%s\n  lifecycle     %s\n\n  %s\n\n", a.decl.name, rulesText(a.decl.rules), a.decl.why)
		switch {
		case a.create && !*write:
			fmt.Println("· missing — would create")
		case a.create:
			if _, err := c.CreateBucket(ctx, a.decl.name, a.decl.rules); err != nil {
				return fmt.Errorf("creating %s: %w", a.decl.name, err)
			}
			fmt.Println("● created")
		case len(a.drift) == 0:
			fmt.Println("· matches")
		default:
			for _, d := range a.drift {
				fmt.Printf("  ! %s\n", d)
			}
			if *write {
				if err := c.SetBucket(ctx, a.live.ID, a.decl.rules); err != nil {
					return fmt.Errorf("updating %s: %w", a.decl.name, err)
				}
				fmt.Println("○ set")
			} else {
				fmt.Println("· would set")
			}
		}
		fmt.Println()
		if a.changes() {
			pending++
		}
	}

	if len(strangers) > 0 {
		fmt.Println("not declared here (reported, never touched):")
		for _, b := range strangers {
			fmt.Printf("  %s   %s, lifecycle %s\n", b.Name, b.Type, rulesText(b.LifecycleRules))
		}
		fmt.Println()
	}

	switch {
	case pending == 0:
		fmt.Println("nothing to do.")
	case !*write:
		fmt.Printf("dry run — %d bucket(s) would change (b2-buckets --write, with the account master key).\n", pending)
	}
	return nil
}
