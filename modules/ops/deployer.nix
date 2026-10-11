# `deployer`: the operator account of EVERY fort (#647) — the one login
# the sfi-* verbs are pinned to (ops-commands.nix) and the one a getty
# accepts where the host enables ops/console.nix (#524).
#
# It lived in production.nix until #647, which made dev forts the
# exception: a dev fort carried the agent accounts and no operator, so
# the verbs auto-rebuild.nix declared there were present and unreachable,
# and console.nix refused the host at evaluation. #647 decided the other
# way — "dev fort" is one of our implementations ON TOP OF a fort, and
# deployer is the maintenance hatch of a fort, not of a cluster — so the
# account moved here, beside the verbs that name it. ops-commands.nix
# imports this module: wherever a sudoers rule says `deployer`, the
# account exists. Both profiles import it too, so a reader of either
# sees the account set in one place.
#
# WHAT IT HOLDS, on every fort alike:
#   - rung 1, OBSERVE: the host journal (systemd-journal). A cluster fort
#     adds the read-only kubeconfig (production.nix → kubeops, #90).
#     Day-0 units (flux-bootstrap, nixos-upgrade, k3s) fail before any
#     kubeconfig can say why, and without journal access the only
#     readable evidence is `systemctl show`'s exit code (#185).
#   - rung 2, NUDGE: the pinned NOPASSWD `sudo sfi-*` verbs each module
#     declares through ops-commands.nix — on a dev fort that is the host
#     set (sfi-nixos-rebuild, sfi-nixos-rollback, sfi-nixos-revision,
#     sfi-systemd-restart); a cluster fort adds the flux and k8s set.
#     `sfi-ops` lists what this host has.
#
# WHAT IT DOES NOT HOLD. No wheel, so no path to root from SSH: writes go
# through git, not a shell, and ops/admin-shell.nix is the opt-in window
# for the ADMIN rung. No nix daemon access (hardened.nix keeps allowed-users to
# root; a profile whose accounts build appends THEM, never this one):
# deployer observes the host, not the store. No password — sshd refuses
# every password (common.nix) and the only hash it can carry is the
# console-only one ops/console.nix reads. No Home Manager payload; the
# shell is users.defaultUserShell, which is bashInteractive on a fort
# that carries no shell module and zsh where the host enables sfi.shell
# (modules/shell.nix, #576) — one module decides it, so no `shell` here.
# On a dev fort it is also not in `agents` (the estate's dev role): the operator
# cannot read the fort's Claude credential, and the agent accounts cannot
# reach the operator's verbs — the kernel keeps the two roles apart, as
# it keeps the agent accounts apart from each other (#532).
#
# The key set is the operator's devices; trim it when vetting (#415)
# settles which devices hold an operator key. The estate's dev role carries
# the same list for the agent accounts today and says so — who may DISPATCH
# and who may OPERATE are two questions, kept as two lists.
{ ... }:

{
  users.users.deployer = {
    isNormalUser = true;
    # No colon — the GECOS field is colon-separated.
    description = "SFI fort operator — the sfi-* ops verbs and the console door";
    extraGroups = [ "systemd-journal" ];
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
}
