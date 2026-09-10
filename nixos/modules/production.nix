# Hardened, slim profile for internet-facing production nodes (#86).
# Imported by hosts/<name>/default.nix — scaffolded automatically when the host
# sets `production: true` in provisioning/inventory.yaml (and dotfiles.nix +
# interactive.nix are dropped from the same scaffold; the profiles are mutually
# exclusive).
#
# Relative to the lab VM shape this changes:
#   - No Home Manager payload, and no steven at all: interactive.nix isn't
#     imported here, so the only login account is the unprivileged `deployer`
#     below. The home-manager.users force is CORRECTNESS-load-bearing, not just
#     a size trim — the flake's uniform VM module set still declares HM config
#     for steven, who does not exist on this host, and the force is what keeps
#     that from evaluating. (It's also the size lever for the 6 GB uncompressed
#     custom-image cap: lab images measure 2.8-4.3 GB with the home stack.)
#   - SSH reachable over the wireguard mesh only. The public/LAN interface
#     serves exactly 80/443 (+ 51820/udp, which IS the mesh transport, from
#     wireguard.nix). Production hosts therefore MUST be mesh members —
#     without wg0 there is no login path at all.
#   - deployer is a plain user: no wheel, so no path to root from SSH (the
#     one carve-out is auto-rebuild.nix's pinned NOPASSWD `sfi-rebuild-now`,
#     which only re-runs the pull loop against vetted main). Root
#     logins stay off (common.nix) and no account has a password, so
#     interactive break-glass is deliberately absent: use the provider's
#     rescue boot (or guestfish on the disk locally) — these boxes are
#     replaced, not repaired.
{ lib, pkgs, ... }:

{
  # Empty the HM user set wholesale rather than removing users.steven's entry —
  # one mkForce, and any future HM user added by a shared module is excluded
  # from production by default instead of sneaking into the image.
  home-manager.users = lib.mkForce { };

  # The production operator account: SSH in over the mesh, watch the machinery,
  # pull a log. No wheel — writes go through git, not a shell (#90 adds the
  # read-only kubeconfig this account will use). Key set matches
  # interactive.nix for now; trim to the ops devices when vetting settles.
  users.users.deployer = {
    isNormalUser = true;
    description = "SFI production operator";
    shell = pkgs.bashInteractive;
    openssh.authorizedKeys.keys = [
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBFSTKL7m9ViSstGGhgg1TBnrWEGkNptCCysU17Oxgfl"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIG0sCtYCOvdb/J7yluSSX9yixiG3pvhZo+OtVQWefjVj"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBuuLb9A36fWGYZexMb2soTxpHFB0HVQTHqR8vihvFMD"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINqO0N5rBif1+bFwnluWEmrkaoTAtqTrP/vONG6/fQKl"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBhrjrcm0Mb4gSMGM/GiWXvkZj3a2ej7/MOcw0Qujx+M"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFSfukFhjws+MpZu+qMqgVCoIitc43jGeqEnGMcF4ydQ"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEdgJK6LiKUZ+fQQZX28B1v82hCl1z4RxomJLn5pHiPH"
    ];
  };

  # Only root talks to the nix daemon: deployer observes the cluster, not the
  # store, and rebuilds arrive as replacement images.
  nix.settings.allowed-users = [ "root" ];

  # Collapse the merged firewall openings (22 from common.nix / openssh
  # openFirewall, 6443 + 80 + 443 from k3s-server.nix) down to the public
  # 80/443, then re-open the admin/cluster ports on the mesh interface only.
  # The node exporter's openFirewall injects an iptables rule outside
  # allowedTCPPorts, so it must be switched off explicitly to scope 9100.
  networking.firewall.allowedTCPPorts = lib.mkForce [ 80 443 ];
  # 30909/30993: edge Prometheus/Alertmanager NodePorts (mesh-services-
  # edge.yaml) — the wg0-only opening IS their access control; silences
  # are set against 30993.
  networking.firewall.interfaces.wg0.allowedTCPPorts = [ 22 6443 9100 10255 30909 30993 ];
  services.prometheus.exporters.node.openFirewall = lib.mkForce false;

  # The in-cluster edge Prometheus (#53) scrapes the host node_exporter at
  # the mesh address; pod traffic to the host's own wg0 IP ingresses on
  # cni0, so 9100 must be open there too. The wg0 9100/10255 rows above are
  # the soundship pull-model path — drop them once the edge stack is
  # verified on production-test.
  networking.firewall.interfaces."cni0".allowedTCPPorts = [ 9100 ];

  # Closure trims: manuals and the default perl/rsync/strace set have no
  # business on a node nobody develops on.
  documentation.enable = false;
  environment.defaultPackages = lib.mkForce [ ];
}
