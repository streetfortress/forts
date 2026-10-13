# Layers — what sits on top of the core

The core of forts is the host, the cluster bootstrap, the mesh and the
link: what every estate needs. After that the estate is NixOS and flux,
and the operator builds what they want with it. A **layer** is a
standard someone keeps on top of that: a hub role with a render on both
ends (a monitoring hub and the file each served fort renders to reach
it), a chart contract and its batteries, a backup store and the
identities that use it. SFI keeps its own in sfi/fort-layers. An estate
installs the layers it wants, and the core knows none of them by name.

What the core knows is the **protocol** (forts#144). It is four things,
and it stays that small on purpose: a registry of layers, a schema per
layer or a render hook would make every layer a feature of the core
again.

## 1. A layer owns one directory

```
estate.toml                    core
forts/<fort>.toml              core
layers/<layer>/estate.toml     the layer's estate declarations
layers/<layer>/forts/<fort>.toml   the layer's declarations per fort
```

The core never reads under `layers/`. `fort check --all` lists what is
installed there and nothing more. `forts/*.toml` is read as one manifest
per file, so a layer's per-fort file cannot live beside the core's; the
directory is the line. One `git rm -r layers/<layer>/` removes a layer
from an estate.

## 2. A layer is one binary

A layer ships `fort-<layer>`, with the verbs the order below expects.
`fort <layer> <verb> [args]` execs it from PATH, git style, in the same
process, with `FORT_ESTATE_ROOT` set to the estate root so the binary
need not find it again:

```
fort monitoring scaffold central      →  fort-monitoring scaffold central
fort backups check --all              →  fort-backups check --all
```

A first word that is neither a core verb nor a binary on PATH is one
error that names both. The binary comes from the layer's flake, at the
pin the estate holds (below).

## 3. `fort layer add` installs one

```
fort layer add git+ssh://git@gitea.zen.lofi:30022/sfi/fort-layers#monitoring \
    --write -- --central central --write
```

The argument is a flake URL and a role. The URL names the input, by its
last path label (`fort-layers`); the role names the binary
(`fort-monitoring`). The verb makes three edits in the core's files,
prints them in the marked-tree shape every author verb uses, and with
`--write` applies and stages them:

```
○ nixos/flake.nix                + fort-layers.url = "…"; + fort-monitoring = inputs.fort-layers.packages.${sys}.fort-monitoring;
○ .gitea/workflows/checks.yaml   + the step `The gate of the monitoring layer (fort monitoring check --all)`
```

- **The input.** `inputs.fort-layers.url` in `nixos/flake.nix`, once
  per repo: a second role from the same repo reuses it. The pin is the
  estate's lock, as the framework's is.
- **The re-export.** One line in the flake's `layers = sys: { … }` set:
  the role's binary at the layer's pin, so `nix run ./nixos#fort-monitoring`
  is the same binary the gate runs. A flake rendered before layers has
  no such set; the verb refuses and says what to add.
- **The gate step.** After the core's step in `.gitea/workflows/checks.yaml`:
  `nix run ./nixos#fort-<role> -- check --all`. The layer's check is the
  layer's gate, and the estate's gate runs both.

Then the layer's own `init` runs — `fort-monitoring init`, from PATH or
through `nix run <url>#fort-monitoring` — with **every flag after `--`
passed through unread**. The core does not parse them. The layer's init
writes `layers/monitoring/` itself.

`fort layer remove <role> [--write]` is the reversal: the directory, the
step, the re-export, and the input when no other role of that repo is
installed. Run the layer's own `dismantle` first, per fort, so the
layer withdraws what it declared in other estates and on hubs.

## 4. The verb order

A layer provides the verbs the core has, where the role needs them:
`init`, `scaffold`, `check`, `mint`, `dismantle`. The operator runs the
core's verb, then the layer's — except on the way out, where the layer's
runs first:

| step | core | then |
|---|---|---|
| create the estate | `fort init` | `fort layer add …` (runs the layer's init) |
| a fort's files | `fort scaffold <fort>` | `fort <layer> scaffold <fort>` |
| a fort's identity | `fort mint <fort>` | `fort <layer> mint <fort>` |
| the gate | `fort check --all` | `fort <layer> check --all` |
| retire a fort | `fort <layer> dismantle <fort>` **first** | `fort dismantle <fort>`, `fort destroy <fort>` |

No hook in the core: the order is this table. A layer that needs the
core to call it at a point this table does not name has found a gap in
the core, and that is an issue there.

## The ports

A role a layer serves needs ports open on the link to each estate it
serves. Those sit on the core's side, on the serves entry of the hub's
`estate.toml`, by hand:

```toml
[[serves.monitoring]]
fort = "central"
estates = ["ssh://git@gitea.zen.lofi:30022/sfi/staging"]
ports = [30990, 31100]
```

The layer's `check` verifies that the ports it needs are there. A layer
writes no core file, and the core writes no layer file. That is the
whole boundary, and it is what lets a layer move between repos, or be
replaced by another, without the estate tree changing shape.
