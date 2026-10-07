# The manifest — `forts/<host>.toml`

One file per fort, TOML, filename = hostname (no renames, as everywhere).
It is the fort's declaration: what every verb reads before it touches
anything, and the only hand-written input a raise needs. Cutting a fort
from a sibling is `cp`, three edits, `fort check --all`.

## Shape

```toml
# sfi-prod-439bf72 — THE production fort, on Linode.   ← the fort's history lives here
mesh_ip = "10.1.0.54"        # WireGuard mesh address (mandatory: every fort is a mesh member)

[cluster]                    # THE shape table; absent, the fort is a plain fort
datastore = "sqlite"         # sqlite (single node, the default) | etcd (embedded, cluster-init)

[dns]                        # public name — the cloud backend's A + wildcard,
zone = "streetfortress.cloud" # and the mesh peer's default endpoint
name = "sfi-prod-439bf72"

[backups]                    # what this fort backs up, and to which store (required)
[backups.db-physical]        # barman: base backups + WAL from its postgres
store = "b2"
hour = 4                     # the nightly base-backup hour (UTC) — forts stagger it
[backups.db-logical]         # databasus: logical dumps
store = "b2"
                             # no [[backups.pv]]: nothing on its volumes is an original

[linode]                     # THE provider table — exactly one per fort
region = "us-sea"
type = "g6-standard-2"
```

```toml
# staging-k3s — the staging fort, on the lab hypervisor.
mesh_ip = "10.1.0.13"

[cluster]                    # a flux cluster; datastore unset = sqlite
audit_logging = true         # the API server's audit log, shipped to Loki (#626)

[[backups.pv]]               # restic, every PVC: the hub's rustfs alone — reproducible data
store = "rustfs-central"

[shape]                      # what libvirt realizes (lab defaults 2 / 4 / 40G)
vcpus = 2
memory = 4
disk = "80G"

[libvirt]
ip = "10.0.0.40"             # the static LAN address baked into the image
```

```toml
# sfi-fort-zenbook — gitea's home, on lab hardware (a laptop on a dock).
mesh_ip = "10.1.0.6"

[cluster]
datastore = "sqlite"

[[backups.pv]]               # B2 every 6 h is the RPO…
store = "b2"

[[backups.pv]]               # …and a weekly copy on the hub's rustfs for LAN-speed restore
store = "rustfs-central"
every = "weekly"

[backups.db-physical]
store = "b2"
hour = 2

[backups.db-logical]
store = "b2"

[baremetal]                  # no [shape]: the machine is the shape
ip = "10.0.0.6"
mac = "f8:ce:72:02:05:07"    # the NIC the address goes on — a dock's USB NIC
                             # is renamed whenever it moves ports
disk = "/dev/nvme0n1"        # the writer's target (docs/baremetal.md)
```

```toml
# sfi-lab-c86dd7b — a plain fort: a lab VM with no cluster. The baseline
# host and nothing else the framework names; what it does is its host file.
mesh_ip = "10.1.0.61"

[backups]                    # empty, and required: nothing on it is precious

[shape]
vcpus = 4
memory = 8
disk = "80G"

[libvirt]
ip = "10.0.0.61"
```

**The neutral top level is the contract.** `mesh_ip`, the optional `mesh_endpoint` /
`nixos_state_version`, `[shape]`, `[dns]`, `[backups]` — this is what scaffold, mint,
add, check, the pulumi program and the mesh read. Every consumer reads the
same Go struct (`internal/manifest`), so the generators cannot drift.
A key that only one shape has does not sit here: `k3s_cluster_init` did,
and the loader had to special-case it (#426).

**A fort is always hardened, on the mesh — and the `[cluster]` table says
whether it is a flux cluster.** There is no `production` or `flux_cluster`
key (#272): every fort imports the hardened profile (mesh-scoped SSH, no
wheel), gets the deploy key (auto-rebuild's pull credential), carries
`protect: true` on its stack, and `mesh_ip` is required. **`[cluster]`**
(#426) makes the host a flux cluster: `k3s-server.nix` on the datastore
it names, `production.nix` (deployer-only), `flux/clusters/<host>`,
`flux/apps/<host>` and the cluster key. The table is handled like the
provider tables — strict keys, decoded by its own Spec — and can be
empty; every key in it has a default. **No `[cluster]` table makes a plain
fort**: the baseline the framework gives every host — `hardened.nix`,
`lab-host.nix`, the operator account `deployer` with the sfi-* verbs
(#647), `shell.nix`, the mesh, auto-rebuild — with no k3s, no flux, no
fleet tree, no cluster age key. Ideally nobody logs in; the prescribed
access is `deployer` over the mesh and the console password. What a plain
fort does beyond that — accounts, services, a role — is plain NixOS in its
host file, the estate's to write; the manifest names no user and no role.
A plain fort requires an empty `[backups]` table and is `[libvirt]`-only
for now (a lab VM on the hypervisor; a cloud plain fort would be its own
host template).

**`[cluster] datastore` is k3s's datastore**, and a positive word rather
than a negated boolean: the reader asks *which datastore*, not *was
cluster-init set*. `sqlite` (the default — every fort today is a single
node, and k3s-server with etcd was a third of a 4 GB node, #163/#164) or
`etcd` (embedded, cluster-init — the opt-in for an HA join; sqlite→etcd
migrates in place, the reverse is a re-provision). No `sfi.` prefix and no
nesting: TOML dotted keys nest tables, and everything in a manifest is
ours — if k3s ever needs its own group it becomes `[cluster.k3s]` then.
scaffold renders `sfi.k3s.datastore = "<value>"` into the host file, always
(a declaration, not a silent default), and `modules/k3s-server.nix` maps
the word to `services.k3s.clusterInit`, so no host file sets `clusterInit`
itself. On a host that predates the key the line is a hand edit, and a
test in `cmd/fort` (`TestDatastoreMatchesHostFile`) keeps the
manifest and the host file equal.

**One provider table, opaque to everyone but its backend.** `[linode]`,
`[libvirt]` and `[baremetal]` are decoded by their own Go types
(`LinodeSpec`, `LibvirtSpec`, `BaremetalSpec`), each with a `Validate`
that runs on every load — so `fort check --all` refuses a bad table
without running pulumi, and a `Provision` never sees one. Adding a
provider = one file in `internal/manifest` + one backend in
`forts/pulumi`; the shared struct never grows a column. The table's fields
are the provider's own words (`type`, not a neutral "size"): the plan is a
pricing decision made in the PR, and catalogs don't align.

**`[baremetal]` is the one table with no backend behind it** (#287,
[baremetal.md](baremetal.md)). The machine exists; the image is written
to its disk by a human. The loader treats it as a lab host (LAN address,
names under the mesh zone, the lab issuer — `Cloud()` is false) that nothing
realizes (`Realized()` is false): `provision` refuses it by name,
`preview --all` lists it as `SKIP`, `destroy --no-stack` revokes its
identities without a stack. `mac` is the one field with no VM analogue
— the NIC's burned-in address, matched instead of a name that a dock
or USB port rewrites.

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
workloads are declared by `flux/apps/<host>/` itself — `fort add` is
imperative, and provider dirs arrive on first need. A checked `[[apps]]`
membership was designed and parked (#269) until a sibling cut shows the
tree copy is the pain.

No toggles for the standard components either — with the exceptions that
earned their way in. There are no `[production]` / `[staging]` tables:
those are not shapes but the same `[cluster]` with a different
`[backups]` table and provider table, both of which the manifest
already says. Monitoring stays per-fort identical (the shared
infrastructure layer is, by design, and per-app monitoring is
contract-driven), but **`[backups]` is the first real second variant**:
every fort's data is backed up the same *way* and not to the same
*places*, and not every layer on every fort (sfi/forts#4, #39).

```toml
[backups]                    # required; empty = "this fort does not back up"

[[backups.pv]]               # restic, every PVC that is an original (one block per store)
store = "b2"
every = "6h"                 # 6h (the default) | daily | weekly — the fleet supplies the phase

[[backups.pv]]
store = "rustfs-central"
every = "weekly"

[backups.db-physical]        # barman: base backups + WAL from the fort's postgres
store = "b2"
hour = 4                     # the nightly base-backup hour, UTC — required, so forts stagger it

[backups.db-logical]         # databasus: logical dumps
store = "b2"
```

**One block per layer, each naming a store.** The three layers are the
three backup mechanisms (docs/infra/backups.md): `pv` is restic from
the host, `db-physical` is CNPG's barman archive, `db-logical` is
databasus. A block names a **store**, never an endpoint: there are
exactly two object stores at fleet level, and enumerating endpoints per
fort is how they drift. `b2` takes every layer. `rustfs-central` — the
hub's rustfs — takes `pv` alone in this cut (`db-physical` never: CNPG
has one `archive_command`, and it points offsite; `db-logical` is #4's
open question). The store decides what the block needs of the estate:
**a block on `rustfs-central` requires a monitoring hub in this estate**
(`[hubs.monitoring]` with `estate` = this estate; the fort registers an
identity in the hub's tree), **a block on `b2` requires nothing** —
which is what lets an estate whose hub is another estate's
(sfi/production; [hub.md](hub.md)) declare backups at all.

**The table is required and its blocks are not**, and that is the point:
a production fort silently inheriting lab-only backups is the failure
worth one line of typing, and it cannot be inferred from the provider
table (a lab VM may want covering; a Linode drill fort should not pay for
B2). `[backups]` with nothing under it is the positive declaration that
nothing on the fort is backed up — the hub of copies and an origin of
nothing, or a staging fort — and renders no `sfi.pv-backup` wiring at
all, rather than an imported module sitting disabled. A manifest without
the table is an error.

**What each block renders.** `[[backups.pv]]` → one `sfi.pv-backup`
target per block in the host file, `{ store; every }` word for word
(`modules/pv-backup.nix` holds the endpoint, the credential name and the
phase per store), the credentials `fort mint` seeds for the stores named
(`restic-password` for any; the rustfs identity on the hub for
`rustfs-central`; the B2 key `sfi-pv-<host>` for `b2`), and the
`PVBackupNotConfigured` rule in the cluster's file for the hub
(`flux/hub/<hub host>/<host>.yaml`) — no pv
block, no unit, no credential, no rule. `[backups.db-physical]` → the
`ObjectStore` and the `ScheduledBackup` (at `hour`) that `fort add`
renders beside the fort's postgres; no block, no archive, said so in
`cluster.yaml`. `[backups.db-logical]` is accepted and checked; nothing
renders from it yet (#4, #375). The cadence vocabulary beyond `every`'s
three words and the one store-registry file both halves would read stay
on #4.

**It replaces `profile = "lab" | "production" | "none"`** (retired in
sfi/forts#39): one word that bundled three layers, a destination set and
a cadence, and could not say what the production fort wants (physical and
logical to B2, no PV leg) or what the hub wants (PV to B2, nothing
physical). The three manifests of sfi/deployments stay on its in-tree
CLI and do not migrate.

**`[cluster] audit_logging` is the second exception** (#626), for the
same reason as the first: every cluster fort audits the same *way* — the
policy, the rotation and the stream are one module and one fluent-bit
config, fleet-wide — and not every fort pays the same disk for it. A
4 GB Linode SKU with a 25 GB disk and the lab hub with 450 GB do not
carry the same log.

```toml
[cluster]
datastore = "sqlite"    # the shape's own key, above
audit_logging = true    # the API server writes an audit log; fluent-bit ships it to Loki
```

**Default false, and the key is absent on a fort that does not audit**,
so a fort inherits nothing by silence. `true` renders
`sfi.k3s.auditLog.enable = true` into the host file
(`modules/k3s-audit.nix` holds the policy and the rotation caps,
docs/nixos/operations.md says what the log is for). Scaffold never
overwrites a host file, so on a fort that predates the key the line is a
hand edit — and a test in `cmd/fort`
(`TestAuditLoggingMatchesHostFile`) keeps the two equal: a manifest that
says `true` must set the option, and a host that sets the option must say
so in its manifest. A plain fort has no API server and no `[cluster]`
table to put the key in.

## Who reads it

| verb | reads |
|---|---|
| `scaffold` | everything: the provider table picks the host shape, the `[cluster]` table picks cluster or plain (a plain fort drops the fleet surface), `mesh_ip` → `MESH_IP`, the seeds, `[[backups.pv]]` → the host's `sfi.pv-backup.targets` (and the `PVBackupNotConfigured` rule in the fort's file for the hub), `[cluster] datastore` → the host's `sfi.k3s.datastore` line, `[cluster] audit_logging` → the host's `sfi.k3s.auditLog.enable` line |
| `go test ./cmd/fort` (the mirror tests) | `[cluster] datastore` ↔ `sfi.k3s.datastore` and `[cluster] audit_logging` ↔ `sfi.k3s.auditLog.enable` in the host file — nix cannot read the manifest, so a test keeps each pair equal |
| `mint` | `mesh_ip` (wg key, peer), the endpoint; the deploy key always; the cluster key and canary for a `[cluster]` fort (a plain fort skips them and the k3s token); `[[backups.pv]]` — the restic password for any block, the rustfs identity it declares on the hub for a block on `rustfs-central`, the B2 application key it mints for a block on `b2` |
| `add`, `seed`, `check` | `[dns] zone` (the app hostname's zone on a cloud fort); `add` reads `[backups.db-physical]` for the postgres archive (the `ObjectStore`, the `ScheduledBackup` at `hour`); `check` traces every `rustfs-central` pv block to an identity on the hub and back |
| `provision`, `preview`, `adopt` | the provider table (which backend, its credentials; `[baremetal]` has none — refused, skipped, not adoptable) |
| the pulumi program | the provider table, `[dns]`, `[shape]`, `mesh_ip` (exported) |
| `dismantle` | removes the file — the declaration goes first; the rest is its realization |
| `destroy` | the gate: no manifest on `origin/main` and none in the working tree (so it revokes the B2 key by name, not by block — there is no manifest left to read) |
| `check --all` (fleet half), the nix-eval job | the list of hosts; a plain fort → a host dir and no fleet dirs is the expected tree (a fleet dir anyway is an error) |

## History

Replaces `provisioning/inventory.yaml` (#252, 2026-09-15): one Ansible-
shaped map (`all.children.nixos.hosts`, `ansible_host`, `wg_vpn_ip`) that
served three vocabularies at once and whose per-host removal was a
line-based excision. The playbooks that read it left in #266; the fort CLI
and `ci/fleet-checks.py` were the only consumers by then.
