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
	for name, tc := range map[string]struct{ text, want string }{
		"no name":     {"repo = \"ssh://x/y\"\n", "name"},
		"bad repo":    {"name = \"e\"\nrepo = \"git@gitea:sfi/e\"\n", "git URL"},
		"bad hub":     {"name = \"e\"\nrepo = \"ssh://x/y\"\nhub = \"Luxor\"\n", "hub"},
		"unknown key": {"name = \"e\"\nrepo = \"ssh://x/y\"\nmesh = \"10.1.0.0/24\"\n", "unknown key mesh"},
	} {
		if _, err := Parse([]byte(tc.text)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: want %q, got %v", name, tc.want, err)
		}
	}
}
