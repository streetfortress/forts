# restic backups of k3s persistent volumes to S3-compatible endpoints —
# layer 3 of the backup strategy (docs/infra/backups.md). CNPG owns the
# database layer (barman, physical + WAL) and databasus the logical one;
# what is left is every other PVC on the node, and on a fort that is the
# app content in the fort-local rustfs, which is deliberately backed up
# as a *volume* rather than replicated bucket-to-bucket.
#
# A target names a STORE, and the store decides the rest:
#
#   sfi.pv-backup.targets = [
#     { store = "b2"; every = "6h"; }
#     { store = "rustfs-central"; every = "weekly"; }
#   ];
#
#   What `fort scaffold` renders from the manifest's [[backups.pv]] blocks
#   (sfi/forts#39), word for word: the store and the cadence are the
#   block's, and the endpoint, the bucket, the credential (the sops key
#   `fort mint` seeds) and the PHASE — which hour of the cycle each store
#   runs at, so two stores on one fort never read the PVCs together — are
#   fleet facts, written once below. There are exactly two object stores
#   at fleet level, and enumerating endpoints per host is how they drift.
#
#   { name = …; repository = …; environmentFile = …; }
#     A hand-configured target on a host that is not a fort — the
#     pre-fort zenbook named its own two, 12 h apart. Every host in the
#     fleet is a fort today, but the explicit form stays the primitive;
#     a store target is sugar that fills it.
#
# The destination SET used to be one word, `profile = "lab" | "production"`,
# which bundled the stores and the cadences and could not say what the
# production fort wants (no PV leg at all) or what the hub wants (B2
# alone). The manifest's per-layer blocks replaced it (sfi/forts#4, #39),
# and this module took the same shape.
#
# THE PRUNE GAP, on purpose. A fort's B2 credential deliberately holds no
# `deleteFiles` (docs/infra/backups.md: a compromised fort must not be
# able to erase the offsite copy — an observed pair of 401s, not a
# citation). `restic forget --prune` deletes pack files of any age, so it
# cannot run on the fort: the repo would fail every cycle. Store targets
# therefore back up and never prune, and retention is owed by a central
# job holding the one delete-capable identity in the fleet (sfi.pv-prune
# on the hub). Hand-configured targets keep their own pruneOpts, because
# their credentials can delete.
#
# Crash-consistent by nature: a PVC is copied while pods write to it.
# That is the right trade for content and for a gitea data dir; it is the
# wrong trade for a database, which is why the fort's postgres is excluded
# below and CNPG owns that layer.
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
    "rustfs-central" = "http://10.1.0.5:30901";
    b2 = "https://s3.us-west-004.backblazeb2.com";
  };

  # One repo per host under the bucket's universal `<fort>/` prefix,
  # which is also what a fort's B2 key is restricted to.
  repo = endpoint: "s3:${endpoint}/${cfg.bucket}/${cfg.prefix}";

  # The store registry, the restic half (the fort CLI holds the other:
  # which layers a store takes, and whether it needs a hub). Per store:
  #
  #   unit     the target's name — the systemd unit `restic-backups-pv-<unit>`,
  #            the `target=` label on every metric, what `sfi-backup-now`
  #            takes. `rustfs`, not `rustfs-central`: the unit and the
  #            label predate the store name, and the fleet rules
  #            (rules-edge.yaml, target=rustfs|b2) and the pruner read
  #            them.
  #   secret   the sops key `fort mint` seeds for the store, declared
  #            below only when a target names the store — a declared-
  #            but-absent sops key fails activation.
  #   phase    which hour of each 6 h cycle (or of the day, or of Sunday)
  #            the store's run starts at. Offsite runs on the opposite
  #            half of the cycle from the hub copy: the two read the same
  #            PVCs, and overlapping them doubles the read load on a
  #            4 GB fort for no gain. The rustfs phase also keeps Sunday's
  #            weekly copy ahead of pv-prune's rustfs tier (Sun 02:00) and
  #            clear of the barman base-backup hours.
  stores = {
    "rustfs-central" = { unit = "rustfs"; secret = "restic-s3-env-rustfs"; phase = 0; };
    b2 = { unit = "b2"; secret = "restic-s3-env-b2"; phase = 3; };
  };

  # The cadence vocabulary (the manifest's `every`, sfi/forts#4): the
  # OnCalendar for a phase, and the staleness threshold that travels with
  # it — twice the cadence, as maxAge documents — so PVBackupStale never
  # judges a weekly copy by a 6-hourly clock.
  cadences = {
    "6h" = { calendar = phase: "*-*-* ${lib.fixedWidthNumber 2 phase}/6:00:00"; maxAge = 36 * 3600; };
    daily = { calendar = phase: "*-*-* ${lib.fixedWidthNumber 2 phase}:30:00"; maxAge = 48 * 3600; };
    weekly = { calendar = phase: "Sun *-*-* ${lib.fixedWidthNumber 2 phase}:30:00"; maxAge = 8 * 86400; };
  };

  isStore = t: t.store != null;
  storeTargets = builtins.filter isStore cfg.targets;
  storesInUse = lib.unique (map (t: t.store) storeTargets);

  # A store target, filled: the submodule's own fields win where set
  # (a host that wants its B2 copy on another timer says so), the store
  # and the cadence supply the rest. A hand target passes through.
  resolve = t:
    if !isStore t then t else
    let
      st = stores.${t.store};
      every = if t.every != null then t.every else "6h";
      c = cadences.${every};
    in
    t // {
      name = if t.name != null then t.name else st.unit;
      repository = if t.repository != null then t.repository else repo cfg.endpoints.${t.store};
      environmentFile = if t.environmentFile != null then t.environmentFile else config.sops.secrets.${st.secret}.path;
      timerConfig =
        if t.timerConfig != null then t.timerConfig
        else {
          OnCalendar = c.calendar st.phase;
          Persistent = "true";
          RandomizedDelaySec = toString cfg.randomizedDelaySec;
        };
      maxAge = if t.maxAge != null then t.maxAge else c.maxAge;
    };

  targets = map resolve cfg.targets;

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
  # timestamp and looks exactly like a stuck one. And the target's MAX
  # AGE: the staleness threshold belongs to the unit that knows its own
  # timer, not to a fleet rule guessing the cadence — PVBackupStale in
  # rules-edge.yaml compares against this series.
  condition = name: maxAge: pkgs.writeShellScript "pv-backup-condition-${name}" ''
    set -eu
    n=$(${pathScript} | ${pkgs.coreutils}/bin/wc -l)
    dir=/var/lib/node-exporter-textfiles
    tmp=$(${pkgs.coreutils}/bin/mktemp "$dir/.pv-backup-paths-${name}.prom.XXXXXX")
    {
      echo "# HELP node_pv_backup_paths PVC directories this target would include."
      echo "# TYPE node_pv_backup_paths gauge"
      echo "node_pv_backup_paths{target=\"${name}\"} $n"
      echo "# HELP node_pv_backup_max_age_seconds Oldest a successful run may be before this target counts as stale (2x its cadence)."
      echo "# TYPE node_pv_backup_max_age_seconds gauge"
      echo "node_pv_backup_max_age_seconds{target=\"${name}\"} ${toString maxAge}"
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

    prefix = lib.mkOption {
      type = lib.types.str;
      default = config.networking.hostName;
      defaultText = lib.literalExpression "config.networking.hostName";
      description = ''
        The repo's prefix inside the bucket, and the `namePrefix` a
        fort's B2 key is restricted to. The hostname by default: a fort
        is never renamed, and a re-provision deliberately starts a fresh
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
      description = "S3 endpoints by store name, what a store target's repository resolves against.";
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
        where the host's job is to back up specific volumes.
        `exclude` applies either way.
      '';
    };

    exclude = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [
        # CNPG owns physical + WAL through barman; restic-ing a live
        # PGDATA produces a copy nobody should ever restore from. The
        # Cluster is `postgres` in the fort's `forts` namespace, beside
        # valkey and rustfs (deployments#671), so the match is the PVC
        # name, not the namespace: rustfs's data volume in the same
        # namespace IS the content backup and must stay in.
        "*_forts_postgres-*"
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
      description = "Default OnCalendar for a hand-configured target that does not override it. A store target's calendar is its cadence's.";
    };

    maxAge = lib.mkOption {
      type = lib.types.int;
      default = 36 * 3600;
      description = ''
        Seconds a hand-configured target's last success may age before
        PVBackupStale fires, published as node_pv_backup_max_age_seconds.
        Twice the cadence: 36 h for the 6-hourly default (the timers carry
        jitter). A store target's threshold is its cadence's — a weekly
        copy carries 8 d.
      '';
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
      description = "Repo password, shared across targets. Defaults to the `restic-password` sops secret when any target names a store.";
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
      example = lib.literalExpression ''
        [
          { store = "b2"; every = "6h"; }
          { store = "rustfs-central"; every = "weekly"; }
        ]
      '';
      description = ''
        The destinations, one target per store (what `fort scaffold`
        renders from the manifest's [[backups.pv]] blocks), or explicit
        `name` + `repository` + `environmentFile` triples on a host that
        is not a fort. A store target's name, repository, credential and
        timer follow from the store and the cadence; set any of them to
        override.
      '';
      type = lib.types.listOf (lib.types.submodule {
        options = {
          store = lib.mkOption {
            type = lib.types.nullOr (lib.types.enum (builtins.attrNames stores));
            default = null;
            description = "The store this target writes to, by its fleet name. Fills name, repository, environmentFile, timerConfig and maxAge.";
          };
          every = lib.mkOption {
            type = lib.types.nullOr (lib.types.enum (builtins.attrNames cadences));
            default = null;
            description = "A store target's cadence: 6h (the default), daily or weekly. The store supplies the phase.";
          };
          name = lib.mkOption {
            type = lib.types.nullOr lib.types.str;
            default = null;
            description = "Identifier for this target; names the systemd unit and the freshness metric. Required without `store`.";
          };
          repository = lib.mkOption {
            type = lib.types.nullOr lib.types.str;
            default = null;
            example = "s3:http://10.1.0.5:30901/sfi-pv-backups/sfi-fort-zenbook";
            description = "restic S3 repository URL. Required without `store`.";
          };
          environmentFile = lib.mkOption {
            type = lib.types.nullOr lib.types.path;
            default = null;
            description = "File defining AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY (and RESTIC_PASSWORD if no passwordFile). Required without `store`.";
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
            description = "Per-target override; null takes the cadence's calendar (a store target) or the top-level schedule.";
          };
          maxAge = lib.mkOption {
            type = lib.types.nullOr lib.types.int;
            default = null;
            description = "Per-target override; null takes the cadence's threshold (a store target) or the top-level maxAge. Set it with a slower timerConfig.";
          };
        };
      });
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.targets != [ ];
        message = ''
          sfi.pv-backup: enabled with no targets. A fort's targets come
          from its manifest's [[backups.pv]] blocks (fort scaffold); a
          fort with none does not import this module at all.
        '';
      }
      {
        assertion = builtins.all (t: isStore t || (t.name != null && t.repository != null && t.environmentFile != null)) cfg.targets;
        message = ''
          sfi.pv-backup: a target names a `store`, or it names all of
          `name`, `repository` and `environmentFile` by hand.
        '';
      }
      {
        assertion = builtins.all (t: isStore t || t.every == null) cfg.targets;
        message = "sfi.pv-backup: `every` is a store target's cadence; a hand target sets timerConfig.";
      }
      {
        assertion = builtins.length storesInUse == builtins.length storeTargets;
        message = "sfi.pv-backup: one target per store — two targets name the same store.";
      }
      {
        assertion = lib.allUnique (map (t: t.name) targets);
        message = "sfi.pv-backup: two targets resolve to the same name; each names its own systemd unit.";
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
    # CONDITIONALLY, per store in use: a declared-but-absent sops key
    # fails activation, and a fort on the hub's rustfs alone has no B2
    # credential to declare — `fort mint` seeds exactly these keys from
    # the same blocks.
    sops.secrets = lib.mkIf (storeTargets != [ ]) (
      { restic-password = { }; }
      // builtins.listToAttrs (map (s: { name = stores.${s}.secret; value = { }; }) storesInUse)
    );

    sfi.pv-backup.passwordFile =
      lib.mkIf (storeTargets != [ ]) (lib.mkDefault config.sops.secrets.restic-password.path);

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
          ExecCondition = toString (condition target.name (if target.maxAge != null then target.maxAge else cfg.maxAge));
          ExecStartPost = toString (freshness target.name);
        };
      })
      targets);
  };
}
