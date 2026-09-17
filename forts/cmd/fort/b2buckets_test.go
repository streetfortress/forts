package main

import (
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/b2"
)

// The two fields sound alike and do opposite things; this is the test
// that a future edit to the declaration cannot swap them quietly.
func TestBucketDeclarationsHoldTheDocsRules(t *testing.T) {
	seen := map[string]bool{}
	for _, d := range b2Buckets {
		seen[d.name] = true
		// One rule, empty prefix: B2 accepts overlapping rules without
		// saying which governs (#291), and one rule cannot overlap.
		if len(d.rules) != 1 || d.rules[0].FileNamePrefix != "" {
			t.Errorf("%s: want exactly one empty-prefix rule, got %s", d.name, rulesText(d.rules))
			continue
		}
		r := d.rules[0]
		if r.DaysFromHidingToDeleting == nil || *r.DaysFromHidingToDeleting < 1 || *r.DaysFromHidingToDeleting > 7 {
			t.Errorf("%s: daysFromHidingToDeleting must be 1-7, or deleted versions are billed forever", d.name)
		}
		if d.name == b2PVBucket && r.DaysFromUploadingToHiding != nil {
			t.Errorf("%s holds restic repos: age-based expiry removes packs live snapshots reference", d.name)
		}
		if d.name != b2PVBucket && r.DaysFromUploadingToHiding == nil {
			t.Errorf("%s is append-only and no key deletes there: without age-based expiry it has no retention at all", d.name)
		}
	}
	// Every bucket a key role can name must be one this verb makes.
	for _, role := range []b2Role{pvRole("h"), physicalRole("h"), logicalRole("h"), restoreRole("h"), b2Roles["pruning"]} {
		for _, b := range role.buckets {
			if !seen[b] {
				t.Errorf("%s names bucket %s, which b2-buckets does not declare", role.name, b)
			}
		}
	}
}

func TestPlanBuckets(t *testing.T) {
	want := b2Buckets
	live := []b2.Bucket{
		// made in the console with the defaults: private, no rules
		{ID: "1", Name: b2PhysicalBucket, Type: b2.Private},
		// correct
		{ID: "2", Name: b2LogicalBucket, Type: b2.Private, LifecycleRules: want[2].rules},
		// someone else's
		{ID: "3", Name: "sketches", Type: "allPublic"},
	}
	actions, strangers := planBuckets(live, want)

	if !actions[0].create {
		t.Errorf("%s is absent and should be created", want[0].name)
	}
	if a := actions[1]; a.create || len(a.drift) != 1 || !strings.Contains(a.drift[0], "lifecycle none") {
		t.Errorf("a console-default bucket should drift on lifecycle alone, got %+v", a.drift)
	}
	if actions[2].changes() {
		t.Errorf("a matching bucket should need nothing, got %+v", actions[2].drift)
	}
	if len(strangers) != 1 || strangers[0].Name != "sketches" {
		t.Errorf("undeclared buckets are reported, never planned: %+v", strangers)
	}

	live[1].Type = "allPublic"
	actions, _ = planBuckets(live, want)
	if len(actions[2].drift) != 1 || !strings.Contains(actions[2].drift[0], "never public") {
		t.Errorf("a public backup bucket must drift, got %+v", actions[2].drift)
	}
}
