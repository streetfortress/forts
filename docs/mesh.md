# The mesh — `mesh/peers.json`

Every fort is a WireGuard mesh member, and `mesh/peers.json` is the one
file that says who is on the estate's mesh and where. `wireguard.nix`
builds each fort's mesh interface from it; `fort mint` adds a fort, `fort dismantle`
removes one; [`ssh-config`](ssh-config.md) and `wg-conf` render the client
side from it; `fort check --all` keeps it honest.

## The subnet

An estate's mesh is one IPv4 /24, declared in `estate.toml`:

```toml
mesh = "10.2.0.0/24"
```

Every `mesh_ip` in a manifest and every address in peers.json lies in it,
and `fort check --all` says so (`estate.toml   mesh 10.2.0.0/24 — 3
address(es) inside it`; an address outside is an error). Two estates
link only when their meshes do not overlap, so the subnet is the first
thing a new estate chooses: `fort init --mesh <cidr>`. The key is
optional for an estate created before it existed; such an estate is checked
by nothing, as before. The convention in the lab is that a fort's last
octet follows its LAN address.

## The interface

One name per mesh, on every host that is on it:

```toml
interface = "wg-second"
```

A fort of the estate gives its own mesh that interface, and a host of
another estate that links here gives its link interface the same name
([links.md](links.md)), so `wg show wg-second` means the same thing on
both. At most 15 characters from `A-Za-z0-9_=+.-`, the kernel's rule;
`fort init --interface <name>` renders it. The modules read it through
`sfi.mesh.interface` (estate.nix): `wireguard.nix` builds the interface,
hardened.nix and k3s-server.nix open the admin ports on it, the metrics
carry it as the `interface` label. An estate that names none is on
`wg0`, as before; renaming a live estate's interface is a switch that
stops one unit and starts another on every fort, so it is a decision per
estate, not a default.

## The zone

Every host on the mesh has a name, and the name says which mesh (forts#45):

```toml
zone = "second-estate.mesh"   # absent: <name>.mesh, the same thing here
```

Every `peers` entry and every `remotes` entry is `<host>.<zone>` at its
`wg_vpn_ip`, on every fort of the estate — in `/etc/hosts`, so the
rebuild's fetch and flux's clone work with every DNS down, and in the
cluster DNS (the `coredns-custom` add-on), where `<service>.<host>.<zone>`
answers the same address, the convention a provider's service hostnames
assume. Both are rendered from peers.json by `mesh-dns.nix` at build time:
a host that moves changes its entry, and every fort rebuilds. No resolver
is on a fort's path. The default derives from the estate's name, so
`luxor.staging.mesh` is luxor as sfi/staging reaches it and
`luxor.infra.mesh` is luxor on its own mesh; `.mesh` is the fleet's one
private TLD, and `fort init --zone <name>` overrides the default. A
`clients` entry gets no name: a roaming device is dialed by nobody.

An app on a lab fort is named `<app>.<fort>.<zone>`. A host
of another estate that links here serves this zone on itself, from its
cached copy of peers.json ([links.md](links.md)).

Two names are not of the zone. The forge keeps its one name on every mesh
(`gitea.zen.lofi`: it is in every git remote, every OCI reference and the
registry certificate, and one gitea is on several meshes), pinned to its
address on this mesh by `sfi.lab-dns.giteaAddress`. That address is
derived, not written (forts#26): `estate.toml` names the forge host in
`[hubs.forge]` ([hub.md](hub.md)), and this file gives that host's
address — its `peers` entry when the forge is a fort of this estate, its
`remotes` entry when it links here. An estate that names no forge host
gets no pin, and each of its forts resolves the name by whatever other
DNS it has. And `.lofi` is the homelab LAN's zone,
not a fort name: a lab fort's cluster DNS forwards it to the LAN
resolvers its static LAN block names; a cloud fort forwards nothing and
knows no `.lofi` name but the forge's. A roaming client is not served the
zone yet; that is a later `dns` role, and `fort wg-conf` says so.

## The three tables

```json
{
  "peers": {
    "sfi-second-4f7aabf": {"wg_vpn_ip": "10.2.0.70", "wg_endpoint_ip": "10.0.0.70", "public_key": "…"}
  },
  "clients": {
    "macmini": {"wg_vpn_ip": "10.2.0.96", "public_key": "…"}
  },
  "remotes": {
    "sfi-fort-zenbook": {"wg_vpn_ip": "10.2.0.4", "wg_endpoint_ip": "10.0.0.4", "wg_endpoint_port": 51821, "public_key": "…"}
  }
}
```

| table | who | dialed? | keepalive | ssh_config block | written by |
|---|---|---|---|---|---|
| `peers` | the forts of this estate, and any hand-managed member | where it has `wg_endpoint_ip` | both ways | yes | `fort mint` / `fort dismantle`; a hand-managed member by hand |
| `clients` | roaming devices (a laptop) | never | none | no | by hand, with the device's own key |
| `remotes` | hosts of **other** estates that link to this mesh — a hub this estate names in `[hubs.<role>]` ([hub.md](hub.md)) | always: `wg_endpoint_ip` is required | both ways | no | `fort estate link <that estate's url>`, from the `[links.<name>]` table in that host's manifest, after it verifies the claimed address here ([links.md](links.md)) |

`peers` and `remotes` are named in the zone (`<host>.<zone>`, above);
`clients` are not.

Four fields, and no more: `wg_vpn_ip` (the address on this mesh),
`wg_endpoint_ip` (where the others dial it: a LAN address for an
always-on lab host, a public name for a cloud fort, absent for a device
behind NAT), `wg_endpoint_port` (only when it is not 51820), and
`public_key`. Private keys are never here: a fort's is in
`nixos/secrets/<host>.yaml`, a client's in its app.

A hand-managed peer (no manifest) can carry `ssh_user`: the account
`mesh/ssh_config` reaches it on ([ssh-config.md](ssh-config.md)). A fort
carries none and answers as `deployer`.

## Linking: a host on another estate's mesh (deployments#674)

A fort is on one mesh, its estate's. Some hosts serve more than one
estate — the forge every estate clones from, a hub a layer serves — and
those **link** to the other estate: one more WireGuard interface on that
host, with its own key, its own listen port and the address the other
estate allocated to it. Linking a host to an estate puts it on that
estate's mesh as if it were a fort there: every fort of that estate
peers with it, and it peers with every one of them.

The two sides of a link:

- **In the estate being linked to**, the host is a `remotes` entry:
  its address on this mesh, its endpoint, the port its link interface
  listens on, the public key of that interface. `fort estate link`
  writes it from the other estate's manifests, after it has verified
  that nothing here holds the address. Every fort here dials it on its
  next rebuild; `fort wg-conf` renders it for a client. It is not a
  fort of this estate: no manifest, no ssh block, no known_hosts line,
  and `fort check` does not expect one.
- **On the linked host**, the link is a `[links.<name>]` table in its
  own estate's manifest — the other estate's repo, the address it
  claims, the port, the interface and zone, the public key — written by
  `fort link add`, rendered into the host file by `fort scaffold`, and
  a reconciler that reads the other estate's peers.json and keeps the
  interface's peer set equal to it. That half is the `links` module
  ([links.md](links.md)).

The endpoint port is the field a link needs: the linked host already
listens on 51820 for its own mesh, so its link interface listens beside
it (51821, 51822, …), and this estate's entry says which.

A hub this estate names in `estate.toml` (`[hubs.<role>]`;
[hub.md](hub.md)) and that is another estate's fort is a `remotes` entry
here, and that entry is its address on this mesh, named `<host>.<zone>`.
A layer that needs the address in a file reads it from this entry. The
address is written once, in the PR that linked the host.

## Joining

**A fort:** `forts/<host>.toml` with `mesh_ip`, then `fort mint <host>
--write`. The key is minted, sops-encrypted into the fort's secrets file,
and the `peers` entry written with the endpoint the manifest implies
(`mesh_endpoint`, else `[libvirt] ip`, else the `[dns]` name).

**A roaming device:** a `clients` entry by hand, with the key the
WireGuard app generated; `fort wg-conf <name>` renders the tunnel to
paste ([ssh-config.md](ssh-config.md#fort-wg-conf-member--a-mac-joins-the-mesh)).

**A hand-managed member** (no manifest): a `peers` entry by hand, the
key kept wherever that host keeps keys. `ssh_config` gives it a block
as an interactive host.

**A host of another estate:** a `remotes` entry by hand, by PR, once
that host's link interface exists and has a key.

Every change rolls out by itself: the forts pull main and rebuild
(`auto-rebuild`); a client re-runs `fort wg-conf` and pastes.

## What `fort check --all` says

| finding | level |
|---|---|
| `estate.toml   mesh <cidr> — N address(es) inside it` | ok |
| `estate.toml   zone <zone> — N name(s) from mesh/peers.json` | ok |
| a host file that sets a `.lofi` name by hand (`networking.hosts`, `extraHosts`), or routes `~lofi` over the mesh (`wg-quick` `dns`) | warning, per line — the zone renders the names; the forge's address derives from `[hubs.forge]` |
| `estate.toml   forge <host> at <ip> (<table>.<host> in mesh/peers.json)` | ok |
| `estate.toml` names no `[hubs.forge]` | warning — no fort pins the forge's name |
| `[hubs.forge]` names a host that neither `peers` nor `remotes` carries | error — `modules/lab-dns.nix` refuses to evaluate; the finding names the entry to add |
| a host file that sets `sfi.lab-dns.giteaAddress` | warning, per line — the address derives, and a hand-set line does not follow the mesh |
| an address in any table outside the mesh; a manifest's `mesh_ip` outside it | error |
| a `remotes` entry with no `wg_endpoint_ip` | error (the file does not parse) |
| `forts   N link(s) — tables and host files agree, public keys match the sops secrets` (or `not verified (no age key here)`) | ok ([links.md](links.md)) |
| a `[links.<name>]` whose estate is this one, or whose interface or zone is this mesh's; a host file with no `sfi.links.<name>` for a table; a `public_key` the sops secret does not derive, or a secret that is gone | error |
| a fort that serves an estate a role with no `[links.<estate>]` in its manifest | warning ([hub.md](hub.md)) |
| `ssh_config` / `known_hosts` stale or missing | error ([ssh-config.md](ssh-config.md#what-fort-check---all-says)) |
