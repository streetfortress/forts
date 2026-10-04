package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
)

// A [cluster] key that reaches nix is declared twice: in the manifest,
// and in the host file that scaffold rendered from it — or, on a host
// that predates the key, that a hand wrote. Nix cannot read
// forts (it is outside the flake's root), so these tests
// are what keep the two equal.
//
// clusterHostFiles is every cluster fort's manifest beside its host
// file; a plain fort has no k3s.
func clusterHostFiles(t *testing.T) (map[string]manifest.Host, func(host string) (file, raw string, ok bool)) {
	t.Helper()
	repo := estateRoot(t)
	hosts, problems, err := manifest.LoadAll(repo)
	if err != nil || len(problems) > 0 {
		t.Fatalf("manifests: %v %v", err, problems)
	}
	clusters := map[string]manifest.Host{}
	for host, entry := range hosts {
		if entry.IsCluster() {
			clusters[host] = entry
		}
	}
	return clusters, func(host string) (string, string, bool) {
		file := filepath.Join("nixos/hosts", host, "default.nix")
		raw, err := os.ReadFile(filepath.Join(repo, file))
		if err != nil {
			t.Errorf("%s: %v", file, err)
			return file, "", false
		}
		return file, string(raw), true
	}
}

// clusterInitLine is a host file setting services.k3s.clusterInit itself
// (an assignment, not the comment that names the mapping).
var clusterInitLine = regexp.MustCompile(`(?m)^\s*services\.k3s\.clusterInit\s*=`)

// The datastore (#426): [cluster] datastore in the manifest (sqlite when
// unset), and sfi.k3s.datastore in the host file — always a line, because
// the datastore is a positive declaration and the host file says it out
// loud. modules/k3s-server.nix owns the mapping to services.k3s.clusterInit,
// so a host file that sets clusterInit itself is a second declaration
// and a finding too.
func TestDatastoreMatchesHostFile(t *testing.T) {
	hosts, read := clusterHostFiles(t)
	for host, entry := range hosts {
		file, raw, ok := read(host)
		if !ok {
			continue
		}
		want := fmt.Sprintf("sfi.k3s.datastore = %q;", entry.Datastore())
		if !strings.Contains(raw, want) {
			t.Errorf("%s says [cluster] datastore = %q, and %s does not set `%s` — scaffold renders the line for a new host; on this one it is a hand edit", manifest.Path(host), entry.Datastore(), file, want)
		}
		if clusterInitLine.MatchString(raw) {
			t.Errorf("%s sets services.k3s.clusterInit — the manifest's [cluster] datastore is the declaration, and modules/k3s-server.nix maps it", file)
		}
	}
}

// The audit log (#626): [cluster] audit_logging in the manifest, and
// sfi.k3s.auditLog.enable in the host file. A fort whose manifest says
// true must set the option, and a fort that sets the option must say so
// in its manifest. A mismatch is a fort that audits without its manifest
// saying so, or a manifest that promises a record no API server writes.
func TestAuditLoggingMatchesHostFile(t *testing.T) {
	hosts, read := clusterHostFiles(t)
	const option = "sfi.k3s.auditLog.enable = true;"
	for host, entry := range hosts {
		file, raw, ok := read(host)
		if !ok {
			continue
		}
		inNix := strings.Contains(raw, option)
		switch {
		case entry.AuditLogging() && !inNix:
			t.Errorf("%s says [cluster] audit_logging = true, and %s does not set `%s` — scaffold renders the block for a new host; on this one it is a hand edit", manifest.Path(host), file, option)
		case !entry.AuditLogging() && inNix:
			t.Errorf("%s sets `%s`, and %s does not say [cluster] audit_logging = true — the manifest is the declaration", file, option, manifest.Path(host))
		}
	}
}
