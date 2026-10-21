# Day-0 Flux bootstrap for a k3s cluster host. Idempotent systemd oneshot that
# runs after k3s on every boot and converges the cluster to "flux syncing this
# repo" with zero manual kubectl/helm steps:
#
#   1. wait for the k3s API (the coredns zone that source-controller
#      needs to resolve the forge's name is a k3s add-on rendered by
#      mesh-dns.nix and lab-dns.nix, applied by k3s itself before this runs)
#   3. flux-system namespace
#   4. `sops-age` secret from the host's flux-sops-age-key (sops-nix)
#   5. `flux-system` git-auth secret from flux-git-deploy-key (identity.pub is
#      derived, known_hosts is public and lives here in the module)
#   6. helm install flux-operator (pinned; the fleet HelmRelease adopts the
#      release — same name + namespace — and reconciles to its own pin after);
#      a release left `pending-*` by an earlier run that died mid-install
#      is cleared first, since helm refuses every operation while one is
#      pending (#408: the bootstrap run was SIGTERMed with k3s and every later
#      run failed "another operation is in progress" until a human deleted
#      the release secret)
#   7. apply the cluster's FluxInstance
#
# The unit retries on failure (a minute apart, no start limit): day 0 is
# when k3s under it is most likely to flap, and a oneshot that stays
# `failed` needs a reboot — or a `systemctl restart` from the admin rung
# (ops/admin.nix) — to try again.
#
# The FluxInstance manifest is NOT duplicated here: the flake's source is
# the whole repo, so the module applies the very file flux later owns
# (flux/clusters/<cluster>/flux-system/flux-instance.yaml).
#
# Consumers declare the two extra host secrets in nixos/secrets/<host>.yaml
# (flux-sops-age-key = the cluster's age key, flux-git-deploy-key = a
# read-only deploy key for this repo on the forge) and set:
#
#   sfi.flux-bootstrap = {
#     enable = true;
#     cluster = "hub";     # → flux/clusters/hub
#   };
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.flux-bootstrap;

  fluxInstanceManifest =
    config.sfi.estate.root + "/flux/clusters/${cfg.cluster}/flux-system/flux-instance.yaml";

  knownHostsFile = pkgs.writeText "flux-git-known-hosts" cfg.gitKnownHosts;
in
{
  imports = [ ./estate.nix ];

  options.sfi.flux-bootstrap = {
    enable = lib.mkEnableOption "day-0 Flux bootstrap (flux-operator + FluxInstance)";

    cluster = lib.mkOption {
      type = lib.types.str;
      description = "Cluster name; the FluxInstance at flux/clusters/<cluster>/flux-system/ is applied.";
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
      default = config.sfi.estate.knownHosts;
      defaultText = "estate.toml's `[source] known_hosts` (sfi.estate.knownHosts)";
      description = "SSH known_hosts line(s) for the git host, baked into the flux-system git-auth secret.";
    };
  };

  config = lib.mkIf cfg.enable {
    # Host secrets consumed below; the host's sops file must carry both (the
    # `fort mint` seeds both, flux-sops-age-key and flux-git-deploy-key).
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

      unitConfig.StartLimitIntervalSec = 0;
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        # First boot pulls the operator image over WAN.
        TimeoutStartSec = "20min";
        # Ride out a flapping k3s / unreachable forge on first boot rather
        # than sit `failed` until someone reboots (#408).
        Restart = "on-failure";
        RestartSec = "60s";
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

        # A helm release stuck pending-* (this run's predecessor died mid
        # install/upgrade) blocks every helm operation. Helm's own remedy
        # is to drop that revision's record: the chart's resources stay,
        # and the next upgrade --install adopts them (they carry helm's
        # ownership labels) or, with a prior deployed revision, upgrades
        # from it. Day-0 safe by construction.
        pending=$(kubectl -n flux-system get secret \
          -l 'owner=helm,name=flux-operator,status in (pending-install,pending-upgrade,pending-rollback)' \
          -o name)
        if [ -n "$pending" ]; then
          echo "clearing stuck helm release record(s): $pending"
          # shellcheck disable=SC2086
          kubectl -n flux-system delete $pending
        fi

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
