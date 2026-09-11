# SFI ops break-glass command framework.
#
# The intervention ladder on production-shaped hosts:
#   1. OBSERVE  — the read-only kubeconfig (ops-kubeconfig.nix): look at
#      anything, change nothing.
#   2. NUDGE    — curated `sudo sfi-<component>-<action>` verbs declared
#      through this module: each one is a narrow, audited escalation
#      (sudo-logged per invocation) that may use root and the admin
#      kubeconfig internally, but only retriggers machinery that git
#      already governs. Verbs nudge MACHINERY, never desired state —
#      nothing here edits a cluster resource's spec.
#   3. ADMIN    — an explicit `KUBECONFIG=/etc/rancher/k3s/k3s.yaml` or a
#      root shell at the console: the actual break-glass event.
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
        printf '  %-22s %s\n' ${lib.escapeShellArg "sfi-${name}"} ${lib.escapeShellArg c.description}
      '') cfg.commands)}
      echo
      echo "Ladder: observe with the read-only kubeconfig; nudge with these verbs"
      echo "(machinery only, never desired state); full admin is console break-glass."
    '';
  };
in
{
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
