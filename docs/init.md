# fort init — create an estate

```
fort init --name <estate> --repo <git url> --admin <age1…> [--admin …] [--operator-key <"ssh-ed25519 … <device>"> …] [--mesh <cidr>] [--interface <name>] [--zone <name>] [--hub <role>=<fort>@<estate git url> … [--forge-name <name>]] [--forts <flake url>] [--write]
```

`init` renders, into the root of a git checkout, the files every other
verb assumes are there. It is the first verb an estate runs and the only
one that runs with no `estate.toml`. Like [`scaffold`](scaffold.md) it is a
pure render: no secrets, no network, nothing overwritten — a re-run keeps
every file that exists.

## What it renders

```
● estate.toml                     the estate: name, repo, mesh, interface and zone (if given), the operator keys
                                  (if given), the hubs by role (if any)
● README.md                       the layout and the first fort
● justfile                        the estate's own verbs, which are not the CLI: fetch-kubeconfig (one fort's
                                  read-only ops kubeconfig) and updatekeys (re-encrypt every sops file)
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
● .gitea/workflows/checks.yaml    the gate: `nix run ./nixos#fort -- check --all` + a nix eval of every host; a layer's step lands after (layers.md)
● nixos/hosts, nixos/secrets, flux/apps, flux/clusters, provisioning/pulumi/state   (.gitkeep)
```

## What it cannot write

The estate's own, named in the report:

- `nixos/certs/step-ca-root.pem` and `flux/infrastructure/configs/step-ca-root.pem` — the lab CA. `estate.nix` trusts the first file on every host of the estate where the file exists; an estate with no CA provides neither.
- `flux/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml` — the edge Alertmanager's webhook.
- `provisioning/secrets.yaml` — the fleet secrets (`provisioning/README.md` lists the keys).

## estate.toml

```toml
name = "second-estate"
repo = "ssh://git@gitea.zen.lofi:30022/sfi/second-estate"   # flux's source, auto-rebuild's pull
mesh = "10.2.0.0/24"   # the WireGuard subnet; fort check keeps every mesh address in it (mesh.md)
interface = "wg-second"   # the mesh interface's name on every host; absent: wg0
zone = "second-estate.mesh"   # every host on the mesh is <host>.<zone> (mesh.md); absent: <name>.mesh

operator_keys = [            # the devices that hold `deployer` on every fort of this estate
  "ssh-ed25519 AAAAC3Nza… macbook-air",
]

[hubs.forge]                  # the roles another fort serves this estate (hub.md)
estate = "ssh://git@gitea.zen.lofi:30022/sfi/infra"   # the estate that holds the hub, by its git URL
fort = "sfi-fort-zenbook"
name = "gitea.zen.lofi"       # the forge's one name on every mesh

[hubs.monitoring]
estate = "ssh://git@gitea.zen.lofi:30022/sfi/infra"   # this estate's own repo URL for an in-estate hub
fort = "luxor"
```

`scaffold` renders the FluxInstance source and `sfi.auto-rebuild.repoUrl`
from `repo`. The hubs are roles ([hub.md](hub.md)): `--hub
<role>=<fort>@<estate git url>` once per role (`--hub
monitoring=luxor@ssh://git@gitea.zen.lofi:30022/sfi/infra`; the fort
comes first because the URL carries an `@` of its own, and the forge's
name is `--forge-name gitea.zen.lofi`), and `init` renders the shared
layer for a monitoring hub or none:

| estate.toml | the fort's file for the hub | the edges push | backups register |
|---|---|---|---|
| `[hubs.monitoring]` with `estate` = this estate | scaffold renders every fort's file into `flux/hub/<host>/`, and the hub's own `serves/` subscribes to it through the flux-system source | `HUB_MESH_IP` = the hub's `mesh_ip` | a `[[backups.pv]]` block on `rustfs-central` registers with `flux/apps/<hub>/rustfs-central`; a block on `b2` needs nothing |
| `[hubs.monitoring]` with `estate` = another | the same `flux/hub/<host>/`; that estate's `[[serves.monitoring]]` entry subscribes to it | `HUB_MESH_IP` = the hub's `remotes` entry in `mesh/peers.json` | a block on `rustfs-central` is refused (nothing here to register with); `b2` blocks are fine |
| no `[hubs.monitoring]` | none | the layer carries no `remote_write` and no fluent-bit | the same: `b2` only |

`scaffold` writes `HUB_MESH_IP` into a cluster's `cluster-vars.yaml` when
there is somewhere to push; the layer's edge Prometheus `remote_write` and
fluent-bit's Loki outputs read it. `check` and `dismantle` read the hubs
too. An estate with no monitoring hub has nobody to notice a silent
fort: the edge Alertmanager is its only alerting, and `fort check --all`
says so. The `[[serves.<role>]]` tables of a hub's estate are a hand
edit after init; `fort scaffold <fort>` renders the subscriptions from
them.

`[hubs.forge]` buys the forge pin. `modules/lab-dns.nix` pins the forge's
one name on every mesh (`gitea.zen.lofi`) to the address `fort` has in
`mesh/peers.json`, in `/etc/hosts` and in the cluster DNS, on every fort
of the estate (forts#26; [mesh.md](mesh.md)). The address is written in
that entry and nowhere else. An estate that names no forge host gets no
pin, and `fort check --all` says so.

Changing the monitoring hub after init is a hand edit: `flux/infrastructure`
is the estate's once rendered (re-render it from a scratch `fort init`
and diff), and `cluster-vars.yaml` is never overwritten by scaffold (it
appends the `ESTATE_NAME` line and nothing else; [scaffold.md](scaffold.md)), so
each cluster gains or loses its `HUB_MESH_IP` line by hand.

## The operators

`deployer` is the operator account of every fort: the `sfi-*` ops verbs
are pinned to it, and `ops/console.nix` opens the console door for it.
**The framework ships no operator key** (forts#81). `operator_keys` is
the one place a key enters an estate — one line per device, in
`authorized_keys` form, with the device as the key's comment — and
`modules/estate.nix` reads the list into
`sfi.ops.deployer.authorizedKeys`, which `modules/ops/deployer.nix`
puts in the account's `authorized_keys` and nothing else adds to.

The list was the framework's until forts#81, and that was a defect: a
NixOS list option merges across modules, so an estate that named its own
devices *added* them to SFI's seven. SFI held the operator account of
every fort this framework built, an estate for another team included,
unless that estate wrote `lib.mkForce` — and nothing asked it to. The
list is the estate's now, and the estate that holds a fort is the team
that operates it.

- `fort init --operator-key "ssh-ed25519 AAAA… macbook-air"`, once per
  device, writes the list; a later device is a hand edit of this file.
- A key with no comment names no device. `fort check --all` warns:
  vetting trims the list by device.
- **An estate that names none has forts that do not evaluate.** The
  assertion in `ops/deployer.nix` refuses a host whose operator account
  no device can reach — the `sfi-*` verbs would be present and
  unreachable. `fort check --all` reports the same gap against
  `estate.toml`, before the eval.
- One fort that must carry a different set sets
  `sfi.ops.deployer.authorizedKeys` in its own host file, which
  *replaces* the estate's list for that host. `check` names the line: a
  device added to the estate does not reach that fort.

Who may OPERATE is this list. Who may DISPATCH — the agent accounts of a
dev fort — is the estate's dev role, with a list of its own: two
questions, two lists.

## After init

1. `nix flake lock ./nixos` — pin the framework. `--override-input forts
   path:../forts` develops against a local checkout.
2. Provide the three files above and create `provisioning/secrets.yaml`.
3. `forts/<host>.toml` ([manifest.md](manifest.md)), then `fort scaffold`,
   `fort mint`, a PR, `fort image`, `fort provision`.
