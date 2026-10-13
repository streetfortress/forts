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
			write("estate.toml", "name = \"deployments\"\nrepo = \"ssh://git@gitea.zen.lofi:30022/sfi/deployments\"\n[hubs.monitoring]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/deployments\"\nfort = \"luxor\"\n")
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
	write("estate.toml", "name = \"fixture\"\nrepo = \"ssh://git@gitea.zen.lofi:30022/sfi/fixture-estate\"\n[hubs.monitoring]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/fixture-estate\"\nfort = \"luxor\"\n")
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
