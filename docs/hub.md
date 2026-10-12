# The hub — a relationship between estates

A **hub** is a fort that serves other forts a role: the **forge** every
fort clones from, the **monitoring** hub the edges push metrics and logs
to and that notices a silent fort, the **ca** a lab issuer reaches
(forts#46, not rendered yet). The forts it serves may be its own
estate's or another's: luxor in sfi/infra is the monitoring hub of
sfi/staging and sfi/production as well as of sfi/infra itself. The hub
is therefore a relationship between two estates, and `estate.toml`
declares it at both ends (forts#38).

## The direction

The framework used to write the hub per fort, into the hub's tree: the
cluster's dead man's switch, its Loki rule and its Grafana datasource
landed in `flux/apps/luxor/monitoring/clusters/`. That was free while
every fort and the hub shared one checkout. With staging and production
served by a hub in another repo it is a write across repos, and the CLI
refused it.

The direction is inverted. **The served estate renders what it asks of
its hub into its own tree, under `flux/hub/<hub host>/`. The hub
subscribes to that path with flux.** A fort's file is rendered by
`fort scaffold`, removed by `fort dismantle`, required by
`fort check --all`; a fort the served estate dismantles leaves the hub by
prune. Nothing about a fort is written into another fort's tree, and
nothing is written across repos. The hub's tree holds one subscription
per served estate, which is a function of estates, not of forts.

The path is the hub *host's* name whichever estate holds the hub. An
in-estate hub (sfi/infra's own forts, luxor among them) renders into the
same `flux/hub/luxor/`; only the subscription differs — it reads the
estate's own flux source instead of a second repo.

## estate.toml

The served estate names each role another fort serves it:

```toml
[hubs.forge]
estate = "ssh://git@gitea.zen.lofi:30022/sfi/infra"   # the estate that holds the hub, by its git URL
fort = "sfi-fort-zenbook"                             # the hub fort
name = "gitea.zen.lofi"                               # the forge's one name on every mesh (forge only)

[hubs.monitoring]
estate = "ssh://git@gitea.zen.lofi:30022/sfi/infra"
fort = "luxor"
```

`estate` is the hub's estate by its git URL — the `repo` its own
`estate.toml` carries, so an estate has one spelling everywhere, and the
owner and name (`sfi/infra`) derive from it where a report needs them.
This estate's own URL is the in-estate case (`estate` = this estate's
`repo` inside sfi/infra); two spellings of one repo (a `.git` suffix,
`https://` for `ssh://`) are the same estate. The hub's address on this
mesh is its entry in `mesh/peers.json`: its manifest's `mesh_ip` when
the hub is a fort of this estate, its `remotes` entry when it is
another's — the link that puts it on this mesh ([links.md](links.md)).
No address is written twice. `hub` and `hub_push` are unknown keys now.

The hub's estate declares the other half, by role — a fort of its own
and the estates it serves that role:

```toml
[[serves.forge]]
fort = "sfi-fort-zenbook"
estates = ["ssh://git@gitea.zen.lofi:30022/sfi/staging", "ssh://git@gitea.zen.lofi:30022/sfi/production"]

[[serves.monitoring]]
fort = "luxor"
estates = ["ssh://git@gitea.zen.lofi:30022/sfi/staging"]
```

The role is the key at both ends, so `[hubs.monitoring] fort = "luxor"`
in staging and `[[serves.monitoring]] fort = "luxor"` in infra are one
sentence read from either side. The URLs are what the hub's flux clones
to read `flux/hub/<fort>/`. Two entries under one role are two hubs: an
infra estate that runs a monitoring hub per team estate lists each fort
with its estates. The one invariant is the served side's — a served
estate names one fort per role — so an estate URL appears at most once
across the entries of a role. An entry for this estate itself is a
refusal: an estate consumes through its `[hubs.*]` tables and provides
through `serves`, and the two never overlap. Every fort a serves table
names must have a manifest in this estate.

## A role is a small contract

| role | the served estate renders | the hub opens on the link | `fort check` |
|---|---|---|---|
| `forge` | the forge's address pinned to its name — `modules/lab-dns.nix` derives `sfi.lab-dns.giteaAddress` from `fort` and `mesh/peers.json` (forts#26) | 30022, 443 | the address derives; a host file that sets it by hand is a warning ([mesh.md](mesh.md#what-fort-check---all-says)) |
| `monitoring` | `flux/hub/<host>/<fort>.yaml`; `HUB_MESH_IP` in `cluster-vars.yaml`; `estate` beside `cluster` on every metric and log line (`ESTATE_NAME`, forts#50) | 30990, 31100 | one file per fort, listed; one subscription per served estate; `ESTATE_NAME` is the estate's name |
| `ca` | the ClusterIssuer (forts#46) | the step-ca NodePort (forts#46) | the issuer's server is this host (forts#46) |

The forge is the one role with a `name`: `gitea.zen.lofi` is an identity,
not a location — it is in every git remote, every OCI reference, every
flux source and the registry certificate, and one gitea is on several
meshes. So the forge keeps that name on every mesh, and each estate pins
it to the address `fort` has here. The name itself is still a constant in
`modules/lab-dns.nix`; forts#1 reads `name`.

`modules/links.nix` derives a link's `openTCPPorts` from its `roles`,
and a link named after a served estate (the URL's last label:
`sfi.links.staging`) takes `roles` from the serves entries that name its
host and that estate ([links.md](links.md)). The link itself is
`fort link add <fort> <estate url>` in the hub's estate and
`fort estate link <hub's estate url>` in the served one.

## What scaffold renders

In the served estate, for every fort ([scaffold.md](scaffold.md)):

```
flux/hub/luxor/
● <host>.yaml          dead man's switch (metrics, logs), edge Alertmanager datasource[, PVBackupNotConfigured]
                       — a plain fort: mesh scrape target (node-exporter), HostDown
○ kustomization.yaml   + <host>.yaml   (● on the estate's first fort)
flux/clusters/<host>/
● cluster-vars.yaml    HUB_MESH_IP — the hub's address on this mesh
```

On a hub fort (`fort scaffold luxor` in sfi/infra), one subscription per
estate it serves: its own estate's when it is that estate's
`[hubs.monitoring]`, and one per estate a `[[serves.monitoring]]` entry
names it the hub of:

```
flux/apps/luxor/
○ kustomization.yaml   + serves
● serves/infra.yaml    this estate's own forts: flux/hub/luxor/ through the flux-system source
● serves/staging.yaml  the forts of sfi/staging: flux/hub/luxor/ of their repo, with this cluster's deploy key
● serves/kustomization.yaml
```

A subscription to another estate is a `GitRepository` on the served repo
(read with the cluster's own `flux-system` deploy key — the read-only
key the link to that estate's mesh already uses, registered on the repo
by that estate's admin), a `ServiceAccount` with a `Role` that can write
`PrometheusRule`, `ScrapeConfig` and `ConfigMap` in `monitoring-central`
and nothing else, and a `Kustomization` on `./flux/hub/luxor` under that
account with `prune: true`. The own estate's subscription is the same
without the `GitRepository`: its source is the FluxInstance's. No
`decryption`: no secret rides `flux/hub/` yet. A secret the hub must
hold for a served fort (a rustfs identity) fits the same path —
encrypted to the hub cluster's age recipient, a public key — but the
two estates of 2026-10 need none (staging declares no backups,
production declares B2 only), and a `[[backups.pv]]` block on
`rustfs-central` therefore still requires an in-estate monitoring hub
([manifest.md](manifest.md)).

The served estate's branch rules keep `flux/hub/` protected where
`flux/apps/<fort>/` is not: the hub applies what lands on `main` there.

## What `fort check --all` says

| finding | level |
|---|---|
| served estate: `flux/hub/<hub>/<fort>.yaml` for every cluster fort and plain fort, listed in `flux/hub/<hub>/kustomization.yaml` | ok per fort; missing or unlisted: error |
| served estate: a file under `flux/hub/<hub>/` with no manifest | error — `fort dismantle` removes it |
| no `[hubs.monitoring]`: no file, no dead man's switch; the edge Alertmanager is the only alerting | ok, said once |
| hub's estate: `serves/<estate>.yaml` in each fort's tree for its own estate (the `[hubs.monitoring]` fort) and for every estate a `[[serves.monitoring]]` entry names it for, listed in `serves/kustomization.yaml`, `serves/` listed in the apps root | ok per estate; missing or unlisted: error |
| hub's estate: a `serves/` file no entry declares | error |
| hub's estate: a fort that serves an estate a role has no `[links.<estate>]` in its manifest | warning, naming `fort link add` — the network half is its own verb ([links.md](links.md)), and the link's reconciler refuses on its own when the two sides disagree |
| `[hubs.monitoring]` or a serves table names a fort that has no manifest here | error |
| a `[[backups.pv]]` block on `rustfs-central` with no in-estate monitoring hub | error ([mint.md](mint.md)) |

## Interim

sfi/deployments stays on the in-tree CLI until it retires. The served
estates render `flux/hub/luxor/` from the start; the deployments side is
one hand-written subscription per served estate, written once — the
shape `serves/<estate>.yaml` renders, placed in luxor's apps tree by
hand. The hand work is then a function of estates, not of forts.
