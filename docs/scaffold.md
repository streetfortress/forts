# fort scaffold — the host's generated surface

```
fort scaffold <host> [--write]
```

`scaffold` renders every file a host needs that is derivable from its
[manifest](manifest.md), and nothing that is not. It is a pure template render:
no secrets, no network, no provider. That is why it is the first verb, why
anyone with a checkout can run it, and why its output is golden-tested
against the committed cluster dirs — the generator must keep reproducing
what is on main.

## Why a separate step

The identity step ([`mint`](mint.md)) needs the scaffold's output (the
canary's home, the flake's view of the host) and needs secrets and a
gitea token; scaffold needs neither. Keeping them apart keeps scaffold
deterministic, keeps its dry run honest (nothing leaves the tree), and
lets a contributor without keys open the PR that a maintainer finishes.
The one-command front door for both is a later `fort new`, composing
the primitives rather than fattening one.

## What it renders

From `forts/<host>.toml`:

```
nixos/hosts/<host>/
● default.nix        lab shape (qcow runtime, static LAN block) when the entry
                     has provider: libvirt or none; cloud shape (linode runtime,
                     split DNS over the mesh, zram, Lish break-glass) when it
                     names a cloud provider
● secrets.nix        sops-nix wiring: the host's age identity is derived from
                     its ssh host key; declares wireguard-private-key when the
                     manifest has mesh_ip
flux/clusters/<host>/
● flux-system/flux-operator.yaml
● flux-system/flux-instance.yaml             sync path flux/clusters/<host>
● flux-system/kustomization.yaml
● cluster-vars.yaml                          CLUSTER_NAME <host>, MESH_IP <mesh_ip>[, HUB_MESH_IP — the hub's address, from estate.toml]
● infrastructure.yaml                        infra-controllers + infrastructure (the shared layer)
● apps.yaml                                  → flux/apps/<host>
flux/apps/<host>/
● kustomization.yaml                         root list: sops-canary (listed, absent until fort mint)
flux/apps/luxor/monitoring/
● clusters/<host>.yaml                       dead man's switch (metrics, logs), edge Alertmanager datasource[, PVBackupNotConfigured]
○ kustomization.yaml                         + clusters/<host>.yaml
```

The last two are the one place scaffold reaches outside the host's own
paths, and they are why nothing about a cluster lives outside a
`<host>`-keyed file (#288): the central layer has to know each cluster
by name — `absent(up{cluster="<host>"})` is the only expression that
can fire when a cluster goes silent, its log analog the same, and its
edge Alertmanager has to be a Grafana datasource for silences to be
managed from one pane. Those used to be hand-edited hunks in shared
files, and forts joined without them (the production fort had no dead
man's switch until 2026-09-20). Now they are one file per cluster on the
hub, rendered here, removed by [`dismantle`](dismantle.md), and required
by `fort check --all` — a flux cluster with no file, or a file with no
manifest, is an error. The kustomization line is a text-level append,
idempotent, so a re-run reports `○ exists — kept (already listed)`.
`PVBackupNotConfigured` is rendered only where the profile promises the
leg (`production`).

Manifest fields it reads: the provider table (`[libvirt]` or
`[baremetal]` `ip` — the lab static IP — and its optional `lan_prefix`,
`lan_gateway`, `lan_dns`; `[baremetal]` `mac`, which turns the LAN
block's `en*` name match into a MAC match; `[linode]` `region` + `type`
for the header comment), `mesh_ip`, `[cluster] datastore` (rendered as
the host's `sfi.k3s.datastore` line, `sqlite` when unset; #426), and the
optional seed `nixos_state_version`. The lab defaults
are `/24`, gateway `10.0.0.1`, DNS `10.0.0.2` + `8.8.8.8`, state version
`25.11`.

A manifest without `mesh_ip` is refused (by the loader, so every verb
says so): every fort is a mesh member (`MESH_IP` is mandatory), and a
hardened host without a mesh address has no login path at all. Every fort
is the hardened profile — there is no key for it (#272) — and a
`[cluster]` fort is a flux cluster; a manifest with no `[cluster]` table
is a plain fort.

## The plain shape

A manifest with no `[cluster]` table renders the host dir and the fort's
file on the hub:

```
● nixos/hosts/sfi-lab-x/default.nix   plain fort shape (qcow runtime, static LAN, deployer + the ops verbs; no k3s/flux/backups)
● nixos/hosts/sfi-lab-x/secrets.nix   sops-nix wiring (host key → age)
flux/apps/luxor/monitoring/
● clusters/sfi-lab-x.yaml             mesh scrape target (node-exporter), HostDown
○ kustomization.yaml                  + clusters/sfi-lab-x.yaml
```

`default.nix` imports the baseline in place of `k3s-server.nix` +
`production.nix` + `flux-bootstrap.nix` (+ `pv-backup.nix`): `hardened.nix`,
`lab-host.nix`, `ops/deployer.nix` and `shell.nix`, with `wireguard.nix`,
`auto-rebuild.nix`, and the LAN and zram blocks of the lab VM. Nothing
under `flux/`: there is no cluster to sync and no canary to prove
decryption. `secrets.nix` declares no `k3s-token`. The file is the estate's
from then on: an account, a service, a role is plain NixOS written below
the imports, and the framework names none of it.

The hub file is the pull-shaped counterpart of a cluster's: with no edge
Prometheus, luxor scrapes the fort over the mesh — node-exporter on 9100 —
and arms `HostDown` on the target. `cluster` is the fort's name, as on
every other fort. No edge Alertmanager datasource: a plain fort has none.
Anything else the estate runs on the fort is the estate's to scrape, in a
file of its own beside this one.

The operator owns `job`: it relabels it to the CR's `jobName` after the
target's own labels are merged, so a `job:` under `staticConfigs` never
survives. The leg is `jobName: host-node-exporter` — the name a cluster
fort's edge ServiceMonitor gives the same exporter, and the job the Host
Overview dashboard keys every panel on — so a plain fort lists there the
moment the hub scrapes it (#442).

## Force-no semantics

`--write` lays a file down only when it does not exist; an existing one is
reported `○ exists — kept` and never touched. So:

- the manifest seeds the host once; hand refinements live in the files and
  are never clobbered;
- a re-run after a template change shows what would be *new*, not a diff
  against your edits;
- the committed `flux/clusters/*` dirs are still byte-identical to a
  fresh render (the test `TestScaffoldMatchesCommittedClusters` says so —
  luxor, hand-bootstrapped, is the recorded exception).

The apps root kustomization lists `sops-canary.yaml` before it exists. That
is deliberate: `kustomize build` fails until [`mint`](mint.md) creates the
canary, which is the repo's pre-merge guard against a half-provisioned
fort.

## Then

```
fort scaffold web3 --write
fort mint web3 --write        # identity
# PR → merge → fort image web3 → fort provision web3 --image …
```

`nix` reads only tracked or staged paths, so the scaffold stages what it
wrote; the flake discovers the new host dir and its image variant from the
runtime module the host file imports — no flake edit.
