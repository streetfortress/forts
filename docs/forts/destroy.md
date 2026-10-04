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
  ✗ still declared on origin/main (provisioning/forts/sfi-x.toml) — merge the `fort dismantle sfi-x` PR first (this refuses to unprotect a declared host)
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

Everything but one thing is on file: the provider token and the pulumi
passphrase, the `gitea-token` the revocations run with, and the admin age
key that reads the retired secrets file. The one thing it may ask for is
the secret half of `sfi-b2-key-minting-key`, from the password manager —
and only when the fort **had a B2 key** (step 7), its own or the fleet's
pruning key. Its id is on file as
`b2-minting-key-id`; `B2_APP_KEY` in the environment answers the prompt
for a scripted run.

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
7. **Revoke the deploy key** `flux-<host>` on gitea — every key under
   that title. Not found is fine (no flux cluster, or removed by hand).
   An identity the estate minted for an account of its own on the fort is
   the estate's to revoke; destroy knows only the fort's.
   Then **the B2 keys** (`sfi-pv-<host>`, the db keys, the legacy
   `fort-<host>`), by name from `b2_list_keys`; `sfi-restore-<host>` is
   left standing, since a dead fort's backups are what it reads. The same
   retired `nixos/secrets/<host>.yaml` says whether there is anything to
   revoke: key *names* are plaintext, and `fort mint` writes
   `restic-s3-env-b2` exactly when it mints a B2 key. A fort whose file
   never held it (`[backups] profile` was not `production`) prints
   `b2 key: none minted for this fort` and asks for nothing. A fort whose
   file held it — or a fort with no retired file in history, since a
   missing record must not skip a live revoke — gets the prompt: the
   minting key's id is printed from `b2-minting-key-id`, the key is
   pasted (or `B2_APP_KEY`), and an empty paste skips the revoke with a
   warning naming the keys to delete by hand.
   Then **the pruning key**, when the host was the fleet's pruner (#400).
   `sfi-b2-pruning-key` is role-scoped, not host-scoped: it belongs to
   whichever host enables `sfi.pv-prune`'s b2 tier, and that host's file
   is the one that names `restic-prune-s3-env-b2`. The same retired file
   says so, and destroy revokes the key with the host and says, loudly,
   that the fleet now has no B2 pruner: enable the tier on another host,
   mint again (`fort b2-key pruning --write`), `sops set` it there.
   `fort check --all` warns until a host holds it again. A host whose
   record does not name the key leaves it alone, and so does a host with
   no record — the fleet's one deleting credential is not revoked on a
   guess; a standing pruning key is named in the output instead.
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
`forts/pulumi` is always available:

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
