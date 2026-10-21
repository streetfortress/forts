package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

const testWGPub = "9n6EdeMmYeyeiDQRIotqWqwDkNUgqMyBZIkHQj1cMUI="

// withLink is a fixture manifest with one [links.<name>] table added in
// memory: the hub shape, without a second fixture file.
func withLink(t *testing.T, host string) manifest.Host {
	t.Helper()
	entry, err := manifest.Load("testdata", host)
	if err != nil {
		t.Fatal(err)
	}
	entry.Links = map[string]manifest.Link{"staging": {Name: "staging", Estate: "ssh://git@gitea.zen.lofi:30022/sfi/staging", Address: "10.2.0.254", Port: 51821, Interface: "wg-staging", Zone: "staging.mesh", PublicKey: testWGPub}}
	if err := entry.Validate(host); err != nil {
		t.Fatal(err)
	}
	return entry
}

// A host with a link renders the module import and the sfi.links.<name>
// block from the manifest (forts#55) — on every shape; a host without
// one renders neither.
func TestScaffoldRendersLinks(t *testing.T) {
	for _, host := range []string{"lab-fort", "cloud-fort", "plain-fort"} {
		entry := withLink(t, host)
		var nix string
		for _, f := range render(t, host, entry) {
			if f.path == filepath.Join("nixos/hosts", host, "default.nix") {
				nix = string(f.content)
				if f.edit == nil {
					t.Errorf("%s: a host file with links carries the edit for an existing file", host)
				}
			}
		}
		for _, s := range []string{"    " + linksImport + "\n", "  sfi.links.staging = {\n", `    repo = "ssh://git@gitea.zen.lofi:30022/sfi/staging.git";`, `    interface = "wg-staging";`, `    zone = "staging.mesh";`, "    port = 51821;", `    address = "10.2.0.254";`, "wireguard-private-key-staging", "sfi/staging"} {
			if !strings.Contains(nix, s) {
				t.Errorf("%s default.nix lacks %q:\n%s", host, s, nix)
			}
		}
		if strings.Contains(nix, "public_key") || strings.Contains(nix, testWGPub) {
			t.Errorf("%s: the public key is the manifest's, not the host file's", host)
		}
		plain, err := manifest.Load("testdata", host)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range render(t, host, plain) {
			if f.path == filepath.Join("nixos/hosts", host, "default.nix") {
				if strings.Contains(string(f.content), "sfi.links") || strings.Contains(string(f.content), "nixosModules.links") || f.edit != nil {
					t.Errorf("%s: a host without links renders a links block", host)
				}
				// The imports list closes right after its last entry, as it
				// did before the links block existed: no stray blank line.
				if strings.Contains(string(f.content), "\n\n  ];") {
					t.Errorf("%s: a blank line before the end of the imports list:\n%s", host, f.content)
				}
			}
		}
		if strings.Contains(nix, "\n\n  ];") || !strings.Contains(nix, linksImport+"\n  ];") {
			t.Errorf("%s: with a link, the import is the last entry and the list closes right after it:\n%s", host, nix)
		}
	}
}

// The append (a hub that exists when it gains a link): the import lands
// as the last entry of the list, the block before the closing brace,
// both once — a re-run is a no-op, and a second link adds only itself.
func TestEnsureLinks(t *testing.T) {
	plain, err := manifest.Load("testdata", "plain-fort")
	if err != nil {
		t.Fatal(err)
	}
	var fresh string
	for _, f := range render(t, "plain-fort", plain) {
		if f.path == "nixos/hosts/plain-fort/default.nix" {
			fresh = string(f.content)
		}
	}
	entry := withLink(t, "plain-fort")
	out, err := ensureLinks(fresh, entry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, linksImport) != 1 || strings.Count(out, "sfi.links.staging = {") != 1 {
		t.Errorf("append:\n%s", out)
	}
	if !strings.Contains(out, "    "+linksImport+"\n  ];\n") {
		t.Errorf("the import is not the last entry of the list:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimRight(out, "\n"), "  };\n}") {
		t.Errorf("the block is not before the closing brace:\n%s", out[len(out)-300:])
	}
	again, err := ensureLinks(out, entry)
	if err != nil || again != out {
		t.Errorf("re-run changed the file: %v", err)
	}
	entry.Links["production"] = manifest.Link{Name: "production", Estate: "ssh://git@gitea.zen.lofi:30022/sfi/production", Address: "10.3.0.254", Port: 51822, PublicKey: testWGPub}
	two, err := ensureLinks(out, entry)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(two, linksImport) != 1 || strings.Count(two, "sfi.links.staging = {") != 1 || strings.Count(two, "sfi.links.production = {") != 1 || !strings.Contains(two, `    interface = "wg-production";`) || !strings.Contains(two, `    zone = "production.mesh";`) {
		t.Errorf("second link:\n%s", two)
	}
	// A hand-written block under the same name is kept, never doubled —
	// spelled plain, or with the name quoted.
	for _, block := range []string{"sfi.links.staging = { port = 51821; address = \"10.2.0.254\"; };", "sfi.links.\"staging\" = { port = 51821; };", "sfi.links.staging.port = 51821;"} {
		hand := strings.Replace(fresh, "\n}", "\n  "+block+"\n}", 1)
		kept, err := ensureLinks(hand, withLink(t, "plain-fort"))
		if err != nil || strings.Count(kept, "sfi.links") != 1 {
			t.Errorf("hand-written block %q: %v\n%s", block, err, kept)
		}
	}
	// A comment that names the block is not the block; a longer name
	// this one is a prefix of is not it either.
	commented := strings.Replace(fresh, "\n}", "\n  # TODO: sfi.links.staging later\n}", 1)
	if got, err := ensureLinks(commented, withLink(t, "plain-fort")); err != nil || strings.Count(got, "sfi.links.staging = {") != 1 {
		t.Errorf("a comment is not the block: %v\n%s", err, got)
	}
	prod := withLink(t, "plain-fort")
	prod.Links = map[string]manifest.Link{"prod": {Name: "prod", Estate: "ssh://git@gitea.zen.lofi:30022/sfi/prod", Address: "10.3.0.254", Port: 51822, PublicKey: testWGPub}}
	longer := strings.Replace(fresh, "\n}", "\n  sfi.links.production = { port = 51823; };\n}", 1)
	if got, err := ensureLinks(longer, prod); err != nil || strings.Count(got, "sfi.links.prod = {") != 1 || strings.Count(got, "sfi.links.production = {") != 1 {
		t.Errorf("a longer name is not this block: %v\n%s", err, got)
	}
	for text, want := range map[string]bool{
		"  sfi.links.staging = {":        true,
		"sfi.links.staging.port = 1;":    true,
		`  sfi.links."staging" = { };`:   true,
		"  # sfi.links.staging = {":      false,
		"  sfi.links.staging-2 = {":      false,
		"  sfi.links.stagingx = {":       false,
		"  x = 1; # sfi.links.staging =": false,
	} {
		if got := nixSetsAttr(text, "sfi.links.staging"); got != want {
			t.Errorf("nixSetsAttr(%q) = %v", text, got)
		}
	}
	// Files that lost the shapes say so.
	if _, err := nixEnsureImport("{ }\n", linksImport); err == nil || !strings.Contains(err.Error(), "imports") {
		t.Errorf("no imports list: %v", err)
	}
	if _, err := nixEnsureBlock("{ imports = [ ]; ", "sfi.links.x", "  x = 1;"); err == nil {
		t.Error("no closing brace: want an error")
	}
	// The scaffold loop applies the edit to a file that exists.
	tmp := t.TempDir()
	copyManifest(t, "testdata", tmp, "plain-fort")
	os.MkdirAll(filepath.Join(tmp, "nixos/hosts/plain-fort"), 0o755)
	os.WriteFile(filepath.Join(tmp, "nixos/hosts/plain-fort/default.nix"), []byte(fresh), 0o644)
	for _, f := range render(t, "plain-fort", entry) {
		if f.path == "nixos/hosts/plain-fort/default.nix" {
			want, _ := ensureLinks(fresh, entry)
			if got, err := f.edit(fresh); err != nil || got != want {
				t.Errorf("scaffold's edit differs from ensureLinks: %v", err)
			}
			if !strings.Contains(f.editNote, "sfi.links.production, sfi.links.staging") {
				t.Errorf("edit note: %q", f.editNote)
			}
		}
	}
}

// The mirror test (docs/manifest.md): nix cannot read the manifest, so
// [links.<name>] in a manifest and sfi.links.<name> in the host file are
// kept equal here — a link the manifest declares that the host file does
// not set is a hub with no interface, and a block the manifest does not
// know is a link no `fort estate link` will ever record. Runs in an
// estate checkout; skips in sfi/forts.
func TestLinksMatchHostFile(t *testing.T) {
	repo := estateRoot(t)
	hosts, problems, err := manifest.LoadAll(repo)
	if err != nil || len(problems) > 0 {
		t.Fatalf("manifests: %v %v", err, problems)
	}
	for host, entry := range hosts {
		file := filepath.Join("nixos/hosts", host, "default.nix")
		raw, err := os.ReadFile(filepath.Join(repo, file))
		if err != nil {
			continue // scaffold has not run; fort check is that finding
		}
		for _, name := range entry.LinkNames() {
			if !nixSetsAttr(string(raw), "sfi.links."+name) {
				t.Errorf("%s declares [links.%s], and %s does not set sfi.links.%s — `fort scaffold %s --write` appends the block", manifest.Path(host), name, file, name, host)
			}
		}
		for _, l := range strings.Split(string(raw), "\n") {
			code := strings.TrimSpace(l)
			if i := strings.Index(code, "#"); i >= 0 {
				code = strings.TrimSpace(code[:i])
			}
			if !strings.HasPrefix(code, "sfi.links.") {
				continue
			}
			name := strings.Trim(strings.Fields(strings.TrimPrefix(code, "sfi.links."))[0], "\"")
			if i := strings.IndexAny(name, ".="); i >= 0 {
				name = name[:i]
			}
			if _, ok := entry.Links[name]; !ok {
				t.Errorf("%s sets sfi.links.%s, and %s has no [links.%s] — the manifest is the declaration (`fort link add %s <estate url>` writes it)", file, name, manifest.Path(host), name, host)
			}
		}
	}
}

// The gate's view of the links (forts#55): tables and host files agree,
// a link's names are not this mesh's, and the public key is the half the
// sops secret derives — where the secrets file decrypts; elsewhere the
// key is reported as not verified, a skip, never a failure.
func TestLinkFindings(t *testing.T) {
	root, tool := linkEstate(t)
	est, _ := estateLoadT(t, root)
	// the link, as fort link add writes it: table, block, secret
	p, err := planLinkAdd(root, "hub", stagingURL, linkAddOpts{}, tool, dirFetcher(stagingFix))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.apply(tool); err != nil {
		t.Fatal(err)
	}
	secrets := filepath.Join(root, "nixos/secrets/hub.yaml")
	withKey := &fakeTool{plain: map[string]string{secrets: tool.plain[secrets] + "wireguard-private-key-staging: " + p.wg.private + "\n"}}
	hosts := loadHostsT(t, root)
	only := func(fs []finding, level string) []finding {
		var out []finding
		for _, f := range fs {
			if f.level == level {
				out = append(out, f)
			}
		}
		return out
	}
	fs := linkFindings(root, est, hosts, withKey)
	if len(fs) != 1 || fs[0].level != "ok" || !strings.Contains(fs[0].msg, "public keys match") {
		t.Errorf("verified: %+v", fs)
	}
	// no age key here: the decrypt fails, the key is not verified, nothing is red
	fs = linkFindings(root, est, hosts, &fakeTool{})
	if len(fs) != 1 || fs[0].level != "ok" || !strings.Contains(fs[0].msg, "not verified (no age key here)") {
		t.Errorf("unverified: %+v", fs)
	}
	// a host with no links contributes nothing
	if fs := linkFindings(root, est, map[string]manifest.Host{"other": hosts["other"]}, withKey); fs != nil {
		t.Errorf("no links: %+v", fs)
	}
	// the secret is not the one the table names
	foreign := &fakeTool{plain: map[string]string{secrets: tool.plain[secrets] + "wireguard-private-key-staging: dwdtDXMYpX08FsFyUbJmRd9MLofrwJkqsXf7pR25LCo=\n"}}
	if fs := only(linkFindings(root, est, hosts, foreign), "error"); len(fs) != 1 || !strings.Contains(fs[0].msg, "is not what wireguard-private-key-staging derives") {
		t.Errorf("foreign key: %+v", fs)
	}
	// the private half is gone
	if fs := only(linkFindings(root, est, hosts, &fakeTool{plain: map[string]string{secrets: tool.plain[secrets]}}), "error"); len(fs) != 1 || !strings.Contains(fs[0].msg, "private half is not in the secrets file") {
		t.Errorf("missing key: %+v", fs)
	}
	// the host file lost the block
	nixPath := filepath.Join(root, "nixos/hosts/hub/default.nix")
	nix := readFile(t, nixPath)
	os.WriteFile(nixPath, []byte(strings.Replace(nix, "sfi.links.staging", "sfi.links.stage", 1)), 0o644)
	if fs := only(linkFindings(root, est, hosts, withKey), "error"); len(fs) != 1 || !strings.Contains(fs[0].msg, "no sfi.links.staging") || fs[0].dir != "nixos/hosts/hub/default.nix" {
		t.Errorf("no block: %+v", fs)
	}
	os.WriteFile(nixPath, []byte(nix), 0o644)
	// the link's names are this mesh's, or the link is to this estate
	own := est
	own.Mesh.Interface, own.Mesh.Zone = "wg-staging", ""
	if fs := only(linkFindings(root, own, hosts, withKey), "error"); len(fs) != 1 || !strings.Contains(fs[0].msg, "own mesh interface") {
		t.Errorf("own interface: %+v", fs)
	}
	own.Mesh.Interface, own.Mesh.Zone = "", "staging.mesh"
	if fs := only(linkFindings(root, own, hosts, withKey), "error"); len(fs) != 1 || !strings.Contains(fs[0].msg, "own mesh zone") {
		t.Errorf("own zone: %+v", fs)
	}
	self := hosts["hub"]
	self.Links = map[string]manifest.Link{"staging": {Name: "staging", Estate: infraURL, Address: "10.1.0.250", Port: 51821, PublicKey: p.wg.public}}
	if fs := only(linkFindings(root, est, map[string]manifest.Host{"hub": self}, withKey), "error"); len(fs) < 1 || !strings.Contains(fs[0].msg, "names this estate") {
		t.Errorf("link to self: %+v", fs)
	}
}

func estateLoadT(t *testing.T, root string) (estate.Estate, error) {
	t.Helper()
	est, err := estate.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return est, nil
}

func loadHostsT(t *testing.T, root string) map[string]manifest.Host {
	t.Helper()
	hosts, problems, err := manifest.LoadAll(root)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	return hosts
}
