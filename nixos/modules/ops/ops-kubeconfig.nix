# Read-only ops kubeconfig (#90): renders the ops-ro ServiceAccount token
# (flux/infrastructure/configs/ops-readonly.yaml) into
# /var/lib/sfi/kubeconfig-ro — the everyday kubeconfig for humans, local
# and fetched (`just fetch-kubeconfig`). The admin k3s.yaml is root-only
# 0600; using it directly IS the break-glass event (README Key
# decisions). Between observe (this file) and full admin sits the curated
# `sudo sfi-*` verb layer (ops-commands.nix in this directory).
#
# THE HOST APPLIES THE ops-ro IDENTITY ITSELF (#185). The identity used to
# arrive with the fleet, so rung 1 of the ladder existed only after flux
# had converged — and the drill fort's flux deadlock is exactly the hour
# that needs rung 1. So this module hands the same file to k3s's own
# add-on directory (services.k3s.manifests →
# /var/lib/rancher/k3s/server/manifests), which k3s applies at start,
# before flux exists. lab-dns.nix does this for the coredns override and
# flux-bootstrap.nix applies the fleet's own FluxInstance the same way:
# the flake's source is the whole repo, so the host applies the very file
# the fleet applies, and the two cannot drift.
#
# The fleet still lists the file as well, on purpose and for now. Dropping
# the fleet's copy in the same commit would race kustomize-controller's
# prune against the host's first apply of the annotation that stops it, so
# the objects carry `kustomize.toolkit.fluxcd.io/prune: disabled` first
# (they do, as of #185) and the fleet's resource entry goes in a later
# change, once every fort has reconciled the annotation. Two applies of
# one file is not a conflict: the bytes are identical, so neither field
# manager sees a diff.
#
# The rendered server URL is the mesh address (same peers.json read as
# k3s-server.nix's --tls-san, so the two can't drift): the file works
# identically on-host and pulled over the VPN.
{ config, lib, pkgs, ... }:

let
  peers = (builtins.fromJSON (builtins.readFile ../../../mesh/peers.json)).peers;
  meshIp = (peers.${config.networking.hostName} or { }).wg_vpn_ip or null;
  server = if meshIp != null then meshIp else "127.0.0.1";
in
{
  # Applied by k3s at start, so the ops-ro token exists before flux does.
  # Same file the fleet applies (flux/infrastructure/configs/) — see the
  # header for why both, and for how the fleet's copy retires.
  services.k3s.manifests.ops-readonly.source =
    ../../../flux/infrastructure/configs/ops-readonly.yaml;

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
    # A oneshot that stays `failed` needs a reboot to try again, and this
    # unit is rung 1: no start limit, so it keeps trying for as long as
    # the cluster takes (flux-bootstrap.nix's reason, #408).
    unitConfig.StartLimitIntervalSec = 0;
    serviceConfig = {
      Type = "oneshot";
      # k3s applies the ops-ro manifest at start and the token controller
      # fills the Secret a moment later; keep retrying until it is there.
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
      # merges every file on KUBECONFIG by key name (first file wins), so an
      # unsuffixed ops-ro in each fort's file sends one fort's token to every
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
