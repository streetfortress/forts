# sfi.ops.console: a password for `deployer` that ONLY a getty accepts, so
# an operator who reaches the provider console can still run
# `sudo sfi-nixos-rollback` after the mesh breaks (#524).
#
#   Lish (linode) / `virsh console <host>` (soundship)
#   <host> login: deployer          # `fort console-password <host> --show`
#   sudo sfi-nixos-rollback         # the menu, then the generation
#
# WHY. A merge to main IS the rollout (auto-rebuild.nix), and
# ops/nixos-rollback.nix is the remedy for a rollout that misbehaves. The
# remedy is reachable over SSH on the mesh, and over nothing else: hosts
# answer SSH on wg0 alone (hardened.nix), so one merged change that
# breaks wireguard, or one generation that does not answer sshd, locks
# every operator out of every fort at the same time. The policy this
# module amends — "no account has a password" (production.nix, root.nix)
# — assumes a fort is replaceable. The fleet is one estate now, and a
# repave is not a remedy for a fault that repaves into the next host.
#
# THIS GRANTS NO NEW REACH. It gives the rungs that already exist a
# second door:
#   - sshd refuses the password. common.nix sets
#     PasswordAuthentication = false and PermitRootLogin = "no", so the
#     credential works at a getty and nowhere else.
#   - The console is already behind one authentication: Lish needs the
#     Linode account, `virsh console` needs SSH to soundship. This is the
#     second factor of a path that has one, not a new front door.
#   - deployer keeps no wheel (production.nix). After the login the
#     operator holds rung 1 (the journal, the read-only kubeconfig) and
#     rung 2 (the pinned NOPASSWD sfi-* verbs, ops/ops-commands.nix).
#     sfi-nixos-rollback chooses among generations git already produced;
#     it never makes a new state.
#   - root keeps no password. The ALL-sudo window is still ops/root.nix,
#     opt-in for a declared window.
#
# The guard is the import, as in ops/root.nix: a host gets this only by
# naming the module in its own hosts/<host>/default.nix and setting
# enable. The credential is minted per fort by
# `fort console-password <host> --write`, which writes the hash here and
# the passphrase into provisioning/console-passwords.yaml — the registry
# an operator reads with `--show`, encrypted to the admin age keys and
# NOT to the fort. A host holds the hash and nothing more, so a fort
# that is taken yields no credential, not even its own. A fort with
# enable and no hash fails `fort check` before it fails activation.
#
# CONSEQUENCE: users.mutableUsers = false. The fleet sets mutableUsers
# nowhere, so it is true today, and under true this failsafe SILENTLY
# does not exist: update-users-groups.pl applies a declared hash to a
# user that already has an /etc/shadow row only when mutableUsers is
# false, and `deployer` has such a row (it holds `!`). The hash would be
# read and discarded, with no warning, and the operator would learn it at
# the console during the incident. So the module sets mutableUsers = false
# and /etc/shadow becomes fully declarative on this host: a password set
# by hand on a live host does not survive the next activation, and an
# account with no declared password stays locked — root included.
{ config, lib, ... }:
let
  cfg = config.sfi.ops.console;
in
{
  imports = [ ./ops-commands.nix ];

  options.sfi.ops.console.enable = lib.mkOption {
    type = lib.types.bool;
    default = false;
    description = "Give `deployer` a console-only password, so an operator on Lish or `virsh console` reaches `sudo sfi-nixos-rollback` when the mesh is gone. Needs `deployer-password-hash` in the host's sops file (`fort console-password <host> --write`).";
  };

  config = lib.mkIf cfg.enable {
    # ops/ops-commands.nix pins its sudo rules to `deployer`, so a login
    # on a host without that account reaches no verb at all. A dev fort
    # carries `agent` instead and is transient (dev-fort.nix), so it is
    # out of scope; this says so at evaluation rather than at the
    # console. The test is isNormalUser, not the presence of the attr:
    # the hashedPasswordFile below defines `users.users.deployer` on
    # every host that enables this, so presence would always be true.
    assertions = [{
      assertion = config.users.users.deployer.isNormalUser;
      message = "sfi.ops.console.enable needs the `deployer` account (nixos/modules/production.nix): ops/ops-commands.nix pins every sfi-* sudo rule to deployer, so a console login on a host without it reaches no recovery verb. A dev fort carries `agent` and is repaved, not recovered.";
    }];

    # neededForUsers is the supported path: sops-nix decrypts such a
    # secret to /run/secrets-for-users BEFORE it makes the users, so
    # there is no catch-22 with the hash the user declaration reads. Nor
    # is there one with the age key: a fort's identity is pre-seeded, so
    # `fort image` grafts the ssh host key into the image and sops-nix
    # finds it at the first boot (docs/forts/mint.md, and the knowledge
    # base's config-management/pre-seeded-identity.md for why).
    sops.secrets.deployer-password-hash.neededForUsers = true;

    # Load-bearing — see the header. Without it the declared hash is
    # discarded on a host that is already provisioned, and nothing says so.
    users.mutableUsers = false;

    # The pair that mutableUsers = false demands. NixOS asserts that root
    # or a wheel user has a password or an ssh key, because a declarative
    # /etc/shadow can lock an operator out. On a fort that is the
    # DECLARED posture and not an accident: root logins are off
    # (common.nix), root has no password and no authorized key, and
    # `deployer` has no wheel (production.nix). mutableUsers = true hides
    # the same fact rather than changing it — the assertion is what
    # NOTICES it. The console login this module adds is the answer to it:
    # the lockout the assertion warns about is what #524 is about, and
    # `deployer` + the sfi-* verbs is the way back in.
    users.allowNoPasswordLogin = true;

    users.users.deployer.hashedPasswordFile =
      config.sops.secrets.deployer-password-hash.path;
  };
}
