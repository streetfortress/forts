# sfi-systemd-restart: unit restart over a per-host allowlist (#212).
#
#   sudo sfi-systemd-restart                    # menu: the allowlist + state
#   sudo sfi-systemd-restart systemd-resolved
#   sudo sfi-systemd-restart flux-bootstrap
#
# Why: 2026-09-13, a hub's systemd-resolved parked itself on 8.8.8.8 after
# a dnsmasq blip and every .lofi name was NXDOMAIN for 12h+; the fix was
# one `systemctl restart` and there was no way to run it — production
# hosts have no root shell and the NUDGE rung only knew flux and workload
# verbs. Same shape since: wg-quick-wg0 after a peer rollout, k3s after a
# cached-cert change, flux-bootstrap wedged on day 0 (#408 — the fort had
# to be rebooted to retry a oneshot). A restart re-runs what git already
# configured: machinery, never desired state — squarely the NUDGE rung.
#
# The allowlist is `sfi.ops.restartableUnits`. It is a list option, so it
# merges: the module that OWNS a unit contributes it (k3s-server.nix adds
# k3s, wireguard.nix adds wg-quick-wg0, auto-rebuild.nix adds
# nixos-upgrade, flux-bootstrap.nix adds flux-bootstrap), a host may add
# more in its default.nix, and a host that runs none of them gets no verb
# at all. Never arbitrary units: the argument is string-compared against
# the allowlist and the allowlist's own spelling is what reaches
# systemctl (the flux.nix/workloads.nix rule — operator input selects
# from a menu, it is never interpolated).
#
# `systemctl restart` is synchronous: for a oneshot (nixos-upgrade,
# flux-bootstrap) that means it returns when the run finishes, and the
# verb's exit status is the unit's. Either way the tail of `systemctl
# status` is echoed so the journal shows the outcome next to the sudo
# line that decided it.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.ops;

  # Bare names mean services; anything with a suffix is taken as typed.
  qualify = u: if lib.hasInfix "." u then u else "${u}.service";
  units = lib.unique (map qualify cfg.restartableUnits);
in
{
  imports = [ ./ops-commands.nix ];

  options.sfi.ops.restartableUnits = lib.mkOption {
    type = lib.types.listOf lib.types.str;
    default = [ ];
    example = [ "systemd-resolved" "wg-quick-wg0" "k3s" "nixos-upgrade" ];
    description = ''
      systemd units `sudo sfi-systemd-restart <unit>` may restart on this
      host. Merged across modules: the module that owns a unit adds it,
      hosts may add more. Empty list = no verb.
    '';
  };

  config = lib.mkIf (units != [ ]) {
    sfi.ops.commands.systemd-restart = {
      description = "Restart an allowlisted systemd unit: sfi-systemd-restart <unit>; synchronous, echoes the unit's status after; no args lists the allowlist";
      runtimeInputs = [ pkgs.systemd ];
      script = ''
        allowed=(${lib.concatMapStringsSep " " lib.escapeShellArg units})

        usage() {
          echo "usage: sudo sfi-systemd-restart <unit>   (allowlisted units only; no args lists them)"
        }

        if [ "$#" -eq 0 ]; then
          usage
          echo "restartable units on this host:"
          for u in "''${allowed[@]}"; do
            printf '  %-28s %s/%s\n' "$u" \
              "$(systemctl show -p ActiveState --value "$u")" \
              "$(systemctl show -p SubState --value "$u")"
          done
          exit 0
        fi
        if [ "$#" -ne 1 ]; then
          usage >&2
          exit 64
        fi

        # Compare only; the value handed to systemctl is the allowlist's.
        requested=$1
        case "$requested" in
          *.*) ;;
          *) requested="$requested.service" ;;
        esac
        unit=""
        for u in "''${allowed[@]}"; do
          if [ "$requested" = "$u" ]; then
            unit=$u
          fi
        done
        if [ -z "$unit" ]; then
          echo "error: '$1' is not in this host's restart allowlist — run without args for the menu" >&2
          exit 1
        fi

        echo "restarting $unit ..."
        rc=0
        systemctl restart "$unit" || rc=$?
        systemctl status --no-pager --lines=8 "$unit" || true
        if [ "$rc" -ne 0 ]; then
          echo "error: restart of $unit exited $rc — see the status above / journalctl -u $unit" >&2
        fi
        exit "$rc"
      '';
    };
  };
}
