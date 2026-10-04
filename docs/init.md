# fort init — birth an estate

```
fort init --name <estate> --repo <git url> --admin <age1…> [--admin …] [--hub <host>] [--forts <flake url>] [--write]
```

`init` renders, into the root of a git checkout, the files every other
verb assumes are there. It is the first verb an estate runs and the only
one that runs with no `estate.toml`. Like [`scaffold`](scaffold.md) it is a
pure render: no secrets, no network, nothing overwritten — a re-run keeps
every file that exists.

## What it renders

```
● estate.toml                     the estate: name, repo, hub (if any)
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
# hub = "luxor"                                               # the monitoring hub; none = edge-only
```

`scaffold` renders the FluxInstance source and `sfi.auto-rebuild.repoUrl`
from `repo`, and a fort's file on the hub only when `hub` names one.
`check` and `dismantle` read the hub too. An estate with no hub has nobody
to notice a silent fort: the edge Alertmanager is its only alerting, and
`fort check --all` says so.

## After init

1. `nix flake lock ./nixos` — pin the framework. `--override-input forts
   path:../forts` develops against a local checkout.
2. Provide the three files above and create `provisioning/secrets.yaml`.
3. `forts/<host>.toml` ([manifest.md](manifest.md)), then `fort scaffold`,
   `fort mint`, a PR, `fort image`, `fort provision`.
