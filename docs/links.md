# Links — a host on another estate's mesh

A fort is on one mesh, its estate's ([mesh.md](mesh.md)). Some hosts serve
more than one estate — the forge every estate clones from, a monitoring
hub — and those **link** to the other estate. Linking a host to an estate
puts it on that estate's mesh as if it were a fort there: it peers with
every fort of that estate, and every one of them peers with it. An
allowlist (a hub-to-hub link) is not a thing yet; decided in
sfi/deployments#674.

## The two halves

**On the linked host**, `sfi.links.<name>` in its host file
(`modules/links.nix`): the other estate's repo, the interface, the
listen port, the address that estate allocated, and the TCP ports its
forts may reach here.

```nix
imports = [ inputs.forts.nixosModules.links ];

sfi.links.second-estate = {
  repo = "https://gitea.zen.lofi/sfi/second-estate.git";  # public: no credential
  interface = "wg-second";   # the name sfi/second-estate's estate.toml gives its mesh (checked)
  zone = "second-estate.mesh";  # its zone, <estate>.mesh unless its estate.toml says (checked); this is the default
  port = 51821;              # never 51820 (this host's own mesh port)
  address = "10.2.0.4";      # what sfi/second-estate's remotes entry says
  openTCPPorts = [ 30022 443 ];  # gitea; nothing else answers on this interface
};
```

The module renders one WireGuard interface with its own key and **no
peers**, opens the port, opens `openTCPPorts` on that interface and
nothing else (no ssh: hardened.nix opens 22 on this host's own mesh
interface only), and inserts a
`DROP` in `FORWARD` for every pair of mesh interfaces, so a hub never
bridges two meshes whatever k3s does to `ip_forward`. The key is the
sops secret `wireguard-private-key-<name>` in the host's secrets file,
set by hand (below).

**In the estate being linked to**, the host is a `remotes` entry in
`mesh/peers.json`: its address there, its endpoint, the link's port, the
public half of the link's key. Written by PR. Every fort of that estate
dials it on its next rebuild.

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
   carries. Then remove every peer on the interface the file no longer
   names.
5. Render the zone (below) into `/var/lib/wg-link-<name>/zone.db`: every
   peer and remote of that estate as `<host>` and `*.<host>` at its
   address on that mesh, the shape `mesh-dns.nix` gives a fort's own
   zone. The serial is the run's time; an unchanged zone is not
   rewritten.
6. Write `wg-link-<name>.prom` for node_exporter:
   `sfi_link_fetch_success`, `sfi_link_last_run_seconds`,
   `sfi_link_peers`.

`sudo sfi-systemd-restart wg-link-<name>` runs it now; `wg-quick-<interface>`
and `coredns` are restartable the same way.

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

## Minting the link's key

`fort mint` mints a fort's own identity and nothing for a link; the
link's key is the estate's, set by hand into the host's secrets file:

```sh
# on a machine with the admin age key, in the linked host's estate
cd nixos
PRIV=$(wg genkey)
sops --config .sops.yaml set secrets/<host>.yaml '["wireguard-private-key-<name>"]' "\"$PRIV\""
echo "$PRIV" | wg pubkey        # → the other estate's remotes entry
unset PRIV
```

Then the host file (above), PR, merge; the host rebuilds and the
interface comes up with no peers. Then the other estate's PR with the
`remotes` entry; the reconciler's next run finds its own key there and
fills the peer set. Order matters only in that the reconciler refuses
until both halves agree.

## Limits

An interface name is at most 15 characters from `A-Za-z0-9_=+.-`, and a
link's is the name the other estate gives its mesh; a link's zone is the
one the other estate gives its mesh (`zone`, else `<name>.mesh`). One UDP
port per link. Two links cannot share any of the three, and none can be
this host's own mesh interface, port or zone. Every estate's mesh is a /24 that no other
estate's overlaps ([mesh.md](mesh.md), the `mesh` key). All of it is an
eval-time assertion, a `fort check` finding, or the reconciler's refusal.
