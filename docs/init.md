# fort init — create an estate

```
fort init --name <estate> --repo <git url> --admin <age1…> [--admin …] [--mesh <cidr>] [--interface <name>] [--zone <name>] [--hub <host>] [--forts <flake url>] [--write]
```

`init` renders, into the root of a git checkout, the files every other
verb assumes are there. It is the first verb an estate runs and the only
one that runs with no `estate.toml`. Like [`scaffold`](scaffold.md) it is a
pure render: no secrets, no network, nothing overwritten — a re-run keeps
every file that exists.

## What it renders

```
● estate.toml                     the estate: name, repo, mesh, interface and zone (if given), hub (if any)
● README.md                       the layout and the first fort
● .sops.yaml                      the flux rules: provisioning/, the shared infrastructure rule,
                                  the `# fort-managed` marker fort mint appends cluster recipients under
● nixos/.sops.yaml                the per-host rules: the catch-all with the admins; mint adds a block per fort above it
● nixos/flake.nix                 the estate flake: sfi/forts as inputs.forts, nixpkgs and sops-nix following it,
                                  hosts auto-discovered under hosts/, an image package per host, fort re-exported
● nixos/certs/README.md           where the lab CA goes
● mesh/peers.json                 empty; mint adds the first peer
● provisioning/README.md          what secrets.yaml holds
● provisioning/pulumi/Pulumi.yaml the project file: `runtime: go`, `binary: fort-pulumi`
● flux/infrastructure/            the shared flux layer: controllers + configs, minus the two files below
● .gitea/workflows/checks.yaml    the gate: `nix run ./nixos#fort -- check --all` + a nix eval of every host
● nixos/hosts, nixos/secrets, flux/apps, flux/clusters, provisioning/pulumi/state   (.gitkeep)
```

## What it cannot write

The estate's own, named in the report:

- `nixos/certs/step-ca-root.pem` and `flux/infrastructure/configs/step-ca-root.pem` — the lab CA.
- `flux/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml` — the edge Alertmanager's webhook.
- `provisioning/secrets.yaml` — the fleet secrets (`provisioning/README.md` lists the keys).

## estate.toml

```toml
name = "second-estate"
repo = "ssh://git@gitea.zen.lofi:30022/sfi/second-estate"   # flux's source, auto-rebuild's pull
mesh = "10.2.0.0/24"   # the WireGuard subnet; fort check keeps every mesh address in it (mesh.md)
interface = "wg-second"   # the mesh interface's name on every host; absent: wg0
zone = "second-estate.mesh"   # every host on the mesh is <host>.<zone> (mesh.md); absent: <name>.mesh
# hub = "luxor"          # the hub is a fort of this estate
# hub_push = "10.1.0.5"  # or a hub in another estate, by mesh address
```

`scaffold` renders the FluxInstance source and `sfi.auto-rebuild.repoUrl`
from `repo`. The hub takes one of three shapes, and `init` renders the
shared layer for the one it is told (`--hub`, `--hub-push`, neither):

| estate.toml | the hub's tree | the edges push | backups register |
|---|---|---|---|
| `hub = "<host>"` | a fort here: scaffold renders every other fort's file into `flux/apps/<hub>/monitoring/clusters/` | `HUB_MESH_IP` = the hub's `mesh_ip` | a `[[backups.pv]]` block on `rustfs-central` registers with `flux/apps/<hub>/rustfs-central`; a block on `b2` needs nothing |
| `hub_push = "<mesh ip>"` | in another estate: nothing renders here, that estate decides what it scrapes | `HUB_MESH_IP` = the address | a block on `rustfs-central` is refused (nothing here to register with); `b2` blocks are fine |
| neither | none | the layer carries no `remote_write` and no fluent-bit | the same: `b2` only |

`scaffold` writes `HUB_MESH_IP` into a cluster's `cluster-vars.yaml` when
there is somewhere to push; the layer's edge Prometheus `remote_write` and
fluent-bit's Loki outputs read it. `check` and `dismantle` read the hub
too. An estate with no hub has nobody to notice a silent fort: the edge
Alertmanager is its only alerting, and `fort check --all` says so.

Changing the hub after init is a hand edit: `flux/infrastructure` is the
estate's once rendered (re-render it from a scratch `fort init` and diff),
and `cluster-vars.yaml` is never overwritten by scaffold, so each cluster
gains or loses its `HUB_MESH_IP` line by hand.

## After init

1. `nix flake lock ./nixos` — pin the framework. `--override-input forts
   path:../forts` develops against a local checkout.
2. Provide the three files above and create `provisioning/secrets.yaml`.
3. `forts/<host>.toml` ([manifest.md](manifest.md)), then `fort scaffold`,
   `fort mint`, a PR, `fort image`, `fort provision`.
