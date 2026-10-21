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

[linode]                     # THE provider table — exactly one per fort
region = "us-sea"
type = "g6-standard-2"
```

```toml
# staging-k3s — the staging fort, on the lab hypervisor.
mesh_ip = "10.1.0.13"

[cluster]                    # a flux cluster; datastore unset = sqlite
audit_logging = true         # the API server's audit log, shipped by a monitoring layer (#626)

[shape]                      # what libvirt realizes (lab defaults 2 / 4 / 40G)
vcpus = 2
memory = 4
disk = "80G"

[libvirt]
uri = "qemu+ssh://soundship/system"   # the hypervisor that raises it
ip = "10.0.0.40"             # the static LAN address baked into the image
# pool = "images"            # the storage pool; the default when unset
```

```toml
# sfi-fort-zenbook — gitea's home, on lab hardware (a laptop on a dock).
mesh_ip = "10.1.0.6"

[cluster]
datastore = "sqlite"

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

[shape]
vcpus = 4
memory = 8
disk = "80G"

[libvirt]
uri = "qemu+ssh://soundship/system"
ip = "10.0.0.61"
```

```toml
# celld-sea — a plain fort on a linode. The same baseline host, raised by
# the cloud backend instead of the hypervisor; what it serves is its host file.
mesh_ip = "10.1.0.62"

[dns]
zone = "streetfortress.cloud"
name = "celld-sea"

[linode]
region = "us-sea"
type = "g6-nanode-1"
```

**The neutral top level is the contract.** `mesh_ip`, the optional `mesh_endpoint` /
`nixos_state_version`, `[shape]`, `[dns]` — this is what scaffold, mint,
check, the pulumi program and the mesh read. Every consumer reads the
same Go struct (`internal/manifest`), so the generators cannot drift.
A key that only one shape has does not sit here: `k3s_cluster_init` did,
and the loader had to special-case it (#426).

**A fort is always hardened, on the mesh — and the `[cluster]` table says
whether it is a flux cluster.** There is no `production` or `flux_cluster`
key (#272): every fort imports the hardened profile (mesh-scoped SSH, no
wheel), gets the deploy key (auto-rebuild's pull credential), carries
`protect: true` on its stack, and `mesh_ip` is required. **`[cluster]`**
(#426) makes the host a flux cluster: `k3s-server.nix` on the datastore
it names (the cluster grant and the ports ride with it, #154),
`flux/clusters/<host>`,
`flux/apps/<host>` and the cluster key. The table is handled like the
provider tables — strict keys, decoded by its own Spec — and can be
empty; every key in it has a default. **No `[cluster]` table makes a plain
fort**: the baseline the framework gives every host — `hardened.nix`,
`estate.nix`, `metrics.nix`, `lab-dns.nix`, the operator account
`deployer` with the ops verbs (#647, [ops.md](ops.md)), `shell.nix`, the mesh,
auto-rebuild — with no k3s, no flux, no
fleet tree, no cluster age key. Ideally nobody logs in; the prescribed
access is `deployer` over the mesh and the console password. What a plain
fort does beyond that — accounts, services, a role — is plain NixOS in its
host file, the estate's to write; the manifest names no user and no role.
**The two tables decide two things, and they do not depend on each other**
(forts#142). `[cluster]` decides k3s and flux. The provider table decides
the runtime module and the backend that raises the host. A plain fort goes
where a cluster fort goes: a lab VM, a linode, a machine on the LAN. The
three shapes of the plain host file differ only in the runtime import and
the LAN block — `runtime-qemu` with a static LAN block, `runtime-linode`
with none, `runtime-baremetal` with the block matched by MAC.

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

**`[libvirt] uri` is the hypervisor** (#27). Which machine raises a lab
fort is the estate's fact, as much as `[linode] region` is a cloud
host's, and the environment variable it replaces recorded it nowhere:
a second estate on a second hypervisor had to export
`LIBVIRT_DEFAULT_URI` before every apply. The CLI gives the URI to the
provider as it stands and appends nothing to it, so the table names the
machine and no credential: one manifest is read by every operator
machine, and a key path is one machine's (`/Users` against `/home`).
Each machine's `~/.ssh/config` supplies the user, the address and the
identity — the same split as the estate's `operator_keys`.
`LIBVIRT_DEFAULT_URI` wins where an operator sets it, which is where a
URI with a credential in it belongs. The key is optional: a fort that
names none needs that variable in the environment of every apply, and
`fort check` warns. `pool` is the storage pool of the host's volumes,
`images` when unset.

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
HelmRelease), no secrets, and **no apps list**: the fort's
workloads are declared by `flux/apps/<host>/` itself — a layer's verb is
imperative, and provider dirs arrive on first need. A checked `[[apps]]`
membership was designed and parked (#269) until a sibling cut shows the
tree copy is the pain.

No toggles for the standard components either — with the one exception
that earned its way in. There are no `[production]` / `[staging]` tables:
those are not shapes but the same `[cluster]` with a different provider
table. The `[backups]` table, which was the first second variant, is the
backups layer's now: `layers/backups/forts/<fort>.toml` (sfi/fort-layers,
forts#143), and a manifest that still carries `[backups]` is refused
with the path it moved to.

**`[cluster] audit_logging` is the exception** (#626): every cluster fort audits the same *way* — the
policy and the rotation are one module, fleet-wide, and a monitoring
layer's shipper takes the stream — and not every fort pays the same disk for it. A
4 GB Linode SKU with a 25 GB disk and the lab hub with 450 GB do not
carry the same log.

```toml
[cluster]
datastore = "sqlite"    # the shape's own key, above
audit_logging = true    # the API server writes an audit log; a monitoring layer ships it
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

**`[links.<name>]` is a hub on another estate's mesh** (forts#55,
[links.md](links.md)). A fort is on one mesh, its estate's; a host that
serves another estate a role — the forge, a hub a layer serves — links to
that estate, and the link's facts live here, one table per estate:

```toml
[links.staging]                  # the name: the last label of the estate URL, and sfi.links.<name>
estate = "ssh://git@gitea.zen.lofi:30022/sfi/staging"
address = "10.2.0.254"           # this host's address on that mesh — a claim, until staging records it
port = 51821                     # the link's UDP listen port; never 51820, this host's own mesh
interface = "wg-staging"         # what staging's estate.toml names its mesh (default wg-<name>)
zone = "staging.mesh"            # its zone (default <name>.mesh)
public_key = "…"                 # the public half of the sops secret wireguard-private-key-staging
```

**The public key is plaintext here on purpose.** The private half is
sops-encrypted in `nixos/secrets/<host>.yaml`, as a fort's own key is,
and the public half sits where the other estate can read it without
this estate's age key — the split `mesh/peers.json` already makes for a
fort's own key. `fort link add <host> <estate url>` fetches that estate's
`estate.toml` and `mesh/peers.json`, picks the address and the port,
mints the key and writes the table; `fort scaffold` renders
`sfi.links.<name>` from it (and `fort link add` appends the block to a
host file that exists). The other estate's `fort estate link <this
estate's url>` reads the table from a fetch of this repo and writes the
`remotes` entry — the manifest is the file every cross-repo read reads,
and no verb evaluates another estate's Nix. `address` is this side's
claim; that estate's `peers.json` is the record, and its verb verifies
the claim before it writes. `fort check --all` compares `public_key`
with what the sops secret derives, where the age key is at hand.

## Who reads it

| verb | reads |
|---|---|
| `scaffold` | everything: the provider table picks the host shape, the `[cluster]` table picks cluster or plain (a plain fort drops the fleet surface), `mesh_ip` → `MESH_IP`, the seeds, `[cluster] datastore` → the host's `sfi.k3s.datastore` line, `[cluster] audit_logging` → the host's `sfi.k3s.auditLog.enable` line, `[links.<name>]` → the host's `sfi.links.<name>` block |
| `link add` | writes `[links.<name>]`: the estate, the address and port it picked, the interface and zone the other estate names, the public half of the key it minted |
| `estate link` (run in the *other* estate) | every `[links.<name>]` of a fetch of this repo that names that estate, plus the host's endpoint → its `remotes` entry there |
| `go test ./cmd/fort` (the mirror tests) | `[cluster] datastore` ↔ `sfi.k3s.datastore`, `[cluster] audit_logging` ↔ `sfi.k3s.auditLog.enable` and `[links.<name>]` ↔ `sfi.links.<name>` in the host file — nix cannot read the manifest, so a test keeps each pair equal |
| `mint` | `mesh_ip` (wg key, peer), the endpoint; the deploy key always; the cluster key and canary for a `[cluster]` fort (a plain fort skips them and the k3s token). A layer's identities are the layer's mint to make ([layers](layers.md)) |
| `check`, a layer's verbs | `[dns] zone` (the app hostname's zone on a cloud fort); a layer reads its own file beside this one, `layers/<layer>/forts/<fort>.toml` |
| `provision`, `preview`, `adopt` | the provider table (which backend, its credentials; `[baremetal]` has none — refused, skipped, not adoptable) |
| the pulumi program | the provider table, `[dns]`, `[shape]`, `mesh_ip` (exported) |
| `dismantle` | removes the file — the declaration goes first; the rest is its realization |
| `destroy` | the gate: no manifest on `origin/main` and none in the working tree (there is no manifest left to read) |
| `check --all` (fleet half), the nix-eval job | the list of hosts; a plain fort → a host dir and no fleet dirs is the expected tree (a fleet dir anyway is an error) |

## History

Replaces `provisioning/inventory.yaml` (#252, 2026-09-15): one Ansible-
shaped map (`all.children.nixos.hosts`, `ansible_host`, `wg_vpn_ip`) that
served three vocabularies at once and whose per-host removal was a
line-based excision. The playbooks that read it left in #266; the fort CLI
and `ci/fleet-checks.py` were the only consumers by then.
