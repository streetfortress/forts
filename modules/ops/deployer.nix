# `deployer`: the operator account of EVERY fort (#647) — the one login
# the ops verbs are pinned to (ops-commands.nix) and the one a getty
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
# WHAT IT HOLDS, on every fort alike (the ladder: docs/ops.md):
#   - OBSERVE: the host journal (systemd-journal) and `nixos-revision`.
#     A cluster fort adds the read-only kubeconfig (k3s-server.nix →
#     kubeops, #90). Day-0 units (flux-bootstrap, nixos-upgrade, k3s)
#     fail before any kubeconfig can say why, and without journal access
#     the only readable evidence is `systemctl show`'s exit code (#185).
#   - HANDS-ON: the pinned NOPASSWD verbs on the pull loop, `nixos-pull`
#     and `nixos-rollback`, declared through ops-commands.nix; plus
#     whatever verbs the estate or a layer declares the same way. `ops`
#     lists what this host has.
#
# WHAT IT DOES NOT HOLD. No wheel, so no path to root from SSH: writes go
# through git, not a shell, and ops/admin.nix is the opt-in ADMIN rung —
# `sudo -i` and kubectl behind the admin-key challenge, which is sudo's
# authentication on every fort (ops/challenge.nix). No nix daemon access (hardened.nix keeps allowed-users to
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
# WHO HOLDS IT is the estate's, not the framework's (forts#81). The key
# list lived here, and a NixOS list option merges across modules: an
# estate that named its own operator devices added them to ours, so
# SFI's seven devices held the operator account of every fort built on
# this framework — a fort handed to another team included — unless that
# estate wrote lib.mkForce, which nothing asked it to. The framework
# ships no key now. The estate names its devices in estate.toml
# (`operator_keys`, read by modules/estate.nix into
# sfi.ops.deployer.authorizedKeys), one list for every fort of that
# estate, and a host file replaces the list for one host by setting the
# option. Vetting (#415) trims a list that is now the estate's to trim.
# The estate's dev role carries a list of its own for the agent accounts
# and says so — who may DISPATCH and who may OPERATE are two questions,
# kept as two lists.
{ config, ... }:

{
  # estate.nix declares the option and reads estate.toml: whatever a
  # module takes from the estate, it takes through that one reader.
  imports = [ ../estate.nix ];

  # An operator account no device can reach is the state #647 refused
  # for dev forts in the other direction: the ops verbs each module
  # declares through ops-commands.nix are present and unreachable, and
  # ops/console.nix opens a door for a login that holds no key. The
  # assertion is the estate's reminder, at evaluation, before an image
  # is built from a host nobody can operate.
  assertions = [
    {
      assertion = config.sfi.ops.deployer.authorizedKeys != [ ];
      message = ''
        sfi.ops.deployer.authorizedKeys is empty: no device holds `deployer`, the
        operator account of this fort, so the ops verbs are unreachable. The
        framework ships no operator key (forts#81). Name the estate's operator
        devices in estate.toml:

          operator_keys = [
            "ssh-ed25519 AAAAC3Nza… macbook-air",
          ]

        `fort check --all` reports the same gap. One host that must carry a
        different set sets sfi.ops.deployer.authorizedKeys in its own file.
      '';
    }
  ];

  users.users.deployer = {
    isNormalUser = true;
    # No colon — the GECOS field is colon-separated.
    description = "fort operator — the ops verbs and the console door";
    extraGroups = [ "systemd-journal" ];
    # The estate's keys, verbatim: this is the one definition of the
    # list, so nothing merges a key in that the estate did not name.
    openssh.authorizedKeys.keys = config.sfi.ops.deployer.authorizedKeys;
  };
}
