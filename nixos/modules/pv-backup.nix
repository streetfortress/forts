# restic backups of k3s persistent volumes to S3-compatible endpoints —
# layer 3 of the backup strategy (docs/infra/backups.md). CNPG owns the
# database layer (barman, physical + WAL) and databasus the logical one;
# what is left is every other PVC on the node, and on a fort that is the
# app content in the fort-local rustfs, which is deliberately backed up
# as a *volume* rather than replicated bucket-to-bucket.
#
# Two ways to configure it, and the difference is the point:
#
#   sfi.pv-backup.profile = "lab" | "production"
#     A fort. The profile names a DESTINATION SET, not a target list —
#     `lab` is luxor's rustfs alone (a lab fort is reproducible; it is
#     not an original), `production` is luxor plus B2 (restic is the one
#     layer that fans out natively, so a production fort's offsite copy
#     does not depend on the lab being up). Endpoints, bucket and the
#     sops key names all follow; a fort declares intent and nothing else.
#
#   sfi.pv-backup.targets = [ … ]
#     A hand-configured host. zenbook is neither profile: it is the lab's
#     crown jewel (gitea's repo PVC and databasus' dumps) rather than a
#     fort, and it alternates its two targets 12h apart. The list stays
#     the primitive; `profile` is sugar that fills it.
#
# THE PRUNE GAP, on purpose. A fort's B2 credential deliberately holds no
# `deleteFiles` (docs/infra/backups.md: a compromised fort must not be
# able to erase the offsite copy — an observed pair of 401s, not a
# citation). `restic forget --prune` deletes pack files of any age, so it
# cannot run on the fort: the repo would fail every cycle. Profile-driven
# hosts therefore back up and never prune, and retention is owed by a
# central job holding the one delete-capable identity in the fleet. Until
# that job exists a fort's repo grows monotonically — a known gap with an
# owner, not an oversight. Hosts configuring `targets` by hand keep their
# own pruneOpts, because their credentials can delete.
#
# Crash-consistent by nature: a PVC is copied while pods write to it.
# That is the right trade for content and for a gitea data dir; it is the
# wrong trade for a database, which is why sites-db is excluded below and
# CNPG owns that layer.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.pv-backup;

  # Fleet facts, not host facts: there are exactly two object stores, and
  # enumerating them per host is how they drift. Overridable for a
  # rehearsal against a scratch endpoint.
  defaultEndpoints = {
    # luxor's rustfs-central — the fleet backup destination, NodePort 30901
    # opened on wg0 only. Plain HTTP over
    # the mesh, path-style (docs/infra/providers.md).
    rustfs = "http://10.1.0.5:30901";
    b2 = "https://s3.us-west-004.backblazeb2.com";
  };

  # One repo per host under the bucket's universal `<fort>/` prefix,
  # which is also what a fort's B2 key is restricted to.
  repo = endpoint: "s3:${endpoint}/${cfg.bucket}/${cfg.prefix}";

  profileTargets = {
    lab = [
      {
        name = "rustfs";
        repository = repo cfg.endpoints.rustfs;
        environmentFile = config.sops.secrets.restic-s3-env-rustfs.path;
      }
    ];
    production = [
      {
        name = "rustfs";
        repository = repo cfg.endpoints.rustfs;
        environmentFile = config.sops.secrets.restic-s3-env-rustfs.path;
      }
      {
        name = "b2";
        repository = repo cfg.endpoints.b2;
        environmentFile = config.sops.secrets.restic-s3-env-b2.path;
        # Offsite runs on the opposite half of the cycle: the two targets
        # read the same PVCs, and overlapping them doubles the read load
        # on a 4 GB fort for no gain.
        timerConfig = {
          OnCalendar = "*-*-* 03/6:00:00";
          Persistent = "true";
          RandomizedDelaySec = toString cfg.randomizedDelaySec;
        };
      }
    ];
  };

  # Profile targets are plain attrsets — they never pass through the
  # `targets` submodule, so they carry none of its defaults. Normalize
  # both paths here or every optional field is a missing-attribute error
  # at use (caught by evaluating the profile path with no host consuming
  # it yet).
  normalize = t: { passwordFile = null; pruneOpts = null; timerConfig = null; } // t;

  targets = map normalize
    (if cfg.profile != null then profileTargets.${cfg.profile} else cfg.targets);

  # A pattern list as a shell `case` arm. Deliberately UNQUOTED: a
  # quoted arm in `case` is a literal, not a glob, so escaping here would
  # silently stop every exclude from matching.
  caseArm = lib.concatStringsSep "|";

  # The path list, recomputed each run: PVC directories come and go with
  # the workloads, so a static list would silently stop covering a new
  # one. Include-all minus the derived, rather than an explicit include
  # list — a new stateful app that is silently UNPROTECTED is the failure
  # that cannot be detected, and one needlessly backed up is the failure
  # that can.
  pathScript = pkgs.writeShellScript "pv-backup-paths" ''
    set -eu
    emit() {
      [ -d "$1" ] || return 0
      name=''${1##*/}
      ${lib.optionalString (cfg.exclude != []) ''
        case "$name" in ${caseArm cfg.exclude}) return 0 ;; esac
      ''}
      printf '%s\n' "$1"
    }
    ${if cfg.includePatterns == [] then ''
      for d in ${cfg.storagePath}/*; do emit "$d"; done
    '' else lib.concatMapStringsSep "\n"
      (pat: ''for d in ${cfg.storagePath}/${pat}; do emit "$d"; done'')
      cfg.includePatterns}
  '';

  # ExecCondition gate: a fort with no PVCs has nothing to protect, and
  # that is neither success nor failure. Without this, restic exits
  # "Fatal: nothing to backup" and the unit goes red — on EVERY newly
  # raised fort, until its first stateful workload lands (sfi-fort-d54d5b3,
  # 2026-09-16). A permanently red backup unit on a fresh fort is how you
  # learn to ignore red backup units.
  #
  # systemd's own vocabulary says it properly (auto-rebuild.nix uses the
  # same gate): exit 1 is a clean "condition failed" skip, not a failure.
  #
  # It also publishes the path COUNT, which is what lets an alert tell
  # "nothing to back up yet" apart from "backups are broken" — the
  # freshness metric alone cannot, because a skipped run writes no
  # timestamp and looks exactly like a stuck one.
  condition = name: pkgs.writeShellScript "pv-backup-condition-${name}" ''
    set -eu
    n=$(${pathScript} | ${pkgs.coreutils}/bin/wc -l)
    dir=/var/lib/node-exporter-textfiles
    tmp=$(${pkgs.coreutils}/bin/mktemp "$dir/.pv-backup-paths-${name}.prom.XXXXXX")
    {
      echo "# HELP node_pv_backup_paths PVC directories this target would include."
      echo "# TYPE node_pv_backup_paths gauge"
      echo "node_pv_backup_paths{target=\"${name}\"} $n"
    } > "$tmp"
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp"
    ${pkgs.coreutils}/bin/mv "$tmp" "$dir/pv-backup-paths-${name}.prom"
    if [ "$n" -eq 0 ]; then
      echo "no PVC directories under ${cfg.storagePath} — nothing to back up yet, skipping"
      exit 1
    fi
  '';

  # The cheap detector the strategy calls for: freshness, published to
  # the textfile collector so a stale repo alerts like anything else.
  # Silent failure is the default state of every backup system, and the
  # age of the newest snapshot is the only signal that catches it before
  # a restore does. ExecStartPost runs only when the backup succeeded.
  freshness = name: pkgs.writeShellScript "pv-backup-freshness-${name}" ''
    set -eu
    dir=/var/lib/node-exporter-textfiles
    tmp=$(${pkgs.coreutils}/bin/mktemp "$dir/.pv-backup-${name}.prom.XXXXXX")
    {
      echo "# HELP node_pv_backup_last_success_seconds Unix time of the last successful restic backup run."
      echo "# TYPE node_pv_backup_last_success_seconds gauge"
      echo "node_pv_backup_last_success_seconds{target=\"${name}\"} $(${pkgs.coreutils}/bin/date +%s)"
    } > "$tmp"
    # 0644 or node_exporter (unprivileged) turns every file in the
    # directory into node_textfile_scrape_error 1 — see textfile-metrics.nix.
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp"
    ${pkgs.coreutils}/bin/mv "$tmp" "$dir/pv-backup-${name}.prom"
  '';
in
{
  # The ops verbs come with the layer: a fort that backs up gets
  # `sfi-backup-now` and `sfi-backup-status` on the intervention ladder,
  # because the alternative is console break-glass to answer "did it run"
  # (found on the sfi-fort-d54d5b3 drill).
  imports = [ ./textfile-metrics.nix ./ops/backups.nix ];

  options.sfi.pv-backup = {
    enable = lib.mkEnableOption "restic backups of k3s persistent volumes";

    profile = lib.mkOption {
      type = lib.types.nullOr (lib.types.enum [ "lab" "production" ]);
      default = null;
      description = ''
        The destination set for a fort. `lab` backs up to luxor's rustfs
        only; `production` adds B2 so the offsite copy does not depend on
        the lab being reachable. Mutually exclusive with `targets`, which
        is what a hand-configured host uses instead.
      '';
    };

    prefix = lib.mkOption {
      type = lib.types.str;
      default = config.networking.hostName;
      defaultText = lib.literalExpression "config.networking.hostName";
      description = ''
        The repo's prefix inside the bucket, and the `namePrefix` a
        fort's B2 key is restricted to. The hostname by default: a fort
        is never renamed, and a rebirth deliberately starts a fresh
        prefix that ages out on its own retention.
      '';
    };

    bucket = lib.mkOption {
      type = lib.types.str;
      default = "sfi-pv-backups";
      description = "Bucket name, identical on every tier so a destination change is an endpoint and a credential.";
    };

    endpoints = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = defaultEndpoints;
      description = "S3 endpoints the profiles resolve against.";
    };

    storagePath = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/rancher/k3s/storage";
      description = "The local-path provisioner's storage directory.";
    };

    includePatterns = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "*_gitea_gitea-shared-storage" ];
      description = ''
        Globs matched against PVC directory names. EMPTY MEANS EVERY PVC,
        which is the intended posture on a fort: opting in by name is how
        a new stateful workload ends up silently unprotected. Set it only
        where the host's job is to back up specific volumes (zenbook).
        `exclude` applies either way.
      '';
    };

    exclude = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [
        # CNPG owns physical + WAL through barman; restic-ing a live
        # PGDATA produces a copy nobody should ever restore from.
        "*_sites-db_*"
        # Derived: the TSDB is rebuilt by remote_write, chunks by
        # re-ingest. Backing them up buys a slower version of both.
        "*_monitoring_*"
        "*_loki_*"
        # rustfs's LOG volume. Its data volume is deliberately NOT
        # excluded — that is the app-content backup.
        "*rustfs*log*"
      ];
      description = "Globs matched against PVC directory names; matches are skipped.";
    };

    schedule = lib.mkOption {
      type = lib.types.str;
      default = "*-*-* 00/6:00:00";
      description = "Default OnCalendar for a target that does not override it.";
    };

    randomizedDelaySec = lib.mkOption {
      type = lib.types.int;
      default = 900;
      description = ''
        Timer jitter. Every fort backs up to the same rustfs on the same
        schedule; without jitter the fleet arrives at luxor together and
        the ingest spike grows with the fort count.
      '';
    };

    passwordFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = "Repo password, shared across targets. Defaults to the `restic-password` sops secret when a profile is set.";
    };

    pruneOpts = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = ''
        Retention flags for `restic forget --prune`. EMPTY BY DEFAULT and
        empty on every fort: pruning deletes pack files, a fort's
        credential holds no `deleteFiles`, and a prune it cannot perform
        fails the whole unit. Retention for fort repos is owed by a
        central job. Hosts whose credentials can delete set it.
      '';
    };

    targets = lib.mkOption {
      default = [ ];
      description = "Explicit destination list, for hosts that are not forts. Mutually exclusive with `profile`.";
      type = lib.types.listOf (lib.types.submodule {
        options = {
          name = lib.mkOption {
            type = lib.types.str;
            description = "Identifier for this target; names the systemd unit and the freshness metric.";
          };
          repository = lib.mkOption {
            type = lib.types.str;
            example = "s3:https://s3.zen.lofi/sfi-pv-backups/zenbook";
            description = "restic S3 repository URL.";
          };
          environmentFile = lib.mkOption {
            type = lib.types.path;
            description = "File defining AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY (and RESTIC_PASSWORD if no passwordFile).";
          };
          passwordFile = lib.mkOption {
            type = lib.types.nullOr lib.types.path;
            default = null;
            description = "Per-target override.";
          };
          pruneOpts = lib.mkOption {
            type = lib.types.nullOr (lib.types.listOf lib.types.str);
            default = null;
            description = "Per-target override; null takes the top-level value.";
          };
          timerConfig = lib.mkOption {
            type = lib.types.nullOr (lib.types.attrsOf lib.types.str);
            default = null;
            description = "Per-target override; null takes the top-level schedule.";
          };
        };
      });
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = (cfg.profile != null) != (cfg.targets != [ ]);
        message = ''
          sfi.pv-backup: set exactly one of `profile` (a fort — the
          destination set follows) or `targets` (a hand-configured host).
        '';
      }
      {
        assertion = builtins.all (t: cfg.passwordFile != null || t.passwordFile != null) targets;
        message = ''
          sfi.pv-backup: every target needs a password source — set
          `passwordFile`, or a per-target one, or RESTIC_PASSWORD in the
          target's environmentFile with a dummy top-level passwordFile.
        '';
      }
    ];

    # Declared here so a fort's secrets.nix needs no edit, and declared
    # CONDITIONALLY: a declared-but-absent sops key fails activation, and
    # a lab fort has no B2 credential to declare.
    sops.secrets = lib.mkIf (cfg.profile != null) (
      {
        restic-password = { };
        restic-s3-env-rustfs = { };
      }
      // lib.optionalAttrs (cfg.profile == "production") { restic-s3-env-b2 = { }; }
    );

    sfi.pv-backup.passwordFile =
      lib.mkIf (cfg.profile != null) (lib.mkDefault config.sops.secrets.restic-password.path);

    environment.systemPackages = [ pkgs.restic ];

    services.restic.backups = builtins.listToAttrs (map
      (target: {
        name = "pv-${target.name}";
        value = {
          repository = target.repository;
          environmentFile = target.environmentFile;
          passwordFile = if target.passwordFile != null then target.passwordFile else cfg.passwordFile;
          dynamicFilesFrom = toString pathScript;
          pruneOpts = if target.pruneOpts != null then target.pruneOpts else cfg.pruneOpts;
          timerConfig =
            if target.timerConfig != null then target.timerConfig
            else {
              OnCalendar = cfg.schedule;
              Persistent = "true";
              RandomizedDelaySec = toString cfg.randomizedDelaySec;
            };
          initialize = true;
        };
      })
      targets);

    systemd.services = builtins.listToAttrs (map
      (target: {
        name = "restic-backups-pv-${target.name}";
        value.serviceConfig = {
          ExecCondition = toString (condition target.name);
          ExecStartPost = toString (freshness target.name);
        };
      })
      targets);
  };
}
