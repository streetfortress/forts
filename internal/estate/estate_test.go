package estate

import (
	"strings"
	"testing"
)

// src is the [source] table with the repo alone: appended after the
// loose keys, as the template renders it.
func src(repo string) string { return "[source]\nrepo = \"" + repo + "\"\n" }

func TestParse(t *testing.T) {
	const metrics = "metrics"
	e, err := Parse([]byte("name = \"second-estate\"\n" + src("ssh://git@gitea.zen.lofi:30022/sfi/second-estate")))
	if err != nil || e.Name != "second-estate" || e.RepoGit() != "ssh://git@gitea.zen.lofi:30022/sfi/second-estate.git" {
		t.Fatalf("minimal: %+v %v", e, err)
	}
	if e.Ref() != "sfi/second-estate" || ShortName(e.Ref()) != "second-estate" {
		t.Errorf("Ref: %q", e.Ref())
	}
	// The in-estate hub: [hubs.metrics] names a fort of this estate —
	// this estate's own URL, in any spelling.
	for _, tbl := range []string{"[hubs.metrics]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/deployments\"\nfort = \"luxor\"\n", "[hubs.metrics]\nestate = \"https://gitea.zen.lofi/sfi/deployments.git\"\nfort = \"luxor\"\n"} {
		e, err = Parse([]byte("name = \"deployments\"\n" + src("ssh://git@gitea.zen.lofi:30022/sfi/deployments.git") + tbl))
		if err != nil || e.RepoGit() != e.Source.Repo || e.LocalHub(metrics) != "luxor" || e.LocalHub(RoleForge) != "" {
			t.Fatalf("with an in-estate hub:\n%s%+v %v", tbl, e, err)
		}
		if h, ok := e.Hub(metrics); !ok || !h.Local(e) || h.Fort != "luxor" || h.Ref() != "sfi/deployments" {
			t.Errorf("Hub(metrics): %+v %v", h, ok)
		}
	}
	// A hub in another estate: the edges push, nothing registers here.
	e, err = Parse([]byte("name = \"staging\"\n" + src("ssh://x/sfi/staging") + "[hubs.forge]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/infra\"\nfort = \"sfi-fort-zenbook\"\nname = \"gitea.zen.lofi\"\n[hubs.metrics]\nestate = \"ssh://git@gitea.zen.lofi:30022/sfi/infra\"\nfort = \"luxor\"\n"))
	if err != nil || e.LocalHub(metrics) != "" {
		t.Fatalf("hub in another estate: %+v %v", e, err)
	}
	if h, ok := e.Hub(metrics); !ok || h.Local(e) || h.Fort != "luxor" || h.Ref() != "sfi/infra" {
		t.Errorf("Hub(metrics) remote: %+v %v", h, ok)
	}
	if h, _ := e.Hub(RoleForge); h.Name != "gitea.zen.lofi" {
		t.Errorf("forge name: %+v", h)
	}
	// The hub's estate: [[serves.<role>]] by role, a fort and the estates
	// it serves; two entries under one role are two hubs.
	e, err = Parse([]byte(`name = "infra"
[source]
repo = "ssh://git@gitea.zen.lofi:30022/sfi/infra"
[hubs.forge]
estate = "ssh://git@gitea.zen.lofi:30022/sfi/infra"
fort = "sfi-fort-zenbook"
name = "gitea.zen.lofi"
[hubs.metrics]
estate = "ssh://git@gitea.zen.lofi:30022/sfi/infra.git"
fort = "luxor"
[[serves.metrics]]
fort = "luxor"
estates = ["ssh://git@gitea.zen.lofi:30022/sfi/staging"]
ports = [30990]
[[serves.metrics]]
fort = "luxor-b"
estates = ["ssh://git@gitea.zen.lofi:30022/sfi/staging-b"]
ports = [30990]
[[serves.forge]]
fort = "sfi-fort-zenbook"
estates = ["ssh://git@gitea.zen.lofi:30022/sfi/staging", "https://gitea.zen.lofi/sfi/production.git", "ssh://git@gitea.zen.lofi:30022/sfi/staging-b"]
`))
	if err != nil || len(e.Serves) != 2 || e.LocalHub(metrics) != "luxor" || e.LocalHub(RoleForge) != "sfi-fort-zenbook" {
		t.Fatalf("serves: %+v %v", e, err)
	}
	if m := e.ServedBy("luxor", metrics); len(m) != 1 || m[0].Ref() != "sfi/staging" || m[0].Name() != "staging" {
		t.Errorf("ServedBy(luxor, metrics): %+v", m)
	}
	if m := e.ServedBy("luxor-b", metrics); len(m) != 1 || m[0].Ref() != "sfi/staging-b" {
		t.Errorf("ServedBy(luxor-b, metrics): %+v", m)
	}
	if f := e.ServedBy("sfi-fort-zenbook", RoleForge); len(f) != 3 || f[1].Name() != "production" || f[1].Ref() != "sfi/production" {
		t.Errorf("a .git suffix and an https scheme still name the estate: %+v", f)
	}
	if e.ServedBy("luxor", RoleForge) != nil || e.ServedBy("nobody", metrics) != nil || e.ServedBy("luxor", "ca") != nil {
		t.Errorf("ServedBy: want none for a fort that serves nobody the role")
	}
	if all := e.Servings(); len(all) != 5 || all[0].Role != RoleForge || all[3].Role != metrics || all[3].Fort != "luxor" {
		t.Errorf("Servings in role order: %+v", all)
	}
	if forts := e.ServingForts(); len(forts) != 3 || forts[0] != "luxor" || forts[2] != "sfi-fort-zenbook" {
		t.Errorf("ServingForts: %v", forts)
	}
	// one estate, two spellings: the same repo
	if !SameRepo("ssh://git@gitea.zen.lofi:30022/sfi/infra", "https://gitea.zen.lofi/sfi/infra.git") || SameRepo("ssh://x/sfi/a", "ssh://x/sfi/b") || SameRepo("ssh://x/sfi/a", "ssh://y/sfi/a") {
		t.Errorf("SameRepo")
	}
	if ShortName("ssh://git@gitea.zen.lofi:30022/sfi/staging.git") != "staging" || ShortName("sfi/staging") != "staging" || WithGit("ssh://x/sfi/a") != "ssh://x/sfi/a.git" || WithGit("ssh://x/sfi/a.git") != "ssh://x/sfi/a.git" {
		t.Errorf("ShortName / WithGit")
	}
	head := "name = \"e\"\n" + src("ssh://x/y")
	hub := "name = \"infra\"\n" + src("ssh://x/sfi/infra") + "[hubs.metrics]\nestate = \"ssh://x/sfi/infra\"\nfort = \"luxor\"\n"
	for name, tc := range map[string]struct{ text, want string }{
		"old hub":            {"name = \"e\"\nhub = \"luxor\"\n" + src("ssh://x/y"), "unknown key hub"},
		"old hub_push":       {"name = \"e\"\nhub_push = \"10.1.0.5\"\n" + src("ssh://x/y"), "unknown key hub_push"},
		"hub no fort":        {head + "[hubs.metrics]\nestate = \"ssh://x/sfi/e\"\n", "fort"},
		"hub no estate":      {head + "[hubs.metrics]\nfort = \"luxor\"\n", "estate"},
		"old host key":       {head + "[hubs.metrics]\nhost = \"luxor\"\n", "unknown key"},
		"hub bad estate":     {head + "[hubs.metrics]\nestate = \"sfi/infra\"\nfort = \"luxor\"\n", "git URL"},
		"hub bad path":       {head + "[hubs.metrics]\nestate = \"ssh://x/sfi\"\nfort = \"luxor\"\n", "git URL"},
		"hub bad fort":       {head + "[hubs.metrics]\nestate = \"ssh://x/sfi/e\"\nfort = \"Luxor\"\n", "fort"},
		"hub name":           {head + "[hubs.metrics]\nestate = \"ssh://x/sfi/e\"\nfort = \"luxor\"\nname = \"x.lofi\"\n", "only the forge"},
		"forge bad name":     {head + "[hubs.forge]\nestate = \"ssh://x/sfi/e\"\nfort = \"z\"\nname = \"Gitea.Zen\"\n", "name"},
		"serves self":        {hub + "[[serves.metrics]]\nfort = \"luxor\"\nestates = [\"https://x/sfi/infra.git\"]\nports = [30990]\n", "this estate"},
		"serves none":        {hub + "[[serves.metrics]]\nfort = \"luxor\"\nestates = []\nports = [30990]\n", "at least one"},
		"serves no fort":     {hub + "[[serves.metrics]]\nestates = [\"ssh://x/sfi/staging\"]\nports = [30990]\n", "fort"},
		"serves bad fort":    {hub + "[[serves.metrics]]\nfort = \"Luxor\"\nestates = [\"ssh://x/sfi/staging\"]\nports = [30990]\n", "fort"},
		"serves bad role":    {hub + "[[serves.dns]]\nfort = \"luxor\"\nestates = [\"ssh://x/sfi/staging\"]\n", "[[serves.dns]]"},
		"serves twice":       {hub + "[[serves.metrics]]\nfort = \"luxor\"\nestates = [\"ssh://x/sfi/staging\", \"https://x/sfi/staging.git\"]\nports = [30990]\n", "twice"},
		"serves two hubs":    {hub + "[[serves.metrics]]\nfort = \"luxor\"\nestates = [\"ssh://x/sfi/staging\"]\nports = [30990]\n[[serves.metrics]]\nfort = \"luxor-b\"\nestates = [\"ssh://x/sfi/staging\"]\nports = [30990]\n", "twice"},
		"serves bad url":     {hub + "[[serves.metrics]]\nfort = \"luxor\"\nestates = [\"git@x:sfi/staging\"]\nports = [30990]\n", "git URL"},
		"serves forge ports": {hub + "[[serves.forge]]\nfort = \"luxor\"\nestates = [\"ssh://x/sfi/staging\"]\nports = [22]\n", "the core's"},
		"serves no ports":    {hub + "[[serves.metrics]]\nfort = \"luxor\"\nestates = [\"ssh://x/sfi/staging\"]\n", "ports"},
		"serves bad port":    {hub + "[[serves.metrics]]\nfort = \"luxor\"\nestates = [\"ssh://x/sfi/staging\"]\nports = [70000]\n", "TCP port"},
		"hub bad role":       {head + "[hubs.Metrics]\nestate = \"ssh://x/sfi/e\"\nfort = \"luxor\"\n", "role name"},
		"serves old list":    {hub + "[[serves]]\nestate = \"ssh://x/sfi/staging\"\nroles = [\"monitoring\"]\n", "serves"},
		"no name":            {src("ssh://x/y"), "name"},
		"bad repo":           {"name = \"e\"\n" + src("git@gitea:sfi/e"), "git URL"},
		"unknown key":        {"name = \"e\"\nsubnet = \"10.1.0.0/24\"\n" + src("ssh://x/y"), "unknown key subnet"},
		"long iface":         {head + "[mesh]\ninterface = \"wg-second-estate\"\n", "interface"},
		"bad iface":          {head + "[mesh]\ninterface = \"wg second\"\n", "interface"},
		"bad mesh":           {head + "[mesh]\ncidr = \"10.2.0.4/24\"\n", "cidr"},
		"mesh v6":            {head + "[mesh]\ncidr = \"fd00::/64\"\n", "cidr"},
		"bad zone":           {head + "[mesh]\nzone = \"Staging.Mesh\"\n", "zone"},
		"zone hyphen":        {head + "[mesh]\nzone = \"-staging.mesh\"\n", "zone"},
		"zone dots":          {head + "[mesh]\nzone = \"staging..mesh\"\n", "zone"},
	} {
		if _, err := Parse([]byte(tc.text)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
}

// The flat schema's loose keys (forts#161): no shim, but the refusal
// names the table each key moved to.
func TestMovedKeys(t *testing.T) {
	for key, want := range map[string]string{
		"repo = \"ssh://x/y\"":      "repo moved to [source] repo",
		"known_hosts = \"h t k\"":   "known_hosts moved to [source] known_hosts",
		"interface = \"wg-second\"": "interface moved to [mesh] interface",
		"zone = \"staging.mesh\"":   "zone moved to [mesh] zone",
		"acme_email = \"ops@x\"":    "acme_email moved to [acme] email",
	} {
		_, err := Parse([]byte("name = \"e\"\n" + key + "\n" + src("ssh://x/y")))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", key, want, err)
		}
	}
	// The old loose `mesh` key collides with the [mesh] table's type, so
	// the decoder refuses it before the moved-key map can: the error
	// still names the key.
	if _, err := Parse([]byte("name = \"e\"\nmesh = \"10.2.0.0/24\"\n" + src("ssh://x/y"))); err == nil || !strings.Contains(err.Error(), "mesh") {
		t.Errorf("old loose mesh: want an error naming the key, got %v", err)
	}
}

// The [source] matrix (forts#161, #167): an ssh:// repo is the ssh
// route, an https:// repo the anonymous one, derived or declared; the
// scheme and the route refused when they disagree; a token named as a
// missing route; a forge kind the core knows; a pin that is not one.
func TestSourceMatrix(t *testing.T) {
	for name, tc := range map[string]struct{ source, want string }{
		"auth token":       {"repo = \"ssh://x/y\"\nauth = \"token\"", "no token route"},
		"auth none ssh":    {"repo = \"ssh://x/y\"\nauth = \"none\"", "https only"},
		"auth ssh https":   {"repo = \"https://github.com/acme/estate\"\nauth = \"ssh\"", "no deploy key"},
		"auth token https": {"repo = \"https://github.com/acme/estate\"\nauth = \"token\"", "no token route"},
		"auth unknown":     {"repo = \"ssh://x/y\"\nauth = \"password\"", "auth"},
		"other scheme":     {"repo = \"git://x/y\"", "want the estate's git URL"},
		"forge unknown":    {"repo = \"ssh://x/y\"\nforge = \"sourcehut\"", "forge"},
		"bad pin":          {"repo = \"ssh://x/y\"\nknown_hosts = \"short\"", "known_hosts"},
	} {
		_, err := Parse([]byte("name = \"e\"\n[source]\n" + tc.source + "\n"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
	e, err := Parse([]byte("name = \"e\"\n[source]\nrepo = \"ssh://x/y\"\nauth = \"ssh\"\nforge = \"gitea\"\n"))
	if err != nil || e.AuthRoute() != AuthSSH || e.ForgeKind() != ForgeGitea || e.Anonymous() {
		t.Fatalf("explicit ssh/gitea: %+v %v", e, err)
	}
	// The anonymous route (forts#167): https plus none, declared or
	// derived, with no host key — and with one, which the gate reports
	// as unread rather than the manifest refusing.
	for name, source := range map[string]string{
		"https derived":  "repo = \"https://github.com/acme/estate\"",
		"https declared": "repo = \"https://github.com/acme/estate.git\"\nauth = \"none\"",
		"https gitea":    "repo = \"https://forge.example/acme/estate\"\nforge = \"gitea\"",
		"https with pin": "repo = \"https://github.com/acme/estate\"\nknown_hosts = \"github.com ssh-ed25519 AAAA\"",
	} {
		e, err := Parse([]byte("name = \"e\"\n[source]\n" + source + "\n"))
		if err != nil || e.AuthRoute() != AuthNone || !e.Anonymous() {
			t.Errorf("%s: want the anonymous route, got %+v %v", name, e.Source, err)
		}
	}
}

// The derivations: the host of repo gives the forge kind, the scheme
// gives the auth route. github.com derives "github", the kind
// cmd/fort/forge.go speaks to at api.github.com.
func TestForgeKindAndAuthRoute(t *testing.T) {
	e := Estate{Source: Source{Repo: "ssh://git@gitea.zen.lofi:30022/sfi/infra"}}
	if e.ForgeKind() != ForgeGitea || e.AuthRoute() != AuthSSH {
		t.Errorf("gitea derivation: %s %s", e.ForgeKind(), e.AuthRoute())
	}
	e = Estate{Source: Source{Repo: "ssh://git@github.com/acme/estate"}}
	if e.ForgeKind() != ForgeGitHub {
		t.Errorf("github.com derives github: %s", e.ForgeKind())
	}
	e = Estate{Source: Source{Repo: "ssh://x/y", Forge: ForgeGitHub}}
	if e.ForgeKind() != ForgeGitHub {
		t.Errorf("explicit forge wins: %s", e.ForgeKind())
	}
}

func TestForge(t *testing.T) {
	for repo, want := range map[string][3]string{
		"ssh://git@gitea.zen.lofi:30022/sfi/second-estate":   {"gitea.zen.lofi", "sfi", "second-estate"},
		"ssh://git@gitea.zen.lofi:30022/sfi/deployments.git": {"gitea.zen.lofi", "sfi", "deployments"},
		"https://gitea.example.org/acme/estate.git":          {"gitea.example.org", "acme", "estate"},
	} {
		e := Estate{Name: "e", Source: Source{Repo: repo}}
		host, owner, name, err := e.Forge()
		if err != nil || host != want[0] || owner != want[1] || name != want[2] {
			t.Errorf("%s: got %s %s %s %v, want %v", repo, host, owner, name, err, want)
		}
	}
	for _, bad := range []string{"ssh://git@gitea.zen.lofi:30022/sfi", "ssh://git@gitea.zen.lofi:30022/a/b/c", "ssh://gitea.zen.lofi/"} {
		if _, _, _, err := (Estate{Source: Source{Repo: bad}}).Forge(); err == nil {
			t.Errorf("%s: want an error", bad)
		}
	}
}

func TestMesh(t *testing.T) {
	e, err := Parse([]byte("name = \"second-estate\"\n" + src("ssh://x/y") + "[mesh]\ncidr = \"10.2.0.0/24\"\n"))
	if err != nil || e.Mesh.CIDR != "10.2.0.0/24" {
		t.Fatalf("mesh: %+v %v", e, err)
	}
	if !e.Contains("10.2.0.70") || e.Contains("10.1.0.4") || e.Contains("nope") {
		t.Errorf("Contains")
	}
	if e.MeshPrefixBits() != 24 {
		t.Errorf("MeshPrefixBits of a /24: %d", e.MeshPrefixBits())
	}
	if !(Estate{}).Contains("10.1.0.4") {
		t.Errorf("no mesh declared: every address is inside")
	}
	if (Estate{}).MeshPrefixBits() != 24 {
		t.Errorf("no mesh declared: the prefix stays 24")
	}
	if (Estate{}).InterfaceName() != "wg0" {
		t.Errorf("no interface declared: wg0")
	}
	// A wider mesh: the prefix is the cidr's, not the /24 convention.
	e, err = Parse([]byte("name = \"second-estate\"\n" + src("ssh://x/y") + "[mesh]\ncidr = \"10.2.0.0/16\"\n"))
	if err != nil || e.MeshPrefixBits() != 16 || !e.Contains("10.2.200.4") {
		t.Fatalf("a /16 mesh: %+v %v", e, err)
	}
	e, err = Parse([]byte("name = \"second-estate\"\n" + src("ssh://x/y") + "[mesh]\ninterface = \"wg-second\"\n"))
	if err != nil || e.InterfaceName() != "wg-second" {
		t.Fatalf("interface: %+v %v", e, err)
	}
}

// The zone (forts#45): <name>.mesh unless estate.toml names one.
func TestZone(t *testing.T) {
	e, err := Parse([]byte("name = \"staging\"\n" + src("ssh://x/y")))
	if err != nil || e.Mesh.Zone != "" || e.ZoneName() != "staging.mesh" {
		t.Fatalf("default zone: %+v %v", e, err)
	}
	for _, zone := range []string{"lab.internal", "mesh", "second-estate.mesh", "a1.b2"} {
		e, err := Parse([]byte("name = \"staging\"\n" + src("ssh://x/y") + "[mesh]\nzone = \"" + zone + "\"\n"))
		if err != nil || e.ZoneName() != zone {
			t.Errorf("zone %s: %+v %v", zone, e, err)
		}
	}
}

// The [acme] table (forts#161): the mailbox, and a directory that is
// https — Let's Encrypt's when absent.
func TestACME(t *testing.T) {
	e, err := Parse([]byte("name = \"e\"\n" + src("ssh://x/y") + "[acme]\nemail = \"ops@example\"\n"))
	if err != nil || e.ACME.Email != "ops@example" || e.ACMEDirectory() != DefaultACMEDirectory {
		t.Fatalf("acme default directory: %+v %v", e, err)
	}
	e, err = Parse([]byte("name = \"e\"\n" + src("ssh://x/y") + "[acme]\nemail = \"ops@example\"\ndirectory = \"https://ca.lab.internal/acme/acme/directory\"\n"))
	if err != nil || e.ACMEDirectory() != "https://ca.lab.internal/acme/acme/directory" {
		t.Fatalf("acme private directory: %+v %v", e, err)
	}
	for text, want := range map[string]string{
		"[acme]\nemail = \"nope\"\n":                           "mailbox",
		"[acme]\nemail = \"ops@x\"\ndirectory = \"ftp://x\"\n": "https",
	} {
		if _, err := Parse([]byte("name = \"e\"\n" + src("ssh://x/y") + text)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", text, want, err)
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
	// The loose keys come before the first table: TOML's rule, which the
	// template keeps.
	estateText := func(keys string) string {
		head := "name = \"staging\"\n"
		if keys != "" {
			head += "operator_keys = " + keys + "\n"
		}
		return head + src("ssh://x/y")
	}
	e, err := Parse([]byte(estateText("[\n  \"" + laptop + "\",\n  \"" + desktop + "\",\n]")))
	if err != nil || len(e.OperatorKeys) != 2 {
		t.Fatalf("two operators: %+v %v", e, err)
	}
	ops := e.Operators()
	if len(ops) != 2 || ops[0].Name != "laptop" || ops[1].Name != "desktop" || ops[0].Key != laptop {
		t.Errorf("Operators: %+v", ops)
	}
	// A key with no comment parses: it names no device, and the gate
	// (fort check --all) is what says so.
	if e, err := Parse([]byte(estateText("[\"" + bare + "\"]"))); err != nil || e.Operators()[0].Name != "" {
		t.Errorf("a key with no comment: %+v %v", e, err)
	}
	// An estate with no operator_keys parses: the key list is optional
	// in the manifest, and the host's assertion is the refusal.
	if e, err := Parse([]byte(estateText(""))); err != nil || len(e.Operators()) != 0 {
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
		_, err := Parse([]byte(estateText(tc.keys)))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("operator_keys = %s: want %q, got %v", tc.keys, tc.want, err)
		}
	}
}
