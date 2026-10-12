package estate

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	e, err := Parse([]byte("name = \"second-estate\"\nrepo = \"ssh://git@gitea.zen.lofi:30022/sfi/second-estate\"\n"))
	if err != nil || e.Name != "second-estate" || e.Pushes() || e.RepoGit() != "ssh://git@gitea.zen.lofi:30022/sfi/second-estate.git" {
		t.Fatalf("minimal: %+v %v", e, err)
	}
	if e.Ref() != "sfi/second-estate" || ShortName(e.Ref()) != "second-estate" {
		t.Errorf("Ref: %q", e.Ref())
	}
	// The in-estate hub: [hubs.monitoring] names a fort of this estate —
	// this estate's own URL, in any spelling.
	for _, tbl := range []string{"[hubs.monitoring]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/deployments\"\nfort = \"luxor\"\n", "[hubs.monitoring]\nestate = \"https://gitea.zen.lofi/sfi/deployments.git\"\nfort = \"luxor\"\n"} {
		e, err = Parse([]byte("name = \"deployments\"\nrepo = \"ssh://git@gitea.zen.lofi:30022/sfi/deployments.git\"\n" + tbl))
		if err != nil || e.RepoGit() != e.Repo || !e.Pushes() || e.LocalHub(RoleMonitoring) != "luxor" || e.LocalHub(RoleForge) != "" {
			t.Fatalf("with an in-estate hub:\n%s%+v %v", tbl, e, err)
		}
		if h, ok := e.Hub(RoleMonitoring); !ok || !h.Local(e) || h.Fort != "luxor" || h.Ref() != "sfi/deployments" {
			t.Errorf("Hub(monitoring): %+v %v", h, ok)
		}
	}
	// A hub in another estate: the edges push, nothing registers here.
	e, err = Parse([]byte("name = \"staging\"\nrepo = \"ssh://x/sfi/staging\"\n[hubs.forge]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/infra\"\nfort = \"sfi-fort-zenbook\"\nname = \"gitea.zen.lofi\"\n[hubs.monitoring]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/infra\"\nfort = \"luxor\"\n"))
	if err != nil || !e.Pushes() || e.LocalHub(RoleMonitoring) != "" {
		t.Fatalf("hub in another estate: %+v %v", e, err)
	}
	if h, ok := e.Hub(RoleMonitoring); !ok || h.Local(e) || h.Fort != "luxor" || h.Ref() != "sfi/infra" {
		t.Errorf("Hub(monitoring) remote: %+v %v", h, ok)
	}
	if h, _ := e.Hub(RoleForge); h.Name != "gitea.zen.lofi" {
		t.Errorf("forge name: %+v", h)
	}
	// The hub's estate: [[serves.<role>]] by role, a fort and the estates
	// it serves; two entries under one role are two hubs.
	e, err = Parse([]byte(`name = "infra"
repo = "ssh://git@gitea.zen.lofi:30022/sfi/infra"
[hubs.forge]
estate = "ssh://git@gitea.zen.lofi:30022/sfi/infra"
fort = "sfi-fort-zenbook"
name = "gitea.zen.lofi"
[hubs.monitoring]
estate = "ssh://git@gitea.zen.lofi:30022/sfi/infra.git"
fort = "luxor"
[[serves.monitoring]]
fort = "luxor"
estates = ["ssh://git@gitea.zen.lofi:30022/sfi/staging"]
[[serves.monitoring]]
fort = "luxor-b"
estates = ["ssh://git@gitea.zen.lofi:30022/sfi/staging-b"]
[[serves.forge]]
fort = "sfi-fort-zenbook"
estates = ["ssh://git@gitea.zen.lofi:30022/sfi/staging", "https://gitea.zen.lofi/sfi/production.git", "ssh://git@gitea.zen.lofi:30022/sfi/staging-b"]
`))
	if err != nil || len(e.Serves) != 2 || e.LocalHub(RoleMonitoring) != "luxor" || e.LocalHub(RoleForge) != "sfi-fort-zenbook" {
		t.Fatalf("serves: %+v %v", e, err)
	}
	if m := e.ServedBy("luxor", RoleMonitoring); len(m) != 1 || m[0].Ref() != "sfi/staging" || m[0].Name() != "staging" {
		t.Errorf("ServedBy(luxor, monitoring): %+v", m)
	}
	if m := e.ServedBy("luxor-b", RoleMonitoring); len(m) != 1 || m[0].Ref() != "sfi/staging-b" {
		t.Errorf("ServedBy(luxor-b, monitoring): %+v", m)
	}
	if f := e.ServedBy("sfi-fort-zenbook", RoleForge); len(f) != 3 || f[1].Name() != "production" || f[1].Ref() != "sfi/production" {
		t.Errorf("a .git suffix and an https scheme still name the estate: %+v", f)
	}
	if e.ServedBy("luxor", RoleForge) != nil || e.ServedBy("nobody", RoleMonitoring) != nil || e.ServedBy("luxor", RoleCA) != nil {
		t.Errorf("ServedBy: want none for a fort that serves nobody the role")
	}
	if all := e.Servings(); len(all) != 5 || all[0].Role != RoleForge || all[3].Role != RoleMonitoring || all[3].Fort != "luxor" {
		t.Errorf("Servings in role order: %+v", all)
	}
	if forts := e.ServingForts(); len(forts) != 3 || forts[0] != "luxor" || forts[2] != "sfi-fort-zenbook" {
		t.Errorf("ServingForts: %v", forts)
	}
	// one estate, two spellings: the same repo
	if !sameRepo("ssh://git@gitea.zen.lofi:30022/sfi/infra", "https://gitea.zen.lofi/sfi/infra.git") || sameRepo("ssh://x/sfi/a", "ssh://x/sfi/b") || sameRepo("ssh://x/sfi/a", "ssh://y/sfi/a") {
		t.Errorf("sameRepo")
	}
	if ShortName("ssh://git@gitea.zen.lofi:30022/sfi/staging.git") != "staging" || ShortName("sfi/staging") != "staging" || WithGit("ssh://x/sfi/a") != "ssh://x/sfi/a.git" || WithGit("ssh://x/sfi/a.git") != "ssh://x/sfi/a.git" {
		t.Errorf("ShortName / WithGit")
	}
	hub := "name = \"infra\"\nrepo = \"ssh://x/sfi/infra\"\n[hubs.monitoring]\nestate = \"ssh://x/sfi/infra\"\nfort = \"luxor\"\n"
	for name, tc := range map[string]struct{ text, want string }{
		"old hub":         {"name = \"e\"\nrepo = \"ssh://x/y\"\nhub = \"luxor\"\n", "unknown key hub"},
		"old hub_push":    {"name = \"e\"\nrepo = \"ssh://x/y\"\nhub_push = \"10.1.0.5\"\n", "unknown key hub_push"},
		"unknown role":    {"name = \"e\"\nrepo = \"ssh://x/y\"\n[hubs.dns]\nestate = \"ssh://x/sfi/e\"\nfort = \"luxor\"\n", "[hubs.dns]"},
		"hub no fort":     {"name = \"e\"\nrepo = \"ssh://x/y\"\n[hubs.monitoring]\nestate = \"ssh://x/sfi/e\"\n", "fort"},
		"hub no estate":   {"name = \"e\"\nrepo = \"ssh://x/y\"\n[hubs.monitoring]\nfort = \"luxor\"\n", "estate"},
		"old host key":    {"name = \"e\"\nrepo = \"ssh://x/y\"\n[hubs.monitoring]\nhost = \"luxor\"\n", "unknown key"},
		"hub bad estate":  {"name = \"e\"\nrepo = \"ssh://x/y\"\n[hubs.monitoring]\nestate = \"sfi/infra\"\nfort = \"luxor\"\n", "git URL"},
		"hub bad path":    {"name = \"e\"\nrepo = \"ssh://x/y\"\n[hubs.monitoring]\nestate = \"ssh://x/sfi\"\nfort = \"luxor\"\n", "git URL"},
		"hub bad fort":    {"name = \"e\"\nrepo = \"ssh://x/y\"\n[hubs.monitoring]\nestate = \"ssh://x/sfi/e\"\nfort = \"Luxor\"\n", "fort"},
		"hub name":        {"name = \"e\"\nrepo = \"ssh://x/y\"\n[hubs.monitoring]\nestate = \"ssh://x/sfi/e\"\nfort = \"luxor\"\nname = \"x.lofi\"\n", "only the forge"},
		"forge bad name":  {"name = \"e\"\nrepo = \"ssh://x/y\"\n[hubs.forge]\nestate = \"ssh://x/sfi/e\"\nfort = \"z\"\nname = \"Gitea.Zen\"\n", "name"},
		"serves self":     {hub + "[[serves.monitoring]]\nfort = \"luxor\"\nestates = [\"https://x/sfi/infra.git\"]\n", "this estate"},
		"serves none":     {hub + "[[serves.monitoring]]\nfort = \"luxor\"\nestates = []\n", "at least one"},
		"serves no fort":  {hub + "[[serves.monitoring]]\nestates = [\"ssh://x/sfi/staging\"]\n", "fort"},
		"serves bad fort": {hub + "[[serves.monitoring]]\nfort = \"Luxor\"\nestates = [\"ssh://x/sfi/staging\"]\n", "fort"},
		"serves bad role": {hub + "[[serves.dns]]\nfort = \"luxor\"\nestates = [\"ssh://x/sfi/staging\"]\n", "[[serves.dns]]"},
		"serves twice":    {hub + "[[serves.monitoring]]\nfort = \"luxor\"\nestates = [\"ssh://x/sfi/staging\", \"https://x/sfi/staging.git\"]\n", "twice"},
		"serves two hubs": {hub + "[[serves.monitoring]]\nfort = \"luxor\"\nestates = [\"ssh://x/sfi/staging\"]\n[[serves.monitoring]]\nfort = \"luxor-b\"\nestates = [\"ssh://x/sfi/staging\"]\n", "twice"},
		"serves bad url":  {hub + "[[serves.monitoring]]\nfort = \"luxor\"\nestates = [\"git@x:sfi/staging\"]\n", "git URL"},
		"serves old list": {hub + "[[serves]]\nestate = \"ssh://x/sfi/staging\"\nroles = [\"monitoring\"]\n", "serves"},
		"no name":         {"repo = \"ssh://x/y\"\n", "name"},
		"bad repo":        {"name = \"e\"\nrepo = \"git@gitea:sfi/e\"\n", "git URL"},
		"unknown key":     {"name = \"e\"\nrepo = \"ssh://x/y\"\nsubnet = \"10.1.0.0/24\"\n", "unknown key subnet"},
		"long iface":      {"name = \"e\"\nrepo = \"ssh://x/y\"\ninterface = \"wg-second-estate\"\n", "interface"},
		"bad iface":       {"name = \"e\"\nrepo = \"ssh://x/y\"\ninterface = \"wg second\"\n", "interface"},
		"bad mesh":        {"name = \"e\"\nrepo = \"ssh://x/y\"\nmesh = \"10.2.0.4/24\"\n", "mesh"},
		"mesh v6":         {"name = \"e\"\nrepo = \"ssh://x/y\"\nmesh = \"fd00::/64\"\n", "mesh"},
		"bad zone":        {"name = \"e\"\nrepo = \"ssh://x/y\"\nzone = \"Staging.Mesh\"\n", "zone"},
		"zone hyphen":     {"name = \"e\"\nrepo = \"ssh://x/y\"\nzone = \"-staging.mesh\"\n", "zone"},
		"zone dots":       {"name = \"e\"\nrepo = \"ssh://x/y\"\nzone = \"staging..mesh\"\n", "zone"},
	} {
		if _, err := Parse([]byte(tc.text)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
}

func TestForge(t *testing.T) {
	for repo, want := range map[string][3]string{
		"ssh://git@gitea.zen.lofi:30022/sfi/second-estate":   {"gitea.zen.lofi", "sfi", "second-estate"},
		"ssh://git@gitea.zen.lofi:30022/sfi/deployments.git": {"gitea.zen.lofi", "sfi", "deployments"},
		"https://gitea.example.org/acme/estate.git":          {"gitea.example.org", "acme", "estate"},
	} {
		e := Estate{Name: "e", Repo: repo}
		host, owner, name, err := e.Forge()
		if err != nil || host != want[0] || owner != want[1] || name != want[2] {
			t.Errorf("%s: got %s %s %s %v, want %v", repo, host, owner, name, err, want)
		}
	}
	e := Estate{Name: "second-estate", Repo: "ssh://git@gitea.zen.lofi:30022/sfi/second-estate"}
	if api, _ := e.RepoAPI(); api != "https://gitea.zen.lofi/api/v1/repos/sfi/second-estate" {
		t.Errorf("RepoAPI: %s", api)
	}
	if base, _ := e.APIBase(); base != "https://gitea.zen.lofi/api/v1" {
		t.Errorf("APIBase: %s", base)
	}
	for _, bad := range []string{"ssh://git@gitea.zen.lofi:30022/sfi", "ssh://git@gitea.zen.lofi:30022/a/b/c", "ssh://gitea.zen.lofi/"} {
		if _, _, _, err := (Estate{Repo: bad}).Forge(); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func TestMesh(t *testing.T) {
	e, err := Parse([]byte("name = \"second-estate\"\nrepo = \"ssh://x/y\"\nmesh = \"10.2.0.0/24\"\n"))
	if err != nil || e.Mesh != "10.2.0.0/24" {
		t.Fatalf("mesh: %+v %v", e, err)
	}
	if !e.Contains("10.2.0.70") || e.Contains("10.1.0.4") || e.Contains("nope") {
		t.Errorf("Contains")
	}
	if !(Estate{}).Contains("10.1.0.4") {
		t.Errorf("no mesh declared: every address is inside")
	}
	if (Estate{}).InterfaceName() != "wg0" {
		t.Errorf("no interface declared: wg0")
	}
	e, err = Parse([]byte("name = \"second-estate\"\nrepo = \"ssh://x/y\"\ninterface = \"wg-second\"\n"))
	if err != nil || e.InterfaceName() != "wg-second" {
		t.Fatalf("interface: %+v %v", e, err)
	}
}

// The zone (forts#45): <name>.mesh unless estate.toml names one.
func TestZone(t *testing.T) {
	e, err := Parse([]byte("name = \"staging\"\nrepo = \"ssh://x/y\"\n"))
	if err != nil || e.Zone != "" || e.ZoneName() != "staging.mesh" {
		t.Fatalf("default zone: %+v %v", e, err)
	}
	for _, zone := range []string{"lab.internal", "mesh", "second-estate.mesh", "a1.b2"} {
		e, err := Parse([]byte("name = \"staging\"\nrepo = \"ssh://x/y\"\nzone = \"" + zone + "\"\n"))
		if err != nil || e.ZoneName() != zone {
			t.Errorf("zone %s: %+v %v", zone, e, err)
		}
	}
}

// The operator keys (forts#81): the devices that hold `deployer` on
// every fort of the estate. The framework ships none, so a line that
// sshd cannot read is a key no fort accepts and nobody reports — the
// manifest refuses it here instead.
func TestOperatorKeys(t *testing.T) {
	const (
		laptop  = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJDZXdVYO58sXRAwLieovSB3fHDAhMF96rjyfCx+vLuL laptop"
		desktop = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBL2o6nnz49u8AXHFIojZiJWJkOFgzH3hplflKDiVz/i desktop"
		bare    = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAICWmh8mR2QTmPWICoSPeEK2Ktki99Y04pJAaJZhiKtXW"
	)
	head := "name = \"staging\"\nrepo = \"ssh://x/y\"\n"
	e, err := Parse([]byte(head + "operator_keys = [\n  \"" + laptop + "\",\n  \"" + desktop + "\",\n]\n"))
	if err != nil || len(e.OperatorKeys) != 2 {
		t.Fatalf("two operators: %+v %v", e, err)
	}
	ops := e.Operators()
	if len(ops) != 2 || ops[0].Name != "laptop" || ops[1].Name != "desktop" || ops[0].Key != laptop {
		t.Errorf("Operators: %+v", ops)
	}
	// A key with no comment parses: it names no device, and the gate
	// (fort check --all) is what says so.
	if e, err := Parse([]byte(head + "operator_keys = [\"" + bare + "\"]\n")); err != nil || e.Operators()[0].Name != "" {
		t.Errorf("a key with no comment: %+v %v", e, err)
	}
	// An estate with no operator_keys parses: the key list is optional
	// in the manifest, and the host's assertion is the refusal.
	if e, err := Parse([]byte(head)); err != nil || len(e.Operators()) != 0 {
		t.Errorf("no operators: %+v %v", e, err)
	}
	for _, tc := range []struct{ keys, want string }{
		{"[\"ssh-ed25519 not-base64 laptop\"]", "want one SSH public key"},
		{"[\"laptop\"]", "want one SSH public key"},
		{"[\"\"]", "want one SSH public key"},
		{"[\"command=\\\"true\\\" " + laptop + "\"]", "no authorized_keys options"},
		{"[\"" + laptop + "\\n" + desktop + "\"]", "one key per entry"},
		{"[\"" + laptop + "\", \"" + laptop + "\"]", "the same key as #1"},
	} {
		_, err := Parse([]byte(head + "operator_keys = " + tc.keys + "\n"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("operator_keys = %s: want %q, got %v", tc.keys, tc.want, err)
		}
	}
}
