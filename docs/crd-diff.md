# `fort crd-diff` — the chart-bump detector

Renovate opens a chart-bump PR and pastes the upstream release notes into
the body. The traefik bump that raised this (sfi/deployments#596) carried
about two hundred lines of them, and one line said the release changes a
CRD. Nothing in CI read that line. A reviewer approves the bump, the tree
moves to the new chart version, and the cluster keeps the old CRD schema
— and the gap then stays silent until a later manifest needs the new
field.

`fort crd-diff` reads the chart instead of the release notes. For every
HelmRelease in the checkout whose `chart.spec.version` differs from the
one a base revision holds, it pulls both chart versions and compares
their CRD files.

```
fort crd-diff [--base <rev>] [--chart-cache <dir>]
```

It writes nothing, reaches no cluster, and needs no key. CI runs it on
every push — a job of its own in sfi/forts, a step of the gate in an
estate ([init](init.md)) — and it annotates when `GITEA_ACTIONS` is set.

## Why a CRD needs its own check

A chart delivers a CRD in one of three shapes, and only one of them
leaves the release lifecycle. The long form is the knowledge base's
`config-management/crd-schema-ownership.md`; the short form is two
defaults:

- A file under a chart's own `crds/` directory is **install-only** by
  Helm's design. Helm installs it when the release is created and never
  touches it again — no upgrade, no rollback, no uninstall.
- The helm-controller defaults `spec.upgrade.crds` to **`Skip`**, which
  neither replaces an existing CRD nor creates a new one. `Skip` is
  stronger than "flux does not update CRDs": a chart release that adds a
  brand-new CRD adds **nothing** to the cluster.

So the chart version in the tree stops describing the CRD schema on the
cluster after the first bump, and no annotation on the object answers the
question either — the traefik CRDs carry only
`controller-gen.kubebuilder.io/version`, which read the same string
across a bump that changed the `Middleware` schema. That is a drift with
no instrument, and this verb is the instrument.

sfi/forts#116 is the ownership half of the same problem: every
HelmRelease whose chart ships a `crds/` directory sets `install.crds` and
`upgrade.crds` to `CreateReplace`.

## What it compares

1. Walk the checkout for every HelmRelease that pins a chart version, and
   every HelmRepository they name. The scope is the whole tree, not a
   list of directories: this repo holds its charts under
   `cmd/fort/templates`, an estate under `flux/`, and the next chart to
   move its CRDs into `crds/` gives no warning. `.git` and `testdata` are
   skipped — a chart version in a golden fixture is a test input, not a
   pin a cluster follows.
2. Read the same releases out of `--base` (`origin/main` by default) with
   `git show <rev>:<path>`, and keep the releases whose version differs.
   One file can hold several HelmReleases, so the old version comes from
   the document with the same name and namespace.
3. `helm pull` both versions of each one.
4. Compare the CRD files of the two charts: every file that differs,
   every file the new version adds, every file it removes, with a line
   delta beside each name. Then print the diff (`git diff --no-index
   --unified=1 --no-renames`, capped at 1200 lines, with the `helm pull`
   commands to read the rest).

Only the `spec.chart` form is read. A `chartRef` release pins an
OCIRepository, which is this estate's own chart — the shape
[`fort add`](add.md) renders — and an SFI chart ships no CRD.

## Where the CRD files are decides the verdict

**The chart's own `crds/` belongs to the release.** The two keys of #116
decide whether the new schema ever reaches a cluster, so the verb reads
them:

| `spec.upgrade.crds` | verdict |
|---|---|
| `CreateReplace` | **warning** — flux applies every changed file. Read the diff: a CRD change is a schema change for every object of that kind. |
| anything else, unset included | **error** — the cluster keeps the old schema. Set `install.crds` and `upgrade.crds` to `CreateReplace` on the HelmRelease. |

A release the base revision does not have is the one exception:
`spec.install.crds` defaults to `Create`, so flux *does* install the
chart's CRDs at the first reconcile. The error there names the bump
after it — with `upgrade.crds` unset, no later chart bump moves them.

**A subchart's `crds/` belongs to the subchart's condition.** The fleet
disables every CRD-bearing subchart it pins (kube-prometheus-stack's
`crds`, loki's and thanos's operators), so those files install nothing
and no CRDsPolicy changes that. The finding is always a **warning**. It
is still a finding: the day somebody enables the subchart, the cluster
inherits install-only CRDs that nobody read.

The shape of the path is exact — `crds` at a chart root, where a chart
root is the top of the archive or `charts/<subchart>`, repeated. "Any
directory called `crds`" is the wrong rule and it reads a safe chart as a
dangerous one: kube-prometheus-stack carries a subchart *named* `crds`,
whose `templates/` CRDs an upgrade applies like any other manifest.

## CI reads one commit of the base branch

`actions/checkout` clones at depth 1, so the checkout holds no `main` and
`git show origin/main:<path>` has nothing to read. One commit is all the
verb wants — it compares two chart versions and never walks the history
between them:

```sh
git fetch --no-tags --depth=1 origin main
```

Without that fetch the verb refuses and names the command, rather than
reporting every release as new.

## Reading the report

```
== chart versions that moved against origin/main
  traefik 41.0.2 → 41.6.0  (flux/infrastructure/controllers/traefik/release.yaml)
    CRD files: 25 → 25; 16 differ, 0 added, 0 removed
      ~ crds/hub.traefik.io_apis.yaml                             +37 -1
      ~ crds/traefik.io_middlewares.yaml                          +8 -0
      …
    diff --git a/old/traefik/crds/traefik.io_middlewares.yaml b/new/…
    +                  errorRequestHeaders:
    …
== chart CRDs
  ✗ flux/infrastructure/controllers/traefik/release.yaml: traefik 41.0.2 → 41.6.0
    moves the chart's own crds/ directory (16 changed, 0 added, 0 removed):
    spec.upgrade.crds is unset, …
```

A release whose chart has no `crds/` directory on either version is
reported and passes: its CRDs, if it has any, are templates of the
release, which an upgrade applies.

A bump that leaves an install-only `crds/` directory **unchanged** moves
no schema, so it is safe — but the verb still warns when
`upgrade.crds` is not `CreateReplace`, because the next bump that does
move a CRD lands nothing on a cluster. That is the cheapest moment to
say so: the verb already knows the chart ships the directory.

The verb exits nonzero on any error finding. A chart it cannot pull is an
error too — a detector that drops a finding quietly is the one failure it
must not have.

`--chart-cache <dir>` reads `<dir>/<version>/<chart>` instead of pulling,
which is how the tests run with no registry.

## The same comparison by hand

```sh
helm pull traefik --repo https://traefik.github.io/charts --version 41.0.2 --untar --untardir old
helm pull traefik --repo https://traefik.github.io/charts --version 41.6.0 --untar --untardir new
diff -rq old/traefik/crds new/traefik/crds
```

16 of 25 files differ. 15 are `hub.traefik.io` CRDs; the 16th is
`traefik.io_middlewares.yaml`, and it adds one field,
`errorRequestHeaders`.
