# Backup verbs (declared via ops-commands.nix): run one now, and see what
# is actually in the repo.
#
# The gap they close, found on the sfi-fort-d54d5b3 drill: the backup
# layer was the one part of the machinery with NO rung on the intervention
# ladder. `deployer` could read the journal and that was all — no way to
# trigger a run after fixing a credential, and no way to answer "is there
# anything in the repo?" without console break-glass. A poor arrangement
# for the subsystem whose failure mode is silence.
#
#   sudo sfi-backup-now              # no args: list the targets
#   sudo sfi-backup-now rustfs       # run that one, wait, report
#   sudo sfi-backup-status           # timers, last result, newest snapshots
#
# Both are NUDGE-rung by the framework's rule: they retrigger machinery
# git already governs (the timer's own unit) and change no desired state.
# sfi-backup-status additionally READS the repo, which needs credentials —
# hence root, hence a verb rather than something deployer could do unaided.
#
# The targets and their repositories are read from
# `services.restic.backups`, i.e. from what sfi.pv-backup actually
# rendered, rather than recomputed from the profile. The verbs therefore
# cannot disagree with the units they nudge, and the framework's
# injection rule is satisfied structurally: the valid names are known at
# EVAL time and rendered into the case arms, so operator input only ever
# selects from that menu and is never interpolated.
{ config, lib, pkgs, ... }:

let
  backups = config.services.restic.backups;
  names = lib.filter (n: lib.hasPrefix "pv-" n) (lib.attrNames backups);
  # `pv-rustfs` is the unit; `rustfs` is what the operator types.
  targets = map (n: lib.removePrefix "pv-" n) names;

  arms = lib.concatStringsSep "|" targets;
  usage = "sudo sfi-backup-now <" + lib.concatStringsSep "|" targets + ">";

  # One stanza per target, reading that unit's OWN repository and
  # credentials.
  statusFor = t:
    let b = backups."pv-${t}"; in ''
      echo "== ${t}"
      systemctl --no-pager --lines=0 status ${lib.escapeShellArg "restic-backups-pv-${t}.service"} 2>/dev/null | head -4 || true
      # The repo is the truth the unit's exit status only implies: a run
      # can succeed against a repo that later loses its snapshots to a
      # prune, and the freshness metric would still look fine.
      # The subshell is load-bearing: each target's credentials must not
      # leak into the next target's restic call, so they are sourced and
      # exported inside it and die with it. That is exactly what SC2030/
      # SC2031 warn about, and exactly what is wanted here.
      # shellcheck disable=SC2030,SC2031
      (
        set -a
        # shellcheck disable=SC1091
        . ${lib.escapeShellArg (toString b.environmentFile)}
        set +a
        export RESTIC_PASSWORD_FILE=${lib.escapeShellArg (toString b.passwordFile)}
        restic -r ${lib.escapeShellArg b.repository} snapshots --latest 3 --no-lock 2>&1 | tail -6
      ) || echo "  (repo unreadable — credentials, endpoint, or nothing written yet)"
      echo
    '';
in
{
  imports = [ ./ops-commands.nix ];

  config = lib.mkIf (targets != [ ]) {
    sfi.ops.commands = {
      backup-now = {
        description = "Run a PV backup now: ${usage}; no args lists targets. Blocks for the run and reports its result";
        runtimeInputs = [ pkgs.systemd ];
        script = ''
          if [ "$#" -eq 0 ]; then
            echo ${lib.escapeShellArg "usage: ${usage}"}
            echo "targets on this host:"
            ${lib.concatMapStringsSep "\n            " (t: "echo '  ${t}'") targets}
            exit 0
          fi
          case "$1" in
            ${arms}) ;;
            *) echo "unknown target $1 — run without arguments to list them" >&2; exit 2 ;;
          esac
          unit="restic-backups-pv-$1.service"
          echo "== starting $unit (blocks until the run finishes; ^C leaves it running)"
          # Deliberately blocking: the verb exists for a human watching a
          # drill or a fix, and a fire-and-forget nudge that reports
          # nothing is the failure mode this layer already has.
          systemctl start "$unit" || true
          systemctl --no-pager --lines=0 status "$unit" | head -5 || true
          echo
          echo "journal: journalctl -u $unit -n 50"
        '';
      };

      backup-status = {
        description = "What the PV backups have actually done: each target's timer, last result, and the newest snapshots in its repo";
        runtimeInputs = [ pkgs.systemd pkgs.restic pkgs.coreutils ];
        script = ''
          echo "== timers"
          systemctl list-timers --no-pager "restic-backups-pv-*" || true
          echo
          ${lib.concatMapStringsSep "\n" statusFor targets}
        '';
      };
    };
  };
}
