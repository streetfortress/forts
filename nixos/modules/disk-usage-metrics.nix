# Directory-size metrics for the host-level dashboard (#130): node_exporter
# only knows filesystems, so "why is /var growing" isn't answerable from
# stock metrics. A root timer walks a configured path list with du and
# publishes via the textfile collector (#121 pattern). Hourly: du isn't
# free, and disk growth is an hours-scale signal — the filesystem-level
# NodeDiskFillingUp alert stays the fast path.
#
# Resolution is dust-shaped but bounded: each entry carries a du
# --max-depth (0 = total only, 1 = + immediate children), so a panel can
# show "/var/lib/rancher is 40G of /var/lib's 50G" without the unbounded
# label cardinality of a full tree — every emitted directory is a
# Prometheus series, so depth stays a deliberate, per-entry choice
# (defaults land well under ~100 series/host). Interactive drill-down
# below the configured depth is a job for du/dust on the host, not for
# metrics.
#
# PVC sizes get their own metric: local-path-provisioner names its dirs
# pvc-<uid>_<namespace>_<claim>, and kubelet_volume_stats_* is famously
# absent for hostPath-class volumes — so node_pvc_directory_size_bytes
# {namespace, pvc} fills a real k8s observability gap (joinable with
# kube-state-metrics' requests_storage_bytes for used-vs-requested).
#
# --one-file-system keeps a path from silently including a mounted
# volume; nested defaults double-count by design — the panels answer
# "where is the space", not accounting identity. Overlapping entries that
# emit the SAME directory are deduped in the script (first entry wins).
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.disk-usage-metrics;

  entries = map (e: if builtins.isString e then { path = e; depth = 0; } else e) cfg.paths;

  emitCalls = lib.concatMapStrings (e: ''
    emit_tree ${lib.escapeShellArg e.path} ${toString e.depth}
  '') entries;

  collector = pkgs.writeShellScript "disk-usage-metrics" ''
    set -eu
    declare -A seen

    # du exit status (e.g. permission noise) is deliberately ignored via
    # process substitution; a missing path just emits nothing.
    emit_tree() {
      local root="$1" depth="$2"
      [ -d "$root" ] || return 0
      while IFS=$'\t' read -r size dir; do
        if [ -n "''${seen[$dir]:-}" ]; then continue; fi
        seen[$dir]=1
        printf 'node_directory_size_bytes{path="%s"} %s\n' "$dir" "$size"
      done < <(${pkgs.coreutils}/bin/du -B1 --one-file-system --max-depth="$depth" "$root" 2>/dev/null)
    }

    tmp=$(${pkgs.coreutils}/bin/mktemp "$STATE_DIRECTORY/.disk-usage.prom.XXXXXX")
    {
      echo "# TYPE node_directory_size_bytes gauge"
      ${emitCalls}
    ${lib.optionalString (cfg.pvcDir != null) ''
      echo "# TYPE node_pvc_directory_size_bytes gauge"
      if [ -d ${lib.escapeShellArg cfg.pvcDir} ]; then
        while IFS=$'\t' read -r size dir; do
          base="''${dir##*/}"
          if [[ "$base" =~ ^pvc-[0-9a-f-]+_([^_]+)_(.+)$ ]]; then
            printf 'node_pvc_directory_size_bytes{namespace="%s",pvc="%s"} %s\n' \
              "''${BASH_REMATCH[1]}" "''${BASH_REMATCH[2]}" "$size"
          fi
        done < <(${pkgs.coreutils}/bin/du -B1 --one-file-system --max-depth=1 ${lib.escapeShellArg cfg.pvcDir} 2>/dev/null)
      fi
    ''}
    } > "$tmp"
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp" # mktemp gives 0600; the exporter is unprivileged
    ${pkgs.coreutils}/bin/mv "$tmp" "$STATE_DIRECTORY/disk-usage.prom"
  '';

  entryType = with lib.types; either str (submodule {
    options = {
      path = lib.mkOption {
        type = str;
        description = "Directory to size.";
      };
      depth = lib.mkOption {
        type = ints.unsigned;
        default = 0;
        description = ''
          du --max-depth: 0 emits the path's total only; 1 adds one
          sample per immediate child. Each emitted directory is a
          Prometheus series — raise with intent.
        '';
      };
    };
  });
in
{
  imports = [ ./textfile-metrics.nix ];

  options.sfi.disk-usage-metrics = {
    paths = lib.mkOption {
      type = lib.types.listOf entryType;
      default = [
        { path = "/var/log"; depth = 1; }
        { path = "/var/lib"; depth = 1; }
        "/var/lib/rancher/k3s/storage" # total; children are the PVC metric
      ];
      description = ''
        Directories to size hourly into node_directory_size_bytes. Plain
        strings mean depth 0 (total only).
      '';
    };

    pvcDir = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = "/var/lib/rancher/k3s/storage";
      description = ''
        local-path-provisioner storage dir whose pvc-<uid>_<ns>_<claim>
        children are published as node_pvc_directory_size_bytes
        {namespace, pvc}. null disables the PVC metric.
      '';
    };
  };

  config = {
    systemd.services.disk-usage-metrics = {
      description = "Export directory sizes for node_exporter";
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${collector}";
        StateDirectory = "node-exporter-textfiles";
        # du over /var/lib walks a lot of inodes; keep it polite.
        Nice = 10;
        IOSchedulingClass = "idle";
      };
    };

    systemd.timers.disk-usage-metrics = {
      wantedBy = [ "timers.target" ];
      timerConfig = {
        OnBootSec = "5min";
        OnUnitActiveSec = "1h";
      };
    };
  };
}
