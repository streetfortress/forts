# Reusable single-node k3s control plane.
#
# Host-specific networking (bridge, static IP) is left to the host config, and
# the host's secrets module (hosts/<name>/secrets.nix) supplies the sops file /
# age key that k3s-token is decrypted from.
# The datastore is `sfi.k3s.datastore` (#426): "sqlite" (a single node, the
# default) or "etcd" (embedded, cluster-init). fort scaffold renders the line
# from the manifest's [cluster] datastore, and a test in cmd/fort keeps
# the manifest and the host file equal; the module maps the word to
# services.k3s.clusterInit, so no host sets clusterInit itself.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.k3s;

  # The API server's serving cert only carries the node's primary IP, the
  # cluster service IP and loopback — not the wireguard address. Since
  # `just fetch-kubeconfig` rewrites the server URL to the mesh IP, every
  # kubectl call over the VPN would fail x509 verification without a SAN for
  # it. Read from the same mesh/peers.json the mesh module keys off, by
  # hostname, so the two can't drift; hosts outside the mesh add nothing.
  #
  # On an ALREADY-RUNNING node, changing the SAN set needs the cached serving
  # cert cleared as well — k3s keeps it in server/tls/dynamic-cert.json and
  # mirrors it to the `k3s-serving` secret in kube-system; delete both and
  # restart k3s if the old SANs survive the rebuild. Production hosts sidestep
  # this: they are replaced as images, so they come up with the SAN baked in.
  peers = (builtins.fromJSON (builtins.readFile (config.sfi.estate.root + "/mesh/peers.json"))).peers;
  meshIp = (peers.${config.networking.hostName} or { }).wg_vpn_ip or null;
in
{
  # lab-host.nix: the lab CA, the gitea pin, node + directory metrics —
  # everything a lab member has whether or not it runs k3s (#417).
  # ops/: the observe/nudge ladder — read-only kubeconfig, the sfi-* verb
  # framework, and the flux verbs every k3s host should carry.
  imports = [
    ./estate.nix
    ./lab-host.nix
    # The API server's audit log, off until a host's manifest says
    # [cluster] audit_logging = true (#626).
    ./k3s-audit.nix
    ./ops/ops-kubeconfig.nix
    ./ops/ops-commands.nix
    ./ops/flux.nix
    ./ops/workloads.nix
    ./ops/logs.nix
    ./ops/systemd.nix
  ];

  options.sfi.k3s.datastore = lib.mkOption {
    type = lib.types.enum [ "sqlite" "etcd" ];
    default = "sqlite";
    description = ''
      The k3s datastore: sqlite (a single node, the default) or etcd
      (embedded, cluster-init — the opt-in for an HA join). From the
      manifest's [cluster] datastore; sqlite→etcd migrates in place, the
      reverse is a repave.
    '';
  };

  config = {
    # Cluster join token, consumed below. The host's secrets module points sops at
    # the file that carries it.
    sops.secrets.k3s-token = { };

    services.k3s = {
      enable = true;
      role = "server";
      # The datastore is the manifest's word ([cluster] datastore, #426);
      # clusterInit is k3s's. One declaration: a host that sets
      # services.k3s.clusterInit itself conflicts here at eval time.
      clusterInit = cfg.datastore == "etcd";
      tokenFile = config.sops.secrets.k3s-token.path;
      extraFlags = [
        "--disable=traefik"
        "--kubelet-arg=read-only-port=10255"
        # Admin kubeconfig is root-only (0600 default): daily ops use the
        # read-only config rendered by ops/ops-kubeconfig.nix (#90);
        # curated interventions go through the `sudo sfi-*` verbs
        # (ops/ops-commands.nix); touching k3s.yaml directly is the
        # break-glass event.
      ] ++ lib.optional (meshIp != null) "--tls-san=${meshIp}";
    };

    # k3s hardcodes "default" as the cluster/context/user name in the kubeconfig
    # it generates and offers no flag to change it. Rename all three to the host
    # name after each start (k3s rewrites k3s.yaml on every startup), so
    # kubeconfigs pulled by `just fetch-kubeconfig` carry distinct context names
    # and merge cleanly under kubectx/kubens.
    systemd.services.k3s.postStart = ''
      kubeconfig=/etc/rancher/k3s/k3s.yaml
      for _ in $(seq 30); do
        [ -s "$kubeconfig" ] && break
        sleep 1
      done
      ${pkgs.gnused}/bin/sed -i \
        's/^\( *\(- \)\?\(name\|cluster\|user\|current-context\): \)default$/\1${config.networking.hostName}/' \
        "$kubeconfig"
    '';

    # Fresh-cluster deadlock guard (#408). k3s gates its embedded cloud-
    # controller-manager on a SubjectAccessReview that passes the moment
    # ccm.yaml's ClusterRoleBinding lands — before the same manifest's
    # kube-system RoleBinding (extension-apiserver-authentication-reader)
    # does. Lose that race and the CCM's fatal exit takes k3s down mid-
    # apply; on every restart the SAR passes instantly on the half-applied
    # RBAC, the CCM dies within a second, and the addon controller (which
    # starts a beat later) never gets to finish. sfi-stage-d95cc36 looped
    # 218 times this way at birth; upstream k3s-io/k3s#7328 is the race.
    #
    # The signature is stable — ClusterRoleBinding present, RoleBinding
    # absent — and lasts milliseconds on a healthy start, forever in the
    # deadlock. Seen twice in a row, re-apply k3s's own ccm.yaml (a no-op
    # on top of what did land; the deploy controller adopts it afterwards,
    # as the manual recovery proved). Polling continues through the API's
    # up-windows because a failed kubectl is "not yet", never an error.
    systemd.services.k3s-ccm-rbac-guard = {
      description = "Re-apply k3s CCM RBAC if a fresh cluster deadlocked mid-apply (#408)";
      after = [ "k3s.service" ];
      requires = [ "k3s.service" ];
      wantedBy = [ "multi-user.target" ];
      path = [ pkgs.k3s ];
      environment.KUBECONFIG = "/etc/rancher/k3s/k3s.yaml";
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        TimeoutStartSec = "35min";
      };
      script = ''
        set -euo pipefail
        kubectl() { k3s kubectl "$@"; }
        manifest=/var/lib/rancher/k3s/server/manifests/ccm.yaml
        # No manifest = CCM disabled or not a server; nothing to guard.
        if [ ! -s "$manifest" ]; then
          echo "no $manifest; nothing to guard"
          exit 0
        fi

        seen=0
        # ~30 min at 2s: a cluster still not up by then is not this problem.
        for _ in $(seq 900); do
          if kubectl -n kube-system get rolebinding \
              k3s-cloud-controller-manager-authentication-reader >/dev/null 2>&1; then
            echo "CCM auth-reader RoleBinding present; cluster is past the race"
            exit 0
          fi
          if kubectl get clusterrolebinding k3s-cloud-controller-manager >/dev/null 2>&1; then
            seen=$((seen + 1))
            if [ "$seen" -ge 2 ]; then
              echo "deadlock signature: CCM ClusterRoleBinding present, auth-reader RoleBinding absent — re-applying $manifest"
              # The API may vanish under us mid-window; just try again.
              kubectl apply -f "$manifest" || true
            fi
          fi
          sleep 2
        done
        echo "gave up waiting for the CCM RBAC to settle; see journalctl -u k3s" >&2
        exit 1
      '';
    };

    networking.firewall.allowedTCPPorts = [ 6443 80 443 ];

    # k3s after a cached-cert change (the SAN note above) is one restart —
    # sfi-systemd-restart k3s (#212).
    sfi.ops.restartableUnits = [ "k3s" ];
  };
}
