# sfi-kubectl: the ADMIN rung as a verb, for hosts that opt in.
#
#   sudo sfi-kubectl delete cluster -n sites-db sites-db
#   sudo sfi-kubectl get pvc -A
#
# The framework's rule (ops-commands.nix) is that verbs nudge machinery
# and never desired state, and that rung 3 — the admin kubeconfig — is a
# console break-glass event. This verb is rung 3 through the front door:
# root's kubeconfig, and whatever the operator typed, passed through as
# is. It exists for DISPOSABLE forts, where the intervention a runbook
# needs (delete a Cluster and its PVC so flux recreates it empty; strip a
# finalizer) is not worth a curated verb each, and where nothing the
# operator can do costs more than a repave.
#
# The guard is the import: a host gets this only by naming it in its own
# default.nix and setting enable, and production.nix hosts must not.
# Every invocation is sudo-logged with its full argument list, which is
# the audit — there is no allowlist here and none is claimed.
{ config, lib, pkgs, ... }:
let
  cfg = config.sfi.ops.kubectl;
in
{
  imports = [ ./ops-commands.nix ];

  options.sfi.ops.kubectl.enable = lib.mkOption {
    type = lib.types.bool;
    default = false;
    description = "Expose the admin kubeconfig as `sudo sfi-kubectl <args…>` — the ADMIN rung as a verb. Disposable forts only.";
  };

  config = lib.mkIf cfg.enable {
    sfi.ops.commands.kubectl = {
      description = "ADMIN RUNG: kubectl with root's kubeconfig, arguments passed through unchanged — this host is disposable";
      runtimeInputs = [ pkgs.kubectl ];
      script = ''
        export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
        if [ "$#" -eq 0 ]; then
          echo "usage: sudo sfi-kubectl <kubectl arguments>   (admin kubeconfig; this host is disposable)" >&2
          exit 64
        fi
        exec kubectl "$@"
      '';
    };
  };
}
