package main

import (
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Replays dismantle on a copy of the real tree's relevant files for a
// lab fort and the production fort (cloud) with a fake sops tool, and
// checks that exactly the generated surface goes and everything else
// survives. luxor is the bystander whose manifest, sops blocks and a
// tree (rustfs-central) must come through untouched.
func TestDismantle(t *testing.T) {
	repo := estateRoot(t)
	for _, tc := range []struct {
		host      string
		cloud     bool
		recipient int // `# cluster:` lines in the shared rule
	}{
		{"sfi-fort-zenbook", false, 1},
		{"sfi-prod-439bf72", true, 1},
	} {
		t.Run(tc.host, func(t *testing.T) {
			tmp := t.TempDir()
			for _, rel := range []string{manifest.Path(tc.host), manifest.Path("luxor"), ".sops.yaml", "nixos/.sops.yaml", "mesh/peers.json",
				"nixos/secrets/" + tc.host + ".yaml", "flux/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml"} {
				copyFile(t, filepath.Join(repo, rel), filepath.Join(tmp, rel))
			}
			for _, rel := range []string{"nixos/hosts/" + tc.host, "flux/clusters/" + tc.host, "flux/apps/" + tc.host, "flux/apps/luxor/rustfs-central"} {
				copyTree(t, filepath.Join(repo, rel), filepath.Join(tmp, rel))
			}
			// a contract on another fort sourcing from this one
			write := func(rel, content string) {
				os.MkdirAll(filepath.Dir(filepath.Join(tmp, rel)), 0o755)
				os.WriteFile(filepath.Join(tmp, rel), []byte(content), 0o644)
			}
			// a doc that names the host, so residue reporting has something to find
			write("README.md", "# fleet\n\nthe fort "+tc.host+" is mentioned here\n")
			write("estate.toml", "name = \"deployments\"\n[source]\nrepo = \"ssh://git@gitea.zen.lofi:30022/sfi/deployments\"\n")
			// the client side (#434), as the tree before the dismantle has it
			m, _ := loadMesh(tmp)
			hosts, _, _ := manifest.LoadAll(tmp)
			write(sshConfigPath, renderSSHConfig(m, hosts))
			write(knownHostsPath, knownHostsSet(knownHostsSet("", tc.host, m.Peers[tc.host].IP, "ssh-ed25519 GONE"), "luxor", "10.1.0.5", "ssh-ed25519 STAYS"))
			write("flux/apps/luxor/app/forts.contract.yaml", "secrets:\n  secrets.sops.yaml:\n    metadata: {name: a, namespace: a}\n    keys:\n      X: {sops: [../../"+tc.host+"/forts/postgres/secrets-db.sops.yaml, stringData.password]}\n")

			p, err := planDismantle(tmp, tc.host)
			if err != nil {
				t.Fatal(err)
			}
			tool := &fakeTool{}
			if err := p.apply(tool); err != nil {
				t.Fatal(err)
			}
			var sb strings.Builder
			p.report(&sb, true)
			rep := sb.String()

			for _, rel := range []string{"nixos/hosts/" + tc.host, "nixos/secrets/" + tc.host + ".yaml", "flux/clusters/" + tc.host, "flux/apps/" + tc.host} {
				if _, err := os.Stat(filepath.Join(tmp, rel)); err == nil {
					t.Errorf("%s still exists", rel)
				}
			}
			for _, rel := range []string{"flux/apps/luxor/rustfs-central/release.yaml", "nixos/secrets/", "flux/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml"} {
				if _, err := os.Stat(filepath.Join(tmp, rel)); err != nil {
					t.Errorf("%s was removed", rel)
				}
			}
			if exists(tmp, manifest.Path(tc.host)) {
				t.Errorf("manifest %s still exists", manifest.Path(tc.host))
			}
			for _, other := range []string{"luxor"} {
				if !exists(tmp, manifest.Path(other)) {
					t.Errorf("manifest %s was removed", other)
				}
			}
			for _, f := range []string{".sops.yaml", "nixos/.sops.yaml"} {
				s := readFile(t, filepath.Join(tmp, f))
				if strings.Contains(s, "sops rule: "+tc.host) || strings.Contains(s, "# cluster: "+tc.host) {
					t.Errorf("%s still names %s", f, tc.host)
				}
				if !strings.Contains(s, "sops rule: luxor") {
					t.Errorf("%s lost another host's block", f)
				}
			}
			if !strings.Contains(readFile(t, filepath.Join(tmp, ".sops.yaml")), "# cluster: luxor") {
				t.Errorf("shared rule lost luxor's recipient")
			}
			peers := readFile(t, filepath.Join(tmp, "mesh/peers.json"))
			if strings.Contains(peers, `"`+tc.host+`"`) || !strings.Contains(peers, `"luxor"`) {
				t.Errorf("peers.json wrong:\n%s", peers[:200])
			}
			if len(tool.rekeyed) != 1 || !strings.HasSuffix(tool.rekeyed[0], "alertmanager-edge-config.sops.yaml") {
				t.Errorf("updatekeys: %v", tool.rekeyed)
			}
			sc := readFile(t, filepath.Join(tmp, sshConfigPath))
			if strings.Contains(sc, tc.host) || !strings.Contains(sc, "Host luxor\n") || !strings.Contains(rep, "◆ mesh/ssh_config") {
				t.Errorf("ssh_config after dismantle:\n%s\n%s", sc, rep)
			}
			if kh := parseKnownHosts(readFile(t, filepath.Join(tmp, knownHostsPath))); len(kh) != 1 || kh["luxor"].key != "ssh-ed25519 STAYS" || !strings.Contains(rep, "◆ mesh/known_hosts") {
				t.Errorf("known_hosts after dismantle: %+v\n%s", kh, rep)
			}
			if strings.Contains(rep, "mesh/ssh_config:") || strings.Contains(rep, "mesh/known_hosts:") {
				t.Errorf("the rendered files are not residue:\n%s", rep)
			}
			if !strings.Contains(rep, "README.md:") {
				t.Errorf("residue should list README.md:\n%s", rep)
			}
			if !strings.Contains(rep, "flux/apps/luxor/app/forts.contract.yaml: secrets.sops.yaml/X") {
				t.Errorf("cross-fort ref not reported:\n%s", rep)
			}
			want := "- 2 `# cluster:"
			if tc.recipient == 1 {
				want = "- 1 `# cluster:"
			}
			if !strings.Contains(rep, want) {
				t.Errorf("recipient count: want %q in\n%s", want, rep)
			}
		})
	}
}

func TestDismantleEdits(t *testing.T) {
	if _, err := removeMarkerBlock("x\n", "h"); err == nil {
		t.Error("missing block should error")
	}
}

// The residue report is for the reviewer, so it counts only what the
// retirement leaves for the reviewer. Three files are not residue: the
// console registry and the hub's rustfs users file (apply unsets the
// host's entry in both) and the stack config (`fort destroy` deletes it
// after the merge). The doc that names the host IS residue.
func TestDismantleResidueSkipsWhatRetirementSettles(t *testing.T) {
	const host = "dismantle-residue"
	const admin = "age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpq0d5e6m"
	tmp := t.TempDir()
	write := func(rel, content string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(tmp, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("estate.toml", "name = \"fixture\"\n[source]\nrepo = \"ssh://git@gitea.zen.lofi:30022/sfi/fixture-estate\"\n")
	write(manifest.Path(host), "mesh_ip = \"10.1.0.94\"\n[cluster]\n[libvirt]\nip = \"10.0.0.94\"\n")
	write("nixos/.sops.yaml", "creation_rules:\n  - path_regex: secrets/.*\\.yaml$\n    age:\n      - "+admin+" # admin\n")
	write(".sops.yaml", "creation_rules:\n  - path_regex: provisioning/.*\\.yaml$\n    age:\n      - "+admin+" # admin\n")
	write("mesh/peers.json", "{\n  \"peers\": {},\n  \"clients\": {}\n}\n")
	// the two files the retirement settles
	write(consoleRegistry, host+": ENC[AES256_GCM,data:aaaa,type:str]\nsops:\n    age: []\n")
	write(stackConfigPath(host), "encryptionsalt: v1:abc\nconfig:\n  forts:name: "+host+"\n")
	// and one the reviewer must still see
	write("README.md", "# fleet\n\nthe fort "+host+" is mentioned here\n")

	p, err := planDismantle(tmp, host)
	if err != nil {
		t.Fatal(err)
	}
	if !p.consolePass || !p.stack {
		t.Fatalf("plan: consolePass=%v stack=%v", p.consolePass, p.stack)
	}
	if len(p.residue) != 1 || !strings.HasPrefix(p.residue[0], "README.md:") {
		t.Errorf("residue: want README.md alone, got %v", p.residue)
	}
	var sb strings.Builder
	p.report(&sb, true)
	rep := sb.String()
	// the report names the settled secret, so the skip is not silence
	for _, want := range []string{
		"! residue — 1 file(s) still mention " + host + ":",
		"○ " + consoleRegistry,
		"Dismantle staged. Merge, then run `fort destroy " + host + "` to delete the pulumi stack.",
	} {
		if !strings.Contains(rep, want) {
			t.Errorf("report misses %q:\n%s", want, rep)
		}
	}
	for _, unwanted := range []string{"triage", "reviewer's call", stackConfigPath(host) + ":"} {
		if strings.Contains(rep, unwanted) {
			t.Errorf("report still says %q:\n%s", unwanted, rep)
		}
	}
}

// The cascade (#180): each installed layer's dismantle runs before the
// core's, in name order, with --write passed through; a layer's failure
// stops the cascade, says the core did not proceed, and the layers
// after it never run. No layers installed is a no-op.
func TestDismantleCascade(t *testing.T) {
	root := t.TempDir()
	for _, l := range []string{"monitoring", "ca"} {
		if err := os.MkdirAll(filepath.Join(root, layersDir, l), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var calls []string
	rec := func(layer string, args ...string) error {
		calls = append(calls, layer+": "+strings.Join(args, " "))
		return nil
	}
	var out strings.Builder
	if err := cascadeLayerDismantles(&out, root, "sfi-x", true, rec); err != nil {
		t.Fatal(err)
	}
	if want := "ca: dismantle sfi-x --write,monitoring: dismantle sfi-x --write"; strings.Join(calls, ",") != want {
		t.Errorf("calls: %v, want %s", calls, want)
	}
	if !strings.Contains(out.String(), "fort ca dismantle sfi-x --write") {
		t.Errorf("the cascade does not name the layer's verb:\n%s", out.String())
	}

	// the dry run cascades the layers' dry runs
	calls = nil
	if err := cascadeLayerDismantles(&out, root, "sfi-x", false, rec); err != nil {
		t.Fatal(err)
	}
	if want := "ca: dismantle sfi-x,monitoring: dismantle sfi-x"; strings.Join(calls, ",") != want {
		t.Errorf("dry-run calls: %v, want %s", calls, want)
	}

	// a layer's failure stops the cascade
	calls = nil
	err := cascadeLayerDismantles(&out, root, "sfi-x", true, func(layer string, args ...string) error {
		calls = append(calls, layer)
		return os.ErrPermission
	})
	if err == nil || !strings.Contains(err.Error(), "fort-ca dismantle") || !strings.Contains(err.Error(), "the core's dismantle did not run") {
		t.Errorf("err: %v", err)
	}
	if strings.Join(calls, ",") != "ca" {
		t.Errorf("a layer ran after ca failed: %v", calls)
	}

	// no layers installed: nothing runs
	if err := cascadeLayerDismantles(&out, t.TempDir(), "sfi-x", true, func(string, ...string) error {
		t.Fatal("a layer ran in an estate with none")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// runLayerDismantle reaches the binary the way execLayer does: from
// PATH, with FORT_ESTATE_ROOT set; a missing binary is an error that
// names it, and the layer's exit status comes back.
func TestRunLayerDismantle(t *testing.T) {
	bin := t.TempDir()
	rec := filepath.Join(bin, "rec")
	if err := os.WriteFile(filepath.Join(bin, "fort-fake"), []byte("#!/bin/sh\necho \"$FORT_ESTATE_ROOT $*\" > "+rec+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "fort-bad"), []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	root := t.TempDir()
	run := runLayerDismantle(root)
	if err := run("fake", "dismantle", "sfi-x", "--write"); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(readFile(t, rec)); got != root+" dismantle sfi-x --write" {
		t.Errorf("the layer saw: %q", got)
	}
	if err := run("gone", "dismantle", "sfi-x"); err == nil || !strings.Contains(err.Error(), "no fort-gone on PATH") {
		t.Errorf("missing binary: %v", err)
	}
	if err := run("bad", "dismantle", "sfi-x"); err == nil {
		t.Error("a failing layer must come back as an error")
	}
}

// Without a stack the next step is the --no-stack destroy, and a dry run
// never names a next step at all.
func TestDismantleNextStep(t *testing.T) {
	p := &dismantlePlan{host: "metal", entry: manifest.Host{Provider: "baremetal"}}
	var sb strings.Builder
	p.report(&sb, true)
	if !strings.Contains(sb.String(), "Dismantle staged. Merge, then run `fort destroy metal --no-stack` to revoke the fort's identities.") {
		t.Errorf("no stack:\n%s", sb.String())
	}
	sb.Reset()
	p.report(&sb, false)
	if !strings.Contains(sb.String(), "dry run — nothing changed") || strings.Contains(sb.String(), "Dismantle staged") {
		t.Errorf("dry run:\n%s", sb.String())
	}
}
