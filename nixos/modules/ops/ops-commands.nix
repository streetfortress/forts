# SFI ops break-glass command framework.
#
# The intervention ladder on production-shaped hosts:
#   1. OBSERVE  — the read-only kubeconfig (ops-kubeconfig.nix) plus the
#      host journal (deployer is in systemd-journal, production.nix): look
#      at anything, change nothing. Day-0 failures (flux-bootstrap before
#      flux exists) are only diagnosable from this rung.
#   2. NUDGE    — curated `sudo sfi-<component>-<action>` verbs declared
#      through this module: each one is a narrow, audited escalation
#      (sudo-logged per invocation) that may use root and the admin
#      kubeconfig internally, but only retriggers machinery that git
#      already governs. Verbs nudge MACHINERY, never desired state —
#      nothing here edits a cluster resource's spec.
#   3. ADMIN    — an explicit `KUBECONFIG=/etc/rancher/k3s/k3s.yaml` as
#      root, through ops/root.nix in a declared window: the actual
#      break-glass event. The console is NOT this rung. Where a host
#      imports ops/console.nix (#524) the console gives a `deployer`
#      login, which holds rungs 1 and 2 and nothing more.
#
# A verb that widens REACH rather than nudging machinery can ask for a
# second factor: `challenge = "<reason>"` makes the verb prove live
# possession of a fleet admin age key before it runs (ops/challenge.nix,
# #256), and a host can gate any verb the same way with
# `sfi.ops.challenge.verbs`. The gate is for UNAUTHORIZED OPERATORS, not
# for a host whose root is already taken. No verb sets it today —
# ops/challenge.nix holds the mechanics and `sudo sfi-challenge`, which
# rehearses them and changes nothing; gating a real verb is a separate
# decision, and that module lists the candidates.
#
# Declaring `sfi.ops.commands.<name> = { description, script }` yields:
#   - a script `sfi-<name>` on the system PATH (writeShellApplication, so
#     `set -euo pipefail` and shellcheck at build time);
#   - a sudoers rule letting `deployer` run exactly
#     /run/current-system/sw/bin/sfi-<name> NOPASSWD — same pinned
#     stable-path pattern auto-rebuild.nix proved for sfi-rebuild-now
#     (matched on the current-system link so it survives generation
#     switches; on hosts without a deployer account it never matches);
#   - a row in `sfi-ops`, the index verb that lists every available
#     command with its description — discoverability is half the value.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.ops;

  # A verb is gated when its own module asked for it, or when this host
  # named it in sfi.ops.challenge.verbs.
  challengeOf = name: c:
    if c.challenge != null then c.challenge
    else if lib.elem name cfg.challenge.verbs then
      "this host requires the admin key for sfi-${name} (sfi.ops.challenge.verbs)"
    else null;

  mkCommand = name: c:
    let reason = challengeOf name c;
    in pkgs.writeShellApplication {
      name = "sfi-${name}";
      runtimeInputs = c.runtimeInputs;
      # The gate runs first and exits non-zero when the operator cannot
      # prove the key; `set -e` from writeShellApplication stops the verb
      # there. It also journals the argv before the body runs.
      text = lib.optionalString (reason != null) ''
        ${cfg.challenge.package}/bin/sfi-ops-challenge ${lib.escapeShellArg reason} \
          -- ${lib.escapeShellArg "sfi-${name}"} "$@"
      '' + c.script;
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
          ${lib.escapeShellArg (lib.optionalString (challengeOf name c != null) "[admin-key challenge] ")} \
          ${lib.escapeShellArg c.description}
      '') cfg.commands)}
      echo
      echo "Ladder: observe with the read-only kubeconfig; nudge with these verbs"
      echo "(machinery only, never desired state); full admin is console break-glass."
    '';
  };
in
{
  imports = [ ./challenge.nix ]; # the admin-key gate, for verbs that ask

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
          type = lib.types.nullOr lib.types.str;
          default = null;
          example = "arbitrary kubectl with the admin kubeconfig";
          description = ''
            One line saying what reach this verb has. Setting it gates
            EVERY invocation behind the admin-key challenge
            (ops/challenge.nix): the operator must decrypt a nonce with a
            fleet admin age key before the verb body runs. Leave it null
            for a verb that only nudges machinery. A verb that needs the
            gate on one argument path leaves this null and calls
            `''${config.sfi.ops.challenge.package}/bin/sfi-ops-challenge`
            itself.
          '';
        };
      };
    });
    default = { };
    description = ''
      Curated break-glass ops verbs. Each attribute <name> becomes an
      `sfi-<name>` command on PATH, runnable by deployer as
      `sudo sfi-<name>` (NOPASSWD, exact current-system path), and listed
      by `sfi-ops`.
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
    # the stable current-system path per verb, NOPASSWD, deployer only.
    security.sudo.extraRules = [{
      users = [ "deployer" ];
      commands = map (name: {
        command = "/run/current-system/sw/bin/sfi-${name}";
        options = [ "NOPASSWD" ];
      }) (lib.attrNames cfg.commands);
    }];
  };
}
