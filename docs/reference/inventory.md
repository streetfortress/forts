# The inventory — what a directory's Secrets hold, and where each value came from

`forts.inventory.yaml` is the fort-side record of the Secret values a
directory under `flux/apps/<fort>/` holds and the provenance of each:
named sources, named vars, and one entry per Secret file with its
metadata and a value spec per key. [`fort add`](add.md) writes one from
a chart's [contract](contracts.md); [`fort seed`](seed.md) executes it
into sops files; [`fort check`](check.md) gates on it. In one sentence:
**`fort add` takes a contract in and writes an inventory out.**

It is not a contract (forts#88). A contract is the chart author's
declaration, checked at a boundary, naming no file and no fort. An
inventory names namespaces, sops file paths, a dotenv on an operator's
machine and paths into other forts' files: one directory's, on one
fort. The name carries the `forts.` qualifier of its chart-side pair:
`forts.contract.yaml` in, `forts.inventory.yaml` out. The qualifier
also keeps the file clear of the Ansible `inventory.yaml` an estate
repo can carry (this project's manifest replaced one, #252) and of
the editor schema that bare filename attracts. The filename is the
schema — there is no sniff for a block. A fort-side `forts.contract.yaml` that carries a `secrets:`
block is the pre-rename shape, and `fort check` names it with the
rename as the fix.

A layer that renders a directory of its own (`forts/postgres/`, say)
writes the same file into it: one `fort seed` executes every inventory
of the fort, and one gate checks them.

## The schema

```yaml
# flux/apps/<fort>/<app>/forts.inventory.yaml — NON-SECRET
sources:
  legacy-env: ~/app/secrets/production.env         # a sops-encrypted dotenv
vars:
  db_password: {sops: [../forts/postgres/secrets-app.sops.yaml, stringData.password]}
secrets:
  secrets.sops.yaml:                               # relative to this directory
    metadata:
      name: app-env
      namespace: app
      # type: kubernetes.io/basic-auth               # Opaque when absent
    keys:
      SESSION:        {generate: b64_32}
      DATABASE_URL:   {template: "postgresql://app:${db_password}@postgres-rw.forts:5432/app"}
      STRIPE_KEY:     {env: [legacy-env, STRIPE_KEY]}
      CACHE_PASSWORD: {sops: [../forts/valkey/secrets.sops.yaml, stringData.app]}
      RESEND_API_KEY: {external: "Resend → API keys", optional: true}
      ADMIN_EMAILS:   {literal: ops@example}
```

Three blocks and nothing else: `sources`, `vars`, `secrets`. Every
value spec is one of:

| kind | seed does |
|---|---|
| `generate` | mints a random value: `hexN`, `b64_N` |
| `literal` | writes the given value (usernames, non-secret config) |
| `template` | composes named `vars` into a value |
| `sops: [file, dotted.key]` | copies a value from another sops file — a provider's Secret a layer seeded, or a sibling fort's file (`../../<fort>/…`, the precedent for cloning staging) |
| `env: [source, KEY]` | copies a value from a sops-encrypted dotenv `sources` names |
| `external` | leaves the key **absent until set** — an API key from a third party; `fort seed --set KEY` adds it |

Each may carry `optional: true`. A `sops:` ref into another directory
of the fort orders that directory first; a ref into another fort's
directory must already be seeded.

**A provider output, unresolved.** `fort add` writes a chart's
`template` key as the chart wrote it when it composes a provider
output (`${postgres.db.password}`): the core resolves no provider. The
layer that renders the provider rewrites that key to a `sops:` ref into
the tenant's Secret; `fort seed` refuses the key by name until then.

## The gate is the product

`fort check` reads every committed inventory of a fort and proves, with
no age key, that its Secret files exist and carry the non-external keys
they declare, and that every `sops:` ref lands on a committed file that
carries the key. Key names are plaintext under the repo's
`encrypted_regex`; values never are. A listed-but-absent Secret file
fails the kustomize build from the other side. Both are the gate
working: a PR that renders an app is red until the ◆ files land, and a
seeded fort stays green until a key goes missing.

## What comes next

The inventories of an estate are a provenance graph: every `sops:` ref
is an edge. `fort inventory` (forts#89) walks it — who consumes a given
file and key — and is the step before `fort rotate`.
