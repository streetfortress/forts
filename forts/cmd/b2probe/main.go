// Command b2probe vets the assumptions docs/infra/backups.md's "Minting
// keys — what the CLI owns" section rests on, against a real B2 account,
// before any of it is built into `fort mint`.
//
// Three of them are not answerable from the API reference, and all three
// decide the shape:
//
//  1. May a NON-MASTER key call b2_create_key? If not, the credential in
//     provisioning/secrets.yaml would have to be the master key — one that
//     can wipe every backup — and the design flips to mint printing the
//     call for a human or a runner to make.
//  2. Must a new key's capabilities be a SUBSET of the creating key's? The
//     whole "the minting credential cannot grant deleteFiles" posture is
//     this one rule. If it does not hold, the laptop's key can mint a key
//     that erases the offsite copy, and only the master key's absence is
//     protecting anything.
//  3. Does bucketIds really take an ARRAY over the API? v4 says so, and it
//     is why a fort needs one credential rather than one per bucket.
//
// It also proves the guard mint will use (b2_list_keys echoes the
// restrictions, so a recorded keyID can be checked against its authority,
// not just its existence) and, with --data-plane, that the restrictions
// are enforced on the data path: a writer key cannot write outside its
// prefix and cannot delete at all.
//
// Safe by construction: every key it creates is named b2probe-* with a
// 1-hour expiry and is deleted before exit (--keep leaves them for
// inspection; they expire regardless). It never creates or deletes a
// bucket. --data-plane writes exactly one small object and removes it if
// the credential it was given can.
//
// Credentials come from the environment when set, else sops-decrypted
// from provisioning/secrets.yaml — the same precedence `cloudEnv` gives
// LINODE_TOKEN and the gitea provisioning token, so the probe needs no
// per-machine env setup and prints nothing.
//
//	b2-key-id / b2-app-key   in provisioning/secrets.yaml
//	B2_KEY_ID / B2_APP_KEY   override it
//
// The real minting credential will NOT be a master key (that is the whole
// point of what this verifies); the first run is the one exception, and
// the key it holds should be replaced with the scoped one as soon as the
// findings say a scoped one works.
//
// Usage:
//
//	go run ./cmd/b2probe
//	go run ./cmd/b2probe --data-plane sfi-probe
//
// Delete this command once the findings are recorded; internal/b2 is the
// part that stays.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gitea.zen.lofi/sfi/deployments/forts/internal/b2"
)

const probePrefix = "b2probe/"

// result is one assumption and what the account said about it.
type result struct {
	name    string
	verdict string // PASS, FAIL, SKIP
	detail  string
	// decides records what a FAIL would change, so the report is readable
	// without the doc open.
	decides string
}

type probe struct {
	ctx     context.Context
	master  *b2.Client
	results []*result
	// keys created along the way, deleted on the way out
	created []string
	keep    bool
	// escalated is the key a minting credential should not have been able
	// to mint. Held (not deleted at once) so the data-plane pass can ask
	// the question the control plane cannot: is its deleteFiles real, or
	// only listed?
	escalated *b2.CreatedKey
}

func main() {
	var dataPlane string
	fs := flag.NewFlagSet("b2probe", flag.ContinueOnError)
	fs.StringVar(&dataPlane, "data-plane", "", "also prove the restrictions are enforced on the data path, using this bucket as scratch")
	keep := fs.Bool("keep", false, "leave the probe keys behind for inspection (they expire in an hour regardless)")
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}

	keyID, appKey, err := credentials()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	p := &probe{ctx: ctx, keep: *keep}
	if err := p.run(keyID, appKey, dataPlane); err != nil {
		fmt.Fprintln(os.Stderr, "\nprobe aborted:", err)
		p.cleanup()
		p.report()
		os.Exit(1)
	}
	p.cleanup()
	os.Exit(p.report())
}

func (p *probe) run(keyID, appKey, dataPlane string) error {
	c, err := b2.Authorize(p.ctx, "", keyID, appKey)
	if err != nil {
		return err
	}
	p.master = c

	fmt.Println("authorized:")
	fmt.Printf("  account          %s\n", mask(c.AccountID))
	fmt.Printf("  looks like       %s\n", role(c))
	fmt.Printf("  capabilities     %d (%s)\n", len(c.Allowed.Capabilities), summarize(c.Allowed.Capabilities))
	fmt.Printf("  bucket scope     %s\n", scope(c))
	fmt.Printf("  s3 endpoint      %s\n", c.S3URL)
	fmt.Println()

	for _, need := range []string{"writeKeys", "deleteKeys", "listKeys", "listBuckets"} {
		if !c.Can(need) {
			return fmt.Errorf("this credential lacks %s — the probe cannot run with it", need)
		}
	}

	buckets, err := c.ListBuckets(p.ctx)
	if err != nil {
		return fmt.Errorf("listing buckets: %w", err)
	}
	fmt.Printf("buckets: %d — %s\n\n", len(buckets), names(buckets))

	stamp := time.Now().UTC().Format("20060102-150405")

	// --- 3. bucketIds as an array
	multi := p.check("bucketIds takes an array", "one credential per fort covering both backup buckets, or one per bucket")
	var writerKey *b2.CreatedKey
	var writerBuckets []b2.Bucket
	switch {
	case len(buckets) < 2:
		multi.skip("need 2 buckets on the account; found %d", len(buckets))
	default:
		writerBuckets = pick(buckets, dataPlane)
		ids := []string{writerBuckets[0].ID, writerBuckets[1].ID}
		writerKey, err = p.mint(b2.KeySpec{
			Name:         "b2probe-writer-" + stamp,
			Capabilities: b2.Writer,
			BucketIDs:    ids,
			NamePrefix:   probePrefix,
			ValidFor:     time.Hour,
		})
		if err != nil {
			multi.fail("b2_create_key refused two bucketIds: %v", err)
		} else if len(writerKey.BucketIDs) != 2 {
			multi.fail("accepted, but the response echoed %d bucketIds", len(writerKey.BucketIDs))
		} else {
			multi.pass("key restricted to %s + %s, prefix %q", writerBuckets[0].Name, writerBuckets[1].Name, probePrefix)
		}
	}

	// --- the guard: does list_keys report the restrictions back?
	guard := p.check("b2_list_keys echoes the restrictions", "whether mint's guard can verify authority, or only existence")
	if writerKey == nil {
		guard.skip("no writer key was minted")
	} else {
		keys, err := p.master.ListKeys(p.ctx)
		if err != nil {
			guard.fail("%v", err)
		} else {
			found := false
			for _, k := range keys {
				if k.ApplicationKeyID != writerKey.ApplicationKeyID {
					continue
				}
				found = true
				prefix := ""
				if k.NamePrefix != nil {
					prefix = *k.NamePrefix
				}
				switch {
				case prefix != probePrefix:
					guard.fail("namePrefix came back %q, not %q", prefix, probePrefix)
				case len(k.BucketIDs) != 2:
					guard.fail("bucketIds came back with %d entries", len(k.BucketIDs))
				case has(k.Capabilities, "deleteFiles"):
					guard.fail("capabilities came back including deleteFiles, which was never asked for")
				default:
					guard.pass("id, %d bucketIds, namePrefix and %d capabilities all readable — the guard can compare the recorded id against its authority", len(k.BucketIDs), len(k.Capabilities))
				}
			}
			if !found {
				guard.fail("the key just created is not in b2_list_keys")
			}
		}
	}

	// --- 1 + 2: mint a minting-key candidate and act as it
	minter, err := p.mint(b2.KeySpec{
		Name:         "b2probe-minting-" + stamp,
		Capabilities: b2.Minting,
		ValidFor:     time.Hour,
	})
	nonMaster := p.check("a non-master key can call b2_create_key", "whether provisioning/secrets.yaml can hold a scoped minting key, or must hold the master key")
	subset := p.check("a new key's capabilities must be a subset of the creator's", "whether the minting credential is structurally unable to grant deleteFiles, or only by convention")
	revoke := p.check("the minting key can revoke what it minted", "whether fort destroy can revoke with the same credential fort mint used")

	if err != nil {
		nonMaster.fail("could not mint the candidate: %v", err)
		subset.skip("no candidate to act as")
		revoke.skip("no candidate to act as")
	} else {
		mc, err := b2.Authorize(p.ctx, "", minter.ApplicationKeyID, minter.ApplicationKey)
		if err != nil {
			nonMaster.fail("the candidate could not authorize: %v", err)
			subset.skip("no session")
			revoke.skip("no session")
		} else {
			fmt.Printf("minting candidate: %s, %d capabilities, deleteFiles=%v\n\n",
				minter.KeyName, len(mc.Allowed.Capabilities), mc.Can("deleteFiles"))

			child, err := mc.CreateKey(p.ctx, b2.KeySpec{
				Name:         "b2probe-child-" + stamp,
				Capabilities: b2.Writer,
				NamePrefix:   probePrefix,
				ValidFor:     time.Hour,
			})
			if err != nil {
				if b2.Unauthorized(err) {
					nonMaster.fail("refused: %v — the credential in provisioning/secrets.yaml would have to be the master key", err)
				} else {
					nonMaster.fail("%v", err)
				}
				subset.skip("the candidate cannot create keys at all")
				revoke.skip("nothing was created")
			} else {
				p.created = append(p.created, child.ApplicationKeyID)
				nonMaster.pass("a key holding writeKeys minted %s without the master key", child.KeyName)

				// The escalation attempt: ask for deleteFiles, which the
				// candidate itself does not hold.
				esc, err := mc.CreateKey(p.ctx, b2.KeySpec{
					Name:         "b2probe-escalate-" + stamp,
					Capabilities: b2.Pruning,
					NamePrefix:   probePrefix,
					ValidFor:     time.Hour,
				})
				switch {
				case err == nil:
					p.created = append(p.created, esc.ApplicationKeyID)
					p.escalated = esc
					subset.fail("REFUSED NOTHING — a key without deleteFiles minted one WITH it (%s). The minting credential is not structurally safe; it is only as safe as what is asked of it", esc.KeyName)
				case b2.Unauthorized(err):
					subset.pass("refused (%v) — a minting key cannot grant what it does not hold", err)
				default:
					subset.fail("refused, but not on authority: %v", err)
				}

				if _, err := mc.DeleteKey(p.ctx, child.ApplicationKeyID); err != nil {
					revoke.fail("%v", err)
				} else {
					revoke.pass("deleted %s with the minting credential", child.KeyName)
					p.forget(child.ApplicationKeyID)
				}
			}
		}
	}

	// --- 4: the restrictions on the data path
	enforced := p.check("the prefix and the missing deleteFiles are enforced", "whether \"a compromised fort cannot erase the offsite copy\" is an observed 401 or a citation")
	if dataPlane == "" {
		enforced.skip("--data-plane <bucket> not given")
		return nil
	}
	if writerKey == nil {
		enforced.skip("no writer key was minted")
		return nil
	}
	real := p.check("an escalated key's deleteFiles is real", "whether the subset failure is an escalation path or only a listing")
	target := byName(writerBuckets, dataPlane)
	if target == nil {
		enforced.skip("bucket %q is not one of the two the writer key was scoped to", dataPlane)
		real.skip("no bucket to test against")
		return nil
	}
	p.dataPlane(enforced, real, writerKey, *target, stamp)
	p.lifecycle(*target)
	return nil
}

// lifecycle asks how the retention instructions in docs/infra/backups.md
// can actually be expressed. The doc says "daysFromHidingToDeleting on
// every prefix" — which is only writable if B2 accepts one rule per
// prefix. It restores whatever the bucket had when it is done.
func (p *probe) lifecycle(bucket b2.Bucket) {
	wide := p.check("a bucket-wide daysFromHidingToDeleting rule is accepted", "whether one rule per bucket can carry the retention, or it has to be spelled per prefix")
	perPrefix := p.check("non-overlapping per-prefix rules are accepted", "whether prefixes can carry different retentions at all")
	overlap := p.check("an overlapping prefix rule is refused", "whether a bucket-wide rule and a per-prefix exception can coexist")

	before, err := p.master.BucketRules(p.ctx, bucket.Name)
	if err != nil {
		wide.fail("%v", err)
		perPrefix.skip("could not read the bucket's rules")
		overlap.skip("could not read the bucket's rules")
		return
	}
	defer func() {
		if err := p.master.SetBucketRules(p.ctx, bucket.ID, before); err != nil {
			fmt.Printf("\ncould not restore %s's lifecycle rules (%d before): %v\n", bucket.Name, len(before), err)
		}
	}()

	// 1. one rule, whole bucket, hidden versions reclaimed after 7 days
	// and nothing expired by age.
	err = p.master.SetBucketRules(p.ctx, bucket.ID, []b2.LifecycleRule{
		{FileNamePrefix: "", DaysFromHidingToDeleting: b2.Days(7)},
	})
	if err != nil {
		wide.fail("%v", err)
	} else if got, err := p.master.BucketRules(p.ctx, bucket.Name); err != nil {
		wide.fail("set, but could not read back: %v", err)
	} else if len(got) != 1 || got[0].DaysFromHidingToDeleting == nil || *got[0].DaysFromHidingToDeleting != 7 {
		wide.fail("read back as %+v", got)
	} else if got[0].DaysFromUploadingToHiding != nil {
		wide.fail("B2 defaulted daysFromUploadingToHiding to %d — a rule would expire live objects nobody asked it to", *got[0].DaysFromUploadingToHiding)
	} else {
		wide.pass("one rule, empty prefix, hiding→deleting 7d, uploading→hiding unset")
	}

	// 2. one rule per prefix, no overlap between them.
	err = p.master.SetBucketRules(p.ctx, bucket.ID, []b2.LifecycleRule{
		{FileNamePrefix: "db/", DaysFromHidingToDeleting: b2.Days(7)},
		{FileNamePrefix: "pv/", DaysFromHidingToDeleting: b2.Days(30)},
	})
	if err != nil {
		perPrefix.fail("%v", err)
	} else if got, err := p.master.BucketRules(p.ctx, bucket.Name); err != nil {
		perPrefix.fail("set, but could not read back: %v", err)
	} else if len(got) != 2 {
		perPrefix.fail("read back %d rules", len(got))
	} else {
		perPrefix.pass("db/ at 7d and pv/ at 30d coexist — retention can differ per prefix in one bucket")
	}

	// 3. a bucket-wide rule PLUS a prefix exception — the shape anyone
	// would reach for first.
	err = p.master.SetBucketRules(p.ctx, bucket.ID, []b2.LifecycleRule{
		{FileNamePrefix: "", DaysFromHidingToDeleting: b2.Days(7)},
		{FileNamePrefix: "db/", DaysFromHidingToDeleting: b2.Days(30)},
	})
	if err != nil {
		overlap.pass("refused (%v) — a catch-all and an exception cannot coexist; rules must partition the bucket", err)
	} else {
		overlap.fail("ACCEPTED an overlapping pair — which of the two applies to db/ is then B2's business, not the design's")
	}
}

// dataPlane writes inside the prefix (must work), outside it (must not),
// then tries to delete what it wrote (must not) — and, when the control
// plane let a minting key escalate, asks whether the escalated key can
// actually carry out the delete it claims.
func (p *probe) dataPlane(r, real *result, key *b2.CreatedKey, bucket b2.Bucket, stamp string) {
	wc, err := b2.Authorize(p.ctx, "", key.ApplicationKeyID, key.ApplicationKey)
	if err != nil {
		r.fail("the writer key could not authorize: %v", err)
		return
	}
	up, err := wc.UploadURL(p.ctx, bucket.ID)
	if err != nil {
		r.fail("b2_get_upload_url in %s: %v", bucket.Name, err)
		return
	}

	inside := probePrefix + "probe-" + stamp + ".txt"
	fileID, err := wc.Upload(p.ctx, up, inside, []byte("b2probe "+stamp+"\n"))
	if err != nil {
		r.fail("could not write inside its own prefix (%s): %v", inside, err)
		return
	}

	outside := "somewhere-else/probe-" + stamp + ".txt"
	if _, err := wc.Upload(p.ctx, up, outside, []byte("should not land\n")); err == nil {
		r.fail("WROTE OUTSIDE ITS PREFIX (%s) — namePrefix is not a write boundary", outside)
	} else if !b2.Unauthorized(err) {
		r.fail("write outside the prefix failed, but not on authority: %v", err)
	} else if err := wc.DeleteFileVersion(p.ctx, inside, fileID); err == nil {
		r.fail("DELETED ITS OWN OBJECT without deleteFiles — the no-delete writer posture does not hold")
	} else if !b2.Unauthorized(err) {
		r.fail("delete failed, but not on authority: %v", err)
	} else {
		r.pass("wrote %s; refused outside the prefix and refused to delete, both 401", inside)
	}

	// The escalation, carried through. A key listing deleteFiles is not
	// the same as a key that deletes; if B2 refuses here, the subset
	// failure is a cosmetic one and the posture survives.
	switch {
	case p.escalated == nil:
		real.skip("no key escalated — the subset rule held")
	default:
		ec, err := b2.Authorize(p.ctx, "", p.escalated.ApplicationKeyID, p.escalated.ApplicationKey)
		switch {
		case err != nil:
			real.fail("the escalated key could not authorize: %v", err)
		case !ec.Can("deleteFiles"):
			real.pass("the escalated key authorizes WITHOUT deleteFiles — B2 listed it at creation but does not honour it")
		default:
			if err := ec.DeleteFileVersion(p.ctx, inside, fileID); err == nil {
				fileID = ""
				real.fail("DELETED the object — a minting credential is a delete-capable credential one call away; treat it as one")
			} else if b2.Unauthorized(err) {
				real.pass("authorizes with deleteFiles but B2 refused the delete (%v) — the escalation is cosmetic", err)
			} else {
				real.fail("delete failed, but not on authority: %v", err)
			}
		}
	}

	// Cleanup needs delete authority the writer deliberately lacks.
	if fileID == "" {
		return
	}
	if err := p.master.DeleteFileVersion(p.ctx, inside, fileID); err != nil {
		fmt.Printf("\nleft behind: s3://%s/%s (the probe credential cannot delete it either) — remove it with a delete-capable key\n", bucket.Name, inside)
	}
}

func (p *probe) mint(spec b2.KeySpec) (*b2.CreatedKey, error) {
	k, err := p.master.CreateKey(p.ctx, spec)
	if err != nil {
		return nil, err
	}
	p.created = append(p.created, k.ApplicationKeyID)
	return k, nil
}

func (p *probe) forget(id string) {
	out := p.created[:0]
	for _, v := range p.created {
		if v != id {
			out = append(out, v)
		}
	}
	p.created = out
}

func (p *probe) cleanup() {
	if len(p.created) == 0 {
		return
	}
	if p.keep {
		fmt.Printf("\n--keep: %d probe key(s) left behind; they expire in an hour\n", len(p.created))
		return
	}
	fmt.Printf("\ncleanup: deleting %d probe key(s)\n", len(p.created))
	for _, id := range p.created {
		if _, err := p.master.DeleteKey(p.ctx, id); err != nil {
			fmt.Printf("  could not delete %s: %v (it expires within the hour)\n", id, err)
		}
	}
}

func (p *probe) check(name, decides string) *result {
	r := &result{name: name, verdict: "SKIP", decides: decides}
	p.results = append(p.results, r)
	return r
}

func (r *result) pass(f string, a ...any) { r.verdict, r.detail = "PASS", fmt.Sprintf(f, a...) }
func (r *result) fail(f string, a ...any) { r.verdict, r.detail = "FAIL", fmt.Sprintf(f, a...) }
func (r *result) skip(f string, a ...any) { r.verdict, r.detail = "SKIP", fmt.Sprintf(f, a...) }

// report prints the findings and returns the exit code: non-zero if any
// assumption the design rests on turned out false.
func (p *probe) report() int {
	fmt.Println("\n── findings ─────────────────────────────────────────────")
	bad := 0
	for _, r := range p.results {
		fmt.Printf("\n%s  %s\n", r.verdict, r.name)
		fmt.Printf("      %s\n", r.detail)
		if r.verdict != "PASS" {
			fmt.Printf("      decides: %s\n", r.decides)
		}
		if r.verdict == "FAIL" {
			bad++
		}
	}
	fmt.Println()
	if bad > 0 {
		fmt.Printf("%d assumption(s) did not hold — docs/infra/backups.md needs the correction before the CLI half is built.\n", bad)
		return 1
	}
	fmt.Println("every assumption held.")
	return 0
}

// --- small helpers, none of which print a secret

func mask(s string) string {
	if len(s) <= 4 {
		return "····"
	}
	return "····" + s[len(s)-4:]
}

func role(c *b2.Client) string {
	if c.Master() {
		return "the account master key (unrestricted — fine for the probe, not for provisioning/secrets.yaml)"
	}
	return "a scoped application key"
}

func scope(c *b2.Client) string {
	if len(c.Allowed.Buckets) == 0 {
		if c.Allowed.NamePrefix == nil {
			return "every bucket, no prefix restriction"
		}
		return "every bucket, prefix " + *c.Allowed.NamePrefix
	}
	var b []string
	for _, x := range c.Allowed.Buckets {
		if x.Name != nil {
			b = append(b, *x.Name)
		} else {
			b = append(b, x.ID)
		}
	}
	return strings.Join(b, ", ")
}

func summarize(caps []string) string {
	if len(caps) > 6 {
		return strings.Join(caps[:6], " ") + " …"
	}
	return strings.Join(caps, " ")
}

func names(bs []b2.Bucket) string {
	var out []string
	for _, b := range bs {
		out = append(out, b.Name)
	}
	return strings.Join(out, ", ")
}

func has(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// pick chooses the two buckets to scope the writer key to, preferring the
// scratch bucket named on --data-plane so the data-path checks can run.
func pick(bs []b2.Bucket, prefer string) []b2.Bucket {
	if prefer != "" {
		if b := byName(bs, prefer); b != nil {
			for _, other := range bs {
				if other.ID != b.ID {
					return []b2.Bucket{*b, other}
				}
			}
		}
	}
	return []b2.Bucket{bs[0], bs[1]}
}

func byName(bs []b2.Bucket, name string) *b2.Bucket {
	for i := range bs {
		if bs[i].Name == name {
			return &bs[i]
		}
	}
	return nil
}

// --- credentials
//
// env wins, else sops, mirroring cmd/fort's cloudEnv. Duplicated here
// rather than exported from package main: this command is the disposable
// half, and `fort mint` will call cmd/fort's own secret() when it grows
// the B2 step.

func credentials() (keyID, appKey string, err error) {
	keyID, appKey = os.Getenv("B2_KEY_ID"), os.Getenv("B2_APP_KEY")
	if keyID != "" && appKey != "" {
		return keyID, appKey, nil
	}
	root, err := repoRoot()
	if err != nil {
		return "", "", fmt.Errorf("B2_KEY_ID/B2_APP_KEY unset and not inside the deployments repo: %w", err)
	}
	if keyID == "" {
		if keyID, err = secret(root, "b2-key-id"); err != nil {
			return "", "", missing("b2-key-id", "B2_KEY_ID")
		}
	}
	if appKey == "" {
		if appKey, err = secret(root, "b2-app-key"); err != nil {
			return "", "", missing("b2-app-key", "B2_APP_KEY")
		}
	}
	return keyID, appKey, nil
}

func missing(key, env string) error {
	return fmt.Errorf("no %s in provisioning/secrets.yaml and %s unset — add it with:\n"+
		"  sops --config .sops.yaml set provisioning/secrets.yaml '[\"%s\"]' '\"<value>\"'", key, env, key)
}

func secret(root, key string) (string, error) {
	out, err := exec.Command("sops", "--config", filepath.Join(root, ".sops.yaml"),
		"-d", "--extract", `["`+key+`"]`,
		filepath.Join(root, "provisioning/secrets.yaml")).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		// A worktree's .git is a file, not a directory — Stat covers both.
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("not inside the deployments repo")
		}
		dir = parent
	}
}
