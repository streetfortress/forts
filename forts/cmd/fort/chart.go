package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

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
// same binary the runner and every operator already has — with the lab
// root CA passed explicitly, exactly like the release workflows.
func resolveChart(root string, pin chartPin) (dir, resolved string, err error) {
	ca := filepath.Join(root, "flux/infrastructure/configs/step-ca-root.pem")
	ref := pin.url
	constraint, err := pin.constraint()
	if err != nil {
		return "", "", err
	}
	show, err := exec.Command("helm", "show", "chart", ref, "--version", constraint, "--ca-file", ca).Output()
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
	pull := exec.Command("helm", "pull", ref, "--version", meta.Version, "--untar", "--untardir", tmp, "--ca-file", ca)
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

func exitDetail(err error) string {
	if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
		return strings.TrimSpace(string(ee.Stderr))
	}
	return err.Error()
}
