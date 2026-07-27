# Reusable single-node k3s control plane.
#
# Host-specific networking (bridge, static IP) is left to the host config, and
# the host's secrets module (hosts/<name>/secrets.nix) supplies the sops file /
# age key that k3s-token is decrypted from.
# clusterInit (embedded etcd) is on by default; a lean single-node host that
# wants the sqlite datastore instead can set `services.k3s.clusterInit = false;`.
{ config, lib, pkgs, ... }:

{
  # Cluster join token, consumed below. The host's secrets module points sops at
  # the file that carries it.
  sops.secrets.k3s-token = { };

  services.k3s = {
    enable = true;
    role = "server";
    clusterInit = lib.mkDefault true;
    tokenFile = config.sops.secrets.k3s-token.path;
    extraFlags = [
      "--disable=traefik"
      "--kubelet-arg=read-only-port=10255"
      # World-readable kubeconfig so `steven` can pull /etc/rancher/k3s/k3s.yaml
      # over SSH without sudo (see `just fetch-kubeconfig`). These are single-
      # purpose infra nodes; tighten to 0640 + a group once the locked-down
      # deployer user lands.
      "--write-kubeconfig-mode=0644"
    ];
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

  networking.firewall.allowedTCPPorts = [ 6443 80 443 ];

  # Trust the lab step-ca root (ACME/TLS for *.lofi services).
  security.pki.certificateFiles = [ ../certs/step-ca-root.pem ];

  # Node metrics
  services.prometheus.exporters.node = {
    enable = true;
    openFirewall = true; # opens port 9100
  };
}
