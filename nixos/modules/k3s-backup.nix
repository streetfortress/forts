# Restic backups for k3s persistent volumes to S3-compatible endpoints
{ config, lib, pkgs, ... }:

let
  cfg = config.services.k3s-backup;
in
{
  options.services.k3s-backup = {
    enable = lib.mkEnableOption "restic backups of k3s persistent volumes to S3";

    storagePath = lib.mkOption {
      type = lib.types.path;
      default = "/var/lib/rancher/k3s/storage";
      description = "Path to the k3s local-path provisioner storage directory.";
    };

    passwordFile = lib.mkOption {
      type = lib.types.nullOr lib.types.path;
      default = null;
      description = ''
        Path to a file containing the restic repository password.
        Shared across all S3 targets unless overridden per-target.
        If null, each target's environmentFile must set RESTIC_PASSWORD.
      '';
    };

    timerConfig = lib.mkOption {
      type = lib.types.attrsOf lib.types.str;
      default = {
        OnCalendar = "*-*-* 00/6:00:00";
        Persistent = "true";
      };
      description = "Systemd timer configuration for the backup schedule.";
    };

    pruneOpts = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [
        "--keep-daily 7"
        "--keep-weekly 4"
        "--keep-monthly 6"
      ];
      description = "Restic forget/prune retention policy flags.";
    };

    includePatterns = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [];
      description = ''
        Shell glob patterns matched against PVC directory names.
        Only directories matching at least one pattern are backed up.
        When empty, all PVCs under storagePath are included.
      '';
      example = [ "*_gitea_gitea-shared-storage" "*_databasus_*" ];
    };

    exclude = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [];
      description = "Patterns to exclude from backup.";
    };

    s3Targets = lib.mkOption {
      default = [];
      description = "List of S3-compatible endpoints to back up to.";
      type = lib.types.listOf (lib.types.submodule {
        options = {
          name = lib.mkOption {
            type = lib.types.str;
            description = "Unique identifier for this backup target (used in systemd unit names).";
            example = "offsite";
          };

          repository = lib.mkOption {
            type = lib.types.str;
            description = "Restic S3 repository URL.";
            example = "s3:https://s3.us-east-1.amazonaws.com/my-bucket";
          };

          environmentFile = lib.mkOption {
            type = lib.types.path;
            description = ''
              Path to an environment file containing S3 credentials.
              Must define AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
              May also define RESTIC_PASSWORD if no top-level passwordFile is set.
            '';
          };

          passwordFile = lib.mkOption {
            type = lib.types.nullOr lib.types.path;
            default = null;
            description = "Per-target override for the restic password file.";
          };

          pruneOpts = lib.mkOption {
            type = lib.types.nullOr (lib.types.listOf lib.types.str);
            default = null;
            description = "Per-target override for retention policy. Uses top-level pruneOpts if null.";
          };

          timerConfig = lib.mkOption {
            type = lib.types.nullOr (lib.types.attrsOf lib.types.str);
            default = null;
            description = "Per-target override for the backup schedule.";
          };
        };
      });
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.s3Targets != [];
        message = "services.k3s-backup.s3Targets must contain at least one target.";
      }
      {
        assertion = builtins.all
          (t: cfg.passwordFile != null || t.passwordFile != null)
          cfg.s3Targets;
        message = ''
          Each S3 target must have a password source: set either
          services.k3s-backup.passwordFile (shared) or per-target passwordFile.
          Alternatively, set RESTIC_PASSWORD in each target's environmentFile
          and set the top-level passwordFile to a dummy file.
        '';
      }
    ];

    environment.systemPackages = [ pkgs.restic ];

    services.restic.backups = let
      useInclude = cfg.includePatterns != [];
      # Build a shell snippet that expands includePatterns globs into matching PVC paths
      matchScript = lib.concatMapStringsSep "\n"
        (pat: "for d in ${cfg.storagePath}/${pat}; do [ -d \"$d\" ] && echo \"$d\"; done")
        cfg.includePatterns;
    in builtins.listToAttrs (map (target: {
      name = "k3s-pv-${target.name}";
      value = {
        repository = target.repository;
        environmentFile = target.environmentFile;
        passwordFile = if target.passwordFile != null then target.passwordFile else cfg.passwordFile;
        paths = lib.mkIf (!useInclude) [ cfg.storagePath ];
        dynamicFilesFrom = lib.mkIf useInclude (toString (pkgs.writeShellScript "k3s-backup-paths" matchScript));
        exclude = cfg.exclude;
        pruneOpts = if target.pruneOpts != null then target.pruneOpts else cfg.pruneOpts;
        timerConfig = if target.timerConfig != null then target.timerConfig else cfg.timerConfig;
        initialize = true;
      };
    }) cfg.s3Targets);
  };
}
