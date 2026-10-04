# Forts — the CLI workflow

A **fort** is one host: one NixOS machine, one pulumi stack named after it,
and one single-node k3s cluster that flux
reconciles from `flux/clusters/<host>` + `flux/apps/<host>`. Everything
about it — the node config, its identity, its workloads, the state that
says it exists — lives in this repo, and the `fort` CLI (`cmd/fort`)
is the one tool that moves a fort through its life. There is no playbook,
no console step, no host shell in the happy path.

```
just <verb> …          # wrappers for the common ones (scaffold, preview, provision, iac-drift)
go run ./cmd/fort <verb> …     # every verb
```

Two shapes, one table apart: a fort with a `[cluster]` table is a flux
cluster; without one it is a **plain fort** — the same hardened host and
the same `deployer` with the ops verbs (#647), and no cluster. The
framework stops there on purpose: ideally nobody logs in, and the
prescribed access is `deployer` over the mesh and the console password.
What a plain fort does beyond that is plain NixOS in its host file, the
estate's. The verbs know the difference from the manifest; nothing else
changes hands ([manifest.md](manifest.md)).

## Verbs by intent

| kind | verbs | what they touch |
|---|---|---|
| **author** | [`scaffold`](scaffold.md), [`mint`](mint.md), [`add`](add.md), [`seed`](seed.md), [`dismantle`](dismantle.md), [`ssh-config`](ssh-config.md) | the repo only — render or remove files, stage them for a PR. Dry run by default; `--write` acts. |
| **apply** | [`provision`](provision.md), [`adopt`](provision.md#adopt), [`destroy`](destroy.md) | the provider (Linode, or soundship's libvirt), through pulumi with inherited stdio: you read the preview and confirm. |
| **read** | `check`, [`preview`](provision.md#preview), [`wg-conf`](ssh-config.md#fort-wg-conf-member--a-mac-joins-the-mesh) | nothing. `check` is the admission gate over the repo (CI runs it); `preview` is the diff against pulumi, `--refresh` the drift against the provider; `wg-conf` prints a mesh member's own wg0 from peers.json. |

Author verbs never overwrite: scaffold keeps a file that exists, add only
creates files and appends list entries, seed adds missing keys and never
rewrites a Secret, mint restores an identity before minting anything. A
re-run fills gaps; rotation and replacement are deliberate acts you make
by removing the thing first.

## The rhythm: author → PR → merge → apply → commit the state

Every fort change follows the same beat, and the reasons are the rules.

1. **Author on a branch.** Run the author verb (dry run, then `--write`).
   It stages exactly what it generated. You refine in place — replicas,
   hostnames, the odd hand edit — and commit.
2. **PR, review, merge.** Generated code is reviewed like any other. CI
   runs `fort check --all` (every manifest valid, the tree matches them,
   every flux layer kustomize-builds — a listed-but-unseeded Secret fails
   it, that red X is the guard working — strict envsubst satisfiable, no
   ConfigMap or Secret above the API server's 1 MiB limit, secrets match
   contracts) and `nix eval` of every host.
3. **Apply from merged main.** `provision` and `destroy` run against what
   main says. `destroy` refuses outright unless the host's manifest is
   *absent* from `origin/main` — the merged dismantle PR is the deliberate
   act `protect: true` was guarding for.
4. **Commit the pulumi state as its own PR.** Apply verbs write the file
   backend under `provisioning/pulumi/state/` and stage it. That PR carries no
   code, just the checkpoint: it is the provenance of the host's birth (or
   death), and `fort preview --all` is red until it lands.

The state lives in the repo on purpose: the owning repo is where state
lives, which dissolves the client-buildout bootstrap problem, and a
checkpoint in git history is an audit trail no backend gives you for free.

## Birth of a fort

```
forts/<host>.toml ← the manifest (manifest.md): copy a sibling's, edit
                                 mesh_ip, [dns], the provider table; fort check --all
fort scaffold <host> --write  ← nixos/hosts/<host>/, flux/clusters/<host>/, apps root
fort mint <host> --write      ← keys, sops rules, mesh peer, deploy key, canary;
                                 the fort's block in mesh/ssh_config + its
                                 host key in mesh/known_hosts (ssh-config.md)
                              → PR → merge
fort image <host>             ← nix build (the variant from the manifest); the output
                                 is the artifact, no secret inside (#659)
                                 → ~/.cache/forts/<host>-nixos.{qcow2,img.gz,img}
fort provision <host> --image <path>   ← pulumi up, you confirm; the host key rides as
                                          metadata.user_data (linode) or on a cidata
                                          volume (libvirt), #606/#657; the image is
                                          released once the host exists; state staged
                              → "<host>: provisioned" PR (state only) → merge
                                 (a [baremetal] host: no provision — you write
                                  the image to the disk, baremetal.md)
fort add <host> oci://…/<chart> --version X --write   ← per workload
fort seed <host> --write      ← the sops Secrets
fort check <host>             ← what CI will say
                              → PR → merge → flux reconciles
```

`add` and `seed` can run before the host exists — they are pure repo
mutations — and usually do, so the first flux sync already finds the
workloads. `fort image` runs on any x86_64 Nix host with KVM (devbox;
soundship needs no shell) and needs nix alone. See
[`docs/nixos/operations.md`](../nixos/operations.md) for the image and
the host side.

## Day 2

- **A provider-side edit** (firewall rule, DNS record, a resize): change
  the program or the manifest, `fort preview <host>` to read the diff, PR,
  merge, `fort provision <host>` to apply, commit the state.
- **Drift:** `just iac-drift` = `fort preview --all --refresh
  --expect-no-changes`, one `OK` / `DRIFT` / `FAILED` / `PENDING` line per
  stack. `PENDING` means state without a manifest: a dismantle merged and
  `fort destroy` has not run. The `--refresh` block above the program diff
  compares the provider with the *checkpoint*, not the program — birth-time
  outputs (status, disks, firewall devices) look stale there until a
  `pulumi refresh` is committed; only the program diff counts.
- **A new workload:** `fort add` → `fort seed` → `fort check` → PR, as at
  birth. Replacing a chart version is an edit to `repository.yaml`
  (renovate proposes it; the gate checks the new contract).
- **OS updates** are pull-based on the host (`auto-rebuild`): a merge to
  main is the deploy. The CLI is not involved.

## Retirement

```
fort dismantle <host> --write   ← the manifest, nix host dir + secrets, both
                                  sops rules (+ updatekeys), the peer, both fleet
                                  trees; prints the residue to triage → PR → merge
fort destroy <host>             ← gated on that merge: power off, refresh,
                                  unprotect, destroy, stack rm, deploy key revoked;
                                  state removal staged → "<host>: destroyed" PR
```

## Credentials and environment

| need | comes from | verbs |
|---|---|---|
| admin age key (`~/.config/sops/age/keys.txt`) | you | mint, seed, dismantle (updatekeys), everything that reads `provisioning/secrets.yaml` |
| `gitea-token` | `provisioning/secrets.yaml` | mint (register deploy key), destroy (revoke) |
| `PULUMI_CONFIG_PASSPHRASE` | env, else `pulumi-passphrase` in `provisioning/secrets.yaml` | provision, adopt, preview, destroy |
| `LINODE_TOKEN` | env, else `linode-token` in `provisioning/secrets.yaml` | the same, for `provider: linode` |
| `LIBVIRT_DEFAULT_URI` | defaults to soundship over ssh with `~/.ssh/id_ed25519` in the URI | the same, for `provider: libvirt` |
| nothing | — | `check` (CI runs it without any key), `scaffold`, `wg-conf`, `ssh-config` (the `ssh_config` half; a fort's `known_hosts` line needs the admin age key) |

`PULUMI_BACKEND_URL` is forced to this checkout's `provisioning/pulumi/state` by
every verb, so a `pulumi login` elsewhere can never redirect state into
another worktree. The stock `pulumi … -s <host>` from `provisioning/pulumi` under
the same environment is always a first-class escape hatch.

**Without a TTY** (an agent, a script, CI) pass `--yes` to the apply
verbs: it is pulumi's own `--yes --non-interactive`, and the merge that
put the manifest on main is the confirmation. Without a TTY and without
`--yes` the verb refuses up front, before it stages anything.

## Pages

- [manifest](manifest.md) — the fort's declaration: neutral intent + one provider table
- [scaffold](scaffold.md) — the host's generated surface, from its manifest
- [mint](mint.md) — the host's identity, sops-encrypted in-repo
- [provision](provision.md) — birth and day-2 applies; `preview`, `adopt`
- [baremetal](baremetal.md) — a fort on hardware: the `[baremetal]` table, the hardware-neutral image, the hand-written birth
- [add](add.md) — a chart's fort-side tree, from its contract
- [seed](seed.md) — the sops Secrets, from contracts
- [dismantle](dismantle.md) — the repo half of retirement
- [ssh-config](ssh-config.md) — the client side of the mesh: `ssh_config` +
  `known_hosts` rendered from peers.json and the manifests; `wg-conf` for a
  Mac joining the mesh
- [destroy](destroy.md) — the side-effects half, gated on the merge
- [fort configuration contracts](fort-configuration-contracts.md) — the
  contract schema, overlays, the gate, chart-author guidelines (the #144
  design document)

## The estate layout (v1, 2026-10-04)

What the verbs read and write in an estate, by directory. Decided in
sfi/deployments#525 and applied to the CLI with the split (sfi/deployments#385,
step 3); sfi/deployments itself moves to it at sfi/deployments#675.

```
forts/<host>.toml                the manifests: one file per fort, the declaration
nixos/
  hosts/<host>/                  default.nix + secrets.nix, rendered by scaffold, then the estate's
  secrets/<host>.yaml            the fort's sops file, written by mint
  modules/                       the estate's own modules (the framework's come from the flake input)
  .sops.yaml                     the per-host rules, a marker block per fort
flux/
  clusters/<host>/               the FluxInstance and the cluster's Kustomizations
  apps/<host>/                   the apps root; fort add renders into it
  infrastructure/                the shared layer every cluster applies
mesh/
  peers.json                     who is on the mesh and where; mint adds, dismantle removes
  ssh_config, known_hosts        rendered from peers.json + the manifests
provisioning/
  secrets.yaml                   the fleet secrets (gitea token, restic password, ...)
  console-passwords.yaml         the console registry
  pulumi/                        the project file, Pulumi.<host>.yaml per stack, and state/ (the file:// backend)
.sops.yaml                       the flux rules: one per cluster, plus the shared infrastructure rule
```

`provisioning/pulumi/Pulumi.yaml` is the estate's: it names the program
(`main:`), which the framework provides. Until `fort init` renders it, a
new estate writes it by hand.

