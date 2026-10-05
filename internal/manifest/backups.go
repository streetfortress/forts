package manifest

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Backups is the [backups] table (sfi/forts#4, cut 1 in #39): what this
// fort backs up and WHERE, one block per layer, each naming a store.
//
//	[backups]                      # required; an empty table is "this fort does not back up"
//
//	[[backups.pv]]                 # restic, every PVC that is an original (modules/pv-backup.nix)
//	store = "b2"
//	every = "6h"                   # 6h (the default) | daily | weekly
//
//	[[backups.pv]]
//	store = "rustfs-central"
//	every = "weekly"
//
//	[backups.db-physical]          # barman: base backups + WAL from the fort's postgres
//	store = "b2"
//	hour = 4                       # the nightly base-backup hour (UTC)
//
//	[backups.db-logical]           # databasus: logical dumps
//	store = "b2"
//
// A block names a STORE, never an endpoint: there are exactly two
// object stores at fleet level (Stores), and enumerating endpoints per
// fort is how they drift. The store decides what the block needs — a
// block on rustfs-central registers with a hub in THIS estate, a block
// on b2 needs nothing of the estate — and the verbs render from the
// blocks: scaffold the restic targets from pv, add the ObjectStore and
// ScheduledBackup from db-physical, mint the credentials each store
// implies.
//
// The table replaces `profile = "lab" | "production" | "none"`, which
// bundled three layers, a destination set and a cadence under one word
// and could not say what the production fort wants (physical and
// logical to B2, no PV leg) or what the hub wants (PV to B2, nothing
// physical). The table is required and its blocks are not: `[backups]`
// with nothing under it is the positive declaration that nothing on the
// fort is backed up, and a manifest without the table is an error — a
// fort that does not say silently gets nothing.
type Backups struct {
	// PV is the [[backups.pv]] list: one restic target per store.
	PV []PVBackup `toml:"pv"`
	// DBPhysical is [backups.db-physical]: the CNPG barman archive.
	DBPhysical *DBPhysical `toml:"db-physical"`
	// DBLogical is [backups.db-logical]: the databasus dumps. Accepted
	// and checked here; nothing renders from it yet (#4, #375).
	DBLogical *DBLogical `toml:"db-logical"`
	// Declared says the table is present at all. Parse sets it; a host
	// built in memory sets it itself. Validate refuses a manifest that
	// never wrote `[backups]`.
	Declared bool `toml:"-"`
}

// PVBackup is one [[backups.pv]] block.
type PVBackup struct {
	Store string `toml:"store"`
	// Every is the cadence: "6h" (the default when unset), "daily" or
	// "weekly". The fleet supplies the phase — modules/pv-backup.nix
	// maps the word to an OnCalendar per store, so two stores on the
	// same fort never read the PVCs at the same time. The vocabulary
	// beyond these three words is #4's.
	Every string `toml:"every"`
}

// DBPhysical is the [backups.db-physical] block.
type DBPhysical struct {
	Store string `toml:"store"`
	// Hour is the nightly base-backup hour, UTC, 0–23. Required: every
	// fort at 04:00 is the collision the key exists to avoid (#284), so
	// there is no default to fall into.
	Hour *int `toml:"hour"`
}

// DBLogical is the [backups.db-logical] block.
type DBLogical struct {
	Store string `toml:"store"`
}

// The stores, by name.
const (
	StoreB2            = "b2"
	StoreRustfsCentral = "rustfs-central"
)

// Store is one of the fleet's object stores: which layers it accepts and
// what naming it asks of the estate. Endpoint, bucket and credential
// shape live with the consumer that needs them (modules/pv-backup.nix
// for restic, the postgres templates for barman); the one registry file
// both read from is #4's open question, not this cut's.
type Store struct {
	Name string
	// Layers the store accepts.
	PV, DBPhysical, DBLogical bool
	// NeedsHub: the store is a service a fort of THIS estate hosts
	// (the hub's rustfs-central), so a block on it registers an
	// identity in the hub's tree and an estate with no hub cannot
	// take one.
	NeedsHub bool
}

// Stores is the registry. b2 takes every layer. rustfs-central takes
// pv alone in this cut: db-physical never (CNPG has one archive_command,
// and it points offsite), db-logical not yet (#4).
var Stores = map[string]Store{
	StoreB2:            {Name: StoreB2, PV: true, DBPhysical: true, DBLogical: true},
	StoreRustfsCentral: {Name: StoreRustfsCentral, PV: true, NeedsHub: true},
}

// StoreNames lists the registry, sorted, for error text.
func StoreNames() []string {
	names := make([]string, 0, len(Stores))
	for n := range Stores {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Cadences are the accepted values of [[backups.pv]] every.
var Cadences = []string{"6h", "daily", "weekly"}

// DefaultCadence is what a pv block that names none means.
const DefaultCadence = "6h"

// Cadence is the block's `every`, resolved.
func (p PVBackup) Cadence() string {
	if p.Every == "" {
		return DefaultCadence
	}
	return p.Every
}

// Backed reports whether any block is declared — whether this fort backs
// anything up anywhere.
func (b Backups) Backed() bool {
	return len(b.PV) > 0 || b.DBPhysical != nil || b.DBLogical != nil
}

// PVTo is the pv block on store, nil when the fort declares none there.
func (b Backups) PVTo(store string) *PVBackup {
	for i := range b.PV {
		if b.PV[i].Store == store {
			return &b.PV[i]
		}
	}
	return nil
}

// Uses reports whether ANY block names store.
func (b Backups) Uses(store string) bool {
	return slices.Contains(b.StoreNames(), store)
}

// StoreNames lists every store a block names, sorted, each once.
func (b Backups) StoreNames() []string {
	seen := map[string]bool{}
	for _, p := range b.PV {
		seen[p.Store] = true
	}
	if b.DBPhysical != nil {
		seen[b.DBPhysical.Store] = true
	}
	if b.DBLogical != nil {
		seen[b.DBLogical.Store] = true
	}
	var out []string
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// NeedsHub reports whether a block names a store that a hub of this
// estate hosts.
func (b Backups) NeedsHub() bool {
	for _, s := range b.StoreNames() {
		if Stores[s].NeedsHub {
			return true
		}
	}
	return false
}

// Summary is the one-line reading of the table for a report: the
// manifest says intent, and the report says where that lands.
func (b Backups) Summary() string {
	if !b.Backed() {
		return "none declared ([backups] is empty)"
	}
	var parts []string
	if len(b.PV) > 0 {
		var legs []string
		for _, p := range b.PV {
			legs = append(legs, p.Store+" every "+p.Cadence())
		}
		parts = append(parts, "pv → "+strings.Join(legs, ", "))
	}
	if b.DBPhysical != nil {
		parts = append(parts, fmt.Sprintf("db-physical → %s at %02d:00 UTC", b.DBPhysical.Store, b.DBPhysical.HourValue()))
	}
	if b.DBLogical != nil {
		parts = append(parts, "db-logical → "+b.DBLogical.Store)
	}
	return strings.Join(parts, "; ")
}

// HourValue is the hour, 0 when unset (Validate refuses unset first).
func (d *DBPhysical) HourValue() int {
	if d == nil || d.Hour == nil {
		return 0
	}
	return *d.Hour
}

// Validate checks the table on its own terms: every block names a store
// the registry knows and that accepts the layer, the cadence words are
// the known ones, db-physical names its hour, and no store is named
// twice under pv. Whether the ESTATE can honour a block (a hub for
// rustfs-central) is mint's and check's to say — the manifest alone
// cannot see estate.toml.
func (b Backups) Validate() error {
	if !b.Declared {
		return fmt.Errorf("[backups] is required — a fort that does not say silently gets nothing backed up; an empty [backups] table says so out loud, and [[backups.pv]] / [backups.db-physical] / [backups.db-logical] blocks say where (docs/manifest.md)")
	}
	store := func(block, name string, accepts func(Store) bool) error {
		if name == "" {
			return fmt.Errorf("%s: store is required — one of %s", block, strings.Join(StoreNames(), ", "))
		}
		s, ok := Stores[name]
		if !ok {
			return fmt.Errorf("%s: store %q: want one of %s", block, name, strings.Join(StoreNames(), ", "))
		}
		if !accepts(s) {
			return fmt.Errorf("%s: store %q does not take this layer", block, name)
		}
		return nil
	}
	seen := map[string]bool{}
	for i, p := range b.PV {
		block := fmt.Sprintf("[[backups.pv]] #%d", i+1)
		if err := store(block, p.Store, func(s Store) bool { return s.PV }); err != nil {
			return err
		}
		if seen[p.Store] {
			return fmt.Errorf("%s: store %q is already named by an earlier pv block — one restic target per store", block, p.Store)
		}
		seen[p.Store] = true
		if p.Every != "" && !slices.Contains(Cadences, p.Every) {
			return fmt.Errorf("%s: every %q: want one of %s", block, p.Every, strings.Join(Cadences, ", "))
		}
	}
	if d := b.DBPhysical; d != nil {
		if err := store("[backups.db-physical]", d.Store, func(s Store) bool { return s.DBPhysical }); err != nil {
			return err
		}
		if d.Hour == nil {
			return fmt.Errorf("[backups.db-physical]: hour is required — the nightly base-backup hour (UTC, 0–23); forts stagger it so the archive uploads do not collide (#284)")
		}
		if *d.Hour < 0 || *d.Hour > 23 {
			return fmt.Errorf("[backups.db-physical]: hour %d: want 0–23 (UTC)", *d.Hour)
		}
	}
	if d := b.DBLogical; d != nil {
		if err := store("[backups.db-logical]", d.Store, func(s Store) bool { return s.DBLogical }); err != nil {
			return err
		}
	}
	return nil
}
