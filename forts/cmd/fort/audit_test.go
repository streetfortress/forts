package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

// The audit log is declared twice (#626): [cluster] audit_logging in the
// manifest, and sfi.k3s.auditLog.enable in the host file that scaffold
// rendered from it — or, on a host that predates the key, that a hand
// wrote. Nix cannot read provisioning/forts (it is outside the flake's
// root), so this is what keeps the two equal, in the shape of
// TestDevAccountsMatchDevFortModule: a fort whose manifest says true must
// set the option, and a fort that sets the option must say so in its
// manifest. A mismatch is a fort that audits without its manifest saying
// so, or a manifest that promises a record no API server writes.
func TestAuditLoggingMatchesHostFile(t *testing.T) {
	repo, err := repoRootFrom(mustGetwd(t))
	if err != nil {
		t.Skip("not inside the deployments checkout")
	}
	hosts, problems, err := manifest.LoadAll(repo)
	if err != nil || len(problems) > 0 {
		t.Fatalf("manifests: %v %v", err, problems)
	}
	const option = "sfi.k3s.auditLog.enable = true;"
	for host, entry := range hosts {
		if entry.IsDev() {
			continue // no k3s, and manifest.Validate refuses [cluster] there
		}
		file := filepath.Join("nixos/hosts", host, "default.nix")
		raw, err := os.ReadFile(filepath.Join(repo, file))
		if err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		inNix := strings.Contains(string(raw), option)
		switch {
		case entry.AuditLogging() && !inNix:
			t.Errorf("%s says [cluster] audit_logging = true, and %s does not set `%s` — scaffold renders the block for a new host; on this one it is a hand edit", manifest.Path(host), file, option)
		case !entry.AuditLogging() && inNix:
			t.Errorf("%s sets `%s`, and %s does not say [cluster] audit_logging = true — the manifest is the declaration", file, option, manifest.Path(host))
		}
	}
}
