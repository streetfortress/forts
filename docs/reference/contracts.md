# Contracts — what a chart declares

A chart declares what it needs in one file, `forts.contract.yaml`,
shipped at the chart root inside the tarball. It says what the app
needs from *any* environment: the keys its `existingSecret` must carry
and how each value is produced, the Service the fleet routes to, the
metrics port, and the platform services it consumes. Never a namespace,
a hostname, a cluster or a provider instance. Helm ignores unknown
files at the chart root, so it ships with `helm package` and comes back
with `helm pull --untar`: one artifact, one version pin, and the
contract cannot skew from the chart it describes.

It is a contract in the plain sense: an interface between the chart
and every fort, written by the chart author and checked at a boundary.
What a fort does with it is recorded elsewhere — the rendered manifests
for the route and the release, and the [inventory](inventory.md) for
the Secret: **`fort add` takes a contract in and writes an inventory
out** (forts#88).

The core reads three of its blocks (forts#250): `secret`, `http` and
`metrics`. [`fort add`](add.md) renders the route, the listener, the
certificate, the PodMonitor and the inventory from them. The fourth
block, `requires`, is the apps layer's: the core reads it only far
enough to say what the layer must render (sfi/fort-layers).

`forts.contract.yaml` is plural because the file is not about one fort:
the chart author writes it once and it holds for every fort the chart
is ever deployed to. A chart released before the rename ships
`secrets.contract.yaml`, immutably, and the loader keeps reading that
name as a fallback; when both are present the new name wins.

## The schema (v1)

```yaml
# forts.contract.yaml — at the chart root, inside the tarball. NON-SECRET.
contract: v1

# The one Secret the chart consumes (values path naming it + the keys it
# must carry). The chart never creates it. OPTIONAL: omit the whole block
# when the chart has no secrets, and the fort renders none of the Secret
# machinery.
secret:
  values: existingSecret        # values path naming the Secret (the default)
  keys:
    APP_SECRET:        {generate: hex32}
    RESEND_API_KEY:    {external: "Resend → API keys", optional: true}
    DATABASE_PASSWORD: {template: "${postgres.db.password}"}   # a layer resolves it

# Platform services the app consumes — the apps layer's block. The core
# names it and renders none of it.
requires:
  postgres:
    db:
      extensions: [vector]
      wire: {host: database.host, port: database.port}

# Ingress: the fleet owns Gateway listener + Certificate + HTTPRoute; the
# chart names the backend.
http:
  service: umami
  port: 3000

# Optional. Rendered into monitoring.yaml as a PodMonitor.
metrics:
  port: metrics
  selector: {app.kubernetes.io/component: worker}
```

Key primitives in a contract:

| primitive | meaning |
|---|---|
| `generate` | fresh randomness: `hexN` / `b64_N` |
| `template` | compose from `${type.instance.output}` — a provider output, which a layer resolves |
| `external` | a human supplies it; `optional: true` if the app runs without it |

No `literal` in a chart: a value that is not secret is a value, so a
change rolls pods. **Pending means absent.** An `external` key with no
value yet is simply left out of the Secret — no sentinel, no marker
file. The seeder skips it and reports it; `fort seed --set KEY` adds
it; `fort check` fails a fort on any missing non-optional key and warns
on an optional one.

An absent contract is not an error anywhere: the chart declares
nothing, `fort add` renders the app dir alone, `fort seed` plans no
Secret, `fort check` has nothing to gate it against. A contract that IS
there and does not parse is an error everywhere.

## The overlay

The same schema, committed in the fort's app dir under the same name
(`flux/apps/<fort>/<app>/forts.contract.yaml`), only when the fort
deviates from the chart and commits no inventory of its own: a key
sourced from a migration-era dotenv, a superset key kept during a
cutover, a provider key the overlay names instead of a layer. It
carries `contract: v1` and no `secrets:` block — a file with one is an
[inventory](inventory.md), under its own name. `fort seed` merges the
overlay over the chart's contract at the pin: an overlay key replaces
the chart's spec by name, an overlay-only key is appended, `sources`
are the overlay's.

```yaml
# flux/apps/<fort>/<app>/forts.contract.yaml — OVERLAY on the chart's
contract: v1
sources:
  legacy-env: ~/app/secrets/production.env
secret:
  keys:
    STRIPE_SECRET_KEY: {env: [legacy-env, STRIPE_SECRET_KEY]}
    DATABASE_PASSWORD: {sops: [../forts/postgres/secrets-app.sops.yaml, stringData.password]}
```

## What `fort add` renders, and the one exception to retry-forever

`fort add` is a pure generator: it reads the chart's contract, the
fort's manifest and the existing tree; writes **new files** and
**appends lines to kustomization lists**; never rewrites an existing
file. An appended entry lands as the last item **of its own list**, not
at the end of the file: a kustomization can carry a
`configMapGenerator:` after `patches:`, and a patch entry under that
generator makes `kustomize build` fail with `unknown field "path"`.

Conventions that make the output deterministic:

| thing | convention |
|---|---|
| namespace, release name, app dir | `<app>` = chart name unless `--name` |
| Secret | `<app>-env` in namespace `<app>` |
| hostname | `<app>.<fort>.<zone>` unless `--hostname` |
| HelmRelease | `install.remediation.retries: -1` and `upgrade.remediation.retries: -1` — retry until ready, never park on a failed state waiting for an ops nudge |

**The one exception to `-1` on upgrade.** Retry-forever assumes a
failed upgrade is safe to repeat. It is not safe to repeat when the
upgrade migrates a schema forward: a retry re-runs the migration
against a database that the last attempt has already moved part of the
way, and `upgrade.remediation.strategy: rollback` is worse. Such a
release writes its own block by hand — `upgrade.remediation.retries: 0`
and `remediateLastFailure: false` — with a header comment that gives the
argument. The release then parks at `Ready=False` and a human chooses
the next step. `fort add` still renders `-1` on both halves for every
new app: the exception is hand-written, per release, and it must carry
the reason with it.
