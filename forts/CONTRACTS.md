# fort add — contracts, overlays, and the merge gate (#144 design)

Design for the service-scaffolding half of the fort CLI: how a chart
declares what it needs, how `fort add` turns that into reviewed manifests
under `fleet/apps/<fort>/`, and how CI refuses a fort whose wiring does
not match the chart it pins. Decisions recorded here were taken in the
2026-09-13 design session; the sequencing at the end is deliberately
incremental (base case first, one provider per PR, decommissioning
separately under #217). A worked example — symbology 0.1.3 on a fresh
fort, and what adopting the contract changes on sfi-prod — closes the doc.

Predictability first: every convention below exists so that two people
running `fort add` on the same chart produce the same tree, and so that
the only judgement calls left are the ones a reviewer can see in the PR.

## The contract lives with whoever authored the workload

One file name, one schema, two locations:

- **In the chart tarball** — `secrets.contract.yaml` at the chart root,
  owned by the chart author. It says what the app needs from *any*
  environment: the keys its `existingSecret` must carry and how each
  value is produced, the platform services it consumes and which values
  paths carry their endpoints, the Service the fleet routes to, the
  metrics port. Never a namespace, hostname, cluster, or provider
  instance. Helm ignores unknown files at the chart root, so it ships
  with `helm package` and comes back with `helm pull --untar`. One
  artifact, one version pin: the contract cannot skew from the chart it
  describes.
- **Beside the fort's manifests** — the same file name in
  `fleet/apps/<fort>/<app>/`, **optional**, an *overlay* merged on top of
  the chart's: fort keys win, `sources` and `env:` refs are fort-only,
  `requires.<x>.provider` is overridable. It exists only when the fort
  deviates from convention — a non-default provider placement, a
  migration-era seeding source, a superset key kept during a cutover.
- **Provider directories** (`sites-db/`, `valkey/`, `rustfs/`) are not
  our charts, so `fort add` renders their contract into the provider dir
  from its embedded template. Same schema, same seeder, same gate.

Charts that ship no contract fall back to the fort-side file being the
whole contract — which is exactly the nine files the repo carries today.
No rename, no flag day: a chart adopts the pattern by shipping the file,
and the fort's copy shrinks to an overlay or disappears at the next
`fort add`/adoption pass.

`fort seed`, `fort status`, and CI all resolve the effective contract the
same way: pull the chart at the tag the app's OCIRepository pins, merge
the overlay if present, resolve provider outputs by convention against
the tree.

## Contract schema (v1)

```yaml
# secrets.contract.yaml — at the chart root, inside the tarball. NON-SECRET.
contract: v1

# The one Secret the chart consumes (values path naming it + the keys it
# must carry). The chart never creates it.
secret:
  values: existingSecret        # values path naming the Secret
  keys:
    APP_SECRET:        {generate: hex32}
    RESEND_API_KEY:    {external: "Resend → API keys", optional: true}
    DATABASE_PASSWORD: {template: "${postgres.db.password}"}

# Platform services the app consumes. Each instance exposes a fixed set of
# outputs (below); secret-typed outputs may only be referenced from
# `secret.keys` templates, non-secret ones may also be wired into values.
requires:
  postgres:
    db:                          # instance handle → ${postgres.db.*}
      extensions: [vector]
      wire:                      # values paths that carry non-secret outputs
        host: database.host
        port: database.port
        name: database.name
        user: database.user
  valkey:
    cache:
      optional: true             # the app runs without it (gate: missing provider is fine)
      wire: {host: cache.host, port: cache.port, db: cache.db, user: cache.user}
  s3:
    media: {}

# Ingress: the fleet owns Gateway listener + Certificate + HTTPRoute; the
# chart names the backend.
http:
  service: umami                 # Service name the HTTPRoute targets
  port: 3000

# Optional. Rendered into monitoring.yaml as a PodMonitor.
metrics:
  port: metrics
  selector: {app.kubernetes.io/component: worker}
```

Outputs per resource type (key names follow the Service Binding spec so
a chart written against `postgres.db.password` works whichever provider
satisfies it):

| Type | Outputs | Secret-typed |
|---|---|---|
| `postgres` | `host`, `port`, `name`, `user`, `password`, `uri` | `password`, `uri` |
| `valkey` | `host`, `port`, `db`, `user`, `password`, `uri` | `password`, `uri` |
| `s3` | `endpoint`, `bucket`, `accessKey`, `secretKey` | `secretKey` |

The vocabulary is **closed** and small on purpose: three provider types
plus `secret`, `http`, `metrics`. No transitive requires (a provider's
own needs — sites-db's backup credentials, valkey's default user — are
the provider directory's own contract, rendered from the template).
Adding a type means adding a template directory to the CLI and a row to
this table, nothing else.

Value primitives:

| Primitive | Meaning | Allowed in |
|---|---|---|
| `generate` | fresh randomness (`hexN` / `b64_N`) | chart, overlay |
| `template` | compose from `${type.instance.output}` and overlay vars | chart, overlay |
| `external` | a human supplies it; `optional: true` if the app runs without it | chart, overlay |
| `literal` | verbatim | **overlay only** — non-secret config is chart values |
| `env`, `sops` | copy from a sops'd dotenv / another sops file | overlay only |

Two rules the chart-side linter enforces (`fort contract lint`, run in
the chart's release workflow): no `literal` (a value that is not secret
is a value, so a change rolls pods — the symbology#180 lesson), and
every `wire` path exists in `values.yaml`.

**Pending means absent.** An `external` key with no value yet is simply
left out of the Secret — no sentinel, no marker file. The seeder skips
it and reports it; `fort seed --set KEY` reads the value from stdin and
adds it with `sops set`; CI's key-set comparison fails a `production:
true` fort on any missing non-optional key and warns elsewhere.

## Overlay schema

Same file name and schema, in the fort's app dir, present only when
needed:

```yaml
# fleet/apps/<fort>/<app>/secrets.contract.yaml — OVERLAY on the chart's
contract: v1
requires:
  postgres.db:
    provider: {fort: db-fort, service: sites-db}   # non-default placement
sources:
  legacy-env: ~/app/secrets/production.env         # migration-era seeding input
secret:
  keys:
    STRIPE_SECRET_KEY: {env: [legacy-env, STRIPE_SECRET_KEY]}   # instead of typing it
    DATABASE_HOST: {literal: "..."}                 # superset kept during a cutover
```

`provider` has three shapes, in order of preference:

1. **absent** → shared instance on this fort, named by type (`sites-db`,
   `valkey`, `rustfs`). Config B is production's topology. Created from
   the embedded template on first need.
2. `{dedicated: true}` — the app's own instance in its own namespace
   (`fort add --dedicated postgres`). web3's old shape.
3. `{fort: db-fort, service: sites-db}` — **another fort's instance over
   the mesh**, for the dedicated-DB-fort + scattered web/cache forts
   topology. Host resolves from inventory `wg_vpn_ip` plus the service's
   mesh port; credentials come in as cross-fort `sops:` refs, which the
   seeder already supports (admin key decrypts the source, output
   encrypts to *this* fort's recipients — how sfi-prod was seeded from
   staging). Needs the provider's `-rw` Service exposed on the mesh IP
   (NodePort, the rustfs `10.1.0.5:30900` precedent). Follow-up scope,
   but the shape accommodates it from day one.

## What `fort add` renders

`fort add <fort> <oci-ref>[:<version>] [--name <app>] [--hostname <fqdn>] [--dedicated <type>...] [--without <type>...]`

Pure generator: reads the chart's contract, `provisioning/inventory.yaml`,
and the existing fort tree; writes **new files** and **appends lines to
kustomization resource/patch lists**; never rewrites an existing file.
Refuses if `fleet/apps/<fort>/<app>/` exists (scaffold's force-no
semantics). Everything is staged for a PR; nothing is applied. Optional
requirements are provisioned by default (`--without valkey` skips one),
so the out-of-the-box result is the full topology.

Conventions that make the output deterministic (no flags for these):

| Thing | Convention |
|---|---|
| namespace, release name, app dir | `<app>` = chart name unless `--name` |
| Secret | `<app>-env` in namespace `<app>` |
| postgres tenant | database `<app>`, role `<app>`, secret `sites-db-<app>-credentials`, per-tenant file `secrets-<app>.sops.yaml` in the provider dir |
| valkey tenant | ACL user `<app>`, next free logical db, key `<app>` in the shared `valkey-users` secret |
| s3 | bucket `<fort>-<app>` |
| hostname | `<app>.<fort>.streetfortress.cloud` unless `--hostname` (apex domains are always explicit) |
| HelmRelease | `install.remediation.retries: -1` and `upgrade.remediation.retries: -1` — retry until ready, never park on a failed state waiting for an ops nudge |

**Base case** (chart with `secret` + `http` only — sfi-website today):

```
fleet/apps/<fort>/<app>/
  kustomization.yaml       namespace, repository, release, secrets.sops.yaml (listed, absent until seeded), route, monitoring
  namespace.yaml
  repository.yaml          OCIRepository pinned to the version fort add resolved; certSecretRef for gitea.zen.lofi
  step-ca-root.pem         + secretGenerator (the existing per-app pattern)
  release.yaml             HelmRelease: existingSecret, wire: values, retries -1
  route.yaml               HTTPRoute → contract.http
  monitoring.yaml          PodMonitor if contract.metrics
  (no secrets.contract.yaml — the chart's is the contract)
fleet/apps/<fort>/gateway/  created from template on first http need: http listener only,
  gateway.yaml              the lab step-ca issuer (or nothing on cloud forts, where
  cluster-issuer-*.yaml     letsencrypt is shared infrastructure), and the catch-all
  redirect-https.yaml       http→https RequestRedirect route (#232 — the gap the first
                            curl on the drill fort found)
  certificate-<app>.yaml
  listener-<app>.yaml      JSON6902 patch: add /spec/listeners/- (+ one line in gateway/kustomization.yaml `patches:`)
fleet/apps/<fort>/kustomization.yaml   + `- <app>`
```

The listener patch is the one designed exception to "new files only":
listeners live in one Gateway object, and a strategic-merge patch cannot
append to a CRD list (kustomize has no merge key for it). A JSON6902
`add` to `/spec/listeners/-` appends deterministically and keeps the
Gateway file untouched. Gateway API's ListenerSet is the eventual
replacement once traefik ships it.

**Provider registration** (when `requires` names a type), same shape for
each — create the shared instance from the embedded template if absent
(including its own `secrets.contract.yaml`), then register the tenant
with new files + kustomization lines:

| Type | Instance (first need) | Per-tenant files |
|---|---|---|
| `postgres` | `sites-db/` from the sfi-prod template (cluster, backup, contract) | `database-<app>.yaml` (Database CR, extensions); `role-<app>.yaml` JSON6902 patch adding the `managed.roles` entry; provider contract gains `secrets-<app>.sops.yaml` (`username` literal, `password` generate hex24) |
| `valkey` | `valkey/` from PR #182's staging template (cache profile) or luxor's (persistent) | `acl-<app>.yaml` JSON6902 patch adding `auth.aclUsers.<app>`; key `<app>` added to the existing `valkey-users` contract → seeder's **add-missing-keys** mode (`sops set`) |
| `s3` | `rustfs/` from luxor's template | bucket entry in the bucket Job; **credential model open** — luxor hands out the instance's root keys today; per-app IAM users via `mc admin user` in the same Job is the likely shape. Last in sequencing for that reason |

Operators (CNPG) stay fleet-wide in `fleet/infrastructure/controllers`;
`fort add` never touches the infrastructure layer. If an operator ever
becomes per-fort optional, that is a separate change to how
`fleet/clusters/<fort>/infrastructure.yaml` selects controllers, and
`fort status` gets an invariant ("fort requires postgres → cnpg present").

**Post-add report**: files written, provider instances created, the
`fort seed` invocation (providers first), and every external key with
its description. Nothing secret is printed.

## The gate is the product

`ci/fleet-checks.py` grows one check per app whose pinned chart ships a
contract. Because the repo's sops rules encrypt only `data`/`stringData`
*values*, the key names of every committed Secret are plaintext — CI can
verify the wiring without any age key:

1. `helm pull` the chart at the tag the app's OCIRepository pins (the
   `ubuntu-deploy` runner already has helm and sops; it needs the
   step-ca root and reaches gitea over the mesh; cache by tag). Merge
   the overlay if present.
2. The Secret's `stringData` key set ⊇ the contract's non-optional keys.
   Missing non-optional keys **fail** on `production: true` forts and
   warn elsewhere. Extra keys warn (the #180 superset pattern during a
   migration) — never fail.
3. Every non-optional `requires` instance resolves to a provider dir in
   the tree (or an overlay `provider`), and its `wire` values paths are
   set in the HelmRelease.
4. Secret-typed outputs never appear in values.

The payoff: a Renovate chart bump whose new contract adds a required key
goes **red until seeded**, which is exactly the misconfiguration this
design wants blocked from merge. Charts without a contract pass
unchanged (the gate is opt-in by shipping the file), so the fleet's
third-party charts are unaffected.

`fort status <fort>` derives the same facts locally (apps, providers,
tenants, missing keys, chart-vs-contract drift); `fort status --all`
is where fleet invariants live ("every production fort has a dead-man
rule", #217; "every production Secret is complete").

Acceptance test for the generator itself: `fort add --dry-run` against
the charts already deployed on sfi-prod must reproduce the committed
tree modulo hand edits. Golden-file tests over the embedded templates
make "deterministic" a checked claim rather than a hope.

## Rotation is an operational duty, so design for it now

Every seeding today is create-once (existing files are never
overwritten), and the lockstep pairs — the sites-db role password
mirrored into symbology's env, the valkey ACL password mirrored as
`CACHE_PASSWORD` — are exactly where rotation hurts first. The contract
graph makes it mechanical, with no registry:

`fort rotate <fort> <app> [<KEY>] [--value-from-stdin]`

1. Regenerate the named `generate:` key(s) in place with `sops set`
   (or take a new `external` value from stdin — same primitive, so an
   API-key rotation is the same command). For a provider tenant secret
   (`fort rotate <fort> sites-db symbology`), the same.
2. Resolve every app's effective contract on the fort; every `template`
   that references the rotated output is re-resolved and `sops set`.
3. Stage everything, report which Secrets changed. One PR carries the
   whole pair, so the DB role and the app env change in the same merge.

Two consequences to accept and record:

- **Pods do not roll on external Secret content** (the #180 lesson;
  `envFrom` reads at container start). CNPG reconciles `managed.roles`
  passwords live, so after the merge the database has the new password
  and the app pods still hold the old one until they restart — and the
  ops kubeconfig is deliberately read-only, so nobody can `rollout
  restart`. The fix is a controller: **Reloader** (stakater) fleet-wide
  in `fleet/infrastructure/controllers`, and the chart guideline that
  workloads carry `reloader.stakater.com/auto: "true"`. Seconds of
  auth failures between the role update and the roll are the accepted
  window; the HelmRelease's retry-forever posture absorbs it.
- The seeder needs a **sops set** path for existing files (add missing
  keys, replace a named key). Valkey tenant registration and `--set`
  need it too, so it lands in PR 1 rather than with rotate.

## Chart author guidelines (internal today; the public interface later)

1. **The chart never creates Secrets.** One `existingSecret` (default
   `<chart>-env`) injected via `envFrom`; a missing Secret leaves pods in
   `CreateContainerConfigError`, a legible failure, not a crash loop.
2. **Endpoints are values, passwords are secret-borne.** `database.host`
   / `cache.host` etc. as values, each rendered only when set (an empty
   block is byte-identical to the chart without it). Moving a database
   is then a HelmRelease edit that rolls pods atomically with the merge.
   The same goes for every non-secret env key (`BETTER_AUTH_URL`,
   `PUBLIC_*`, mail addresses): an `env:` map in values, not the Secret.
3. **Ship `secrets.contract.yaml` at the chart root** and lint it in the
   release workflow (`fort contract lint .`).
4. **The fleet owns ingress.** Expose one Service + port and name it in
   `http`; keep `httpRoute.enabled: false` by default.
5. **Migrations tolerate a database that is not there yet.** Hook Job
   with a generous `backoffLimit`, or migrate-on-start; the fort's
   HelmRelease retries forever, so the race resolves itself.
6. **Carry the Reloader annotation** on every workload that reads the
   Secret.
7. **Declare a metrics port** if you export any; the fort renders the
   PodMonitor. Alert rules stay fort-side: they encode fleet policy.
8. **Size for the 4 GB shape**: requests set, one replica default,
   nothing per-pod that belongs in valkey.
9. **Versioning**: first-party apps pin chart version = appVersion (one
   pin moves everything); wrapper charts (umami) version independently.

## Prior art, and why this is built rather than adopted

The closest tools all take one of two positions this repo has already
rejected. Score, KubeVela/OAM, and Timoni replace the chart as the unit
of deployment with their own workload spec. Crossplane compositions,
Radius recipes, Kratix promises, and Dapr add an in-cluster control
plane that generates or brokers at runtime — moving the generated
manifests out of git, against "the program declares, the human applies,
git is the record". Dapr in particular solves a different problem: a
runtime sidecar giving app code portable state/pubsub/secrets APIs. Our
apps keep speaking plain postgres and redis.

What is borrowed: Score's resource-type-with-outputs shape, the Service
Binding spec's output key names, and Kratix's insight that the pipeline
should write to the GitOps repo. What is built: a few Go templates and
a CI check. That is the right size for the problem.

## Worked example: symbology 0.1.3

The chart-side contract is sfi/symbology PR "chart: ship the fort
secrets contract in the tarball" (`helm/symbology/secrets.contract.yaml`
against the 0.1.3 values: `database.*`, `cache.*`, `symbology-ui:3000`,
worker `metrics`). Two scenarios.

### A fresh fort

`sfi-fort-e33e2d5` has `sites-db/` and nothing else. After

```
fort add sfi-fort-e33e2d5 oci://gitea.zen.lofi/sfi/symbology:0.1.3 --hostname symbology.online
fort seed sfi-fort-e33e2d5
```

the tree (● new, ○ existing + appended lines, ◆ written by seed):

```
fleet/apps/sfi-fort-e33e2d5/
○ kustomization.yaml            + gateway, valkey, symbology
● gateway/                      first http need → from template
    kustomization.yaml          resources: issuer, gateway.yaml, redirect-https.yaml, certificate-symbology.yaml; patches: listener-symbology.yaml
    cluster-issuer-step-ca.yaml lab issuer (per-lab-cluster, not shared infra)
    gateway.yaml                http listener only
    redirect-https.yaml         catch-all http→https 301 on the http listener
    certificate-symbology.yaml  letsencrypt, dnsNames [symbology.online]
    listener-symbology.yaml     JSON6902 add /spec/listeners/- {name: https-symbology, hostname, tls: symbology-tls}
○ sites-db/                     exists → tenant registration only
    kustomization.yaml          + database-symbology.yaml; patches: + role-symbology.yaml
  ● database-symbology.yaml     Database CR: name/owner symbology, extensions [vector]
  ● role-symbology.yaml         JSON6902 add /spec/managed/roles/- {name: symbology, login, passwordSecret: sites-db-symbology-credentials}
  ○ secrets.contract.yaml       + secrets-symbology.sops.yaml: {username: literal symbology, password: generate hex24}
  ◆ secrets-symbology.sops.yaml
● valkey/                       first valkey need → cache-profile template (PR #182)
    kustomization.yaml, namespace.yaml, repository.yaml
    release.yaml                aclUsers: default only; patches: acl-symbology.yaml
    acl-symbology.yaml          JSON6902 add /spec/values/auth/aclUsers/symbology {permissions: "~* &* +@all -@admin -@dangerous"}
    secrets.contract.yaml       valkey-users: {default: generate hex24, symbology: generate hex24}
  ◆ secrets.sops.yaml
● symbology/
    kustomization.yaml          namespace, repository, release, secrets.sops.yaml, route, monitoring
    namespace.yaml
    repository.yaml             OCIRepository oci://gitea.zen.lofi/sfi/symbology tag 0.1.3, certSecretRef
    step-ca-root.pem
    release.yaml                see below
    route.yaml                  HTTPRoute symbology.online → symbology-ui:3000, sectionName https-symbology
    monitoring.yaml             PodMonitor symbology-worker (component: worker, port metrics)
  ◆ secrets.sops.yaml           BETTER_AUTH_SECRET, DATABASE_PASSWORD, CACHE_PASSWORD
                                absent: STRIPE_SECRET_KEY, STRIPE_WEBHOOK_SECRET, OPENAI_API_KEY (required),
                                        RESEND_API_KEY, ANTHROPIC_API_KEY (optional)
    (no secrets.contract.yaml — nothing deviates from convention)
```

`release.yaml` as rendered — the `wire` paths filled from the providers'
conventional outputs, everything else chart defaults for the operator to
refine in place:

```yaml
spec:
  chartRef: {kind: OCIRepository, name: symbology, namespace: flux-system}
  install: {remediation: {retries: -1}}
  upgrade: {remediation: {retries: -1}}
  values:
    existingSecret: symbology-env
    database:
      host: sites-db-rw.sites-db.svc.cluster.local
      port: "5432"
      name: symbology
      user: symbology
    cache:
      host: valkey.valkey.svc.cluster.local
      port: "6379"
      db: "0"
      user: symbology
    # hand-refined after add: inference.address, ui.env (BETTER_AUTH_URL,
    # PUBLIC_*, RESEND_EMAIL_*, ADMIN_EMAILS), replicas for the 4 GB shape
```

The report ends with the three required external keys and the two
optional ones, each with the chart's description of where it comes from.
`fort seed sfi-fort-e33e2d5 symbology --set STRIPE_SECRET_KEY` adds one.
The PR's CI: kustomize builds (every listed sops file exists now); the
contract gate pulls 0.1.3 and finds three required keys missing. The
drill fort is `production: true` in inventory (hardened host profile),
so that is a **failure**, not a warning — the rehearsal is held to the
production bar, and test values go in with `--set` before the PR is
mergeable. Only a non-production fort gets the warning.

### Exercising the CLI against an unreleased chart

The drill fort is where `fort add` gets exercised, and the chart it
needs (contract included) is on a PR, not a tag. Symbology's
`chart-preview` workflow (sfi/symbology#189) publishes an **immutable
prerelease chart per PR commit**:

```
0.0.0-pr.<N>.<run>.g<sha>      chart version = OCI tag; appVersion = <sha>
```

`0.0.0-` sorts below every real release, so no `>=0.1.0` pin and no
Renovate bump can ever pick one up; `<run>` (the workflow run number)
is monotonic, so semver ordering among a PR's builds is chronological
— `g<sha>` alone would sort lexically. `appVersion = <sha>` makes the
chart pull the per-commit images ci.yaml already pushes for every
branch (`symbology-ui:<sha>`), so a preview costs one `helm package`.

The fort follows the PR head without a deployments commit per
iteration by pinning a semver *filter* rather than a tag:

```yaml
# fleet/apps/sfi-fort-e33e2d5/symbology/repository.yaml — preview pin
spec:
  ref:
    semver: ">=0.0.0-0"
    semverFilter: '^0\.0\.0-pr\.189\.'
```

Git still says what runs — "PR 189's head" — and the OCIRepository
status records the exact revision. Mutable tags (`pr-189`, overwritten
and re-pulled) were rejected for the same reason #150 rejected mutable
image tags: git stops describing what runs, and rolling back is
"re-push the old build". Promotion is unchanged: merge, `just tag`, the
fort's pin becomes a plain `tag: 0.1.4` (a hand edit on the drill fort,
Renovate elsewhere). Registry hygiene is a gitea package cleanup rule
on `^0\.0\.0-pr\.` by age — admin settings, not repo code.

### Adopting it on sfi-prod (the drift the gate would report today)

sfi-prod runs 0.1.2 with a full fort-side contract. Bumping to 0.1.3 and
reading the chart's contract, `fort status sfi-prod-439bf72` would say:

- **Secret is complete** — every required key is present (seeded from
  web3's production values). Nothing to seed.
- **Extra keys** (warn): `BETTER_AUTH_URL`, `ADMIN_EMAILS`, `PUBLIC_*`,
  `RESEND_EMAIL_FROM/REPLY_TO`, `DATABASE_HOST/PORT/NAME/USER` are in
  the Secret but not in the contract. They move to `ui.env` and
  `database.*` in release.yaml (the #180 follow-up), then get dropped
  from the Secret with `sops unset` at the next natural edit.
- **`wire` unset**: `database.*` values are empty in release.yaml while
  the contract wires them — set them (the sites-db tenant already exists
  under the conventional names, so the values are the ones above).
- **Optional provider absent**: no `valkey/` on the fort; `cache.*`
  stays empty; fine.
- The fort-side `secrets.contract.yaml` carries nothing the chart's does
  not, so it is deleted; sfi-prod's symbology dir ends up with no
  overlay. (staging-k3s never had one.)

That is the whole adoption: a values edit, a file deletion, and a later
Secret cleanup — no reseed.

## Status (what #225 lands)

`fort add` exists and reproduces the drill fort: the golden test
(`forts/cmd/fort/add_test.go`) replays the add of symbology's #189
preview chart against the fort's pre-add state and compares with the
committed tree — byte-exact for everything the generator owns, structural
for the files the post-add report says to refine (release values, the
root kustomization's comments) and for the seeding contract, where the
human replaced `external` keys with sops refs. Implemented:

- chart resolution by tag or `--follow-pr` (helm, lab CA), contract v1
  parsing (`internal/contract`), `--chart-dir` for chart authors
- the app dir; gateway from template on first http need, listener patch
  + certificate after; valkey from the cache template on first need, ACL
  patch + contract key after; **sites-db from the greenfield template on
  first need** (barman archive to luxor's rustfs, roles only via
  patches), tenant registration (Database CR, role patch, contract
  entry) after — tenant present → untouched
- the fort-side seeding contract in the python seeder's full form:
  provider passwords as `${<handle>_password}` vars backed by the
  conventional sops refs, non-secret outputs inlined, `external` keys
  carried through (the seeder now leaves them absent and says so)
- the marked-tree report (CLAUDE.md), dry-run by default, `--write`
  never overwrites, appends are text-level so comments survive

Two goldens: the drill fort (lab path, providers pre-existing) and a
fresh Linode fort for an sfi-website draft contract (cloud path, every
provider from template — a reviewed snapshot under
`forts/cmd/fort/testdata/golden`, `UPDATE_GOLDEN=1` to regenerate). CI
runs vet + tests (`checks.yaml`, `go` job).

Not yet, in the order below: `fort seed` in Go (chart pull + overlay +
`sops set`; the overlay merge is therefore not live either — today every
fort-side file is the full form), the CI gate, `fort status`, `fort
rotate`, s3, remote providers.

## Sequencing (one PR each, in this order)

1. ~~**Chart side**: symbology ships its contract~~ (sfi/symbology#189);
   umami's in-repo file still ports to the v1 schema and into the tarball.
2. ~~**`fort add`**: app dir, gateway, valkey, sites-db tenant
   registration, golden test~~ (this PR).
3. ~~**sites-db instance template** + `fort add` on a fort without one
   (sfi-website on a fresh fort as the second golden)~~.
4. **`fort seed`** (Go port of `provisioning/seed-secrets.py`): reads
   the effective contract (chart pull + overlay), `sops set` mode for
   existing files (add missing keys, `--set KEY`). The python seeder
   retires when this reproduces the fleet's Secrets' key sets; the
   fort-side file shrinks to the overlay.
5. **CI gate**: contract ↔ Secret key names ↔ `wire` values; missing
   required keys fail production.
6. **`fort status`** (local derivation; `--all` invariants).
7. **`fort rotate`** + Reloader fleet-wide + the chart annotation.
8. **s3**: bucket + the per-app credential decision.
9. **Remote providers** (`provider.fort`): mesh exposure of `-rw`
   Services, inventory-derived hosts.

`fort remove <fort> <app>` (the inverse of add) and `fort dismantle` /
`fort destroy` (#217) follow once the add shape has produced one real
fort; their form falls out of what 2–4 leave in the tree.
