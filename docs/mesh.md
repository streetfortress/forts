# The mesh — `mesh/peers.json`

Every fort is a WireGuard mesh member, and `mesh/peers.json` is the one
file that says who is on the estate's mesh and where. `wireguard.nix`
builds each fort's `wg0` from it; `fort mint` adds a fort, `fort dismantle`
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
optional for an estate born before it existed; such an estate is checked
by nothing, as before. The convention in the lab is that a fort's last
octet follows its LAN address.

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
| `remotes` | hosts of **other** estates that link to this mesh | always: `wg_endpoint_ip` is required | both ways | no | by hand, by PR, when the other estate's host links here |

Four fields, and no more: `wg_vpn_ip` (the address on this mesh),
`wg_endpoint_ip` (where the others dial it: a LAN address for an
always-on lab host, a public name for a cloud fort, absent for a device
behind NAT), `wg_endpoint_port` (only when it is not 51820), and
`public_key`. Private keys are never here: a fort's is in
`nixos/secrets/<host>.yaml`, a client's in its app.

## Linking: a host on another estate's mesh (deployments#674)

A fort is on one mesh, its estate's. Some hosts serve more than one
estate — the forge every estate clones from, the monitoring hub — and
those **link** to the other estate: one more WireGuard interface on that
host, with its own key, its own listen port and the address the other
estate allocated to it. Linking a host to an estate puts it on that
estate's mesh as if it were a fort there: every fort of that estate
peers with it, and it peers with every one of them.

The two sides of a link:

- **In the estate being linked to**, the host is a `remotes` entry:
  its address on this mesh, its endpoint, the port its link interface
  listens on, the public key of that interface. Every fort here dials
  it on its next rebuild; `fort wg-conf` renders it for a client. It is
  not a fort of this estate: no manifest, no ssh block, no known_hosts
  line, and `fort check` does not expect one.
- **On the linked host**, the link is a declaration in its own estate's
  host file — the other estate's repo, the interface, the port, the
  address — and a reconciler that reads the other estate's peers.json
  and keeps the interface's peer set equal to it. That half is the
  `links` module; its page follows when it lands.

The endpoint port is the field a link needs: the linked host already
listens on 51820 for its own mesh, so its link interface listens beside
it (51821, 51822, …), and this estate's entry says which.

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
| an address in any table outside the mesh; a manifest's `mesh_ip` outside it | error |
| a `remotes` entry with no `wg_endpoint_ip` | error (the file does not parse) |
| `ssh_config` / `known_hosts` stale or missing | error ([ssh-config.md](ssh-config.md#what-fort-check---all-says)) |
