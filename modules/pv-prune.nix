# The other half of `sfi.pv-backup`: retention for every fort's restic
# repo, run centrally because the forts themselves cannot do it.
#
# WHY IT IS NOT ON THE FORT. A fort's B2 key deliberately holds no
# `deleteFiles` — verified, not assumed: a writer key writes inside its
# prefix and takes a 401 both outside it and on delete (#47). `restic
# forget --prune` removes pack files of any age, so it cannot run under
# that credential, and `sfi.pv-backup` sets `pruneOpts = [ ]` on every
# profile for exactly that reason. Retention therefore moves to one host
# holding one delete-capable identity per tier. That host is luxor: the
# hub already reaches its own rustfs and the internet, and concentrating
# the only deleting credential in the fleet in one declared place is the
# point rather than a side effect.
#
# WHAT IT PRUNES IS DISCOVERED, NOT CONFIGURED. Repos are the top-level
# prefixes of the bucket — one per fort, `<fort>/` — so the list is read
# from the bucket at run time. A fort born yesterday is pruned tonight
# with no config change, and a retired fort's repo keeps ageing out on
# its own policy instead of being forgotten about. A prefix without a
# restic `config` object is skipped and named, so a stray upload can
# never be handed to `restic forget`.
#
# THE KEEP-LAST FLOOR IS LOAD-BEARING. A repo whose fort stopped backing
# up — broken credential, dead timer, a host nobody noticed was down —
# still ages normally, so a pure age policy walks it to zero snapshots
# and turns a backup *failure* into data *loss*, silently, some weeks
# later. `--keep-last` is the floor that makes that impossible: whatever
# the age rules say, the newest few snapshots survive. It is cheap and it
# is the difference between retention and deletion.
#
# ONE RESTIC PASSWORD ACROSS THE FLEET, by necessity: the pruner opens
# every fort's repo, and per-host passwords would mean a keyring here. The
# exposure is small because reach is governed by the B2 key's prefix
# restriction rather than by the password — a fort cannot address another
# fort's repo at all — but it is a real decision, so `fort mint` must
# treat `restic-password` as a fleet secret copied into each host's file,
# not as a per-host generated value.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.pv-prune;

  defaultTiers = {
    # luxor's rustfs-central, over the mesh NodePort. Short window: the local
    # tier is the operational-recovery copy, and its disk is the fleet's
    # real constraint. Production forts write here WEEKLY (pv-backup.nix's
    # production profile — B2 carries their RPO), so the dailies only ever
    # matter for lab forts, which are reproducible; three is enough to
    # step back over a bad day. `--keep-last` stays the floor.
    rustfs = {
      endpoint = "http://10.1.0.5:30901";
      keep = [ "--keep-last 3" "--keep-daily 3" "--keep-weekly 4" ];
      schedule = "Sun *-*-* 02:00:00";
    };
    # Offsite carries the tail.
    b2 = {
      endpoint = "https://s3.us-west-004.backblazeb2.com";
      region = "us-west-004";
      keep = [ "--keep-last 3" "--keep-daily 14" "--keep-weekly 8" "--keep-monthly 12" ];
      schedule = "Sun *-*-* 04:00:00";
    };
  };

  # A tier switched off declares nothing — no secret, no unit, no timer
  # — so the two can be enabled a key at a time.
  enabledTiers = lib.filterAttrs (_: tier: tier.enable) cfg.tiers;

  pruneScript = tierName: tier: pkgs.writeShellScript "pv-prune-${tierName}" ''
    set -euo pipefail

    # Credentials for both s5cmd (discovery) and restic (the prune).
    set -a
    . ${tier.environmentFile}
    set +a
    export RESTIC_PASSWORD_FILE=${cfg.passwordFile}
    ${lib.optionalString (tier.region != null) "export AWS_REGION=${lib.escapeShellArg tier.region}"}

    endpoint=${lib.escapeShellArg tier.endpoint}
    bucket=${lib.escapeShellArg cfg.bucket}
    failed=0
    pruned=0

    # Top-level prefixes = one restic repo per fort.
    prefixes=$(${pkgs.s5cmd}/bin/s5cmd --endpoint-url "$endpoint" ls "s3://$bucket/" \
      | ${pkgs.gawk}/bin/awk '$1 == "DIR" { print $2 }')

    if [ -z "$prefixes" ]; then
      echo "no repos under s3://$bucket/ — nothing to prune"
    fi

    for prefix in $prefixes; do
      repo=''${prefix%/}

      # Named, not ours: a repo the fleet password does not open. Left
      # in place on purpose (an archive kept readable under its own
      # password); handing it to `forget` is a failed sweep every week.
      case " ${lib.concatStringsSep " " cfg.exclude} " in
        *" $repo "*) echo "skip $repo: excluded (sfi.pv-prune.exclude)"; continue ;;
      esac

      # A restic repo always has a `config` object at its root. Anything
      # else under the bucket is someone else's upload, and handing it to
      # `restic forget` is how a prune becomes an incident.
      if ! ${pkgs.s5cmd}/bin/s5cmd --endpoint-url "$endpoint" ls "s3://$bucket/$repo/config" >/dev/null 2>&1; then
        echo "skip $repo: no restic config object at its root"
        continue
      fi

      echo "== $repo"
      # Clear STALE locks first — `unlock` without --remove-all drops only
      # locks older than 30 min (a live backup refreshes its lock every
      # 5 min, so one stays). A fort's writer could not delete its own
      # lock on rustfs until its policy gained locks/ (the same PR), and
      # a run that dies mid-backup leaves one on either tier; without
      # this the sweep sat out the full retry on every repo and then
      # failed (2026-09-21, the first dry run). Never --remove-all here:
      # a genuinely held lock means a backup in flight, and forgetting
      # under it is how a retention job becomes an incident.
      ${pkgs.restic}/bin/restic -r "s3:$endpoint/$bucket/$repo" unlock || true
      if ${pkgs.restic}/bin/restic -r "s3:$endpoint/$bucket/$repo" \
          forget ${lib.concatStringsSep " " tier.keep} --prune ${lib.optionalString cfg.dryRun "--dry-run"} \
          --retry-lock ${cfg.retryLock}; then
        pruned=$((pruned + 1))
      else
        # A live lock that outlasted the retry, or the tier refusing. The
        # next run is a week away and an interrupted prune is worse than
        # a late one, so fail this repo and move on; a repeat shows up as
        # PVPruneStale on the freshness metric.
        echo "::failed:: $repo"
        failed=$((failed + 1))
      fi
    done

    echo "pruned $pruned repo(s), $failed failure(s)${lib.optionalString cfg.dryRun " [DRY RUN — nothing was deleted]"}"
    [ "$failed" -eq 0 ]
  '';

  # Same contract as pv-backup's freshness metric: a prune that quietly
  # stopped running is a bill, and eventually a full disk on luxor.
  freshness = tierName: pkgs.writeShellScript "pv-prune-freshness-${tierName}" ''
    set -eu
    dir=/var/lib/node-exporter-textfiles
    tmp=$(${pkgs.coreutils}/bin/mktemp "$dir/.pv-prune-${tierName}.prom.XXXXXX")
    {
      echo "# HELP node_pv_prune_last_success_seconds Unix time of the last successful restic prune sweep."
      echo "# TYPE node_pv_prune_last_success_seconds gauge"
      echo "node_pv_prune_last_success_seconds{tier=\"${tierName}\"} $(${pkgs.coreutils}/bin/date +%s)"
    } > "$tmp"
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp"
    ${pkgs.coreutils}/bin/mv "$tmp" "$dir/pv-prune-${tierName}.prom"
  '';
in
{
  imports = [ ./textfile-metrics.nix ];

  options.sfi.pv-prune = {
    enable = lib.mkEnableOption ''
      central retention for the fleet's restic PV repos. Enable on exactly
      one host: this is where the fleet's delete-capable credentials live
    '';

    bucket = lib.mkOption {
      type = lib.types.str;
      default = "sfi-pv-backups";
      description = "The bucket whose top-level prefixes are the repos. Matches `sfi.pv-backup.bucket`.";
    };

    passwordFile = lib.mkOption {
      type = lib.types.path;
      default = config.sops.secrets.restic-password.path;
      defaultText = lib.literalExpression "config.sops.secrets.restic-password.path";
      description = "The fleet-wide restic repo password — the same one every fort was minted with.";
    };

    dryRun = lib.mkOption {
      type = lib.types.bool;
      default = false;
      description = ''
        Report what would be removed and delete nothing. Worth one cycle
        on first enable, and on any retention change: this is the only
        thing in the fleet that deletes backups.
      '';
    };

    exclude = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "zenbook" ];
      description = ''
        Top-level prefixes the sweep names and leaves alone: repos in
        the bucket that are not the fleet's — kept readable under a
        password of their own, so `forget` under the fleet password
        fails on them and fails the whole sweep's freshness metric.
        Say why beside each entry; an excluded repo is never pruned,
        so it either stays for a reason or gets deleted.
      '';
    };

    retryLock = lib.mkOption {
      type = lib.types.str;
      default = "15m";
      description = ''
        How long to wait for a repo lock before giving up. A fort's own
        backup holds it during a run; waiting is right, and force-
        unlocking never is.
      '';
    };

    tiers = lib.mkOption {
      default = { };
      description = ''
        Destinations to sweep, keyed by name. The two the fleet has are
        pre-declared below as defaults a host merges with (`tiers.b2.enable
        = false` leaves the rest of b2 and all of rustfs in place); the
        retention windows differ on purpose — luxor is the
        operational-recovery copy on a constrained disk, B2 carries the
        tail.
      '';
      type = lib.types.attrsOf (lib.types.submodule ({ name, ... }: {
        options = {
          enable = lib.mkOption {
            type = lib.types.bool;
            default = true;
            description = ''
              Sweep this tier. Off, the tier declares nothing — no sops
              secret, no unit, no timer — so a tier whose delete-capable
              key is not yet in the host's file can wait without holding
              the other back. Both default tiers are on.
            '';
          };
          endpoint = lib.mkOption {
            type = lib.types.str;
            description = "S3 endpoint, matching the one `sfi.pv-backup` writes to.";
          };
          region = lib.mkOption {
            type = lib.types.nullOr lib.types.str;
            default = null;
            description = ''
              Exported as AWS_REGION for the sweep. B2 signs against the
              region in its endpoint's hostname; s5cmd, unlike restic,
              does not infer it. Unset for an endpoint that ignores it.
            '';
          };
          environmentFile = lib.mkOption {
            type = lib.types.path;
            default = config.sops.secrets."restic-prune-s3-env-${name}".path;
            defaultText = lib.literalExpression ''config.sops.secrets."restic-prune-s3-env-<tier>".path'';
            description = ''
              AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY for the tier's
              DELETE-capable identity — `prune` on rustfs (declared in
              luxor's provider contract, scoped to sfi-pv-backups and
              every fort's prefix in it) and sfi-b2-pruning-key on B2 — the one credential per tier that
              holds `deleteFiles`, and the reason this module belongs on
              one host.
            '';
          };
          keep = lib.mkOption {
            type = lib.types.listOf lib.types.str;
            description = ''
              `restic forget` retention flags. Always include a
              `--keep-last` floor: a fort that silently stopped backing up
              would otherwise have its last snapshot expire on schedule.
            '';
          };
          schedule = lib.mkOption {
            type = lib.types.str;
            description = "OnCalendar for this tier's sweep.";
          };
        };
      }));
    };
  };

  config = lib.mkMerge [
    # The fleet's two tiers, as defaults: a host overrides a field or
    # switches one off without restating the other.
    { sfi.pv-prune.tiers = lib.mapAttrs (_: lib.mapAttrs (_: lib.mkDefault)) defaultTiers; }
    (lib.mkIf cfg.enable {
    assertions = lib.mapAttrsToList
      (name: tier: {
        assertion = lib.any (k: lib.hasPrefix "--keep-last" k) tier.keep;
        message = ''
          sfi.pv-prune.tiers.${name}.keep must include a `--keep-last N`
          floor. Without it, a repo whose fort stopped backing up ages to
          zero snapshots and a backup failure becomes data loss.
        '';
      })
      enabledTiers;

    sops.secrets = { restic-password = { }; }
      // lib.mapAttrs' (name: _: lib.nameValuePair "restic-prune-s3-env-${name}" { }) enabledTiers;

    systemd.services = lib.mapAttrs'
      (name: tier: lib.nameValuePair "pv-prune-${name}" {
        description = "restic retention sweep for the ${name} tier";
        serviceConfig = {
          Type = "oneshot";
          ExecStart = toString (pruneScript name tier);
          ExecStartPost = toString (freshness name);
          # The sweep walks every fort's repo; one slow tier must not
          # wedge the host's timers forever.
          TimeoutStartSec = "6h";
          PrivateTmp = true;
          ProtectSystem = "strict";
          ProtectHome = true;
          ReadWritePaths = [ "/var/lib/node-exporter-textfiles" ];
          CacheDirectory = "pv-prune";
          Environment = [ "RESTIC_CACHE_DIR=%C/pv-prune" ];
        };
      })
      enabledTiers;

    systemd.timers = lib.mapAttrs'
      (name: tier: lib.nameValuePair "pv-prune-${name}" {
        wantedBy = [ "timers.target" ];
        timerConfig = {
          OnCalendar = tier.schedule;
          Persistent = "true";
          RandomizedDelaySec = "1h";
        };
      })
      enabledTiers;

    environment.systemPackages = [ pkgs.restic pkgs.s5cmd ];
    })
  ];
}
