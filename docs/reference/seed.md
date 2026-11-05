# fort seed — the inventory, executed into sops Secrets

```
fort seed <fort> [<dir>...] [--set [FILE/]KEY] [--chart-dir path] [--dry-run]
```

`seed` executes a fort's [inventories](inventory.md) — the ones
[`add`](add.md) wrote and `fort check` gates on — into the
sops-encrypted Kubernetes Secrets its workloads consume. It is the last author step before the PR and the
only one that touches secret material; it is designed so that no value
is ever printed, no existing file is ever rewritten, and the plan can be
read in full before anything is written.

## What an inventory asks for

A dir under `flux/apps/<fort>/` carries an `forts.inventory.yaml` where one
is committed; else the inventory is derived from the chart's own
contract (pulled at the pin `repository.yaml` names) merged with the
fort's overlay ([contracts](contracts.md)). An inventory names each
Secret file, its metadata, and its keys, each key one of:

| kind | seed does |
|---|---|
| `generate` | mints a random value (passwords, tokens): `hexN`, `b64_N` |
| `literal` | writes the given value (usernames, non-secret config) |
| `template` | composes named `vars` into a value — `postgresql://app:${db_password}@…` |
| `sops: [file, path]` | copies a value from another sops file — a provider's Secret a layer seeded (`../forts/postgres/secrets-<app>.sops.yaml` `stringData.password`), or a sibling fort's file (`../../<fort>/…`, the precedent for cloning staging) |
| `env: [source, KEY]` | copies a value from a sops-encrypted dotenv the inventory's `sources` names |
| `external` | leaves the key **absent until set** — an API key from a third party; `--set KEY` prompts for one value (or reads stdin) |

Sources come first: a dir whose `sops:` refs point into another dir of
the fort seeds after it, and the order is derived from the refs, not
declared. The inventory is the core's schema, and a layer that renders
a directory of its own (`forts/postgres/`, say) writes one too: one
`fort seed` executes every inventory of the fort, whoever rendered it.
The schema itself is in [inventory](inventory.md).

A `template` that still composes a **provider output**
(`${postgres.db.password}` — what `fort add` writes verbatim for a
chart that `requires` a service) is not seedable: the core resolves no
provider. The layer that renders the provider rewrites the key to a
`sops:` ref; until it does, seed refuses that dir and names the key
and the layer.

## What it does

```
flux/apps/sfi-x/forts/postgres/   (forts.inventory.yaml)
  ◆ secrets-symbology.sops.yaml  created (2 keys)
flux/apps/sfi-x/symbology/     (forts.inventory.yaml)
  ◆ secrets.sops.yaml            created (8 keys)
      absent until set: STRIPE_SECRET_KEY — Stripe → API keys
```

- **A new file** is written 0600 with every key it can fill, then
  encrypted in place under the repo's creation rules (the path-scoped
  rule [`mint`](mint.md) added, so it encrypts to the fort's own key and
  the admins — identity must exist first), and staged.
- **An existing file is never rewritten.** Missing keys are added one at
  a time with `sops set`, a reviewable one-value diff. A key that is
  present stays as it is — rotation is deliberate, one key at a time
  through `--set`. Extra keys the inventory does not name are left alone
  and reported by `check` as a warning (a migration superset).
- **`--set KEY`** takes exactly one `<dir>`. It asks for the value —
  prompted without echo on a terminal, naming the key and the file;
  read from stdin otherwise, so `fort seed sfi-x symbology --set
  STRIPE_SECRET_KEY < key.txt` still works in a script. The plan runs
  first: a key no Secret in `<dir>` declares is refused by name before
  anything is pasted. With `--dry-run` it only reports what `--set`
  would touch and asks for nothing. For an `external` key, or a
  deliberate replacement of any other key. One value lands in one file:
  when two Secrets of the dir share the key name, a bare `KEY` is
  refused and **`--set FILE/KEY`** names the file — the same file/key a
  `sops:` ref in an inventory spells. The file must already exist (a
  plain seed creates it with its external keys absent).
- Decrypted material flows through memory only; the report shows key
  names, never values.

`<dir>` is a path under the fort's apps root — an app's name
(`symbology`), or what a layer renders beneath it (`forts/postgres`) —
or the repo-relative path itself; with none, every dir of the fort
that carries an inventory, an overlay or a pinned chart, in dependency
order. A named dir with nothing to seed
is an error (a typo), an auto-enumerated one (the gateway) is skipped.

## The gate it feeds

`fort check <fort>` / `--all` is what CI runs ([check](check.md)):
every committed inventory's Secret file exists and carries the keys it
declares (plaintext key names, no decryption — CI has no age key),
every `sops:` ref resolves to a seeded file and key. Run it before
the PR; its kustomize build fails on a listed-but-absent Secret file,
which is the same guard from the other side.

## Needs

The admin age key and `sops` on PATH ([the tool set](tools.md)). A
chart is pulled with `helm` at the pinned tag when a dir commits no
inventory (`--chart-dir` reads a local chart instead — chart authors,
tests).
