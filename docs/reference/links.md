# Links — a host on another estate's mesh

A fort is on one mesh, its estate's ([mesh.md](mesh.md)). Some hosts serve
more than one estate — the forge every estate clones from, a hub a layer
serves — and those **link** to the other estate. Linking a host to an estate
puts it on that estate's mesh as if it were a fort there: it peers with
every fort of that estate, and every one of them peers with it. An
allowlist (a hub-to-hub link) is not a thing yet; decided in
sfi/deployments#674.

## The two halves

**On the linked host**, the link is a `[links.<name>]` table in its
manifest, `forts/<host>.toml` ([manifest.md](manifest.md)) — named after
the other estate (the last label of its URL), one table per estate:

```toml
[links.staging]
estate = "ssh://git@gitea.zen.lofi:30022/sfi/staging"
address = "10.2.0.254"           # this host's address on staging's mesh — a claim, until staging records it
port = 51821                     # the link's UDP listen port; never 51820 (this host's own mesh)
interface = "wg-staging"         # what staging's estate.toml names its mesh (checked by the reconciler)
zone = "staging.mesh"            # its zone (checked the same way)
public_key = "…"                 # the public half of the sops secret wireguard-private-key-staging
```

`fort link add` writes it ([below](#establishing-a-link)); `fort scaffold`
renders `sfi.links.<name>` in the host file from it (`modules/links.nix`),
and appends the block to a host file that exists — nobody writes the
block by hand, and a mirror test keeps the two equal:

```nix
imports = [ inputs.forts.nixosModules.links ];

sfi.links.staging = {
  repo = "ssh://git@gitea.zen.lofi:30022/sfi/staging.git";
  interface = "wg-staging";
  zone = "staging.mesh";
  port = 51821;
  address = "10.2.0.254";
};
```

A link is the network half of a hub relationship ([hub.md](hub.md)),
and `roles` says which: `forge` opens 30022 and 443 (gitea); any other
role opens the `ports` its `[[serves.<role>]]` entry declares (forts#105;
[hub.md](hub.md)). `openTCPPorts` derives from the roles and can
still be set by hand. `roles` is not in the manifest and not in the
rendered block: it defaults to the roles whose `[[serves.<role>]]`
entries in this estate's `estate.toml` name this host and an estate
whose URL's last label is the link's name (`ssh://…/sfi/staging` for
`sfi.links.staging`), and `repo` would default the same way. `fort link
add` says which roles the link gets, and warns when none does;
`fort check --all` warns when a host that serves an estate a role has no
link to it.

The module renders one WireGuard interface with its own key and **no
peers**, opens the port, opens the roles' ports on that interface and
nothing else (no ssh: hardened.nix opens 22 on this host's own mesh
interface only), and inserts a
`DROP` in `FORWARD` for every pair of mesh interfaces, so a hub never
bridges two meshes whatever k3s does to `ip_forward`. The key is the
sops secret `wireguard-private-key-<name>` in the host's secrets file,
minted by `fort link add`.

**In the estate being linked to**, the host is a `remotes` entry in
`mesh/peers.json`: its address there, its endpoint, the link's port, the
public half of the link's key. `fort estate link` writes it from a fetch
of the linking estate's manifests ([below](#establishing-a-link)), after
it has verified the claimed address against this file. Every fort of
that estate dials it on its next rebuild.

## The reconciler

`wg-link-<name>.timer` (30 s after boot, then every `interval`, 5 min)
runs `links/reconcile.sh`:

1. Clone the other estate's repo, shallow, at `ref` (main). Read
   `mesh/peers.json`. On success, cache it under
   `/var/lib/wg-link-<name>/`; on failure, use the cache and say so. A
   boot with the forge down still brings the peers up.
2. One name per mesh: when that estate's `estate.toml` names its
   interface, this link must be called the same — exit 1 otherwise,
   apply nothing. (An estate on the default `wg0` names none and is not
   checked; a link cannot be `wg0`, this host's own mesh.) The same for
   the zone: that estate's `zone`, else `<name>.mesh`, must be this
   link's `zone`.
3. Find this host's own key in that file's `remotes`. No entry: the
   other estate has not added the link yet — exit 1, apply nothing. An
   entry whose address or port differs from the link's — exit 1, apply
   nothing: fix one side, never silently.
4. `wg set <interface> peer …` for every peer (endpoint where stable,
   keepalive), every roaming client (neither), every other linked host
   (endpoint and port, keepalive): exactly what a fort of that estate
   carries. An endpoint name that does not resolve — a minted fort
   before its provision — does not abort the run: the peer is added
   without the endpoint (one line on stderr says so), the fort dials
   this host first, and the next run adds the endpoint once the name
   resolves. Then remove every peer on the interface the file no
   longer names.
5. Render the zone (below) into `/var/lib/wg-link-<name>/zone.db`: every
   peer and remote of that estate as `<host>` and `*.<host>` at its
   address on that mesh, the shape `mesh-dns.nix` gives a fort's own
   zone. The serial is the run's time; an unchanged zone is not
   rewritten.
6. Write `wg-link-<name>.prom` for node_exporter:
   `sfi_link_fetch_success`, `sfi_link_last_run_seconds`,
   `sfi_link_peers`, `sfi_link_peers_without_endpoint`.

`systemctl restart wg-link-<name>` runs it now, from the admin rung
([ops.md](ops.md)); an estate that wants a pinned verb for it declares
one through `sfi.ops.commands`, as that page shows.

## The zone of a linked estate

A fort renders its own mesh's names at build time, from its own
peers.json ([mesh.md](mesh.md), "The zone"). A linked host learns the
other estate's peers at run time, so its names are served from the
reconciler's zone file instead: the module runs one CoreDNS on the host
(`services.coredns`) with a server block per link, `file
/var/lib/wg-link-<name>/zone.db`, reloaded when the serial changes. It
binds loopback and this host's own mesh address, after this host's own
mesh is up. Until the first reconcile the file is a SOA alone, so the
server starts.

Two consumers, one zone each:

- **the host**: `wg-quick-<interface>` routes the zone, and only the
  zone, to the server on loopback — `resolvectl dns <interface>
  127.0.0.1`, `domain <interface> ~<zone>`, `default-route <interface>
  false` on post-up. Explicit, not wg-quick's `dns` setting, whose
  exclusive mode would route the host's every query there. A host
  without systemd-resolved routes nothing.
- **the pods**: the host's `coredns-custom` ConfigMap gains a `<zone>:53`
  block that forwards to the server at the host's own mesh address
  (`sfi.mesh.coredns`, rendered by `mesh-dns.nix`); the firewall opens
  53 on `cni0` and on no mesh interface — the other estate's forts
  resolve their own names.

`luxor.staging.mesh` therefore means the same address on a staging fort
and on the hub that serves staging, and the hub's own names
(`luxor.infra.mesh`) cannot collide with them. A link's zone cannot be
this host's own, and two links cannot share one.

## A private estate

The clone needs a read credential: a **read-only deploy key on that
repo**, registered by that estate's admin. gitea lets one key be a
deploy key on several repos, so the default `sshKeyFile` is this host's
own flux deploy key (`flux-git-deploy-key`): give its public half to the
other estate's admin, who registers it, and set `repo` to the ssh URL.
`knownHosts` defaults to the auto-rebuild module's pin of the forge.

## Establishing a link

Two verbs, one per estate, one PR each (forts#55). The link's facts live
in the linking estate's manifest, where the other estate can read them
from a fetch without this estate's age key and without evaluating its
Nix; the address written there is a **claim**, and the other estate's
`mesh/peers.json` is the **record**.

**1. In the linking estate** — the hub's, where the admin age key is:

```sh
fort link add luxor ssh://git@gitea.zen.lofi:30022/sfi/staging
```

The verb fetches staging's repo (a shallow clone with the operator's own
git access) and reads its `estate.toml` — the mesh, the interface, the
zone — and its `mesh/peers.json` and manifests for the addresses taken:
every peer, client and remote, every fort's `mesh_ip` whether minted
yet or not, and every other host of this estate that already claims an
address there. It picks the **highest free address, from .254 down**
(forts choose `mesh_ip` by hand and those numbers run low, so the two
allocators do not meet) and the lowest free UDP port beside 51820
(`--address` and `--port` override, and are checked the same way),
mints the WireGuard key, and writes — all in this estate:

```
○ nixos/secrets/luxor.yaml       + wireguard-private-key-staging   (sops set; the private half)
○ forts/luxor.toml               + [links.staging]                 (the public half, plaintext)
○ nixos/hosts/luxor/default.nix  + sfi.links.staging               (the block scaffold renders; the module imported)
```

Then PR, merge; the host rebuilds and the interface comes up with no
peers. The verb is re-runnable: the key is restored from the secrets
file when it is there (rotation is a deliberate act, as in `fort mint`),
an address that is still free is kept, and one that something has taken
since is picked again — the report says whose it is now. A table a
hand spelled is left as it is: only a key whose value moved is
rewritten, in place, and the edited file is parsed before it is
written. A hand-written `sfi.links.<name>` block is kept, and the
report says when it is not the block the table renders. The report
also says what staging's `remotes` entry for the host says, if anything.

**2. In the estate being linked to** — staging, after that merge:

```sh
fort estate link ssh://git@gitea.zen.lofi:30022/sfi/infra
```

The verb fetches infra's repo and reads every `forts/*.toml` for a
`[links.<name>]` table whose `estate` is this one — matched as a repo
(forge host, owner, name), so `https://` and `ssh://` spellings agree —
and builds the `remotes` entry from each: the claimed address, the host's
endpoint (its manifest's default: `mesh_endpoint`, else the lab address,
else the `[dns]` name), the link's port, the public key. **It verifies
the claim before it writes**: the address lies in this mesh; no peer,
client, other remote or not-yet-minted manifest holds it; two hosts of
infra did not pick the same one; the link's interface and zone are this
mesh's names. What crosses the boundary is content for root-level
config, so its shape is checked too (forts#99, #100): the endpoint is
one IP address or one DNS name, and a manifest's file name is a host
name — one DNS label — or the manifest is reported as not parsing
there and never claimed. One refusal refuses the run and names the
holder — the fix is `fort link add` again on the other side, which
picks another address.
An entry already there with the same facts is current; the same host
with other facts follows the manifest, and the report says what moved (a
changed key is a hub that re-minted, said loudly). Then:

```
○ mesh/peers.json                remotes.luxor
```

PR, merge; every fort of staging dials luxor on its next rebuild, and
luxor's reconciler finds its own key in the file on its next run and
fills the peer set. Order matters only in that the reconciler refuses
until both halves agree. Nothing here removes an entry: a host that
stopped linking is a hand edit, and the report names the candidates
(a `remotes` entry named like a fort of infra that no longer links here).

**The gate.** `fort check --all` in the linking estate judges every
table: the link's estate is not this one and its interface and zone are
not this mesh's; the host file sets `sfi.links.<name>`; and `public_key`
is what the sops secret derives — where the secrets file decrypts. CI
holds no age key, so there the key is reported as not verified, a skip,
never a failure. In the estate linked to, the subnet check covers the
`remotes` entry like any address ([mesh.md](mesh.md)).

## Limits

An interface name is at most 15 characters from `A-Za-z0-9_=+.-`, and a
link's is the name the other estate gives its mesh; a link's zone is the
one the other estate gives its mesh (`zone`, else `<name>.mesh`). One UDP
port per link. Two links cannot share any of the three, and none can be
this host's own mesh interface, port or zone. Every estate's mesh is a subnet that no other
estate's overlaps ([mesh.md](mesh.md), the `[mesh] cidr` key). All of it is an
eval-time assertion, a `fort check` finding, or the reconciler's refusal.
