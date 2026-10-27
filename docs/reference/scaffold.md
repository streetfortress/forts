# fort scaffold — the host's generated surface

```
fort scaffold <host> [--dry-run]
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
● default.nix        the provider table picks the runtime and the LAN block —
                     qcow + static LAN for libvirt, linode + no LAN block for a
                     cloud provider, baremetal + the LAN block by MAC; the
                     [cluster] table picks the cluster shape or the plain shape
                     (below); every shape imports shell.nix and
                     ops/console.nix and enables both (the operator defaults,
                     below)
● secrets.nix        sops-nix wiring: the host's age identity is derived from
                     its ssh host key; declares wireguard-private-key when the
                     manifest has mesh_ip
flux/clusters/<host>/
● flux-system/flux-operator.yaml
● flux-system/flux-instance.yaml             sync path flux/clusters/<host>; pullSecret on the estate's ssh route only
● flux-system/kustomization.yaml
● cluster-vars.yaml                          CLUSTER_NAME <host>, ESTATE_NAME <estate>, MESH_IP <mesh_ip>
● infrastructure.yaml                        infra-controllers + infrastructure (the shared layer)
● apps.yaml                                  → flux/apps/<host>
flux/apps/<host>/
● kustomization.yaml                         root list: sops-canary (listed, absent until fort mint)
```

What a hub asks of the fort — a dead man's switch file, a datasource —
is a layer's render into the layer's own directory ([layers](layers.md));
the core's tree stops at the apps root.

Manifest fields it reads: the provider table (`[libvirt]` or
`[baremetal]` `ip` — the lab static IP — and its optional `lan_prefix`,
`lan_gateway`, `lan_dns`; `[baremetal]` `mac`, which turns the LAN
block's `en*` name match into a MAC match; `[linode]` `region` + `type`
for the header comment), `mesh_ip`, `[cluster] datastore` (rendered as
the host's `sfi.k3s.datastore` line, `sqlite` when unset; #426),
and the
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

A manifest with no `[cluster]` table renders the host dir alone:

```
● nixos/hosts/sfi-lab-x/default.nix   plain fort shape (qcow runtime, static LAN, deployer + the ops verbs; no k3s/flux)
● nixos/hosts/sfi-lab-x/secrets.nix   sops-nix wiring (host key → age)
```

`default.nix` imports the baseline without `k3s-server.nix` and
`flux-bootstrap.nix`: `hardened.nix`,
`ops/deployer.nix`, `shell.nix` and `ops/console.nix`, with the four
modules a role names — `estate.nix`, `wireguard.nix`, `metrics.nix`,
`lab-dns.nix` (forts#60) — plus `auto-rebuild.nix` and the zram block.
Nothing under `flux/`: there is no cluster to sync and no canary to
prove decryption. `secrets.nix` declares no `k3s-token`. The file is the
estate's from then on: an account, a service, a role is plain NixOS
written below the imports, and the framework names none of it.

**The plain shape renders on every provider** (forts#142). The
`[cluster]` table decides k3s and flux; the provider table decides the
runtime module and the backend. One template serves the three, and the
note says which runtime you got:

| provider table | runtime import | the rest of the difference |
|---|---|---|
| `[libvirt]` | `runtime-qemu` | the static LAN block, `en*` by name; the NoCloud channel for the host key (#657) |
| `[linode]` | `runtime-linode` | no LAN block; `resolved`; `PermitRootLogin` forced to `no` over upstream's `linode-config.nix`; Lish as the break-glass; the Metadata channel (#606) |
| `[baremetal]` | `runtime-baremetal` | the static LAN block, by MAC where `[baremetal] mac` names one; no backend raises it, and the host key's channel is #658 |

`lab-dns.nix` is imported on all three, as `k3s-server.nix` imports it
on all three: the module pins the forge's name in a block of its own,
and forwards the LAN zone (`.lofi`) to the host's own static resolvers
in another, which a cloud host has none of, so there it pins the forge's
name and forwards nothing.

## The operator defaults

Every shape imports `shell.nix` and `ops/console.nix` and sets
`sfi.shell.enable = true` and `sfi.ops.console.enable = true`
(sfi/forts#24). Both are the operator's, not the role's: every fort
carries `deployer` (`ops/deployer.nix`, #647), the shell is what a
person reads at a pane as that account, and the console password is
the one door that survives a broken mesh — hosts answer SSH on the
mesh alone, so a merged change that breaks wireguard locks every
operator out of every fort at once, and the getty login to `sudo
nixos-rollback` is the way back in (#524). The first fort of
sfi/second-estate came up with neither, and both had to be added by
hand after the provision; a door each estate must remember to add is not a
door. The three cluster forts of sfi/deployments had added the same
lines by hand.

The credential follows: [`mint`](mint.md) reads the host file, sees the
enable line, and mints the console password with the rest of the
identity — the hash into the fort's sops file, the passphrase into
`provisioning/console-passwords.yaml`. `fort check` fails a host that
enables the module without the hash, which is the state between
`scaffold` and `mint`.

A host that wants no door takes the two console lines out of its own
`default.nix` before mint; `fort check` says nothing about a host with
no half of the credential. The prompt's host label
(`sfi.shell.starshipSettings.hostname.style`, #582) stays a hand edit
in the host file: the manifest has no key for it.

## The two identity labels

`cluster-vars.yaml` carries the two labels every metric and log line
leaves the fort with: `CLUSTER_NAME`, the fort's name, and `ESTATE_NAME`,
the estate's (`estate.toml` `name`; forts#50). The shared layer stamps
both on the edge Prometheus as external labels, so whatever a monitoring
layer pushes on carries them. A hub that serves several estates
(forts#38) receives one flat set of clusters otherwise.

The label is one value per estate, so a log store gains no streams. On
metrics it is an external label, and an external label that changes on
a running edge changes the identity of every series it remote-writes:
the hub's old series end and new ones start at the rollout. A dashboard
that queries by `cluster` alone shows no gap.

`fort check` reads the values, not only the keys: a `CLUSTER_NAME` that
is not the fort's name, or an `ESTATE_NAME` that is not this estate's,
passes flux's strict substitution and mislabels the fort wherever its
series go. A cluster scaffolded before
`ESTATE_NAME` existed fails the gate (the shared layer references a
var its cluster-vars lacks) until `fort scaffold <host>` adds
the line — the one key scaffold appends to an existing
`cluster-vars.yaml`, below.

## Force-no semantics

The apply lays a file down only when it does not exist; an existing one is
reported `○ exists — kept` and never touched. The one exception is the
`ESTATE_NAME` line of an existing `cluster-vars.yaml`: a cluster-vars
without it gains the line (a text-level append under `data:`, comments
and order kept), and one that has it — whatever the value — is kept as it
is. So:

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
fort scaffold web3
fort mint web3            # identity, the console password included
# PR → merge → fort image web3 → fort provision web3 --image …
```

`nix` reads only tracked or staged paths, so the scaffold stages what it
wrote; the flake discovers the new host dir and its image variant from the
runtime module the host file imports — no flake edit.
