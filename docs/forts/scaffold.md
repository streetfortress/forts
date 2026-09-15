# fort scaffold — the host's generated surface

```
fort scaffold <host> [--write]
```

`scaffold` renders every file a host needs that is derivable from its
inventory entry, and nothing that is not. It is a pure template render:
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

From `provisioning/inventory.yaml`'s entry for `<host>`:

```
nixos/hosts/<host>/
● default.nix        lab shape (qcow runtime, static LAN block) when the entry
                     has provider: libvirt or none; cloud shape (linode runtime,
                     split DNS over the mesh, zram, Lish break-glass) when it
                     names a cloud provider
● secrets.nix        sops-nix wiring: the host's age identity is derived from
                     its ssh host key; declares wireguard-private-key when the
                     entry has wg_vpn_ip
fleet/clusters/<host>/                       (flux_cluster: true only)
● flux-system/flux-operator.yaml
● flux-system/flux-instance.yaml             sync path fleet/clusters/<host>
● flux-system/kustomization.yaml
● cluster-vars.yaml                          CLUSTER_NAME <host>, MESH_IP <wg_vpn_ip>
● infrastructure.yaml                        infra-controllers + infrastructure (the shared layer)
● apps.yaml                                  → fleet/apps/<host>
fleet/apps/<host>/
● kustomization.yaml                         root list: sops-canary (listed, absent until fort mint)
```

Inventory fields it reads: `provider`, `region`, `instance_type`,
`ansible_host` (the lab static IP), `wg_vpn_ip`, `flux_cluster`,
`production`, and the optional lab overrides `lan_prefix`, `lan_gateway`,
`lan_dns`, `nixos_state_version`, `k3s_cluster_init`. The lab defaults
are `/24`, gateway `10.0.0.1`, DNS `10.0.0.2` + `8.8.8.8`, state version
`25.11`.

A flux cluster without `wg_vpn_ip` is refused: every flux cluster is a
mesh member (`MESH_IP` is mandatory), and a production host without a
mesh address has no login path at all.

## Force-no semantics

`--write` lays a file down only when it does not exist; an existing one is
reported `○ exists — kept` and never touched. So:

- inventory seeds the host once; hand refinements live in the files and
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
# PR → merge → nix build ./nixos#web3-linode → inject-host-key.sh → fort provision web3 --image …
```

`nix` reads only tracked or staged paths, so the scaffold stages what it
wrote; the flake discovers the new host dir and its image variant from the
runtime module the host file imports — no flake edit.
