# SFI ops break-glass command framework.
#
# The intervention ladder on every fort — cluster forts and dev forts
# alike (#647); a dev fort has the host rungs and no cluster ones:
#   1. OBSERVE  — the read-only kubeconfig (ops-kubeconfig.nix) plus the
#      host journal (deployer is in systemd-journal, ops/deployer.nix): look
#      at anything, change nothing. Day-0 failures (flux-bootstrap before
#      flux exists) are only diagnosable from this rung. A few verbs
#      declared here BELONG TO THIS RUNG rather than the next one:
#      sfi-flux-status, sfi-k8s-logs and sfi-nixos-revision read and
#      print, and their reach is the read-only kubeconfig's own — the
#      built-in `view` role already carries every object they show,
#      pods/log included (#185). They are verbs and not a group
#      membership because a verb journals who read what, and because the
#      operator needs them in the window where rung 1 has no mesh and no
#      token to render from.
#   2. NUDGE    — curated `sudo sfi-<component>-<action>` verbs declared
#      through this module: each one is a narrow, audited escalation
#      (sudo-logged per invocation) that may use root and the admin
#      kubeconfig internally, but only retriggers machinery that git
#      already governs. Verbs nudge MACHINERY, never desired state —
#      nothing here edits a cluster resource's spec.
#   3. ADMIN    — `sudo -i` and the real kubectl, on a host that enables
#      ops/admin.nix, behind the admin-key challenge (ops/challenge.nix):
#      the actual break-glass event. The console is NOT this rung. Where
#      a host imports ops/console.nix (#524) the console gives a
#      `deployer` login, which holds rungs 1 and 2 and nothing more.
#
# The second factor is sudo's authentication (ops/challenge.nix, #115):
# a sudoers line WITHOUT NOPASSWD runs the admin-key ceremony once per
# terminal and window. The pinned verbs below are NOPASSWD and never
# reach it. A verb that widens REACH rather than nudging machinery asks
# for it with `challenge = true`, and a host can put any verb behind it
# with `sfi.ops.challenge.verbs`. The gate is for UNAUTHORIZED OPERATORS,
# not for a host whose root is already taken. `sudo sfi-challenge`
# rehearses the ceremony and changes nothing.
#
# Declaring `sfi.ops.commands.<name> = { description, script }` yields:
#   - a script `sfi-<name>` on the system PATH (writeShellApplication, so
#     `set -euo pipefail` and shellcheck at build time);
#   - a sudoers rule letting `deployer` run exactly
#     /run/current-system/sw/bin/sfi-<name> NOPASSWD — same pinned
#     stable-path pattern auto-rebuild.nix proved for sfi-rebuild-now
#     (matched on the current-system link so it survives generation
#     switches). The account is ./deployer.nix's, imported below, so the
#     rule's subject exists on every host that declares a verb (#647) —
#     there is no host where the verbs are present and unreachable;
#   - a row in `sfi-ops`, the index verb that lists every available
#     command with its description — discoverability is half the value.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.ops;

  # A verb is behind the challenge when its own module asked for it, or
  # when this host named it in sfi.ops.challenge.verbs.
  gated = name: c: c.challenge || lib.elem name cfg.challenge.verbs;

  mkCommand = name: c:
    pkgs.writeShellApplication {
      name = "sfi-${name}";
      runtimeInputs = c.runtimeInputs;
      text = c.script;
    };

  commandPkgs = lib.mapAttrsToList mkCommand cfg.commands;

  # sudo is required for the verbs (they run privileged machinery); the
  # index itself is plain-user readable.
  index = pkgs.writeShellApplication {
    name = "sfi-ops";
    text = ''
      echo "SFI ops verbs on this host (run as: sudo sfi-<verb>; each use is sudo-logged):"
      ${lib.concatStringsSep "\n" (lib.mapAttrsToList (name: c: ''
        printf '  %-22s %s%s\n' ${lib.escapeShellArg "sfi-${name}"} \
          ${lib.escapeShellArg (lib.optionalString (gated name c) "[admin key] ")} \
          ${lib.escapeShellArg c.description}
      '') cfg.commands)}
      echo
      echo "Ladder: observe with the read-only kubeconfig, or with the verbs marked"
      echo "READ ONLY above; nudge with the rest (machinery only, never desired"
      echo "state); a verb marked [admin key] runs sudo's admin-key challenge once"
      echo "per terminal and window. Full admin is a host that enables ops/admin.nix:"
      echo "sudo -i and kubectl behind the same challenge."
    '';
  };
in
{
  imports = [
    ./challenge.nix # the admin-key gate: sudo's authentication
    ./deployer.nix # the account every rule below names (#647)
  ];

  options.sfi.ops.commands = lib.mkOption {
    type = lib.types.attrsOf (lib.types.submodule {
      options = {
        description = lib.mkOption {
          type = lib.types.str;
          description = "One-line description shown by the sfi-ops index.";
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
            reach; leave it off for a verb that only nudges machinery.
          '';
        };
      };
    });
    default = { };
    description = ''
      Curated break-glass ops verbs. Each attribute <name> becomes an
      `sfi-<name>` command on PATH, runnable by deployer as
      `sudo sfi-<name>` (NOPASSWD, exact current-system path, unless the
      verb is behind the challenge), and listed by `sfi-ops`.
    '';
  };

  config = lib.mkIf (cfg.commands != { }) {
    environment.systemPackages = commandPkgs ++ [ index ];

    # A misspelled name in the host's policy list would gate nothing and
    # say nothing — the one failure this option must not have.
    assertions = map (name: {
      assertion = cfg.commands ? ${name};
      message = "sfi.ops.challenge.verbs names '${name}', but this host declares no sfi-${name}: the challenge would gate nothing. Verbs on this host: ${lib.concatStringsSep ", " (lib.attrNames cfg.commands)}.";
    }) cfg.challenge.verbs;

    # Same proven mechanism as auto-rebuild.nix's original carve-out: pin
    # the stable current-system path per verb, deployer only. The tag is
    # written on EVERY command: a sudoers tag carries over to the next
    # command in a list, so an untagged gated verb after a NOPASSWD one
    # would inherit NOPASSWD and meet no challenge.
    security.sudo.extraRules = [{
      users = [ "deployer" ];
      commands = lib.mapAttrsToList (name: c: {
        command = "/run/current-system/sw/bin/sfi-${name}";
        options = [ (if gated name c then "PASSWD" else "NOPASSWD") ];
      }) cfg.commands;
    }];
  };
}
