# Day-0 Flux bootstrap for a k3s cluster host. Idempotent systemd oneshot that
# runs after k3s on every boot and converges the cluster to "flux syncing this
# repo" with zero manual kubectl/helm steps:
#
#   1. wait for the k3s API (the coredns .lofi forwarder that source-
#      controller needs to resolve gitea.zen.lofi is a k3s add-on rendered
#      by lab-dns.nix, applied by k3s itself before this runs)
#   3. flux-system namespace
#   4. `sops-age` secret from the host's flux-sops-age-key (sops-nix)
#   5. `flux-system` git-auth secret from flux-git-deploy-key (identity.pub is
#      derived, known_hosts is public and lives here in the module)
#   6. helm install flux-operator (pinned; the fleet HelmRelease adopts the
#      release — same name + namespace — and reconciles to its own pin after)
#   7. apply the cluster's FluxInstance
#
# The FluxInstance manifest is NOT duplicated here: the flake's source is
# the whole repo, so the module applies the very file flux later owns
# (fleet/clusters/<cluster>/flux-system/flux-instance.yaml).
#
# Consumers declare the two extra host secrets in nixos/secrets/<host>.yaml
# (flux-sops-age-key = the cluster's age key, flux-git-deploy-key = a
# read-only gitea deploy key for this repo) and set:
#
#   sfi.flux-bootstrap = {
#     enable = true;
#     cluster = "luxor";   # → fleet/clusters/luxor
#   };
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.flux-bootstrap;

  fluxInstanceManifest =
    ../../fleet + "/clusters/${cfg.cluster}/flux-system/flux-instance.yaml";

  knownHostsFile = pkgs.writeText "flux-git-known-hosts" cfg.gitKnownHosts;
in
{
  options.sfi.flux-bootstrap = {
    enable = lib.mkEnableOption "day-0 Flux bootstrap (flux-operator + FluxInstance)";

    cluster = lib.mkOption {
      type = lib.types.str;
      description = "Cluster name; the FluxInstance at fleet/clusters/<cluster>/flux-system/ is applied.";
    };

    operatorVersion = lib.mkOption {
      type = lib.types.str;
      default = "0.55.0";
      description = ''
        flux-operator chart version for the day-0 install. Only needs to be
        recent enough to serve the FluxInstance CRD; once flux syncs, the
        fleet's flux-operator HelmRelease adopts the release and upgrades it
        to the fleet-pinned (Renovate-managed) version.
      '';
    };

    gitKnownHosts = lib.mkOption {
      type = lib.types.str;
      default = "[gitea.zen.lofi]:30022 ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAACAQClPt06+mIzU3p5SGeGELvfWqoxgLAtvnG4yAbLz/jvReNLyj44aoKC3ciIKQc+yH1UZIaghznzZ+k8ChdoVX6vnV1qQWa9270/tGcpFT9UcYsgIdfkGfWyy9Kh8SGw56qldK+wcX3tWzbZU1ZyzS5d0RJjDu62MqkomVJ4nbnMuZIdw6v3qiOm7q1VdorqtVgn/0QbcLmT8kHiGopqR/BUoCG4+b7utmgrxTuHoYvpfLNIJcYKu8DLEPbLAjSlt38Z+qK25usWxuS4ZHIE7+q4yGZfSYfyG768594OE7OKlcUrlH5nArvI9eCi0U2L+/sWSAKkSE5P8UEmlXiaVl6+XLQVI036w/t6VJO4+Z4gM2FaukH5+XMhZcngdoMY5h46rUC1zV6fepd/MpyaEh5JO9Kntfkbx7Q+mvqvJBMh5+2hLQx5Umm6dmv+y9oXll/PATzxVFxe5+D3O03u0R0cAqp010ddSqHPRS3FNhuyLfiW53L0t9kGKWJQJoMKLSZuCEBt+F+iTe3bVeaIbSu4RBClFA/cHC1+CAWb3OsScdkP7vNObB5dDjMqfd/BaAw8CEJp0wwbR/gXjK4RHQ0WzSFi7++09zbaEZ4cYJwsNuePrhTrFpOMWVM1RdO70eCihMH7aqgTsa95LxD8dCdifiECQuz877gCZ3uEk8ekbQ==";
      description = "SSH known_hosts line(s) for the git host, baked into the flux-system git-auth secret.";
    };
  };

  config = lib.mkIf cfg.enable {
    # Host secrets consumed below; the host's sops file must carry both (the
    # create-nixos flow seeds flux-sops-age-key; the deploy key is added per
    # cluster with `sops set`).
    sops.secrets.flux-sops-age-key = { };
    sops.secrets.flux-git-deploy-key = { };

    systemd.services.flux-bootstrap = {
      description = "Day-0 Flux bootstrap (${cfg.cluster})";
      after = [ "k3s.service" "network-online.target" ];
      requires = [ "k3s.service" ];
      wants = [ "network-online.target" ];
      wantedBy = [ "multi-user.target" ];

      path = [ pkgs.k3s pkgs.kubernetes-helm pkgs.openssh ];
      environment.KUBECONFIG = "/etc/rancher/k3s/k3s.yaml";

      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        # First boot pulls the operator image over WAN.
        TimeoutStartSec = "20min";
      };

      script = ''
        set -euo pipefail
        kubectl() { k3s kubectl "$@"; }

        echo "waiting for the k3s API..."
        for _ in $(seq 120); do
          kubectl get --raw=/readyz >/dev/null 2>&1 && break
          sleep 5
        done
        kubectl get --raw=/readyz >/dev/null

        kubectl create namespace flux-system --dry-run=client -o yaml \
          | kubectl apply -f -

        kubectl create secret generic sops-age \
          --namespace flux-system \
          --from-file=age.agekey=${config.sops.secrets.flux-sops-age-key.path} \
          --dry-run=client -o yaml | kubectl apply -f -

        tmp=$(mktemp -d)
        trap 'rm -rf "$tmp"' EXIT
        ssh-keygen -y -f ${config.sops.secrets.flux-git-deploy-key.path} > "$tmp/identity.pub"
        kubectl create secret generic flux-system \
          --namespace flux-system \
          --from-file=identity=${config.sops.secrets.flux-git-deploy-key.path} \
          --from-file=identity.pub="$tmp/identity.pub" \
          --from-file=known_hosts=${knownHostsFile} \
          --dry-run=client -o yaml | kubectl apply -f -

        installed=""
        for attempt in 1 2 3; do
          if helm upgrade --install flux-operator \
            oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator \
            --version ${cfg.operatorVersion} \
            --namespace flux-system --wait; then
            installed=1
            break
          fi
          echo "helm attempt $attempt failed; retrying..."
          sleep 15
        done
        [ -n "$installed" ]

        kubectl wait --for=condition=Established \
          crd/fluxinstances.fluxcd.controlplane.io --timeout=180s
        kubectl apply -f ${fluxInstanceManifest}

        echo "flux bootstrap for ${cfg.cluster} converged"
      '';
    };
  };
}
