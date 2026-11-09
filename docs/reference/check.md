# fort check — the merge gate

```
fort check (<fort>... | --all) [--eval]
```

`check` is the admission gate over the estate repo: static validation of
the tree as it sits in the working copy. It reads files and runs
`kustomize build`; it never touches the network, a provider, or a
running fort, and it needs no credential. CI runs it on every push, and
an operator runs it before a PR for the same answer sooner.

The one tool the static checks need is `kustomize`
([the tool set](tools.md)), resolved before the gate reads a single
tree. `--all` selects the forts (every host with a manifest, against
naming them); `--eval` adds the nix half of the gate.

## What it checks

Per fort (every run):

- the manifest parses and validates: exactly one provider table, the
  neutral fields consistent, the table complete
- the fort's tree matches its manifest — a `[cluster]` fort has its flux
  cluster and apps trees, a plain fort has a host dir and no fleet
  surface (a flux dir on a plain fort is its own finding)
- the fort's two flux layers `kustomize build`, as do the shared
  infrastructure layers (built once per run)
- strict envsubst is satisfiable: every `${VAR}` the built layers
  reference is a key of that cluster's cluster-vars, with
  `substitute: disabled` resources exempt and `$$` escapes honored
- `ESTATE_NAME` on every cluster says the estate's name
- the console failsafe's two halves agree — the module enabled in the
  host file and the hash in the secrets file, since either half alone
  fails quietly
- a `[libvirt]` fort names its hypervisor URI, and that URI carries no
  `keyfile=` path that only one operator machine owns
- every committed inventory ([inventory](inventory.md)) under the
  fort's apps root: its Secret files exist and carry the keys it
  declares (key names are plaintext under the repo's `encrypted_regex`,
  so no age key is involved), a required `external` key unset is an
  error and an optional one a warning, and every `sops:` ref lands on a
  committed file that carries the key; a fort-side `forts.contract.yaml`
  that carries a `secrets:` block is an inventory under the old name,
  and the finding says to rename it

`--all` adds the estate section, the estate-wide facts no single fort
owns:

- every manifest in `forts/` parses — a host with a bad manifest is
  inert (nothing provisions, previews or destroys it), which is an
  error, not a default
- orphans: a dir under `nixos/hosts`, `flux/clusters` or `flux/apps`
  that no manifest claims (a warning, so a dismantle in flight does not
  go red on its residue)
- the mesh: the committed `ssh_config` is what the tree renders,
  `known_hosts` names every fort and nothing else, and every address —
  peer, client, linked host, every manifest's `mesh_ip` — lies inside
  the declared subnet
- the zone: no hand-written line claims a name the mesh renders
- the forge: `[hubs.forge]` resolves against the mesh, and no host file
  pins an address that stopped following it
- the operator keys: `operator_keys` names at least one device (no fort
  evaluates otherwise), each key carries a device comment, and a host
  file that replaces the estate's list is named per host
- the source route: ssh pins the forge's host key, anonymous https
  reads none — and the line states which API mint and destroy will
  speak, or that they speak none
- the links: every `[links.<name>]` claim against the local records.
  Verifying a link's private key needs the admin age key, which CI does
  not hold; where the secrets file does not decrypt, the key is
  reported as not verified — a skip, never a failure
- the layers: each installed layer by name, and nothing beneath it —
  a layer's own check is its own gate step after this one
  ([layers](layers.md))

## --eval: the nix half

With `--eval`, after the static checks, each checked fort's whole
module system evaluates through to its toplevel derivation
(`nix eval --raw ./nixos#nixosConfigurations.<host>…drvPath`), and the
derivation's name prints as the proof (the store hash is trimmed — it
says nothing a reader verifies by eye). This is the expensive half: it
needs `nix` on the host ([the tool set](tools.md)) and the framework
closure, which a cold store fetches first. The static checks stay
sub-second without it, which is why the flag is opt-in rather than part
of `--all`.

A staged-but-unprovisioned host fails the eval by design: its mesh peer
does not exist until `fort mint` runs, and the module system refuses a
fort the mesh does not know.

## Reading the output

One `== ⛫ <section>` block per fort, plus `estate` and (under `--all`,
when a manifest is broken) `forts`. A section with nothing to say
prints nothing, not even its header. Within a block, two kinds of line:

- an unmarked `label  value` line is a fact — a config touchpoint the
  gate read on its way through (the repo and its route, the mesh
  subnet and zone, the operator devices; per fort, its hypervisor, its
  mesh address with the name the zone renders for it, and each link).
  A fact cannot fail; it is there so a reviewer reads the estate off
  the gate — a typo'd address catches an eye in CI before a provision
  trips over it.
- `✓` marks a check that could have failed and did not; `!` a warning,
  `✗` an error, each with the path to look at and what to do.

Errors fail the run — the exit is non-zero and the last line counts
them — while warnings never do. On a clean run the last line is
`all checks passed`.

Two red states are the gate working, not breakage:

- a staged-but-unseeded app tree: a kustomization lists sops files that
  only exist after `fort seed` (or `fort mint`, for the canary), so the
  build fails until the ◆ files land — exactly what keeps that PR
  unmergeable
- an orphan warning during a decommission: the dismantle PR removes the
  manifest first, and the residue scan names what is left to delete

## In CI

Inside Gitea Actions or GitHub Actions (`GITEA_ACTIONS` /
`GITHUB_ACTIONS` set), every warning and error is also emitted as a
workflow command (`::warning::`, `::error::`), so findings annotate the
diff instead of hiding in a log. The workflow `fort estate init` renders runs
`fort check --all --eval` on every push — the static checks and the
eval, one step — pinned to the framework version the estate's flake
locks: the CLI that checks the tree is the one the forts run.
`fort layer add` appends the layer's own gate step after the core's.
