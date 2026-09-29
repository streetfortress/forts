# sfi-nixos-revision: which revision of this repo the host actually runs
# (#185).
#
#   sudo sfi-nixos-revision
#
# Why: `main` is the deploy (auto-rebuild.nix), and the three revisions
# that describe a host are not the same number. The gate records what it
# last ATTEMPTED, the loop records what the host has APPLIED (a
# fleet-only commit advances this without a rebuild), and a successful
# switch records what the running closure was EFFECTIVELY built from. All
# three are textfile metrics on the fleet dashboards, and a host with no
# metrics path out — no mesh, no Thanos, day 0 — had no way to say any of
# them on its own console. The revision is deliberately not in the
# closure (embedding it would churn a generation per commit), so
# `nixos-version` cannot answer this and this verb has to.
#
# It reads and prints; it changes nothing. The way to act on what it says
# is sfi-nixos-rebuild (pull main now) or sfi-nixos-rollback (go back and
# pin), both in this directory.
{ config, lib, pkgs, ... }:

let
  stateDir = "/var/lib/sfi-auto-rebuild"; # auto-rebuild.nix's StateDirectory
  textfileDir = "/var/lib/node-exporter-textfiles"; # textfile-metrics.nix
  profile = "/nix/var/nix/profiles/system";
in
{
  imports = [ ./ops-commands.nix ];

  sfi.ops.commands.nixos-revision = {
    description = "READ ONLY: which revision of this repo the host runs — attempted, applied and effective, the running generation and what it was built from, the pin, and the loop's state; no arguments";
    runtimeInputs = [ pkgs.coreutils pkgs.gnused pkgs.gawk pkgs.systemd ];
    script = ''
      if [ "$#" -ne 0 ]; then
        echo "usage: sudo sfi-nixos-revision   (no arguments; reads only)" >&2
        exit 64
      fi

      ledger=${stateDir}/generations
      pin=${stateDir}/pinned-generation

      # Each metric file holds one line: name{revision="<sha>"} 1.
      metric_revision() {
        sed -n 's/^.*revision="\([^"]*\)".*$/\1/p' "${textfileDir}/$1" 2>/dev/null | tail -n1
      }

      short() {
        if [ -n "$1" ]; then
          printf '%s' "''${1:0:7}"
        else
          printf 'none'
        fi
      }

      current=$(readlink ${profile} | sed -n 's/^system-\([0-9][0-9]*\)-link$/\1/p')
      attempted=$(metric_revision nixos-attempted.prom)
      applied=$(metric_revision nixos-applied.prom)
      effective=$(metric_revision nixos-effective.prom)
      built_from=$(awk -v g="$current" '$1 == g {print $2}' "$ledger" 2>/dev/null | tail -n1)

      echo "${config.networking.hostName} runs generation ''${current:-unknown}"
      if [ -n "$built_from" ]; then
        echo "  built from:  main@$(short "$built_from")  ($built_from)"
      else
        echo "  built from:  no revision recorded — this generation was built outside the pull loop (an image's first boot, or sfi-nixos-rebuild --from)"
      fi
      echo

      echo "revisions of ${config.networking.hostName}:"
      printf '  %-10s %s   %s\n' effective "$(short "$effective")" \
        "what the running closure was built from; recorded only after a successful switch"
      printf '  %-10s %s   %s\n' applied "$(short "$applied")" \
        "what the host has caught up to; a fleet-only commit advances this without a rebuild"
      printf '  %-10s %s   %s\n' attempted "$(short "$attempted")" \
        "what the gate last tried"
      echo

      # The three comparisons that mean something, said plainly. Anything
      # else is normal and gets no line.
      if [ -n "$attempted" ] && [ "$applied" != "$attempted" ]; then
        echo "applied is behind attempted: the last run FAILED. journalctl -u nixos-upgrade, then 'sudo sfi-nixos-rebuild' to retry."
      fi
      if [ -n "$effective" ] && [ -n "$applied" ] && [ "$effective" != "$applied" ]; then
        echo "effective is behind applied: normal — every commit since the last closure-relevant one was fleet-only."
      fi

      stamp=$(sed -n 's/^node_nixos_effective_timestamp_seconds \([0-9][0-9]*\).*$/\1/p' \
        ${textfileDir}/nixos-effective.prom 2>/dev/null | tail -n1)
      if [ -n "$stamp" ]; then
        now=$(date +%s)
        age=$(( (now - stamp) / 3600 ))
        printf 'last switch: %s (%sh ago)\n' "$(date -u -d "@$stamp" +%Y-%m-%dT%H:%M:%SZ)" "$age"
      else
        echo "last switch: not recorded — no switch has finished on this host since the metric existed"
      fi

      if [ -e "$pin" ]; then
        echo "pin: generation $(cat "$pin") — nixos-upgrade SKIPS while the host is on it (sudo sfi-nixos-rollback --lift, or sudo sfi-nixos-rebuild)"
      else
        echo "pin: none — the loop follows main"
      fi

      printf 'loop: nixos-upgrade.service %s/%s, nixos-upgrade.timer %s\n' \
        "$(systemctl show -p ActiveState --value nixos-upgrade.service)" \
        "$(systemctl show -p Result --value nixos-upgrade.service)" \
        "$(systemctl show -p ActiveState --value nixos-upgrade.timer)"
      echo "generations on disk: sudo sfi-nixos-rollback (no arguments lists them with the commit each was built from)"
    '';
  };
}
