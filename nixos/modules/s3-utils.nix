# Wrapper scripts for s5cmd with sops-managed S3 credentials

# Usage: (requires sudo to access credentials)
# sudo s5cmd-rustfs ls s3://k3s-backup/ — targets your local rustfs
# sudo s5cmd-offsite ls s3://zen-cluster-backups/ — targets Linode


{ config, pkgs, ... }:

{
  environment.systemPackages = [
    (pkgs.writeShellScriptBin "s5cmd-rustfs" ''
      set -euo pipefail
      set -a
      source ${config.sops.secrets.restic-s3-env-rustfs.path}
      set +a
      exec ${pkgs.s5cmd}/bin/s5cmd --endpoint-url https://s3.zen.lofi "$@"
    '')
    (pkgs.writeShellScriptBin "s5cmd-offsite" ''
      set -euo pipefail
      set -a
      source ${config.sops.secrets.restic-s3-env-offsite.path}
      set +a
      exec ${pkgs.s5cmd}/bin/s5cmd --endpoint-url https://us-sea-1.linodeobjects.com "$@"
    '')
  ];
}
