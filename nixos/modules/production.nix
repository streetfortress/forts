# Hardened, slim profile for internet-facing production nodes (#86).
# Imported by every flux fort's hosts/<name>/default.nix (`fort scaffold`
# renders it unconditionally for a cluster fort, #272; dev-fort.nix is the
# other profile, and the two are mutually exclusive). The hardening itself — mesh-only SSH, UTC, the
# closure trims, root-only nix — is hardened.nix (#417), shared with
# dev-fort.nix; this module is what a CLUSTER fort adds on top:
#   - No Home Manager payload: the only login account is the unprivileged
#     `deployer` below, and the home-manager.users force below keeps it so
#     against any shared module that would declare one. (Also the size
#     lever for the 6 GB uncompressed custom-image cap: lab images measured
#     2.8-4.3 GB with a home stack.)
#   - The public/LAN interface serves exactly 80/443; the cluster and edge
#     ports re-open on the mesh interface only.
#   - deployer is a plain user: no wheel, so no path to root from SSH (the
#     carve-outs are the pinned NOPASSWD `sfi-*` ops verbs from
#     ops/ops-commands.nix — e.g. `sfi-nixos-rebuild`, which only re-runs
#     the pull loop against vetted main; `sfi-ops` lists them). Root
#     logins stay off (common.nix) and no account has a password, so
#     interactive break-glass is deliberately absent: use the provider's
#     rescue boot (or guestfish on the disk locally) — these boxes are
#     replaced, not repaired.
{ lib, ... }:

{
  imports = [ ./hardened.nix ];

  # Empty the HM user set wholesale: one mkForce, and a Home Manager user
  # added by any shared module is excluded from production by default
  # instead of sneaking into the image.
  home-manager.users = lib.mkForce { };

  # The production operator account: SSH in over the mesh, watch the machinery,
  # pull a log (journal read via the systemd-journal group). No wheel —
  # writes go through git, not a shell (#90 adds the read-only kubeconfig
  # this account uses), and no nix daemon access (hardened.nix keeps it
  # root-only): deployer observes the cluster, not the store; rebuilds
  # arrive as replacement images. Key set is the operator's devices, the
  # same list dev-fort.nix carries for agent; trim it when vetting settles.
  users.users.deployer = {
    extraGroups = [
      # Read the ops-ro kubeconfig (ops-kubeconfig.nix, #90).
      "kubeops"
      # Read the whole system journal (#185). This is the OBSERVE rung
      # for the host half of the machinery: day-0 units (flux-bootstrap,
      # nixos-upgrade, k3s) fail before any kubeconfig can say why, and
      # without journal access the only readable evidence is
      # `systemctl show`'s exit code. Lish is no help — no account has a
      # password by design. Read-only, no sudo path; the grant is a group
      # membership, not a verb.
      "systemd-journal"
    ];
    isNormalUser = true;
    description = "SFI production operator";
    # No `shell` here: an isNormalUser follows users.defaultUserShell, and
    # one module decides it. That is bashInteractive on a fort that carries
    # no shell module, and zsh where the host imports modules/shell.nix and
    # enables sfi.shell — the module's ticking clock and vi-mode indicator
    # are zsh-only (#576).
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

  # The public face of a cluster fort is exactly 80/443; hardened.nix
  # collapses every other merged opening to this list. The admin/cluster
  # ports below re-open on the mesh interface only (22 + 9100 are already
  # there from hardened.nix).
  sfi.hardened.publicTCPPorts = [ 80 443 ];
  # 30909/30993: edge Prometheus/Alertmanager NodePorts (mesh-services-
  # edge.yaml) — the wg0-only opening IS their access control; silences
  # are set against 30993.
  # 30432: sites-db's primary NodePort (fleet/apps/<fort>/sites-db/
  # mesh-service.yaml), read by databasus-central on luxor (#327) — every
  # fort, like the edge pair, so the consumer can assume the coordinate.
  networking.firewall.interfaces.wg0.allowedTCPPorts = [ 6443 10255 30432 30909 30993 ];

  # The in-cluster edge Prometheus (#53) scrapes the host node_exporter at
  # the mesh address; pod traffic to the host's own wg0 IP ingresses on
  # cni0, so 9100 must be open there too. The wg0 10255 row above and
  # hardened.nix's wg0 9100 are the soundship pull-model path — drop them
  # once the edge stack is verified on an edge cluster.
  networking.firewall.interfaces."cni0".allowedTCPPorts = [ 9100 ];
}
