# The manifest — `provisioning/forts/<host>.toml`

One file per fort, TOML, filename = hostname (no renames, as everywhere).
It is the fort's declaration: what every verb reads before it touches
anything, and the only hand-written input a raise needs. Cutting a fort
from a sibling is `cp`, three edits, `fort check --all`.

## Shape

```toml
# sfi-prod-439bf72 — THE production fort, on Linode.   ← the fort's history lives here
mesh_ip = "10.1.0.54"        # WireGuard mesh address (mandatory for a flux cluster)
k3s_cluster_init = false     # optional scaffold seed: sqlite datastore, not etcd

[dns]                        # public name — the cloud backend's A + wildcard,
zone = "streetfortress.cloud" # and the mesh peer's default endpoint
name = "sfi-prod-439bf72"

[linode]                     # THE provider table — exactly one per fort
region = "us-sea"
type = "g6-standard-2"
```

```toml
# staging-k3s — the staging fort, on the lab hypervisor.
mesh_ip = "10.1.0.13"
k3s_cluster_init = false

[shape]                      # what libvirt realizes (lab defaults 2 / 4 / 40G)
vcpus = 2
memory = 4
disk = "80G"

[libvirt]
ip = "10.0.0.40"             # the static LAN address baked into the image
```

**The neutral top level is the contract.** `mesh_ip`, the optional `mesh_endpoint` / `k3s_cluster_init` /
`nixos_state_version`, `[shape]`, `[dns]` — this is what scaffold, mint,
add, check, the pulumi program and the mesh read. Every consumer reads the
same Go struct (`forts/internal/manifest`), so the generators cannot drift.

**A fort is always hardened, flux-managed, on the mesh.** There is no
`production` or `flux_cluster` key (#272): every fort imports
`production.nix` (deployer-only, mesh-scoped SSH), gets
`fleet/clusters/<host>` + the cluster key + the deploy key, carries
`protect: true` on its stack, and `mesh_ip` is required. A different shape
(#273) will be a positive declaration, not the absence of flux.

**One provider table, opaque to everyone but its backend.** `[linode]` and
`[libvirt]` are decoded by their own Go types (`LinodeSpec`, `LibvirtSpec`),
each with a `Validate` that runs on every load — so `fort check --all`
refuses a bad table without running pulumi, and a `Provision` never sees
one. Adding a provider = one file in `internal/manifest` + one backend in
`forts/pulumi`; the shared struct never grows a column. The table's fields
are the provider's own words (`type`, not a neutral "size"): the plan is a
pricing decision made in the PR, and catalogs don't align.

**Strict.** An unknown key anywhere — `productoin`, `ansible_host`, a
second provider table, none at all — is an error naming the file. A typo'd
flag never silently means its default.

**`[shape]` is what the lab realizes.** libvirt sizes the domain and grows
the qcow2 to `disk` before upload. A cloud host's shape is its plan;
nothing reads `[shape]` there, so leave it out.

**The endpoint follows.** Other peers dial `mesh_endpoint` when set, else
a lab VM's `[libvirt] ip`, else the `[dns]` name — `fort mint` writes that
into `peers.json`.

## What it never carries

No chart versions (Renovate owns `repository.yaml`), no values (the
HelmRelease), no secrets (`fort seed`), and **no apps list**: the fort's
workloads are declared by `fleet/apps/<host>/` itself — `fort add` is
imperative, and provider dirs arrive on first need. A checked `[[apps]]`
membership was designed and parked (#269) until a sibling cut shows the
tree copy is the pain.

No toggles for the standard components (monitoring, backups) either: the
shared infrastructure layer is per-fort identical by design, and per-app
monitoring is contract-driven. The first toggle arrives with the first real
second variant.

## Who reads it

| verb | reads |
|---|---|
| `scaffold` | everything: the provider table picks the host shape, `mesh_ip` → `MESH_IP`, the seeds |
| `mint` | `mesh_ip` (wg key, peer), the endpoint; the cluster key, deploy key and canary always |
| `add`, `seed`, `check` | `[dns] zone` (the app hostname's zone on a cloud fort) |
| `provision`, `preview`, `adopt` | the provider table (which backend, its credentials) |
| the pulumi program | the provider table, `[dns]`, `[shape]`, `mesh_ip` (exported) |
| `dismantle` | removes the file — the declaration goes first; the rest is its realization |
| `destroy` | the gate: no manifest on `origin/main` and none in the working tree |
| `ci/fleet-checks.py`, the nix-eval job | the list of hosts |

## History

Replaces `provisioning/inventory.yaml` (#252, 2026-09-15): one Ansible-
shaped map (`all.children.nixos.hosts`, `ansible_host`, `wg_vpn_ip`) that
served three vocabularies at once and whose per-host removal was a
line-based excision. The playbooks that read it left in #266; the fort CLI
and `ci/fleet-checks.py` were the only consumers by then.
