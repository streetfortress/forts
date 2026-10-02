# The manifest — `provisioning/forts/<host>.toml`

One file per fort, TOML, filename = hostname (no renames, as everywhere).
It is the fort's declaration: what every verb reads before it touches
anything, and the only hand-written input a raise needs. Cutting a fort
from a sibling is `cp`, three edits, `fort check --all`.

## Shape

```toml
# sfi-prod-439bf72 — THE production fort, on Linode.   ← the fort's history lives here
mesh_ip = "10.1.0.54"        # WireGuard mesh address (mandatory: every fort is a mesh member)

[cluster]                    # THE shape table — exactly one of [cluster] | [dev]
datastore = "sqlite"         # sqlite (single node, the default) | etcd (embedded, cluster-init)

[dns]                        # public name — the cloud backend's A + wildcard,
zone = "streetfortress.cloud" # and the mesh peer's default endpoint
name = "sfi-prod-439bf72"

[backups]                    # where this fort's volumes land (required)
profile = "production"       # lab | production | none

[linode]                     # THE provider table — exactly one per fort
region = "us-sea"
type = "g6-standard-2"
```

```toml
# staging-k3s — the staging fort, on the lab hypervisor.
mesh_ip = "10.1.0.13"

[cluster]                    # a flux cluster; datastore unset = sqlite
audit_logging = true         # the API server's audit log, shipped to Loki (#626)

[backups]
profile = "lab"

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

[backups]
profile = "production"

[baremetal]                  # no [shape]: the machine is the shape
ip = "10.0.0.6"
mac = "f8:ce:72:02:05:07"    # the NIC the address goes on — a dock's USB NIC
                             # is renamed whenever it moves ports
disk = "/dev/nvme0n1"        # the writer's target (docs/forts/baremetal.md)
```

```toml
# sfi-dev-c86dd7b — a dev fort (#413): a transient lab VM hosting one
# agent account per role (#532); no cluster.
mesh_ip = "10.1.0.61"

[dev]                        # THE declaration: dev-fort.nix, no k3s/flux/backups

[[dev.accounts]]             # the accounts, in order; the first one answers
user = "agent"               # on the fort's own name and its sfi-dev alias
verb = "pickup"
gitea_user = "sfi-agent"
email = "support@streetfortress.com"
token_scopes = ["write:repository", "write:issue", "read:organization", "read:user", "write:misc"]

[[dev.accounts]]
user = "analyst"
verb = "review"
gitea_user = "sfi-analyst"
email = "analyst@streetfortress.com"
ssh_alias = "sfi-analyst"    # required on every account after the first
token_scopes = ["write:issue", "read:repository", "read:organization", "read:user", "write:misc"]

[backups]
profile = "none"             # required with [dev] — nothing on it is precious

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
same Go struct (`forts/internal/manifest`), so the generators cannot drift.
A key that only one shape has does not sit here: `k3s_cluster_init` did,
and the loader had to special-case it next to `[dev]` (#426).

**A fort is always hardened, on the mesh — and one shape table says what
it is.** There is no `production` or `flux_cluster` key (#272): every fort
imports the hardened profile (mesh-scoped SSH, no wheel), gets the deploy
key (auto-rebuild's pull credential), carries `protect: true` on its
stack, and `mesh_ip` is required. Beyond that the manifest carries
**exactly one of `[cluster]` and `[dev]`**, handled like the provider
tables — required, strict keys, decoded by its own Spec — and both are
positive declarations, neither the absence of the other. **`[cluster]`**
(#426) makes the host a flux cluster: `k3s-server.nix` on the datastore
it names, `production.nix` (deployer-only), `fleet/clusters/<host>`,
`fleet/apps/<host>` and the cluster key. The table can be empty; every
key in it has a default. **`[dev]`** (#413) makes the host a dev fort —
`dev-fort.nix` (unprivileged accounts that build, the toolchains, the
dotfiles, nix-ld) with no k3s, no flux, no fleet tree, no cluster age key.
It requires `[backups] profile = "none"` and is `[libvirt]`-only for now
(forts#12: a lab VM on the hypervisor). A `[cluster]` key next to `[dev]`
is two shape tables, refused structurally, not a special case.

**`[cluster] datastore` is k3s's datastore**, and a positive word rather
than a negated boolean: the reader asks *which datastore*, not *was
cluster-init set*. `sqlite` (the default — every fort today is a single
node, and k3s-server with etcd was a third of a 4 GB node, #163/#164) or
`etcd` (embedded, cluster-init — the opt-in for an HA join; sqlite→etcd
migrates in place, the reverse is a repave). No `sfi.` prefix and no
nesting: TOML dotted keys nest tables, and everything in a manifest is
ours — if k3s ever needs its own group it becomes `[cluster.k3s]` then.
scaffold renders `sfi.k3s.datastore = "<value>"` into the host file, always
(a declaration, not a silent default), and `modules/k3s-server.nix` maps
the word to `services.k3s.clusterInit`, so no host file sets `clusterInit`
itself. On a host that predates the key the line is a hand edit, and a
test in `forts/cmd/fort` (`TestDatastoreMatchesHostFile`) keeps the
manifest and the host file equal.

**`[[dev.accounts]]` names the fort's identities** (#532). One entry per
role, and every field is required: `user` (the Unix account, which also
names its keys in `nixos/secrets/<host>.yaml`), `verb` (`pickup`, `scout`
or `review` — the one verb that account installs), `gitea_user`, `email`
(an address that user holds on gitea; the GPG uid carries it) and
`token_scopes` (the account's own, so an analyst holds no
`write:repository`). `ssh_alias` is the mesh name that dials the account
and is required on every entry **after the first** — the first answers on
the fort's own name and on its role alias. The names are unique within the
fort, and the list must not be empty: a dev fort with no account mints
nothing and nobody can log in.

`fort mint` reads this list and holds no account name of its own, because
the CLI leaves this repo later. Nix cannot read it — `provisioning/forts`
sits outside the flake's root — so `nixos/modules/dev-fort.nix` carries
the same list, and a test in `forts/cmd/fort`
(`TestDevAccountsMatchDevFortModule`) compares the two field by field.

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
`.lofi` names, the lab issuer — `Cloud()` is false) that nothing
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
workloads are declared by `fleet/apps/<host>/` itself — `fort add` is
imperative, and provider dirs arrive on first need. A checked `[[apps]]`
membership was designed and parked (#269) until a sibling cut shows the
tree copy is the pain.

No toggles for the standard components either — with the exceptions that
earned their way in. There are no `[production]` / `[staging]` tables:
those are not shapes but the same `[cluster]` with a different
`[backups]` profile and provider table, both of which the manifest
already says. Monitoring stays per-fort identical (the shared
infrastructure layer is, by design, and per-app monitoring is
contract-driven), but **`[backups] profile` is the first real second
variant**: every fort's volumes are backed up the same *way* and not to
the same *places*. A lab fort is reproducible and keeps one local copy;
a production fort's offsite copy must not depend on the lab being
reachable (docs/infra/backups.md).

```toml
[backups]
profile = "production"   # luxor's rustfs-central AND B2
profile = "lab"          # luxor's rustfs-central only
profile = "none"         # nothing — a positive declaration, not an omission
```

**Required, with no default**, and that is the point: a production fort
silently inheriting lab-only backups is the failure worth one line of
typing, and it cannot be inferred from the provider table (staging-k3s
is a lab VM that may want covering; a Linode drill fort should not pay
for B2). `none` is how luxor says what it is — the hub of copies and an
origin of nothing, so backing its volumes up to its own rustfs would be
circular. It renders no `sfi.pv-backup` wiring at all, rather than an
imported module sitting disabled.

The profile names a DESTINATION SET, never individual targets: there are
exactly two object stores at fleet level, and enumerating endpoints per
fort is how they drift.

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
hand edit, as `sfi.pv-backup.profile` was — and a test in `forts/cmd/fort`
(`TestAuditLoggingMatchesHostFile`) keeps the two equal: a manifest that
says `true` must set the option, and a host that sets the option must say
so in its manifest. `[dev]` refuses the table: a dev fort has no API
server.

## Who reads it

| verb | reads |
|---|---|
| `scaffold` | everything: the provider table picks the host shape, the shape table picks cluster or dev (`[dev]` drops the fleet surface), `mesh_ip` → `MESH_IP`, the seeds, `[backups] profile` → the host's `sfi.pv-backup` block, `[cluster] datastore` → the host's `sfi.k3s.datastore` line, `[cluster] audit_logging` → the host's `sfi.k3s.auditLog.enable` line |
| `go test ./cmd/fort` (the mirror tests) | `[dev]` ↔ the accounts in `modules/dev-fort.nix`; `[cluster] datastore` ↔ `sfi.k3s.datastore` and `[cluster] audit_logging` ↔ `sfi.k3s.auditLog.enable` in the host file — nix cannot read the manifest, so a test keeps each pair equal |
| `mint` | `mesh_ip` (wg key, peer), the endpoint; the deploy key always; the cluster key and canary for a `[cluster]` fort (`[dev]` skips them and the k3s token); `[backups] profile` — the restic password and rustfs credentials it copies in, and on `production` the B2 application key it mints |
| `add`, `seed`, `check` | `[dns] zone` (the app hostname's zone on a cloud fort) |
| `provision`, `preview`, `adopt` | the provider table (which backend, its credentials; `[baremetal]` has none — refused, skipped, not adoptable) |
| the pulumi program | the provider table, `[dns]`, `[shape]`, `mesh_ip` (exported) |
| `dismantle` | removes the file — the declaration goes first; the rest is its realization |
| `destroy` | the gate: no manifest on `origin/main` and none in the working tree (so it revokes the B2 key by name, not by profile — there is no manifest left to read) |
| `check --all` (fleet half), the nix-eval job | the list of hosts; `[dev]` → a host dir and no fleet dirs is the expected tree (a fleet dir anyway is an error) |

## History

Replaces `provisioning/inventory.yaml` (#252, 2026-09-15): one Ansible-
shaped map (`all.children.nixos.hosts`, `ansible_host`, `wg_vpn_ip`) that
served three vocabularies at once and whose per-host removal was a
line-based excision. The playbooks that read it left in #266; the fort CLI
and `ci/fleet-checks.py` were the only consumers by then.
