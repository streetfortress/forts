package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The config the tests share: the init-rendered shape — provisioning,
// one fort rule, the shared marked rule — plus the two shapes a layer
// brings: a marked rule for its own tree, and an unmarked one whose
// files are the layer's own business.
const rulesFixture = `creation_rules:
  - path_regex: provisioning/.*\.yaml$
    age:
      - age1admin # admin
  - path_regex: flux/(clusters|apps)/luxor/.*
    encrypted_regex: ^(data|stringData)$
    age:
      - age1admin # admin
      - age1luxor # luxor
  - path_regex: flux/infrastructure/.*
    encrypted_regex: ^(data|stringData)$
    age:
      - age1admin # admin
      # fort-managed: one recipient per cluster
      - age1luxor # cluster: luxor
  - path_regex: layers/monitoring/.*
    encrypted_regex: ^(data|stringData)$
    age:
      - age1admin # admin
      # fort-managed: one recipient per cluster
  - path_regex: layers/private/.*
    age:
      - age1admin # admin
`

func TestSopsRules(t *testing.T) {
	rules := sopsRules(rulesFixture)
	if len(rules) != 5 {
		t.Fatalf("rules: %d", len(rules))
	}
	for i, managed := range []bool{false, false, true, true, false} {
		if rules[i].managed != managed {
			t.Errorf("rule %d managed = %v", i, rules[i].managed)
		}
	}
	if !rules[2].regex.MatchString("flux/infrastructure/configs/x.sops.yaml") || rules[1].regex.MatchString("flux/infrastructure/x.yaml") {
		t.Error("regexes did not compile to sops' meaning")
	}
}

// fortManagedFiles: first match wins the way sops picks a creation
// rule, so a fort's own file never rides another fort's mint; the
// marked rules' files do, wherever their tree sits; nixos/ answers to
// its own config and is never a candidate.
func TestFortManagedFiles(t *testing.T) {
	tmp := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(tmp, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tmp, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const encrypted = "data: x\nsops:\n  version: 3\n"
	write(".sops.yaml", rulesFixture)
	write("flux/infrastructure/configs/monitoring/alertmanager.sops.yaml", encrypted)
	write("flux/infrastructure/configs/plain.yaml", "data: x\n")
	write("flux/apps/luxor/secrets.sops.yaml", encrypted)
	write("layers/monitoring/flux/webhook.sops.yaml", encrypted)
	write("layers/private/own.sops.yaml", encrypted)
	write("layers/unruled/orphan.sops.yaml", encrypted)
	write("nixos/secrets/luxor.yaml", encrypted)
	write("provisioning/secrets.yaml", encrypted)

	files, err := fortManagedFiles(tmp)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"flux/infrastructure/configs/monitoring/alertmanager.sops.yaml",
		"layers/monitoring/flux/webhook.sops.yaml",
	}
	if len(files) != len(want) || files[0] != want[0] || files[1] != want[1] {
		t.Errorf("files: %v, want %v", files, want)
	}
}

// addInfraRecipient inserts under every marker whose rule lacks the
// line, and only there: the fixture's shared rule carries luxor
// already, the layer's marked rule does not.
func TestAddInfraRecipient(t *testing.T) {
	line := "      - age1luxor # cluster: luxor"
	if !missingManagedRecipient(rulesFixture, line) {
		t.Fatal("the layer rule lacks luxor, missingManagedRecipient says no")
	}
	out, err := addInfraRecipient(rulesFixture, line)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "# cluster: luxor") != 2 {
		t.Errorf("want luxor under both markers:\n%s", out)
	}
	if missingManagedRecipient(out, line) {
		t.Error("still missing after the insert")
	}
	again, err := addInfraRecipient(out, line)
	if err != nil {
		t.Fatal(err)
	}
	if again != out {
		t.Error("a second run changed the file")
	}
	if _, err := addInfraRecipient("creation_rules: []\n", line); err == nil {
		t.Error("no marker must be an error")
	}
	// A second fort lands under both markers too.
	out2, err := addInfraRecipient(out, "      - age1zen # cluster: zen")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out2, "# cluster: zen") != 2 {
		t.Errorf("want zen under both markers:\n%s", out2)
	}
}
