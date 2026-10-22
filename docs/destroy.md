# fort destroy — the side-effects half, gated on the merge

```
fort destroy <host> [--yes] [--no-stack]
```

`destroy` is everything about retiring a fort that happens outside git,
and it refuses to start until git says the host is gone. The steps are
the web3 runbook (the first Linode fort's retirement, done by hand) as
one verb, each pulumi step with inherited stdio so you read and confirm
exactly as by hand — or `--yes` confirms them all (an agent, a script,
CI); without a TTY and without `--yes` it refuses up front.

## Gates — the inverse of every other verb's

```
sfi-x   (provider linode, from the checkpoint)
  ✗ still declared on origin/main (forts/sfi-x.toml) — merge the `fort dismantle sfi-x` PR first (this refuses to unprotect a declared host)
  ✗ working tree not clean — the state removal must land as its own commit
refusing: 2 gate(s) not met
```

- The host's manifest must be **absent from `origin/main`** (fetched
  first) — the merged [dismantle](dismantle.md) PR is the deliberate act
  `protect: true` was guarding for — and absent from the working tree.
- The **tree must be clean**, so the state removal lands as its own
  commit.
- The **stack must exist**. A host nothing raised through the CLI has no
  stack and goes by hand on its hypervisor or console. A `[baremetal]`
  host never has one: `--no-stack` declares that and runs step 7 alone
  ([baremetal](baremetal.md)).

## Needs

Everything is on file: the provider token and the pulumi passphrase, and
the forge's token the revocation runs with (`gitea-token` or
`github-token`, by the kind of `estate.toml`'s `repo`). A layer's keys (a backup
store's) are the layer's dismantle to revoke, before this
([layers](layers.md)).

The provider comes from the checkpoint's `pulumi:providers:*` resource,
not from the manifest: `pulumi destroy` never runs the program, so the
missing manifest is no obstacle.

## Steps

1. **Power off** (linode only): Linode refuses disk deletion on a
   running instance and the provider does not shut it down for you.
2. **`pulumi refresh`** — so the destroy works from truth. Anything
   removed by hand in the provider's console is dropped from state here
   instead of failing the destroy.
3. **`pulumi state unprotect --all`** — state-only, no API call,
   reversible with `pulumi state protect`, and only ever right before the
   destroy. `--all` is the right granularity: a stack is one host by
   construction. Never reach for `pulumi destroy --exclude-protected`.
4. **`pulumi destroy`** — the preview lists the deletes in reverse
   dependency order (DNS record, boot config, disks, instance, firewall;
   or domain then volume). Read it, type `yes`.
5. **`pulumi stack rm`** — refuses while any resource remains, which is
   the check that step 4 finished (a declined preview leaves them).
6. **Clear the file backend's residue** `stack rm` does not touch: the
   stack's history, backups, `.bak` and `.attrs`, `Pulumi.<host>.yaml`
   (the image-id provenance lives on in git history).
7. **Revoke the deploy key** `flux-<host>` on the forge — every key under
   that title. Not found is fine (no flux cluster, or removed by hand).
   On the anonymous route (an `https://` repo, forts#167) mint registered
   none: the step reports that and makes no call to the forge.
   An identity the estate minted for an account of its own on the fort is
   the estate's to revoke; destroy knows only the fort's.
8. **Stage the state removal.** Commit it as its own PR
   (`<host>: destroyed`); `just iac-drift` is green again since the
   stack no longer exists to report on.

A failure mid-way leaves the (partly deleted, unprotected) state in the
working tree — the right place for it — and says
`fix and rerun fort destroy <host>`; the rerun picks up where it was.

## Off-repo residue to check

- The `.files` wireguard copy still lists the host for the ubuntu mesh
  members — remove the entry there and re-run that playbook (#183).
- `~/.local/share/nixos-provisioning/<host>` on soundship, for hosts that
  predate the in-repo key custody.

## The manual equivalent

The verb execs the stock CLI, so the same sequence by hand from
`provisioning/pulumi` is always available:

```
export PULUMI_BACKEND_URL=file://$PWD/state PULUMI_IGNORE_AMBIENT_PLUGINS=true
export PULUMI_CONFIG_PASSPHRASE=$(sops --config ../../.sops.yaml -d --extract '["pulumi-passphrase"]' ../../provisioning/secrets.yaml)
export LINODE_TOKEN=$(sops --config ../../.sops.yaml -d --extract '["linode-token"]' ../../provisioning/secrets.yaml)   # cloud
pulumi refresh -s <host>
pulumi state unprotect --all -s <host>
pulumi destroy -s <host>
pulumi stack rm <host>
```

Then the deploy key through the repo's settings or the keys API, and the
residue above. This is exactly what `cloudEnv` sets for every fort
command; the difference is only who types it.
