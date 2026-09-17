package main

import (
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/b2"
)

// The invariant the reader role exists to hold: it crosses a prefix
// boundary, so it must not be able to change anything on the other side
// of it. Asserted against the capability list rather than the role's
// prose, because the prose is not what B2 is handed.
func TestReaderRoleCannotWrite(t *testing.T) {
	role := readerRole("zenbook")
	for _, cap := range role.caps {
		if strings.HasPrefix(cap, "write") || strings.HasPrefix(cap, "delete") {
			t.Errorf("reader holds %q — a restore credential must not mutate its source", cap)
		}
	}
	if !hasKey(role.caps, "readFiles") || !hasKey(role.caps, "listFiles") {
		t.Errorf("reader cannot read: %v", role.caps)
	}
	if role.prefix != "zenbook/" {
		t.Errorf("prefix %q — a restore credential is scoped to the host it reads", role.prefix)
	}
	if role.name != "restore-zenbook" {
		t.Errorf("name %q — must not be mistakable for the fort's own key in b2_list_keys", role.name)
	}
	if role.name == b2KeyName("zenbook") {
		t.Error("reader and writer key names collide")
	}
	if len(role.buckets) != 3 {
		t.Errorf("buckets %v — a restore may need any of the three backup buckets", role.buckets)
	}
}

// writer keeps its manifest guard; reader deliberately does not, because
// the host restoring a dead fort is never that fort.
func TestHostRoleKinds(t *testing.T) {
	if got := writerRole("zenbook").kind; got != "writer" {
		t.Errorf("writer kind %q — the manifest guard keys off this", got)
	}
	if got := readerRole("zenbook").kind; got != "reader" {
		t.Errorf("reader kind %q", got)
	}
}

// The host-bearing roles. The fixed ones are covered by
// TestParseB2KeyArgs in mintbackup_test.go; what is new here is that the
// host is mandatory, that the two roles spell the key name differently,
// and that the name is validated after the role picks it.
func TestParseB2KeyHostRoles(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		name  string
		write bool
	}{
		{[]string{"reader", "zenbook"}, "restore-zenbook", false},
		{[]string{"reader", "zenbook", "--write"}, "restore-zenbook", true},
		{[]string{"writer", "zenbook"}, "fort-zenbook", false},
	} {
		role, write, err := parseB2KeyArgs(tc.args)
		if err != nil || role.name != tc.name || write != tc.write {
			t.Errorf("%v: got %q write=%v err=%v; want %q write=%v", tc.args, role.name, write, err, tc.name, tc.write)
		}
	}

	for _, args := range [][]string{
		{"reader"},                 // host is not optional
		{"reader", "--write"},      // a flag is not a host
		{"writer"},                 // same rule, unchanged
		{"restorer", "zenbook"},    // not a role
		{"reader", "zen_book"},     // B2 rejects underscores in key names
		{"reader", "zenbook", "x"}, // stray positional
	} {
		if _, _, err := parseB2KeyArgs(args); err == nil {
			t.Errorf("%v: accepted, want an error", args)
		}
	}
}

// The dry-run hint is copy-pasteable only if a two-word role round-trips.
func TestRoleArgRoundTrip(t *testing.T) {
	for _, want := range []string{"minting", "pruning", "writer zenbook", "reader zenbook"} {
		role, _, err := parseB2KeyArgs(strings.Fields(want))
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if got := roleArg(role); got != want {
			t.Errorf("roleArg = %q, want %q", got, want)
		}
	}
}

func TestB2RoleNamesSorted(t *testing.T) {
	got := b2RoleNames()
	for i := 1; i < len(got); i++ {
		if got[i-1] > got[i] {
			t.Fatalf("usage order is not stable: %v", got)
		}
	}
}

// A console-made key wearing the right name is the failure this catches:
// same name, quietly more power.
func TestRoleDriftSpotsAWritingReader(t *testing.T) {
	prefix := "zenbook/"
	k := b2.Key{
		ApplicationKeyID: "x",
		Capabilities:     append(append([]string{}, b2.Reader...), "writeFiles"),
		BucketIDs:        []string{"a", "b"},
		NamePrefix:       &prefix,
	}
	drift := roleDrift(k, readerRole("zenbook"), []string{"a", "b"})
	if !strings.Contains(drift, "writeFiles") {
		t.Errorf("drift %q — must name the extra capability", drift)
	}
	clean := b2.Key{ApplicationKeyID: "x", Capabilities: b2.Reader, BucketIDs: []string{"a", "b"}, NamePrefix: &prefix}
	if drift := roleDrift(clean, readerRole("zenbook"), []string{"a", "b"}); drift != "" {
		t.Errorf("a correct key reported drift: %q", drift)
	}
}

// The role zenbook's databasus needed: dumps live in their own bucket,
// laid out <host>/ like the others, which a host's own key does not cover.
func TestLogicalRoleIsItsOwnBucket(t *testing.T) {
	logical, writer := logicalRole("zenbook"), writerRole("zenbook")
	if logical.prefix != "zenbook/" {
		t.Errorf("prefix = %q, want <host>/ — every bucket is laid out the same way", logical.prefix)
	}
	if len(logical.buckets) != 1 || logical.buckets[0] != b2LogicalBucket {
		t.Errorf("buckets = %v, want just %s: dumps never go near the restic repo", logical.buckets, b2LogicalBucket)
	}
	// The separation is the bucket now, so it has to hold both ways.
	if hasKey(writer.buckets, b2LogicalBucket) {
		t.Errorf("the host's own key must not reach %s — databasus holds the other one", b2LogicalBucket)
	}
	// A restore reads dumps too, and one <host>/ prefix now spans them.
	if !hasKey(readerRole("zenbook").buckets, b2LogicalBucket) {
		t.Errorf("the restore key should read %s", b2LogicalBucket)
	}
	if hasKey(logical.caps, "deleteFiles") {
		t.Error("no writer deletes on B2; dumps expire by lifecycle rule")
	}
	if logical.name == writer.name {
		t.Error("the two keys must be tellable apart in b2_list_keys")
	}
	if logical.master {
		t.Error("the minting key is enough for this one")
	}
}

func TestOnlyTheMintingRoleNeedsTheMasterKey(t *testing.T) {
	if !b2Roles["minting"].master {
		t.Error("nothing but the master key can bootstrap the minting key")
	}
	if b2Roles["pruning"].master {
		t.Error("B2 does not enforce subsetting (#291): the minting key creates the pruning key")
	}
	for kind, mk := range b2HostRoles {
		if mk("zenbook").master {
			t.Errorf("%s should not ask for the master key", kind)
		}
	}
}
