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

// resolveChart fetches the chart at the requested pin into a temp dir and
// returns its directory and the exact version pulled. Exactly one of
// version / followPR is set: a tag, or a PR whose per-commit prereleases
// (0.0.0-pr.<N>.<run>.g<sha>, symbology's chart-preview workflow) are
// resolved to the newest by semver. helm does the registry work — the
// same binary the runner and every operator already has — with the lab
// root CA passed explicitly, exactly like the release workflows.
func resolveChart(root, ref, version, followPR string) (dir, resolved string, err error) {
	ca := filepath.Join(root, "fleet/infrastructure/configs/step-ca-root.pem")
	constraint := version
	if followPR != "" {
		n, err := prNumber(followPR)
		if err != nil {
			return "", "", err
		}
		constraint = fmt.Sprintf(">=0.0.0-pr.%d.0 <0.0.0-pr.%d.0", n, n+1)
	}
	show, err := exec.Command("helm", "show", "chart", ref, "--version", constraint, "--ca-file", ca).Output()
	if err != nil {
		return "", "", fmt.Errorf("helm show chart %s --version %q: %s", ref, constraint, exitDetail(err))
	}
	var meta chartMeta
	if err := yaml.Unmarshal(show, &meta); err != nil {
		return "", "", err
	}
	if followPR != "" && !regexp.MustCompile(`^0\.0\.0-pr\.`+regexp.QuoteMeta(followPR)+`\.`).MatchString(meta.Version) {
		return "", "", fmt.Errorf("resolved %s, not a PR %s preview build", meta.Version, followPR)
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
