package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

// The pruning key's lifecycle (#400). sfi-b2-pruning-key is the one B2
// credential in the fleet that deletes backups, and it is ROLE-scoped,
// not host-scoped: it belongs to whichever host enables sfi.pv-prune's
// b2 tier (today luxor), and it is consumed from that host's
// nixos/secrets/<host>.yaml as restic-prune-s3-env-b2. The convention
// that fell out of #395/#397: operator-only keys live in the password
// manager; host-consumed keys live in that host's sops file and
// lifecycle with it. So the holder is discoverable offline, from a
// plaintext key name — the same property secretHasKey and the console
// check run on — and three verbs read it:
//
//	fort check      a host that sweeps a tier names that tier's key, and
//	                a host that names one sweeps the tier (per fort);
//	                exactly one host holds the B2 key (--all)
//	fort dismantle  the repo half reports that the host held the key
//	fort destroy    revokes sfi-b2-pruning-key with the host, and says
//	                the fleet now has no pruner
//
// The rustfs tier's `prune` identity needs none of this: rustfs-central's
// contract generates it and it dies with the instance.

const (
	b2PruningKeyName = "sfi-b2-pruning-key"
	pruneTierB2      = "b2"
	pruneTierRustfs  = "rustfs"
)

// pruneTiers mirrors defaultTiers in nixos/modules/pv-prune.nix: both are
// on by default once the module is enabled, and a host switches one off
// with tiers.<name>.enable = false.
var pruneTiers = []string{pruneTierRustfs, pruneTierB2}

// pruneEnvKey is the sops key the module declares for an enabled tier
// (sops.secrets."restic-prune-s3-env-${name}" in pv-prune.nix).
func pruneEnvKey(tier string) string { return "restic-prune-s3-env-" + tier }

// The textual read of a host's default.nix, like consoleModuleRE: `fort
// check` runs in CI with no nix, and the lines it must see are the ones
// docs/infra/backups.md tells an operator to write — `sfi.pv-prune = {
// enable = true; ... }` or the dotted `sfi.pv-prune.enable = true;`, and
// `tiers.<name>.enable = false;` to leave a tier out.
var (
	pruneBlockRE    = regexp.MustCompile(`sfi\.pv-prune\s*=\s*\{`)
	pruneDottedOnRE = regexp.MustCompile(`sfi\.pv-prune\.enable\s*=\s*true\s*;`)
	pruneBlockOnRE  = regexp.MustCompile(`(?m)^\s*enable\s*=\s*true\s*;`)
	pruneTierOffRE  = func(tier string) *regexp.Regexp {
		return regexp.MustCompile(`(?m)(sfi\.pv-prune\.)?tiers\.` + regexp.QuoteMeta(tier) + `\.enable\s*=\s*false\s*;`)
	}
	pruneTierOffNest = func(tier string) *regexp.Regexp {
		return regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(tier) + `\s*=\s*\{[^}]*enable\s*=\s*false\s*;`)
	}
)

// pruneState is what the tree says about one host and sfi.pv-prune.
type pruneState struct {
	enabled bool     // the module is on in nixos/hosts/<host>/default.nix
	sweeps  []string // the tiers the module declares a secret for: enabled, and not switched off
	holds   []string // the tiers whose restic-prune-s3-env-<tier> the host's sops file names
}

// readPruneState reads both halves offline. A host with no default.nix
// (a peer with no nix dir) is simply not enabled.
func readPruneState(root, host string) pruneState {
	var s pruneState
	nix, err := os.ReadFile(filepath.Join(root, "nixos/hosts", host, "default.nix"))
	if err == nil {
		text := string(nix)
		block := pruneBlock(text)
		s.enabled = pruneDottedOnRE.MatchString(text) || (block != "" && pruneBlockOnRE.MatchString(block))
		if s.enabled {
			for _, tier := range pruneTiers {
				if pruneTierOffRE(tier).MatchString(text) || (block != "" && pruneTierOffNest(tier).MatchString(block)) {
					continue
				}
				s.sweeps = append(s.sweeps, tier)
			}
		}
	}
	secrets := filepath.Join(root, "nixos/secrets", host+".yaml")
	for _, tier := range pruneTiers {
		if sopsHasKey(secrets, pruneEnvKey(tier)) {
			s.holds = append(s.holds, tier)
		}
	}
	return s
}

// pruneBlock is the text of `sfi.pv-prune = { ... }` at brace depth, ""
// when the host writes none (or only the dotted form).
func pruneBlock(text string) string {
	loc := pruneBlockRE.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	depth := 0
	for i := loc[1] - 1; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return text[loc[1]:i]
			}
		}
	}
	return text[loc[1]:]
}

// holdsPruneKey: does this host's sops file name the B2 tier's key? The
// one fact dismantle and destroy need. For a live tree the file is on
// disk; destroy reads the retired copy through retiredSecrets.has.
func holdsPruneKey(root, host string) bool {
	return sopsHasKey(filepath.Join(root, "nixos/secrets", host+".yaml"), pruneEnvKey(pruneTierB2))
}

// pruneFindings is the per-fort half of the check: the repo-side
// invariant that sops-nix would otherwise enforce at activation, which
// is loud but late (#400: a luxor rebuild that fails).
//
//	forward   a host that sweeps a tier names that tier's key   → else an ERROR
//	reverse   a host that names a tier's key sweeps the tier    → else a WARNING
//
// The forward case fails the activation of the host, so it is an error.
// The reverse case is a delete-capable credential nothing reads — the
// state the fleet was in for four days (#395) — and a warning, because a
// tier waiting for its key to be minted is the documented way in.
func pruneFindings(root, fort string) []finding {
	s := readPruneState(root, fort)
	if !s.enabled && len(s.holds) == 0 {
		return nil
	}
	hostNix := filepath.Join("nixos/hosts", fort, "default.nix")
	secretsPath := filepath.Join("nixos/secrets", fort+".yaml")
	var out []finding
	for _, tier := range s.sweeps {
		if hasKey(s.holds, tier) {
			continue
		}
		out = append(out, finding{"error", secretsPath, fmt.Sprintf("sfi.pv-prune sweeps the %s tier in %s but %s is absent — sops-nix fails the activation on a declared-but-absent key. %s", tier, hostNix, pruneEnvKey(tier), pruneKeyHint(tier))})
	}
	for _, tier := range s.holds {
		if hasKey(s.sweeps, tier) {
			continue
		}
		out = append(out, finding{"warning", hostNix, fmt.Sprintf("%s names %s but this host does not sweep the %s tier of sfi.pv-prune — a delete-capable credential nothing reads%s. Enable the tier, or revoke the key and unset it", secretsPath, pruneEnvKey(tier), tier, pruneKeyIdentity(tier))})
	}
	if len(out) == 0 {
		return []finding{{"ok", hostNix, "sfi.pv-prune: sweeps " + strings.Join(s.sweeps, ", ") + ", each tier's restic-prune-s3-env key in " + secretsPath}}
	}
	return out
}

func pruneKeyHint(tier string) string {
	switch tier {
	case pruneTierB2:
		return "Mint it: fort b2-key pruning --write, then sops set it into the file (the verb prints the command)"
	case pruneTierRustfs:
		return "Its value is the `prune` identity of rustfs-central's provider contract (fleet/apps/luxor/rustfs-central)"
	}
	return ""
}

func pruneKeyIdentity(tier string) string {
	if tier == pruneTierB2 {
		return " (" + b2PruningKeyName + ", the one B2 credential that deletes backups)"
	}
	return ""
}

// pruneHolderFindings is the fleet half (--all): the module says "enable
// on exactly one host", and the B2 pruning key is one credential, so
// exactly one declared host names restic-prune-s3-env-b2. Zero is the
// state right after `fort destroy` of the holder — the fleet has no
// pruner and every fort's B2 repo only grows — and a warning, because
// it is also the state before the key is first minted. Two is two places
// the fleet's deleting credential leaks from, an error.
func pruneHolderFindings(root string, hosts map[string]manifest.Host) []finding {
	var holders []string
	for host := range hosts {
		if holdsPruneKey(root, host) {
			holders = append(holders, host)
		}
	}
	sort.Strings(holders)
	key := pruneEnvKey(pruneTierB2)
	switch len(holders) {
	case 0:
		return []finding{{"warning", "nixos/secrets", "no host names " + key + " — the fleet has no B2 pruner (" + b2PruningKeyName + "), so every fort's B2 repo only grows. Enable sfi.pv-prune's b2 tier on one host and mint: fort b2-key pruning --write"}}
	case 1:
		return []finding{{"ok", filepath.Join("nixos/secrets", holders[0]+".yaml"), "the B2 pruner: " + holders[0] + " holds " + key + " (" + b2PruningKeyName + ")"}}
	}
	return []finding{{"error", "nixos/secrets", fmt.Sprintf("%d hosts name %s (%s) — sfi.pv-prune belongs on exactly one host, and %s is one credential; unset it from every host but the pruner", len(holders), key, strings.Join(holders, ", "), b2PruningKeyName)}}
}
