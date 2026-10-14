# The hardening every fort shares (#417): mesh-only admin surface and
# the closure trims. Carries NO account: the operator account every fort
# shares is ops/deployer.nix (#647), and what imports this adds the rest
# — k3s-server.nix the cluster grant and the public/cluster ports (#154;
# production.nix until then), the estate's dev role the agent accounts
# and nothing public. Split out of production.nix so the shapes are the
# same lines, not drifting copies.
#
#   - SSH answers on the wireguard mesh only. The public/LAN interface
#     serves exactly `sfi.hardened.publicTCPPorts` (+ 51820/udp, which IS
#     the mesh transport, from wireguard.nix). Hardened hosts therefore
#     MUST be mesh members — without wg0 there is no login path at all.
#   - Exporters listen on the mesh: metrics.nix's node exporter opens
#     9100 on every interface; here that is switched off and re-opened on
#     the mesh interface only. (The exporter's openFirewall injects an
#     iptables rule outside allowedTCPPorts, so it must be disabled
#     explicitly.)
#   - Only root talks to the nix daemon by default. A profile whose
#     accounts build (the dev role's agent accounts) appends them;
#     deployer never builds.
{ config, lib, ... }:

let
  cfg = config.sfi.hardened;
in
{
  # sfi.mesh.interface is estate.nix's option: named here so a host that
  # imports this module alone evaluates, not only one that also imports
  # wireguard.nix.
  imports = [ ./estate.nix ];

  options.sfi.hardened.publicTCPPorts = lib.mkOption {
    type = lib.types.listOf lib.types.port;
    default = [ ];
    description = ''
      TCP ports served on the public/LAN interface. Everything else the
      merged module set opens (22 from common.nix / openssh openFirewall)
      is collapsed to this list. A plain fort serves nothing; k3s-server.nix
      sets the ingress pair and opens the cluster ports on the mesh
      interface instead.
    '';
  };

  config = {
    nix.settings.allowed-users = [ "root" ];

    networking.firewall.allowedTCPPorts = lib.mkForce cfg.publicTCPPorts;
    # The mesh interface by the estate's name (estate.nix): wg0 unless
    # estate.toml names it (deployments#674).
    networking.firewall.interfaces.${config.sfi.mesh.interface}.allowedTCPPorts = [ 22 9100 ];
    services.prometheus.exporters.node.openFirewall = lib.mkForce false;

    # Closure trims: manuals and the default perl/rsync/strace set have no
    # business on a node nobody develops on.
    documentation.enable = false;
    environment.defaultPackages = lib.mkForce [ ];
  };
}
