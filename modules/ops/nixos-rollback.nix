# nixos-rollback: switch the host to a previous NixOS generation and
# pin the pull loop there.
#
#   sudo nixos-rollback            # menu: generations on disk, what
#                                      #   each was built from, the pin
#   sudo nixos-rollback 46         # switch to generation 46 + pin
#   sudo nixos-rollback --lift     # unpin, stay on this generation
#   sudo nixos-pull             # unpin AND pull main now
#
# Why: a merge to main IS the rollout (auto-rebuild.nix), and a merged
# host-config change that misbehaves on one fort — a firewall rule, a
# peer set, a module split like #417 — had no way back short of a
# provider console: deployer has no root shell, `nixos-rebuild
# --rollback` is root's, and the only lever was "merge a revert and wait
# for the loop". Every generation on disk was built from a vetted main
# commit — `nixos-pull` builds main and nothing else, and its
# `--from <ip>` names another address for that same main (#76) — so
# choosing one is choosing among states git already produced: the
# HANDS-ON rung (ops/ops-commands.nix) — a deliberate, sudo-logged,
# VISIBLE hold on desired state, never a new state. The admin-key
# challenge (ops/challenge.nix) is for verbs that widen REACH; this one
# narrows a menu, and the menu holds only what main built.
#
# The pin is what makes a rollback more than a race with the timer.
# Without it the next closure-relevant merge — fixing this fort or
# unrelated — rolls the host straight back onto the config it just left.
# So the verb writes /var/lib/sfi-auto-rebuild/pinned-generation and the
# gate in auto-rebuild.nix skips while the host is on that generation;
# a switch to any other generation makes the pin stale and the gate
# drops it, which is what keeps an older generation's nixos-pull
# (which knows nothing of pins) from stranding the host. The pin is a
# textfile metric, node_nixos_pinned_generation_info, and
# NixosGenerationPinned (rules-edge.yaml) nags after 6 h: a forgotten
# hold is the 2026-09-03 drift incident with extra steps, and this verb
# must not be a quiet way to fall off main.
#
# Reach: generations younger than sfi.auto-rebuild.gc.olderThan (30 d by
# default, #423) — nix.gc in auto-rebuild.nix collects older ones; the
# running (and so a pinned) generation is never collected.
#
# Caveat, stated by the verb when it applies: a generation built before
# this module carries a gate that does not read the pin. Rolling back to
# one, the verb also stops nixos-upgrade.timer for this boot — a reboot
# restarts it, and from then on main's next closure-relevant merge rolls
# the host forward. The caveat ages out as generations do.
#
# Menu-only, like every argument-taking verb: the number is compared
# against nix-env's own generation listing and the LISTED value is what
# reaches nix-env; the current generation is refused (nothing to do).
# The revision column comes from the ledger auto-rebuild.nix appends on
# every successful switch (generation ← main@rev); a generation without
# a row was built outside the pull loop (an image's first boot, a
# `--from` switch) and says so.
{ config, lib, pkgs, ... }:

let
  stateDir = "/var/lib/sfi-auto-rebuild"; # auto-rebuild.nix's StateDirectory
  textfileDir = "/var/lib/node-exporter-textfiles"; # textfile-metrics.nix
  profile = "/nix/var/nix/profiles/system";
in
{
  imports = [ ./ops-commands.nix ];

  sfi.ops.commands.nixos-rollback = {
    description = "Switch to a previous NixOS generation and pin the pull loop there: nixos-rollback <generation>; --lift unpins; no args lists generations and what each was built from";
    runtimeInputs = [ pkgs.coreutils pkgs.gawk pkgs.gnused pkgs.gnugrep pkgs.systemd config.nix.package ];
    script = ''
      pin=${stateDir}/pinned-generation
      ledger=${stateDir}/generations
      metric=${textfileDir}/nixos-pinned.prom

      current_generation() {
        readlink ${profile} | sed -n 's/^system-\([0-9][0-9]*\)-link$/\1/p'
      }

      write_pin_metric() {
        tmp=$(mktemp ${textfileDir}/.nixos-pinned.prom.XXXXXX)
        printf 'node_nixos_pinned_generation_info{generation="%s"} 1\n' "$1" > "$tmp"
        chmod 0644 "$tmp"
        mv "$tmp" "$metric"
      }

      lift_pin() {
        if [ -e "$pin" ]; then
          echo "lifted the pin to generation $(cat "$pin"); main flows again on the next closure-relevant merge"
          rm -f "$pin"
        else
          echo "no pin set"
        fi
        rm -f "$metric"
        # The pre-pin-gate stopgap (below) may have stopped the timer.
        if systemctl list-unit-files nixos-upgrade.timer >/dev/null 2>&1; then
          systemctl start nixos-upgrade.timer
        fi
      }

      usage() {
        echo "usage: sudo nixos-rollback [<generation> | --lift]   (no args lists generations)"
      }

      # nix-env's listing is the menu: "  47   2026-09-22 05:10:33   (current)".
      mapfile -t rows < <(nix-env -p ${profile} --list-generations)
      current=$(current_generation)

      if [ "$#" -eq 0 ]; then
        echo "NixOS generations on ${config.networking.hostName} (sudo nixos-rollback <n> switches and pins; --lift unpins):"
        for row in "''${rows[@]}"; do
          gen=$(awk '{print $1}' <<<"$row")
          when=$(awk '{print $2 " " substr($3, 1, 5)}' <<<"$row")
          rev=$(awk -v g="$gen" '$1 == g {print "main@" substr($2, 1, 7)}' "$ledger" 2>/dev/null | tail -n1)
          mark=""
          [ "$gen" = "$current" ] && mark="  (current)"
          printf '  %4s  %s  %-14s%s\n' "$gen" "$when" "''${rev:-(no revision recorded: built outside the pull loop)}" "$mark"
        done
        if [ -e "$pin" ]; then
          echo "pin: generation $(cat "$pin") — nixos-upgrade skips while the host is on it (sudo nixos-pull lifts it and pulls main)"
        else
          echo "pin: none — the pull loop follows main"
        fi
        exit 0
      fi
      if [ "$#" -ne 1 ]; then
        usage >&2
        exit 64
      fi

      if [ "$1" = "--lift" ]; then
        lift_pin
        exit 0
      fi

      case "$1" in
        *[!0-9]*|"") echo "error: want a generation number (or --lift); run without args for the menu" >&2; exit 2 ;;
      esac

      # Compare only; the value handed on is the listing's.
      target=""
      for row in "''${rows[@]}"; do
        gen=$(awk '{print $1}' <<<"$row")
        if [ "$gen" = "$1" ]; then
          target=$gen
        fi
      done
      if [ -z "$target" ]; then
        echo "error: generation $1 is not on this host — run without args for the menu" >&2
        exit 1
      fi
      if [ "$target" = "$current" ]; then
        echo "error: generation $target is the running one; nothing to switch to" >&2
        exit 1
      fi

      link=/nix/var/nix/profiles/system-$target-link
      rev=$(awk -v g="$target" '$1 == g {print $2}' "$ledger" 2>/dev/null | tail -n1)
      echo "switching ${config.networking.hostName} from generation $current to $target (''${rev:+main@''${rev:0:7}}''${rev:-no revision recorded}) ..."
      nix-env -p ${profile} --switch-generation "$target"
      ${profile}/bin/switch-to-configuration switch
      echo "now on generation $target; the bootloader default follows"

      mkdir -p ${stateDir}
      printf '%s\n' "$target" > "$pin"
      write_pin_metric "$target"
      echo "pinned: nixos-upgrade skips while the host is on generation $target (node_nixos_pinned_generation_info; NixosGenerationPinned nags after 6 h)"
      echo "to move on: merge the fix, then 'sudo nixos-pull' (lifts the pin, pulls main) — or 'sudo nixos-rollback --lift' to let the timer take main's next change"

      # A generation from before the pin-aware gate would roll forward on
      # main's next closure-relevant merge regardless of the pin.
      unit=$link/etc/systemd/system/nixos-upgrade.service
      if [ -e "$unit" ]; then
        gate=$(sed -n 's/^ExecCondition=//p' "$unit" | head -n1)
        if [ -n "$gate" ] && ! grep -q pinned-generation "$gate" 2>/dev/null; then
          echo "CAVEAT: generation $target predates the pin-aware gate — its timer would roll forward on main's next closure-relevant merge. Stopping nixos-upgrade.timer for this boot as a stopgap; a reboot restarts it." >&2
          systemctl stop nixos-upgrade.timer
        fi
      fi
    '';
  };
}
