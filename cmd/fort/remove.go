package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
)

// remove is the inverse of add, scoped to one app (forts#251): the app
// dir leaves, its entry leaves the apps kustomization, and when the
// app had a listener, its certificate and listener files leave the
// gateway dir with their two entries in its kustomization. Nothing
// else is touched — the Gateway, the redirect and the issuer are the
// estate's by then, and stay when the last listener goes; the next add
// appends to them. Like dismantle, remove deletes what the core wrote
// and names what else still mentions the app (the residue): a layer's
// render outside the app dir, another inventory's sops: ref into the
// app's Secret. The default stages the removal for a PR; flux prunes
// the workload on the merge (the apps Kustomization has prune: true).
func remove(args []string) error {
	if len(args) < 2 || strings.HasPrefix(args[0], "-") || strings.HasPrefix(args[1], "-") {
		return fmt.Errorf("usage: fort remove <fort> <app | oci-ref> [--dry-run]")
	}
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "print the plan and remove nothing (default: remove and stage)")
	fs.Bool("write", true, "deprecated no-op: writing is the default")
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	root, err := repoRoot()
	if err != nil {
		return err
	}
	p, err := planRemove(root, args[0], args[1])
	if err != nil {
		return err
	}
	if !*dryRun {
		if err := p.apply(); err != nil {
			return err
		}
		if err := (sopsCLI{root: root}).GitAdd(filepath.Join("flux/apps", p.fort)); err != nil {
			fmt.Fprintf(os.Stderr, "warning: git add flux/apps/%s: %v\n", p.fort, err)
		}
	}
	p.report(os.Stdout, !*dryRun)
	return nil
}

type removePlan struct {
	root, fort, app string
	pin             *chartPin
	removes         []string // repo-relative: the app dir, then the gateway files
	edits           []dEdit
	edited          map[string]string // post-edit text, for the report's judgments
	// gatewayEmpty: no listener patch remains after this removal.
	gatewayEmpty bool
	// nested: dirs under the app dir that carry an inventory or a pin
	// of their own — a layer's render beneath the app, which goes with
	// the dir.
	nested  []string
	residue []string
}

func planRemove(root, fort, target string) (*removePlan, error) {
	if _, err := manifest.Load(root, fort); err != nil {
		return nil, err
	}
	apps := filepath.Join("flux/apps", fort)
	if !exists(root, apps) {
		return nil, fmt.Errorf("%s has no %s tree: nothing to remove", fort, apps)
	}
	app, pin, err := resolveApp(root, apps, target)
	if err != nil {
		return nil, err
	}
	p := &removePlan{root: root, fort: fort, app: app, pin: pin, edited: map[string]string{}}
	dir := filepath.Join(apps, app)
	p.removes = append(p.removes, dir)
	p.nested, _ = inventoryDirs(root, dir)
	for i, d := range p.nested {
		p.nested[i] = strings.TrimPrefix(d, dir+"/")
	}

	p.edits = append(p.edits, dEdit{filepath.Join(apps, "kustomization.yaml"), "- " + app, func(s string) (string, error) {
		return removeListItem(s, "resources", app)
	}})

	// The gateway half: present when add rendered a listener for the app.
	gw := filepath.Join(apps, "gateway")
	cert, listener := "certificate-"+app+".yaml", "listener-"+app+".yaml"
	hasCert, hasListener := exists(root, filepath.Join(gw, cert)), exists(root, filepath.Join(gw, listener))
	if hasCert {
		p.removes = append(p.removes, filepath.Join(gw, cert))
	}
	if hasListener {
		p.removes = append(p.removes, filepath.Join(gw, listener))
	}
	if hasCert || hasListener {
		var notes []string
		if hasCert {
			notes = append(notes, "- "+cert)
		}
		if hasListener {
			notes = append(notes, "patches: - "+listener)
		}
		p.edits = append(p.edits, dEdit{filepath.Join(gw, "kustomization.yaml"), strings.Join(notes, "; "), func(s string) (string, error) {
			var err error
			if hasCert {
				if s, err = removeListItem(s, "resources", cert); err != nil {
					return "", err
				}
			}
			if hasListener {
				if s, err = removePatchEntry(s, listener); err != nil {
					return "", err
				}
			}
			return s, nil
		}})
	}

	// The edits run now, so a tree that is not the shape add wrote
	// fails the plan (and the dry run) rather than the apply.
	for _, e := range p.edits {
		raw, err := os.ReadFile(filepath.Join(root, e.path))
		if err != nil {
			return nil, err
		}
		out, err := e.fn(string(raw))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.path, err)
		}
		p.edited[e.path] = out
	}
	if hasListener {
		p.gatewayEmpty = !strings.Contains(p.edited[filepath.Join(gw, "kustomization.yaml")], "\n  - path: listener-")
	}
	p.residue = scanResidue(root, app, p.removes, p.edited)
	return p, nil
}

// pinNote: the pin as the report names it — the tag, the PR, or main.
func pinNote(pin *chartPin) string {
	switch {
	case pin.followPR != "":
		return "PR " + pin.followPR
	case pin.followMain:
		return "main"
	}
	return pin.version
}

// resolveApp names the app dir a target means: the dir's own name, or
// the OCI ref its repository.yaml pins. Only a dir add rendered — one
// with an OCIRepository pin — qualifies; gateway/ is the fort's. An
// OCI ref two dirs pin (add --name, twice) is ambiguous, and the error
// names them.
func resolveApp(root, apps, target string) (string, *chartPin, error) {
	if strings.HasPrefix(target, "oci://") {
		url := "oci://" + strings.SplitN(strings.TrimPrefix(target, "oci://"), ":", 2)[0]
		entries, err := os.ReadDir(filepath.Join(root, apps))
		if err != nil {
			return "", nil, err
		}
		var names []string
		var match *chartPin
		for _, e := range entries {
			if !e.IsDir() || e.Name() == "gateway" {
				continue
			}
			pin, _ := readChartPin(filepath.Join(root, apps, e.Name(), "repository.yaml"))
			if pin != nil && pin.url == url {
				names = append(names, e.Name())
				match = pin
			}
		}
		switch len(names) {
		case 0:
			return "", nil, fmt.Errorf("no app under %s pins %s", apps, url)
		case 1:
			return names[0], match, nil
		}
		sort.Strings(names)
		return "", nil, fmt.Errorf("%s is pinned by %s under %s — name the app instead", url, strings.Join(names, " and "), apps)
	}
	app := target
	if app == "gateway" {
		return "", nil, fmt.Errorf("gateway/ is the fort's, not an app's: it stays when its last listener leaves")
	}
	if app != filepath.Base(app) || app == "." || app == ".." {
		return "", nil, fmt.Errorf("%s: an app is the name of a dir under %s", target, apps)
	}
	if !exists(root, filepath.Join(apps, app)) {
		return "", nil, fmt.Errorf("no app %s under %s", app, apps)
	}
	pin, err := readChartPin(filepath.Join(root, apps, app, "repository.yaml"))
	if err != nil {
		return "", nil, fmt.Errorf("%s/%s: %w", apps, app, err)
	}
	if pin == nil {
		return "", nil, fmt.Errorf("%s/%s carries no OCIRepository pin: not an app fort add rendered — remove it by hand", apps, app)
	}
	return app, pin, nil
}

// apply: the edits first (read-modify-write), then the removes.
func (p *removePlan) apply() error {
	for _, e := range p.edits {
		if err := os.WriteFile(filepath.Join(p.root, e.path), []byte(p.edited[e.path]), 0o644); err != nil {
			return err
		}
	}
	for _, rel := range p.removes {
		if err := os.RemoveAll(filepath.Join(p.root, rel)); err != nil {
			return err
		}
	}
	return nil
}

// report prints the marked tree in add's shape, then what else still
// names the app, then what the human does next.
func (p *removePlan) report(w io.Writer, written bool) {
	base := filepath.Join("flux/apps", p.fort)
	fmt.Fprintf(w, "%s/   (remove %s: %s %s)\n", base, p.app, p.pin.url, pinNote(p.pin))
	rel := func(path string) string { return strings.TrimPrefix(path, base+"/") }
	var gateway []string
	for _, r := range p.removes {
		if strings.HasPrefix(rel(r), "gateway/") {
			gateway = append(gateway, rel(r))
		}
	}
	for _, e := range p.edits {
		if strings.HasPrefix(rel(e.path), "gateway/") {
			gateway = append(gateway, rel(e.path))
		}
	}
	if len(gateway) > 0 {
		fmt.Fprintln(w, "○ gateway/")
		for _, r := range p.removes {
			if strings.HasPrefix(rel(r), "gateway/") {
				fmt.Fprintf(w, "  ✗ %s\n", strings.TrimPrefix(rel(r), "gateway/"))
			}
		}
		for _, e := range p.edits {
			if strings.HasPrefix(rel(e.path), "gateway/") {
				fmt.Fprintf(w, "  ○ %-28s %s\n", strings.TrimPrefix(rel(e.path), "gateway/"), e.note)
			}
		}
	}
	for _, e := range p.edits {
		if !strings.HasPrefix(rel(e.path), "gateway/") {
			fmt.Fprintf(w, "○ %-30s %s\n", rel(e.path), e.note)
		}
	}
	note := "the app dir, whole"
	if len(p.nested) > 0 {
		note = "the app dir, whole — with " + strings.Join(p.nested, ", ") + " beneath it (a layer's render)"
	}
	fmt.Fprintf(w, "✗ %-30s %s\n", p.app+"/", note)
	fmt.Fprintln(w)
	if p.gatewayEmpty {
		fmt.Fprintln(w, "note: no listener remains. gateway/ keeps the Gateway and the redirect: they are the")
		fmt.Fprintln(w, "      fort's, and the next `fort add … --http` or contract http block appends to them.")
		fmt.Fprintln(w)
	}
	if len(p.residue) > 0 {
		fmt.Fprintf(w, "residue — files outside this plan that still name %s:\n", p.app)
		for _, r := range p.residue {
			fmt.Fprintf(w, "  %s\n", r)
		}
		fmt.Fprintln(w, "  a sops: ref into the app's Secret from another inventory is now dangling —")
		fmt.Fprintln(w, "  `fort check` on that fort reports it; a layer's file naming the app goes by hand.")
		fmt.Fprintln(w)
	}
	if !written {
		fmt.Fprintln(w, "dry run — nothing removed.")
		return
	}
	fmt.Fprintln(w, "staged. then: review, commit, PR — flux prunes the app on merge.")
}
