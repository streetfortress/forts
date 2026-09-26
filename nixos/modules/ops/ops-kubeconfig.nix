# Read-only ops kubeconfig (#90): renders the ops-ro ServiceAccount token
# (fleet/infrastructure/configs/ops-readonly.yaml) into
# /var/lib/sfi/kubeconfig-ro — the everyday kubeconfig for humans, local
# and fetched (`just fetch-kubeconfig`). The admin k3s.yaml is root-only
# 0600; using it directly IS the break-glass event (README Key
# decisions). Between observe (this file) and full admin sits the curated
# `sudo sfi-*` verb layer (ops-commands.nix in this directory).
#
# The rendered server URL is the mesh address (same peers.json read as
# k3s-server.nix's --tls-san, so the two can't drift): the file works
# identically on-host and pulled over the VPN.
{ config, lib, pkgs, ... }:

let
  peers = (builtins.fromJSON (builtins.readFile ../../../wireguard/peers.json)).peers;
  meshIp = (peers.${config.networking.hostName} or { }).wg_vpn_ip or null;
  server = if meshIp != null then meshIp else "127.0.0.1";
in
{
  # Read access to the rendered file; production.nix adds deployer.
  users.groups.kubeops = { };

  # Everyday kubectl resolves to the RO config for every login shell —
  # including root's, deliberately: break-glass is an explicit
  # `KUBECONFIG=/etc/rancher/k3s/k3s.yaml`, never an accident.
  environment.sessionVariables.KUBECONFIG = "/var/lib/sfi/kubeconfig-ro";

  systemd.services.ops-kubeconfig = {
    description = "Render the read-only ops kubeconfig from the ops-ro SA token";
    wantedBy = [ "multi-user.target" ];
    after = [ "k3s.service" ];
    serviceConfig = {
      Type = "oneshot";
      # Flux has to apply the fleet manifest first on a fresh cluster;
      # keep retrying until the token exists.
      Restart = "on-failure";
      RestartSec = "30s";
    };
    path = [ pkgs.kubectl ];
    script = ''
      set -euo pipefail
      export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
      token=$(kubectl -n kube-system get secret ops-ro-token \
        -o jsonpath='{.data.token}' 2>/dev/null | base64 -d)
      ca=$(kubectl -n kube-system get secret ops-ro-token \
        -o jsonpath='{.data.ca\.crt}' 2>/dev/null)
      [ -n "$token" ] && [ -n "$ca" ]

      install -d -m 0755 /var/lib/sfi
      umask 027
      cat > /var/lib/sfi/.kubeconfig-ro.tmp <<EOF
      apiVersion: v1
      kind: Config
      clusters:
        - name: ${config.networking.hostName}
          cluster:
            server: https://${server}:6443
            certificate-authority-data: $ca
      # The user entry is keyed by host, like the cluster and context: kubectl
      # merges every file on KUBECONFIG by key name (first file wins), so a
      # bare `ops-ro` in each fort's file sends one fort's token to every
      # other fort — "Unauthorized" the moment a second fort is fetched
      # (sfi-prod-439bf72 vs the drill fort, 2026-09-12).
      users:
        - name: ops-ro@${config.networking.hostName}
          user:
            token: $token
      contexts:
        - name: ${config.networking.hostName}
          context:
            cluster: ${config.networking.hostName}
            user: ops-ro@${config.networking.hostName}
      current-context: ${config.networking.hostName}
      EOF
      chgrp kubeops /var/lib/sfi/.kubeconfig-ro.tmp
      mv /var/lib/sfi/.kubeconfig-ro.tmp /var/lib/sfi/kubeconfig-ro
    '';
  };

  # Re-render daily so a rotated/recreated token heals without a rebuild.
  systemd.timers.ops-kubeconfig = {
    wantedBy = [ "timers.target" ];
    timerConfig = {
      OnCalendar = "daily";
      Persistent = true;
    };
  };
}
