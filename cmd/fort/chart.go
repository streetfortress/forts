package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/tools"
	"gopkg.in/yaml.v3"
)

// estateCA is the estate's CA root, when it runs a private CA
// (nixos/certs/ca-root.pem, the init template's README): what helm
// trusts when it pulls from the estate's own registry, and what the
// OCIRepository is handed as certSecretRef (source-controller pods do
// not inherit the node's trust). Absent, the system roots serve.
const estateCA = "nixos/certs/ca-root.pem"

// chartMeta is what add needs from Chart.yaml.
type chartMeta struct {
	Name    string `yaml:"name"`
	Version string `yaml:"version"`
}

// chartPin says which artifact of a chart a fort follows. Exactly one
// form is set: a release tag, a PR's per-commit prereleases, or main's.
// url is the OCI reference the pin belongs to.
type chartPin struct {
	url, version, followPR string
	followMain             bool
}

// key identifies the pin for the per-run pull cache.
func (p chartPin) key() string {
	main := ""
	if p.followMain {
		main = "main"
	}
	return p.url + "@" + p.version + "@" + p.followPR + "@" + main
}

// constraint turns the pin into the semver range helm resolves.
//
// A PR is bounded by the next PR number. main has no such number, so the
// upper bound is the first alphanumeric identifier: semver ranks every
// numeric prerelease identifier below every alphanumeric one, so
// 0.0.0-main.<run> is below 0.0.0-main.z for every run, and 0.0.0-pr.*
// stays outside the range.
func (p chartPin) constraint() (string, error) {
	switch {
	case p.followPR != "":
		n, err := prNumber(p.followPR)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf(">=0.0.0-pr.%d.0 <0.0.0-pr.%d.0", n, n+1), nil
	case p.followMain:
		return ">=0.0.0-main.0 <0.0.0-main.z", nil
	default:
		return p.version, nil
	}
}

var mainBuild = regexp.MustCompile(`^0\.0\.0-main\.`)

// resolveChart fetches the chart at the requested pin into a temp dir and
// returns its directory and the exact version pulled. A follow pin
// resolves to the newest per-commit prerelease by semver
// (0.0.0-pr.<N>.<run>.g<sha> or 0.0.0-main.<run>.g<sha>, the chart
// repo's chart-preview workflow). helm does the registry work — the
// same binary every operator already has — with the estate's CA passed
// explicitly when the estate carries one.
func resolveChart(root string, pin chartPin) (dir, resolved string, err error) {
	ref := pin.url
	constraint, err := pin.constraint()
	if err != nil {
		return "", "", err
	}
	helm, err := tools.Runner("helm")
	if err != nil {
		return "", "", err
	}
	var caArgs []string
	if exists(root, estateCA) {
		caArgs = []string{"--ca-file", filepath.Join(root, estateCA)}
	}
	show, err := helm(append([]string{"show", "chart", ref, "--version", constraint}, caArgs...)...).Output()
	if err != nil {
		return "", "", fmt.Errorf("helm show chart %s --version %q: %s", ref, constraint, exitDetail(err))
	}
	var meta chartMeta
	if err := yaml.Unmarshal(show, &meta); err != nil {
		return "", "", err
	}
	if pin.followPR != "" && !regexp.MustCompile(`^0\.0\.0-pr\.`+regexp.QuoteMeta(pin.followPR)+`\.`).MatchString(meta.Version) {
		return "", "", fmt.Errorf("resolved %s, not a PR %s preview build", meta.Version, pin.followPR)
	}
	if pin.followMain && !mainBuild.MatchString(meta.Version) {
		return "", "", fmt.Errorf("resolved %s, not a main preview build", meta.Version)
	}
	tmp, err := os.MkdirTemp("", "fort-add-chart-")
	if err != nil {
		return "", "", err
	}
	pull := helm(append([]string{"pull", ref, "--version", meta.Version, "--untar", "--untardir", tmp}, caArgs...)...)
	if out, err := pull.CombinedOutput(); err != nil {
		os.RemoveAll(tmp)
		return "", "", fmt.Errorf("helm pull %s@%s: %s", ref, meta.Version, strings.TrimSpace(string(out)))
	}
	return filepath.Join(tmp, meta.Name), meta.Version, nil
}

func loadChartMeta(dir string) (chartMeta, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if err != nil {
		return chartMeta{}, err
	}
	var m chartMeta
	return m, yaml.Unmarshal(raw, &m)
}

func prNumber(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n <= 0 {
		return 0, fmt.Errorf("--follow-pr wants a PR number, got %q", s)
	}
	return n, nil
}

var prFilter = regexp.MustCompile(`^\^0\\\.0\\\.0-pr\\\.(\d+)\\\.`)
var mainFilter = regexp.MustCompile(`^\^0\\\.0\\\.0-main\\\.`)

// readChartPin reads the dir's OCIRepository: a tag pin, or the
// semver-filter form `fort add --follow-pr` / `--follow-main` writes.
// nil when there is no OCIRepository there.
func readChartPin(path string) (*chartPin, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	var doc struct {
		Kind string `yaml:"kind"`
		Spec struct {
			URL string `yaml:"url"`
			Ref struct {
				Tag          string `yaml:"tag"`
				SemverFilter string `yaml:"semverFilter"`
			} `yaml:"ref"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil || doc.Kind != "OCIRepository" {
		return nil, nil
	}
	pin := &chartPin{url: doc.Spec.URL, version: doc.Spec.Ref.Tag}
	if m := prFilter.FindStringSubmatch(doc.Spec.Ref.SemverFilter); m != nil {
		pin.followPR = m[1]
	}
	pin.followMain = mainFilter.MatchString(doc.Spec.Ref.SemverFilter)
	if pin.version == "" && pin.followPR == "" && !pin.followMain {
		return nil, fmt.Errorf("repository.yaml: OCIRepository has neither ref.tag nor a --follow-pr or --follow-main semverFilter")
	}
	return pin, nil
}
