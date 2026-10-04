package estate

import (
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	e, err := Parse([]byte("name = \"second-estate\"\nrepo = \"ssh://git@gitea.zen.lofi:30022/sfi/second-estate\"\n"))
	if err != nil || e.Name != "second-estate" || e.HasHub() || e.RepoGit() != "ssh://git@gitea.zen.lofi:30022/sfi/second-estate.git" {
		t.Fatalf("minimal: %+v %v", e, err)
	}
	e, err = Parse([]byte("name = \"deployments\"\nrepo = \"ssh://git@gitea.zen.lofi:30022/sfi/deployments.git\"\nhub = \"luxor\"\n"))
	if err != nil || e.Hub != "luxor" || e.RepoGit() != e.Repo {
		t.Fatalf("with hub: %+v %v", e, err)
	}
	e, err = Parse([]byte("name = \"second-estate\"\nrepo = \"ssh://x/y\"\nhub_push = \"10.1.0.5\"\n"))
	if err != nil || e.HasHub() || !e.Pushes() || e.HubPush != "10.1.0.5" {
		t.Fatalf("hub by address: %+v %v", e, err)
	}
	for name, tc := range map[string]struct{ text, want string }{
		"bad push":    {"name = \"e\"\nrepo = \"ssh://x/y\"\nhub_push = \"luxor\"\n", "hub_push"},
		"both hubs":   {"name = \"e\"\nrepo = \"ssh://x/y\"\nhub = \"luxor\"\nhub_push = \"10.1.0.5\"\n", "together"},
		"no name":     {"repo = \"ssh://x/y\"\n", "name"},
		"bad repo":    {"name = \"e\"\nrepo = \"git@gitea:sfi/e\"\n", "git URL"},
		"bad hub":     {"name = \"e\"\nrepo = \"ssh://x/y\"\nhub = \"Luxor\"\n", "hub"},
		"unknown key": {"name = \"e\"\nrepo = \"ssh://x/y\"\nsubnet = \"10.1.0.0/24\"\n", "unknown key subnet"},
		"long iface":  {"name = \"e\"\nrepo = \"ssh://x/y\"\ninterface = \"wg-second-estate\"\n", "interface"},
		"bad iface":   {"name = \"e\"\nrepo = \"ssh://x/y\"\ninterface = \"wg second\"\n", "interface"},
		"bad mesh":    {"name = \"e\"\nrepo = \"ssh://x/y\"\nmesh = \"10.2.0.4/24\"\n", "mesh"},
		"mesh v6":     {"name = \"e\"\nrepo = \"ssh://x/y\"\nmesh = \"fd00::/64\"\n", "mesh"},
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
