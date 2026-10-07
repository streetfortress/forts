# ssh-config and wg-conf — the client side of the mesh

`mesh/peers.json` says who is on the mesh and where; the manifests
say what each fort is. Everything a *client* needs to reach them used to
be hand-kept on top of that — `Host` aliases in `.files`, the account per
fort class, a `ProxyJump` for machines off the mesh, a host-key prompt on
every first login — and every raise added an alias by hand, every
dismantle left one behind (#183, #434). Two verbs render it instead:

| verb | kind | renders | from |
|---|---|---|---|
| `fort ssh-config [--write]` | author | `mesh/ssh_config`, `mesh/known_hosts` | peers.json + manifests (+ each fort's sops file, for its host key) |
| `fort wg-conf <member>` | read | a member's own mesh interface on stdout | peers.json + estate.toml (the interface's name) |

`fort mint` and `fort dismantle` keep the two files current as they add
or remove a peer, so `ssh-config` itself is the whole-fleet render: after
a hand edit to peers.json, to backfill, or to read what the tree says.
`fort check --all` fails on a stale `ssh_config` and warns on a fort
without a key line.

## `mesh/ssh_config`

One `Host` block per mesh peer, in name order. Roaming clients (the
`clients` table) are not ssh targets and are left out.

```
# plain fort, libvirt — the sfi-dev role
Host sfi-dev sfi-dev-831c637
  Hostname 10.1.0.61
  User deployer
  HostKeyAlias sfi-dev-831c637
```

**The account is the fort's, not a field.** Every fort answers as
`deployer`, the operator account the framework gives each host
(`ops/deployer.nix`, #647), and a peer with no manifest is a hand-managed
interactive host — `steven`'s: the ubuntu holdout soundship (no NixOS host
carries an interactive profile any more). That answers the open question
of how the interactive hosts get an entry: being in peers.json without a
manifest *is* the declaration. peers.json stays three fields per peer, plus
an endpoint port where it is not the default ([mesh.md](mesh.md)).

**Other accounts on a fort are the estate's.** The manifest names no
user, so this file has no block for one. A client that dials an account
the estate added (an agent's, say) keeps its own `Host` block for it above
the `Include`, with `HostKeyAlias <fort>` so it trusts the one host key
the fort's `known_hosts` line carries.

**Role aliases** follow the naming convention (2026-09-12): a fort named
`<role>-<7-hex sha>` has the stem as its role, and the alias exists while
exactly one fort carries that stem — `sfi-dev` → the current dev fort,
`sfi-prod`, `sfi-stage`. So `ssh sfi-dev pickup …` survives the dev fort
being replaced, which it is by design; and a replacement raised before
the old one is dismantled suspends the alias rather than pointing it at
either. `sfi-fort-zenbook` and `luxor` are outside the convention and get
none. (The issue said `sfi-staging`; the stem is mechanical, and the
forts are named `sfi-stage-*`.)

**Port** is 22 everywhere on the mesh; gitea's 30022 is not a fort and
not here.

**`HostKeyAlias <fort>`** makes ssh look the host key up by the fort's
name whichever alias or address was typed. A fort's successor
reuses a mesh address with a new key: keyed by address that is "REMOTE
HOST IDENTIFICATION HAS CHANGED"; keyed by name it is a new host.

## `mesh/known_hosts`

One line per fort, `<fort>,<mesh ip> ssh-ed25519 …`, so the first login
is prompt-free — the mirror of what `auto-rebuild.nix` does for gitea's key.
The key is the fort's `ssh-host-key.pub`, which lives sops-encrypted
with the rest of its secrets in `nixos/secrets/<host>.yaml` (the whole
file is encrypted; there is no plaintext copy anywhere). Hence who
writes the line:

- **`fort mint --write`** has the key in hand — minted, or restored from
  the file — and writes the line with the peer. A line that names the
  host under another key is refused the way a foreign peer entry is.
- **`fort ssh-config --write`** decrypts each fort's file for the one
  public key. Without the admin age key (or `sops` on PATH) it keeps the
  committed line and says so; a fort with no line and no readable key is
  reported, not invented.
- **`fort dismantle`** removes the line.

Hand-managed peers have no manifest and no key in the repo; their host
keys stay trust-on-first-use, as today.

## Using it

From `~/.ssh/config`, after anything of your own — the first value ssh
finds wins, so local overrides go above the `Include`:

```
Include ~/deployments/mesh/ssh_config
UserKnownHostsFile ~/.ssh/known_hosts ~/deployments/mesh/known_hosts
```

`~/deployments` is the checkout; macmini has one. New
keys ssh learns are appended to the first `UserKnownHostsFile`; the
rendered one is read-only in practice.

## `fort wg-conf <member>` — a Mac joins the mesh

macOS gets its WireGuard config through the app's UI, not a file, and the
app generates the keypair itself. The verb renders everything *except*
the key, from peers.json:

```
[Interface]
Address = 10.1.0.7/24
ListenPort = 51820
# no DNS: the names of the mesh (<host>.<zone>, docs/mesh.md) are not served to
# a roaming client yet; .lofi is the lab LAN's zone, resolved on the LAN

[Peer]
# luxor
PublicKey = …
AllowedIPs = 10.1.0.5/32
Endpoint = 10.0.0.5:51820
PersistentKeepalive = 25
…
```

Mirrors `wireguard.nix` exactly: every other peer with keepalive and,
where it has one, an `Endpoint`; every roaming client with neither; every
linked host of another estate (the `remotes` table, [mesh.md](mesh.md))
with its endpoint and port. A
member in the `clients` table gets no `ListenPort` (nobody dials it) and a
note on the peers it cannot reach without a hand-set endpoint (soundship
through the router's DDNS forward, which is not in peers.json).

The join, for an always-on Mac on the lab LAN (macmini):

1. In the WireGuard app: *Add Empty Tunnel*. It shows a fresh
   `PrivateKey` and the matching public key. The private key never
   leaves the app; this CLI never handles one.
2. Register the peer by hand — it has no manifest — per
   [`mesh.md`](mesh.md#joining): `.peers.macmini`
   with the next free lab address, `wg_endpoint_ip` = its LAN address (an
   always-on LAN host is a peer the others can dial, not a roaming
   client), and the app's public key. `fort ssh-config --write` in the
   same change gives it a `Host macmini` block as `steven`.
3. `fort wg-conf macmini` → paste everything below the app's `PrivateKey`
   line into the tunnel. Save, activate.
4. PR, merge. The forts pick the peer up on their next auto-rebuild; the
   ubuntu members (soundship) still need the `.files` mirror and
   `just wireguard` until they retire (#183).
5. Re-run `fort wg-conf macmini` after any peers.json change and paste
   again — the app has no other way to learn it. The forts' side of the
   same fact rolls out by itself.

## What `fort check --all` says

| finding | level | fix |
|---|---|---|
| `ssh_config` missing or not what the tree renders | error | `fort ssh-config --write` |
| a fort on the mesh with no `known_hosts` line | warning | `fort ssh-config --write` with the admin age key, or `fort mint <host> --write` |
| a line keyed to an address peers.json does not give that fort | error | `fort ssh-config --write` |
| a line for a host with no manifest | error | dismantle leaves none; `fort ssh-config --write` prunes it |

The key line is a warning and not an error because verifying it needs the
age key CI does not hold, and every fort minted before this file existed
starts without one. The backfill is one `fort ssh-config --write` on a
machine with the key.
