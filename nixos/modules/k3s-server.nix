# Reusable single-node k3s control plane.
#
# Host-specific networking (bridge, static IP) is left to the host config, and
# the host's secrets module (hosts/<name>/secrets.nix) supplies the sops file /
# age key that k3s-token is decrypted from.
# clusterInit (embedded etcd) is on by default; a lean single-node host that
# wants the sqlite datastore instead can set `services.k3s.clusterInit = false;`.
{ config, lib, pkgs, ... }:

let
  # The API server's serving cert only carries the node's primary IP, the
  # cluster service IP and loopback — not the wireguard address. Since
  # `just fetch-kubeconfig` rewrites the server URL to the mesh IP, every
  # kubectl call over the VPN would fail x509 verification without a SAN for
  # it. Read from the same wireguard/peers.json the mesh module keys off, by
  # hostname, so the two can't drift; hosts outside the mesh add nothing.
  #
  # On an ALREADY-RUNNING node, changing the SAN set needs the cached serving
  # cert cleared as well — k3s keeps it in server/tls/dynamic-cert.json and
  # mirrors it to the `k3s-serving` secret in kube-system; delete both and
  # restart k3s if the old SANs survive the rebuild. Production hosts sidestep
  # this: they are replaced as images, so they come up with the SAN baked in.
  peers = (builtins.fromJSON (builtins.readFile ../../wireguard/peers.json)).peers;
  meshIp = (peers.${config.networking.hostName} or { }).wg_vpn_ip or null;
in
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

  networking.firewall.allowedTCPPorts = [ 6443 80 443 ];

  # Trust the lab step-ca root (ACME/TLS for *.lofi services).
  security.pki.certificateFiles = [ ../certs/step-ca-root.pem ];

  # Node metrics
  services.prometheus.exporters.node = {
    enable = true;
    openFirewall = true; # opens port 9100
  };

  # Directory-size metrics for the host dashboard (#130); k3s-cluster
  # inherits this via its k3s-server import, so every exporter host has it.
  # ops/: the observe/nudge ladder — read-only kubeconfig, the sfi-* verb
  # framework, and the flux verbs every k3s host should carry.
  imports = [
    ./disk-usage-metrics.nix
    ./ops/ops-kubeconfig.nix
    ./ops/ops-commands.nix
    ./ops/flux.nix
  ];
}
