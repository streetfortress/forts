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
#
# The surface of a k3s host is here too (#154; production.nix held it
# until then, under a name from #86 that stopped meaning anything once
# every fort was hardened, #272): the baseline every fort imports —
# hardened.nix and ops/deployer.nix — plus what a CLUSTER fort adds to
# both, the cluster grant for deployer and the ports, declared once
# below instead of opened here, collapsed by hardened.nix and re-opened
# by a third module.
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
  # restart k3s if the old SANs survive the rebuild. A fort sidesteps this:
  # it is replaced as an image, so it comes up with the SAN baked in.
  peers = (builtins.fromJSON (builtins.readFile (config.sfi.estate.root + "/mesh/peers.json"))).peers;
  meshIp = (peers.${config.networking.hostName} or { }).wg_vpn_ip or null;
in
{
  # The import list of a role (forts#60): the estate, the mesh and its
  # names, the metrics the hub scrapes, and today's homelab DNS. A role
  # module names each one — it does not depend on a bundle.
  # ops/: the OBSERVE rung of the ladder for the cluster, the read-only
  # kubeconfig (docs/ops.md).
  imports = [
    # The baseline every fort imports (the plain shape names the same
    # pair): the hardening — mesh-only admin surface, root-only nix, the
    # closure trims — and the operator account `deployer` with the ops
    # verbs. The cluster grant and the ports are below.
    ./hardened.nix
    ./ops/deployer.nix
    # The estate's files under one root, and the CA root it carries.
    ./estate.nix
    # The mesh, and with it the names of the mesh (mesh-dns.nix): a k3s
    # fort reaches the forge, the hub and the CA by those names.
    ./wireguard.nix
    # The node exporter and the textfile collectors (forts#38).
    ./metrics.nix
    # The forge's name pinned to its address on this mesh ([hubs.forge]
    # + mesh/peers.json, forts#26), and the .lofi forward of a lab fort.
    ./lab-dns.nix
    # The API server's audit log, off until a host's manifest says
    # [cluster] audit_logging = true (#626).
    ./k3s-audit.nix
    # The OBSERVE rung of a cluster fort: the read-only kubeconfig. The
    # core ships no verb for the cluster (forts#151) — kubectl and flux
    # are the cluster's own tools, behind the two kubeconfigs.
    ./ops/ops-kubeconfig.nix
  ];

  options.sfi.k3s.datastore = lib.mkOption {
    type = lib.types.enum [ "sqlite" "etcd" ];
    default = "sqlite";
    description = ''
      The k3s datastore: sqlite (a single node, the default) or etcd
      (embedded, cluster-init — the opt-in for an HA join). From the
      manifest's [cluster] datastore; sqlite→etcd migrates in place, the
      reverse is a re-provision.
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
        # read-only config rendered by ops/ops-kubeconfig.nix (#90); the
        # admin rung is deployer's kubectl behind the admin-key challenge
        # on a host that enables ops/admin.nix; touching k3s.yaml
        # directly is the break-glass event.
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
    # 218 times this way at bootstrap; upstream k3s-io/k3s#7328 is the race.
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

    # What a cluster fort adds to the operator account (ops/deployer.nix
    # holds the account itself — isNormalUser, the journal, the keys):
    # the ops-ro kubeconfig (ops/ops-kubeconfig.nix, #90), so deployer
    # observes the cluster as well as the host. Still no wheel and no nix
    # daemon access: writes go through git, and rebuilds arrive through
    # the pull loop.
    users.users.deployer.extraGroups = [ "kubeops" ];

    # The public face of a cluster fort is exactly 80/443, the ingress;
    # hardened.nix collapses every other merged opening to this list.
    # mkDefault: a host that serves no public ingress narrows it.
    sfi.hardened.publicTCPPorts = lib.mkDefault [ 80 443 ];
    # The cluster ports on the mesh interface only (22 + 9100 are already
    # there from hardened.nix): the API server, the kubelet's read-only
    # port, and 30909/30993 — the edge Prometheus/Alertmanager NodePorts
    # (mesh-services-edge.yaml); the mesh-only opening IS their access
    # control, and silences are set against 30993. A port a layer's
    # workload answers on over the mesh (a database's NodePort for a hub's
    # reader) is the estate's to open, in the host file.
    networking.firewall.interfaces.${config.sfi.mesh.interface}.allowedTCPPorts = [ 6443 10255 30909 30993 ];
    # The in-cluster edge Prometheus (#53) scrapes the host node_exporter
    # at the mesh address; pod traffic to the host's own mesh IP
    # ingresses on cni0, so 9100 must be open there too. The 10255 row
    # above and hardened.nix's mesh 9100 are the hypervisor pull-model
    # path — drop them once the edge stack is verified on an edge cluster.
    networking.firewall.interfaces."cni0".allowedTCPPorts = [ 9100 ];
  };
}
