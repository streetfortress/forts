# fort seed — the sops Secrets, from contracts

```
fort seed <fort> [<dir>...] [--set [FILE/]KEY] [--chart-dir path] [--write]
```

`seed` produces the sops-encrypted Kubernetes Secrets a fort's workloads
consume, from the same contracts [`add`](add.md) rendered and
`fort check` gates on. It is the last author step before the PR and the
only one that touches secret material; it is designed so that no value
is ever printed, no existing file is ever rewritten, and the plan can be
read in full before anything is written.

## What a contract asks for

Every app dir under `flux/apps/<fort>/` carries a `forts.contract.yaml`
— the fort-side **full form** where one is committed, else the chart's
own contract (pulled at the tag `repository.yaml` pins) merged with the
fort's overlay. A contract names each Secret file, its metadata, and its
keys, each key one of:

| kind | seed does |
|---|---|
| `generate` | mints a random value (passwords, tokens) |
| `literal` | writes the given value (usernames, non-secret config) |
| `sops: [file, path]` | copies a value from another sops file — a provider's output (`sites-db/secrets-<app>.sops.yaml` `stringData.password`), or a sibling fort's file (`../../<fort>/…`, the drill-fort precedent for cloning staging) |
| `external` | leaves the key **absent until set** — an API key from a third party; `--set KEY` prompts for one value (or reads stdin) |

Providers come first: `sites-db`, `valkey` and `rustfs` dirs are seeded before the
apps whose `sops:` refs point into them, and the order is derived from
the refs, not declared. The contract schema itself is in
[fort configuration contracts](fort-configuration-contracts.md).

## What it does

```
flux/apps/sfi-x/sites-db/   (forts.contract.yaml (full form))
  ◆ secrets-backup.sops.yaml     seeded (2 keys)
  ◆ secrets-symbology.sops.yaml  seeded (2 keys)
flux/apps/sfi-x/valkey/
  ◆ secrets.sops.yaml            seeded (2 keys)
flux/apps/sfi-x/symbology/     (chart 0.1.9 contract + overlay)
  ◆ secrets.sops.yaml            seeded (8 keys); absent until set: STRIPE_SECRET_KEY

dry run — nothing seeded (seed --write).
```

- **A new file** is written 0600 with every key it can fill, then
  encrypted in place under the repo's creation rules (the path-scoped
  rule [`mint`](mint.md) added, so it encrypts to the fort's own key and
  the admins — identity must exist first).
- **An existing file is never rewritten.** Missing keys are added one at
  a time with `sops set`, a reviewable one-value diff. A key that is
  present stays as it is — `rotation is deliberate: fort rotate` (not
  built yet). Extra keys the contract does not name are left alone and
  reported by `check` as a warning (a migration superset).
- **`--set KEY`** takes exactly one `<dir>`. With `--write` it asks for
  the value — prompted without echo on a terminal, naming the key and the
  file; read from stdin otherwise, so `fort seed sfi-x symbology --set
  STRIPE_SECRET_KEY --write < key.txt` still works in a script. The plan
  runs first: a key no Secret in `<dir>` declares is refused by name
  before anything is pasted (the usual cause is a checkout whose contract
  does not carry the key yet). Without `--write` it only reports what
  `--set` would touch and asks for nothing. For an `external` key, or a
  deliberate replacement of any other key. One value lands in one file:
  when two Secrets of the dir share the key name (databasus-central's
  per-host `access-key`/`secret-key`), a bare `KEY` is refused and
  **`--set FILE/KEY`** names the file — `--set
  secrets-b2-sfi-fort-zenbook.sops.yaml/access-key`, the same file/key a
  `sops:` ref in a contract spells. The file must already exist (a plain
  `--write` creates it with its external keys absent).
- Decrypted material flows through memory only; the report shows key
  names, never values.

`<dir>` is the app's name (`sites-db`) or its repo-relative path; with
none, every contract-bearing dir of the fort in dependency order. A
named dir with nothing to seed is an error (a typo), an auto-enumerated
one (the gateway) is skipped.

## The gate it feeds

`fort check <fort>` / `--all` is what CI runs: every committed Secret
carries the keys its effective contract requires (plaintext key names,
no decryption — CI has no age key), every provider variable resolves to
a seeded file and key, every required provider is present and its `wire:`
values are set in the HelmRelease, and a chart bump that adds a required
key goes red on every fort that pins it. Strict on every fort. Run it before the PR; its
kustomize build fails on a listed-but-absent Secret file, which is the
same guard from the other side.

## Needs

The admin age key. Charts are pulled from the registry at the pinned tag
when no full-form contract is committed (`--chart-dir` reads a local
chart instead — chart authors, tests). `sops` on PATH.
