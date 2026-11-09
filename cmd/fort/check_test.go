package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

// The manifest gate: LoadAll's problems become findings, one per bad
// file, named by the file, sorted, the path prefix stripped from the
// message (the dir column carries it).
func TestManifestFindings(t *testing.T) {
	root := t.TempDir()
	write := func(host, content string) {
		os.MkdirAll(filepath.Join(root, manifest.Dir), 0o755)
		os.WriteFile(filepath.Join(root, manifest.Path(host)), []byte(content), 0o644)
	}
	write("d", "mesh_ip = \"10.1.0.2\"\n")
	write("a", "mesh_ip = \"10.1.0.1\"\n[cluster]\n[libvirt]\nip = \"10.0.0.1\"\n")
	write("b", "[libvirt]\nip = \"10.0.0.2\"\n[linode]\nregion = \"r\"\ntype = \"t\"\n")
	hosts, problems, err := manifest.LoadAll(root)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("hosts %v err %v", hosts, err)
	}
	got := manifestFindings(problems)
	if len(got) != 2 || got[0].dir != manifest.Path("b") || !strings.HasPrefix(got[0].msg, "2 provider tables") || got[1].dir != manifest.Path("d") || !strings.HasPrefix(got[1].msg, "no provider table") || got[0].level != "error" {
		t.Errorf("findings: %+v", got)
	}
	if len(manifestFindings(nil)) != 0 {
		t.Error("no problems, no findings")
	}
}

// The operator keys (forts#81): the framework ships none, so the gate
// reads estate.toml — an estate that names no device, the devices it
// names, a key that names no device, and a host file that replaces the
// estate's list for one fort.
func TestOperatorFindings(t *testing.T) {
	const (
		laptop = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJDZXdVYO58sXRAwLieovSB3fHDAhMF96rjyfCx+vLuL macbook-air"
		bare   = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBL2o6nnz49u8AXHFIojZiJWJkOFgzH3hplflKDiVz/i"
	)
	root := testTree(t)
	hosts := testHosts(t)
	est := func(keys ...string) estate.Estate {
		return estate.Estate{Name: "deployments", Source: estate.Source{Repo: "ssh://git@gitea.zen.lofi:30022/sfi/deployments"}, OperatorKeys: keys}
	}
	// No operator_keys: an error on estate.toml — no fort of the estate
	// evaluates, and the finding says what to add.
	fs := operatorFindings(root, est(), hosts)
	if len(fs) != 1 || fs[0].level != "error" || fs[0].dir != estate.Path || !strings.Contains(fs[0].msg, "no operator_keys") {
		t.Errorf("no operator keys: %+v", fs)
	}
	// One device: an info line that names it.
	fs = operatorFindings(root, est(laptop), hosts)
	if len(fs) != 1 || fs[0].level != "info" || fs[0].dir != "ops" || fs[0].msg != "macbook-air" {
		t.Errorf("one device: %+v", fs)
	}
	// A key with no comment: a warning beside the info line, because
	// vetting (#415) trims the list by device.
	fs = operatorFindings(root, est(laptop, bare), hosts)
	if len(fs) != 2 || fs[0].level != "warning" || !strings.Contains(fs[0].msg, "operator_keys #2 carries no comment") || fs[1].level != "info" || !strings.Contains(fs[1].msg, "macbook-air, #2 (no device named)") {
		t.Errorf("a key with no device: %+v", fs)
	}
	// A host file that sets the option: one warning per line, named by
	// the host — a device added to the estate does not reach that fort.
	hostNix := filepath.Join("nixos/hosts/luxor/default.nix")
	os.MkdirAll(filepath.Join(root, filepath.Dir(hostNix)), 0o755)
	os.WriteFile(filepath.Join(root, hostNix),
		[]byte("{\n  # sfi.ops.account.authorizedKeys is the estate's\n  sfi.ops.account.authorizedKeys = [ \"ssh-ed25519 AAAA other\" ];\n}\n"), 0o644)
	fs = operatorFindings(root, est(laptop), hosts)
	if len(fs) != 2 || fs[0].level != "warning" || fs[0].dir != hostNix || !strings.Contains(fs[0].msg, "line 3 sets the operator keys of this fort alone") || fs[1].level != "info" {
		t.Errorf("a host that overrides: %+v", fs)
	}
	// Every host of the estate overrides and the estate names none: a
	// warning, not an error — the forts that exist have an operator, and
	// the next one has none.
	fs = operatorFindings(root, est(), map[string]manifest.Host{"luxor": hosts["luxor"]})
	if len(fs) != 2 || fs[1].level != "warning" || !strings.Contains(fs[1].msg, "every host file names its own operator keys") {
		t.Errorf("every host overrides: %+v", fs)
	}
}

// The forge kind (forts#1): derived from the host of repo, or declared;
// two info lines — the repo and the forge — and the forge line names
// the API and the token the two verbs use.
func TestSourceFindings(t *testing.T) {
	fs := sourceFindings(estate.Estate{Source: estate.Source{Repo: "ssh://git@github.com/acme/estate"}})
	if len(fs) != 2 || fs[0].level != "info" || fs[0].dir != "repo" || fs[1].level != "info" || fs[1].dir != "forge" || !strings.Contains(fs[1].msg, "github — ssh") || !strings.Contains(fs[1].msg, "https://api.github.com") || !strings.Contains(fs[1].msg, "github-token") {
		t.Errorf("github: %+v", fs)
	}
	fs = sourceFindings(estate.Estate{Source: estate.Source{Repo: "ssh://git@forge.example:30022/acme/estate", Forge: "gitea"}})
	if len(fs) != 2 || fs[1].level != "info" || !strings.Contains(fs[1].msg, "gitea — ssh") || !strings.Contains(fs[1].msg, "https://forge.example/api/v1") || !strings.Contains(fs[1].msg, "gitea-token") {
		t.Errorf("gitea: %+v", fs)
	}
	if fs = sourceFindings(estate.Estate{Source: estate.Source{Repo: "ssh://forge.example/"}}); len(fs) != 1 || fs[0].level != "error" {
		t.Errorf("bad repo: %+v", fs)
	}
}

// The host-key finding (forts#139, #167): on the ssh route an estate
// that names none has no fort that evaluates, and the gate says so; on
// the anonymous route there is no key to pin, and a key named anyway is
// unread — a warning, not a refusal.
func TestKnownHostsFindings(t *testing.T) {
	ssh := estate.Estate{Source: estate.Source{Repo: "ssh://git@github.com/acme/estate"}}
	if fs := knownHostsFindings(ssh); len(fs) != 1 || fs[0].level != "error" || !strings.Contains(fs[0].msg, "no known_hosts") {
		t.Errorf("ssh, no pin: %+v", fs)
	}
	ssh.Source.KnownHosts = "github.com ssh-ed25519 AAAA"
	if fs := knownHostsFindings(ssh); len(fs) != 1 || fs[0].level != "ok" || !strings.Contains(fs[0].msg, "pins the forge's host key (github.com)") {
		t.Errorf("ssh, pinned: %+v", fs)
	}
	anon := estate.Estate{Source: estate.Source{Repo: "https://github.com/acme/estate"}}
	if fs := knownHostsFindings(anon); len(fs) != 0 {
		t.Errorf("anonymous: no key to pin, nothing to say: %+v", fs)
	}
	anon.Source.KnownHosts = "github.com ssh-ed25519 AAAA"
	if fs := knownHostsFindings(anon); len(fs) != 1 || fs[0].level != "warning" || !strings.Contains(fs[0].msg, "reads no host key") {
		t.Errorf("anonymous with a pin: %+v", fs)
	}
}

// The source finding on the anonymous route (forts#167): the kind is
// still named, the route is named, and the line says no deploy key is
// registered and no token read.
func TestSourceFindingsAnonymous(t *testing.T) {
	fs := sourceFindings(estate.Estate{Source: estate.Source{Repo: "https://github.com/acme/estate"}})
	if len(fs) != 2 || fs[0].level != "info" || fs[0].msg != "https://github.com/acme/estate.git" || !strings.Contains(fs[1].msg, "github — anonymous https") || !strings.Contains(fs[1].msg, "no github-token") || strings.Contains(fs[1].msg, "api.github.com") {
		t.Errorf("anonymous github: %+v", fs)
	}
	fs = sourceFindings(estate.Estate{Source: estate.Source{Repo: "https://forge.example/acme/estate", Auth: "none"}})
	if len(fs) != 2 || !strings.Contains(fs[1].msg, "gitea — anonymous https") || !strings.Contains(fs[1].msg, "no gitea-token") {
		t.Errorf("anonymous gitea, declared: %+v", fs)
	}
	fs = sourceFindings(estate.Estate{Source: estate.Source{Repo: "ssh://git@github.com/acme/estate", Auth: "ssh"}})
	if len(fs) != 2 || !strings.Contains(fs[1].msg, "github — ssh") || !strings.Contains(fs[1].msg, "https://api.github.com") {
		t.Errorf("ssh, declared: %+v", fs)
	}
}

// evalHost prints the section, counts a failure as one error, and
// annotates it under a workflow; a pass costs nothing.
func TestEvalHost(t *testing.T) {
	saved := nixEval
	defer func() { nixEval = saved }()

	nixEval = func(root, host string) (string, error) {
		return "/nix/store/p8ci7rcb5qg61m8j4qvganhwb5927vdc-nixos-system-lab-fort.drv", nil
	}
	if got := evalHost("/estate", "lab-fort", false); got != 0 {
		t.Fatalf("passing eval: want 0 errors, got %d", got)
	}

	nixEval = func(root, host string) (string, error) { return "", errors.New("evaluation aborted") }
	if got := evalHost("/estate", "lab-fort", true); got != 1 {
		t.Fatalf("failing eval: want 1 error, got %d", got)
	}
}

// drvName trims the store hash: the derivation's name is the proof a
// reader can check by eye, the hash is not.
func TestDrvName(t *testing.T) {
	if got := drvName("/nix/store/p8ci7rcb5qg61m8j4qvganhwb5927vdc-nixos-system-outpost-26.11.drv\n"); got != "nixos-system-outpost-26.11.drv" {
		t.Errorf("drvName: %q", got)
	}
	if got := drvName("not-a-store-path"); got != "not-a-store-path" {
		t.Errorf("drvName fallback: %q", got)
	}
}
