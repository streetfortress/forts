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

From `provisioning/forts/<host>.toml`:

```
nixos/hosts/<host>/
● default.nix        lab shape (qcow runtime, static LAN block) when the entry
                     has provider: libvirt or none; cloud shape (linode runtime,
                     split DNS over the mesh, zram, Lish break-glass) when it
                     names a cloud provider
● secrets.nix        sops-nix wiring: the host's age identity is derived from
                     its ssh host key; declares wireguard-private-key when the
                     manifest has mesh_ip
fleet/clusters/<host>/
● flux-system/flux-operator.yaml
● flux-system/flux-instance.yaml             sync path fleet/clusters/<host>
● flux-system/kustomization.yaml
● cluster-vars.yaml                          CLUSTER_NAME <host>, MESH_IP <mesh_ip>
● infrastructure.yaml                        infra-controllers + infrastructure (the shared layer)
● apps.yaml                                  → fleet/apps/<host>
fleet/apps/<host>/
● kustomization.yaml                         root list: sops-canary (listed, absent until fort mint)
fleet/apps/luxor/monitoring/
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
for the header comment), `mesh_ip`, and the
optional seeds `nixos_state_version`,
`k3s_cluster_init`. The lab defaults
are `/24`, gateway `10.0.0.1`, DNS `10.0.0.2` + `8.8.8.8`, state version
`25.11`.

A manifest without `mesh_ip` is refused (by the loader, so every verb
says so): every fort is a mesh member (`MESH_IP` is mandatory), and a
hardened host without a mesh address has no login path at all. Every fort
is the hardened profile and a flux cluster — there is no key for either
(#272).

## The dev shape

A manifest with a `[dev]` table (#413) renders the host dir and the
fort's file on the hub:

```
● nixos/hosts/sfi-dev-x/default.nix   dev fort shape (qcow runtime, static LAN, agent account; no k3s/flux/backups)
● nixos/hosts/sfi-dev-x/secrets.nix   sops-nix wiring (host key → age)
fleet/apps/luxor/monitoring/
● clusters/sfi-dev-x.yaml             mesh scrape targets (node-exporter, otel collector), DevFortDown, LogStreamSilent
○ kustomization.yaml                  + clusters/sfi-dev-x.yaml
```

`default.nix` imports `dev-fort.nix` in place of `k3s-server.nix` +
`production.nix` + `flux-bootstrap.nix` (+ `pv-backup.nix`), keeps
`wireguard.nix` and `auto-rebuild.nix`, and the LAN and zram blocks of
the lab VM. Nothing under `fleet/`: there is no cluster to sync and no
canary to prove decryption. `secrets.nix` declares no `k3s-token`.

The hub file is the pull-shaped counterpart of a cluster's (#437): with
no edge Prometheus, luxor scrapes the fort over the mesh — node-exporter
on 9100 and the fort's OpenTelemetry collector on 9464, which holds
Claude Code's usage metrics — and arms `DevFortDown` /
`DevFortCollectorDown` on the targets and `LogStreamSilent` on the
journal stream the collector pushes to Loki. `cluster` is the fort's
name, as on every other fort. No edge Alertmanager datasource: a dev
fort has none.

One `ScrapeConfig` per leg, because the operator owns `job`: it
relabels it to the CR's `jobName` after the target's own labels are
merged, so a `job:` under `staticConfigs` never survives. The node leg
is `jobName: host-node-exporter` — the name a cluster fort's edge
ServiceMonitor gives the same exporter, and the job the Host Overview
dashboard keys every panel on — so a dev fort lists there the moment
the hub scrapes it (#442). The collector leg is `jobName: claude-code`,
the same name as its events' Loki stream.

## Force-no semantics

`--write` lays a file down only when it does not exist; an existing one is
reported `○ exists — kept` and never touched. So:

- the manifest seeds the host once; hand refinements live in the files and
  are never clobbered;
- a re-run after a template change shows what would be *new*, not a diff
  against your edits;
- the committed `fleet/clusters/*` dirs are still byte-identical to a
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
