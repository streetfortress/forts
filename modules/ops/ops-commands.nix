# The ops verbs of a fort: `sudo <name>` for every `sfi.ops.commands.<name>`,
# and `ops`, the index that lists them.
#
# The ladder (forts#151) has three rungs, and every verb the core ships
# is on one of them:
#   OBSERVE      read-only signals. The host journal (deployer is in
#                systemd-journal, ops/deployer.nix), the read-only
#                kubeconfig on a cluster fort (ops-kubeconfig.nix), and
#                the verbs marked READ ONLY: `ops` (this index) and
#                `nixos-revision` (what the host runs).
#   HANDS-ON     bounded actions on the pull loop, each one sudo-logged:
#                `nixos-pull` (pull main now; `--from <ip>` is the
#                recovery path, forts#76) and `nixos-rollback` (a prior
#                generation, pinned). They choose among states git
#                already produced and never make a new one.
#   BREAK-GLASS  the admin rung, behind the admin-key challenge that is
#                sudo's authentication on a fort (ops/challenge.nix):
#                `sudo -i` and the real kubectl on a host that enables
#                ops/admin.nix, `admin-unlock` (mints the kubectl
#                credential), `challenge` (the rehearsal), and the
#                console password where a host enables ops/console.nix.
#
# The core ships verbs for the mechanisms it owns and no verb for the
# cluster. The cluster has kubectl and flux, and the framework gives two
# kubeconfigs to use them with: the read-only one (the OBSERVE rung from
# an operator's machine) and the admin one behind the challenge (the
# BREAK-GLASS rung). A maintenance window is the admin kubeconfig and
# `flux suspend`, flux's own command. A verb that exists for one
# workload, one runbook or one unit is a layer's or an estate's: it
# declares the verb through this module and gets the same pinned sudoers
# line and the same index row (docs/ops.md has a worked example).
#
# Declaring `sfi.ops.commands.<name> = { description, script }` yields:
#   - a script `<name>` on the system PATH (writeShellApplication, so
#     `set -euo pipefail` and shellcheck at build time);
#   - a sudoers rule letting `deployer` run exactly
#     /run/current-system/sw/bin/<name> — matched on the current-system
#     link so it survives generation switches. The rule is NOPASSWD
#     unless the verb is behind the challenge (below). The account is
#     ./deployer.nix's, imported below, so the rule's subject exists on
#     every host that declares a verb (#647) — there is no host where the
#     verbs are present and unreachable;
#   - a row in `ops`, the index verb that lists every available command
#     with its description — discoverability is half the value.
# The name is the whole of the verb's path, so it must not be the name
# of a tool the host already carries: the system path resolves a
# collision silently (ignoreCollisions), and sudo would then pin
# whichever one won. `nixos-pull` is not `nixos-rebuild` for this
# reason.
#
# The second factor is sudo's authentication (ops/challenge.nix, #115):
# a sudoers line WITHOUT NOPASSWD runs the admin-key ceremony once per
# terminal and window. The NOPASSWD verbs never reach it. A verb that
# widens REACH rather than acting on the pull loop asks for it with
# `challenge = true`, and a host can put any verb behind it with
# `sfi.ops.admin.challenge.verbs`. The gate is for UNAUTHORIZED
# OPERATORS, not for a host whose root is already taken. `sudo challenge`
# rehearses the ceremony and changes nothing. A host that sets
# sfi.ops.admin.challenge.enable = false has no gate: every verb's line
# is NOPASSWD.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.ops;

  # A verb is behind the challenge when its own module asked for it, or
  # when this host named it in sfi.ops.admin.challenge.verbs — and the
  # host has not switched the challenge off, which turns every line into
  # NOPASSWD (challenge.nix's enable option says why).
  gated = name: c:
    cfg.admin.challenge.enable && (c.challenge || lib.elem name cfg.admin.challenge.verbs);

  mkCommand = name: c:
    pkgs.writeShellApplication {
      inherit name;
      runtimeInputs = c.runtimeInputs;
      text = c.script;
    };

  commandPkgs = lib.mapAttrsToList mkCommand cfg.commands;

  # sudo is required for the verbs (they run privileged machinery); the
  # index itself is plain-user readable.
  index = pkgs.writeShellApplication {
    name = "ops";
    text = ''
      echo "ops verbs on this host (run as: sudo <verb>; each use is sudo-logged):"
      ${lib.concatStringsSep "\n" (lib.mapAttrsToList (name: c: ''
        printf '  %-18s %s%s\n' ${lib.escapeShellArg name} \
          ${lib.escapeShellArg (lib.optionalString (gated name c) "[admin key] ")} \
          ${lib.escapeShellArg c.description}
      '') cfg.commands)}
      echo
      echo "The ladder: OBSERVE with the journal, the read-only kubeconfig and the"
      echo "verbs marked READ ONLY; HANDS-ON with the rest, which act on the pull"
      echo "loop and nothing else; BREAK-GLASS is a verb marked [admin key], which"
      echo "runs sudo's admin-key challenge once per terminal and window, and, on"
      echo "a host that enables ops/admin.nix, sudo -i and kubectl behind the same"
      echo "challenge."
    '';
  };
in
{
  imports = [
    ./challenge.nix # the admin-key gate: sudo's authentication
    ./deployer.nix # the account every rule below names (#647)
    # The admin rung rides with the verbs, disabled (#195): enabling it is
    # `sfi.ops.admin.enable = true` in the host file, never a module import.
    ./admin.nix
  ];

  options.sfi.ops.commands = lib.mkOption {
    type = lib.types.attrsOf (lib.types.submodule {
      options = {
        description = lib.mkOption {
          type = lib.types.str;
          description = "One-line description shown by the `ops` index.";
        };
        script = lib.mkOption {
          type = lib.types.str;
          description = ''
            Body of the command (writeShellApplication text: runs with
            `set -euo pipefail`, shellchecked at build time). Runs as root
            via the generated sudoers rule and may use the admin
            kubeconfig internally — the verb IS the audited escalation.
          '';
        };
        runtimeInputs = lib.mkOption {
          type = lib.types.listOf lib.types.package;
          default = [ ];
          description = "Packages put on the command's PATH.";
        };
        challenge = lib.mkOption {
          type = lib.types.bool;
          default = false;
          description = ''
            Put this verb behind the admin-key challenge: its sudoers
            line requires authentication, which on a fort is the
            ceremony of ops/challenge.nix — once per terminal, then the
            window sudo's timestamp keeps. Set it on a verb that widens
            reach; leave it off for a verb that acts on machinery git
            already governs.
          '';
        };
      };
    });
    default = { };
    description = ''
      The ops verbs of this host. Each attribute <name> becomes a
      `<name>` command on PATH, runnable by deployer as `sudo <name>`
      (NOPASSWD, exact current-system path, unless the verb is behind
      the challenge), and listed by `ops`. The name must not be the
      name of a tool the host already carries.
    '';
  };

  config = lib.mkIf (cfg.commands != { }) {
    environment.systemPackages = commandPkgs ++ [ index ];

    # A misspelled name in the host's policy list would gate nothing and
    # say nothing — the one failure this option must not have.
    assertions = map (name: {
      assertion = cfg.commands ? ${name};
      message = "sfi.ops.admin.challenge.verbs names '${name}', but this host declares no such verb: the challenge would gate nothing. Verbs on this host: ${lib.concatStringsSep ", " (lib.attrNames cfg.commands)}.";
    }) cfg.admin.challenge.verbs;

    # Pin the stable current-system path per verb, deployer only. The tag
    # is written on EVERY command: a sudoers tag carries over to the next
    # command in a list, so an untagged gated verb after a NOPASSWD one
    # would inherit NOPASSWD and meet no challenge.
    security.sudo.extraRules = [{
      users = [ "deployer" ];
      commands = lib.mapAttrsToList (name: c: {
        command = "/run/current-system/sw/bin/${name}";
        options = [ (if gated name c then "PASSWD" else "NOPASSWD") ];
      }) cfg.commands;
    }];
  };
}
