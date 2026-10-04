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
   checked; a link cannot be `wg0`, this host's own mesh.)
3. Find this host's own key in that file's `remotes`. No entry: the
   other estate has not added the link yet — exit 1, apply nothing. An
   entry whose address or port differs from the link's — exit 1, apply
   nothing: fix one side, never silently.
4. `wg set <interface> peer …` for every peer (endpoint where stable,
   keepalive), every roaming client (neither), every other linked host
   (endpoint and port, keepalive): exactly what a fort of that estate
   carries. Then remove every peer on the interface the file no longer
   names.
5. Write `wg-link-<name>.prom` for node_exporter:
   `sfi_link_fetch_success`, `sfi_link_last_run_seconds`,
   `sfi_link_peers`.

`sudo sfi-systemd-restart wg-link-<name>` runs it now; `wg-quick-<interface>`
is restartable the same way.

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
link's is the name the other estate gives its mesh. One UDP port per
link. Two links cannot share either, and neither can be this host's own
mesh interface or port. Every estate's mesh is a /24 that no other
estate's overlaps ([mesh.md](mesh.md), the `mesh` key). All of it is an
eval-time assertion, a `fort check` finding, or the reconciler's refusal.
