# The hardening every fort profile shares (#417): mesh-only admin surface,
# a server clock, and the closure trims. Carries NO account: the operator
# account every fort shares is ops/deployer.nix (#647), and the profiles
# add the rest — production.nix the cluster grant and the public/cluster
# ports, the estate's dev role the agent accounts and nothing public. Split out of
# production.nix so the two profiles are the same lines, not drifting
# copies.
#
#   - SSH answers on the wireguard mesh only. The public/LAN interface
#     serves exactly `sfi.hardened.publicTCPPorts` (+ 51820/udp, which IS
#     the mesh transport, from wireguard.nix). Hardened hosts therefore
#     MUST be mesh members — without wg0 there is no login path at all.
#   - Exporters listen on the mesh: lab-host.nix's node exporter opens
#     9100 on every interface; here that is switched off and re-opened on
#     wg0 only. (The exporter's openFirewall injects an iptables rule
#     outside allowedTCPPorts, so it must be disabled explicitly.)
#   - Only root talks to the nix daemon by default. A profile whose
#     accounts build (the dev role's agent accounts) appends them;
#     deployer never builds.
{ config, lib, ... }:

let
  cfg = config.sfi.hardened;
in
{
  options.sfi.hardened.publicTCPPorts = lib.mkOption {
    type = lib.types.listOf lib.types.port;
    default = [ ];
    description = ''
      TCP ports served on the public/LAN interface. Everything else the
      merged module set opens (22 from common.nix / openssh openFirewall,
      6443 + 80 + 443 from k3s-server.nix) is collapsed to this list and
      re-opened, where wanted, on wg0 by the profile.
    '';
  };

  config = {
    # Servers run UTC (common.nix's America/Los_Angeles is the interactive-
    # host leftover): journald, timer calendars, and dashboard timestamps
    # stop needing a 7-hour mental offset.
    time.timeZone = lib.mkForce "UTC";

    nix.settings.allowed-users = [ "root" ];

    networking.firewall.allowedTCPPorts = lib.mkForce cfg.publicTCPPorts;
    networking.firewall.interfaces.wg0.allowedTCPPorts = [ 22 9100 ];
    services.prometheus.exporters.node.openFirewall = lib.mkForce false;

    # Closure trims: manuals and the default perl/rsync/strace set have no
    # business on a node nobody develops on.
    documentation.enable = false;
    environment.defaultPackages = lib.mkForce [ ];
  };
}
