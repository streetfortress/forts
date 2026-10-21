package main

import (
	"fmt"
	"os"
	"path/filepath"

	"gitea.zen.lofi/sfi/forts/internal/estate"
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gitea.zen.lofi/sfi/forts/internal/tools"
)

// The two link verbs (forts#55) read ANOTHER estate's repo: `fort link
// add` the mesh it is joining (estate.toml for the subnet, the
// interface and the zone; mesh/peers.json and the manifests for the
// addresses taken), `fort estate link` the estate whose hosts link here
// (the manifests, for their [links.<name>] tables). Both read TOML and
// JSON out of a shallow clone and nothing else — never that estate's
// Nix (docs/links.md) — through this one seam, so a test hands them a
// directory instead.

// estateFetcher yields a checkout of an estate's default branch: the
// directory, and how to drop it.
type estateFetcher func(repo string) (dir string, cleanup func(), err error)

// gitFetch is the real fetcher: `git clone --depth 1` into a temp dir,
// with whatever credential the operator's own git has for the forge —
// the verbs run where the age key is, so the admin's access covers it;
// the hub's read-only deploy key stays the reconciler's alone.
func gitFetch(repo string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "fort-fetch-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	cmd, err := tools.Command("git", "clone", "-q", "--depth", "1", repo, dir)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("git clone %s: %s", repo, lastLine(string(out)))
	}
	return dir, cleanup, nil
}

// fetchedEstate is what a fetch yields once read: the estate, its mesh
// and its manifests. Problems are the manifests that did not parse —
// reported, never fatal: that estate's gate judges them.
type fetchedEstate struct {
	dir      string
	est      estate.Estate
	mesh     mesh
	hosts    map[string]manifest.Host
	problems map[string]error
}

// readEstate reads a checkout the way the verbs need it. The URL the
// operator typed must name the estate the checkout says it is: a clone
// of the wrong repo, or an estate.toml whose repo is not where it
// lives, is refused before an address is allocated against it.
func readEstate(dir, repo string) (*fetchedEstate, error) {
	est, err := estate.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", repo, err)
	}
	if !estate.SameRepo(est.Source.Repo, repo) {
		return nil, fmt.Errorf("%s: its %s says repo = %q, not this URL — the wrong repo, or an estate.toml that does not know where it lives", repo, estate.Path, est.Source.Repo)
	}
	m, err := loadMesh(dir)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", est.Ref(), err)
	}
	// An estate with no fort yet has no forts/ directory: nothing taken,
	// nothing linking.
	hosts, problems := map[string]manifest.Host{}, map[string]error{}
	if exists(dir, manifest.Dir) {
		if hosts, problems, err = manifest.LoadAll(dir); err != nil {
			return nil, fmt.Errorf("%s: %w", est.Ref(), err)
		}
	}
	return &fetchedEstate{dir: dir, est: est, mesh: m, hosts: hosts, problems: problems}, nil
}

// fetchEstate fetches and reads in one call.
func fetchEstate(fetch estateFetcher, repo string) (*fetchedEstate, func(), error) {
	dir, cleanup, err := fetch(repo)
	if err != nil {
		return nil, nil, err
	}
	fe, err := readEstate(dir, repo)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return fe, cleanup, nil
}

// dirFetcher is the fetcher tests use: every URL is one directory.
func dirFetcher(dir string) estateFetcher {
	return func(string) (string, func(), error) {
		abs, err := filepath.Abs(dir)
		return abs, func() {}, err
	}
}
