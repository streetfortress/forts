# Hardened, slim profile for internet-facing production nodes (#86).
# Imported by every flux fort's hosts/<name>/default.nix (`fort scaffold`
# renders it unconditionally for a cluster fort, #272; the estate's dev role
# is the other profile, and the two are mutually exclusive). The hardening itself — mesh-only SSH, UTC, the
# closure trims, root-only nix — is hardened.nix (#417), shared with
# the estate's role modules; this module is what a CLUSTER fort adds on top:
#   - No Home Manager payload: the only login account is the unprivileged
#     `deployer` (ops/deployer.nix — every fort's operator account,
#     #647). The framework declares no Home Manager user and does not
#     import Home Manager; an estate that wires it in owns what it
#     declares. (The payload is also the size lever for the 6 GB
#     uncompressed custom-image cap: lab images measured 2.8-4.3 GB with
#     a home stack.)
#   - The public/LAN interface serves exactly 80/443; the cluster and edge
#     ports re-open on the mesh interface only.
#   - deployer is a plain user: no wheel, so no path to root from SSH (the
#     carve-outs are the pinned NOPASSWD `sfi-*` ops verbs from
#     ops/ops-commands.nix — e.g. `sfi-nixos-rebuild`, which only re-runs
#     the pull loop against vetted main; `sfi-ops` lists them). Root
#     logins stay off (common.nix) and no account here has a password,
#     so SSH on the mesh is the only login path this profile makes. A
#     host that also imports ops/console.nix (#524) gives deployer a
#     password a GETTY accepts and sshd still refuses — the door to
#     `sudo sfi-nixos-rollback` when the mesh is the thing that broke.
#     Without that import the console offers only the GRUB menu that
#     precedes it (3 s, qcow-runtime.nix / upstream linode-config.nix,
#     #523), and then the offline paths: the provider's rescue boot, or
#     a tool that edits the disk offline.
{ config, lib, ... }:

{
  imports = [
    ./hardened.nix
    ./ops/deployer.nix # the operator account; this profile adds the cluster grant below
  ];

  # What a CLUSTER fort adds to the operator account (ops/deployer.nix
  # holds the account itself — isNormalUser, the journal, the keys): the
  # ops-ro kubeconfig (ops-kubeconfig.nix, #90), so deployer observes the
  # cluster as well as the host. Still no wheel and no nix daemon access:
  # writes go through git, and rebuilds arrive through the pull loop.
  users.users.deployer.extraGroups = [ "kubeops" ];

  # The public face of a cluster fort is exactly 80/443; hardened.nix
  # collapses every other merged opening to this list. The admin/cluster
  # ports below re-open on the mesh interface only (22 + 9100 are already
  # there from hardened.nix).
  sfi.hardened.publicTCPPorts = [ 80 443 ];
  # 30909/30993: edge Prometheus/Alertmanager NodePorts (mesh-services-
  # edge.yaml) — the wg0-only opening IS their access control; silences
  # are set against 30993.
  # A port a layer's workload answers on over the mesh (a database's
  # NodePort for a hub's reader) is the estate's to open, in the host file.
  networking.firewall.interfaces.${config.sfi.mesh.interface}.allowedTCPPorts = [ 6443 10255 30909 30993 ];

  # The in-cluster edge Prometheus (#53) scrapes the host node_exporter at
  # the mesh address; pod traffic to the host's own wg0 IP ingresses on
  # cni0, so 9100 must be open there too. The wg0 10255 row above and
  # hardened.nix's wg0 9100 are the hypervisor pull-model path — drop them
  # once the edge stack is verified on an edge cluster.
  networking.firewall.interfaces."cni0".allowedTCPPorts = [ 9100 ];
}
