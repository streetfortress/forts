# WireGuard mesh membership. Reusable across hosts: the local peer is picked by
# hostname, so any host whose networking.hostName matches a key in
# wireguard/peers.json can import this. Requires the sops secret
# `wireguard-private-key` (declared in the host's secrets module).
#
# Hosts that route the k8s LAN into the mesh (the bridge host) additionally
# import ./wireguard-lan-gateway.nix.
#
# Mesh observability (#121): a root-side timer exports per-peer WireGuard
# state (`wg show wg0 dump`) as node_exporter textfile metrics — the
# deployer-safe replacement for `sudo wg show`, which no longer exists on
# the all-prod fleet. Labels use peer names from the same peers.json the
# config is built from, so drift alerts read as hostnames, not pubkeys.
# The WireGuardPeerSilent / WireGuardMetricsStale rules live in the edge
# rule set (fleet/infrastructure/configs/monitoring/rules-edge.yaml):
# mesh-health must evaluate locally with the mesh down.
{ config, lib, pkgs, ... }:

let
  peersData = builtins.fromJSON (builtins.readFile ../../wireguard/peers.json);
  selfName = config.networking.hostName;
  self = peersData.peers.${selfName};
  otherPeers = lib.filterAttrs (n: _: n != selfName) peersData.peers;
  clients = peersData.clients or {};

  mkMeshPeer = _name: p: {
    publicKey = p.public_key;
    allowedIPs = [ "${p.wg_vpn_ip}/32" ];
    persistentKeepalive = 25;
  } // lib.optionalAttrs (p ? wg_endpoint_ip) {
    endpoint = "${p.wg_endpoint_ip}:51820";
  };

  mkClientPeer = _name: c: {
    publicKey = c.public_key;
    allowedIPs = [ "${c.wg_vpn_ip}/32" ];
  };

  textfileDir = "/var/lib/node-exporter-textfiles";

  # pubkey → name lookup for metric labels, from the same source of truth
  # as the peer config itself (mesh peers + roaming clients).
  peerNames = pkgs.writeText "wireguard-peer-names" (lib.concatStringsSep "\n"
    (lib.mapAttrsToList (n: p: "${p.public_key}\t${n}")
      (otherPeers // clients)));

  # dump fields per peer line: pubkey, psk, endpoint, allowed-ips,
  # latest-handshake (epoch, 0 = never), rx bytes, tx bytes, keepalive
  # ("off" or seconds). Samples are grouped per metric in END because the
  # exposition format forbids interleaving metric names. Atomic
  # write-then-rename so node_exporter never reads a torn file.
  collector = pkgs.writeShellScript "wireguard-metrics" ''
    set -eu
    tmp=$(${pkgs.coreutils}/bin/mktemp "$STATE_DIRECTORY/.wireguard.prom.XXXXXX")
    ${pkgs.wireguard-tools}/bin/wg show wg0 dump | ${pkgs.gawk}/bin/awk -v map=${peerNames} '
      BEGIN {
        FS = "\t"
        while ((getline line < map) > 0) { split(line, a, "\t"); names[a[1]] = a[2] }
      }
      NR == 1 { next }  # interface line
      {
        n++
        peer[n] = ($1 in names) ? names[$1] : substr($1, 1, 8)
        hs[n] = $5; rx[n] = $6; tx[n] = $7
        ka[n] = ($8 == "off") ? 0 : $8
      }
      END {
        print "# TYPE node_wireguard_latest_handshake_seconds gauge"
        for (i = 1; i <= n; i++) printf "node_wireguard_latest_handshake_seconds{interface=\"wg0\",peer=\"%s\"} %s\n", peer[i], hs[i]
        print "# TYPE node_wireguard_peer_keepalive_seconds gauge"
        for (i = 1; i <= n; i++) printf "node_wireguard_peer_keepalive_seconds{interface=\"wg0\",peer=\"%s\"} %s\n", peer[i], ka[i]
        print "# TYPE node_wireguard_receive_bytes_total counter"
        for (i = 1; i <= n; i++) printf "node_wireguard_receive_bytes_total{interface=\"wg0\",peer=\"%s\"} %s\n", peer[i], rx[i]
        print "# TYPE node_wireguard_transmit_bytes_total counter"
        for (i = 1; i <= n; i++) printf "node_wireguard_transmit_bytes_total{interface=\"wg0\",peer=\"%s\"} %s\n", peer[i], tx[i]
      }' > "$tmp"
    ${pkgs.coreutils}/bin/mv "$tmp" "$STATE_DIRECTORY/wireguard.prom"
  '';
in
{
  networking.firewall.allowedUDPPorts = [ 51820 ];

  networking.wg-quick.interfaces.wg0 = {
    address = [ "${self.wg_vpn_ip}/24" ];
    listenPort = 51820;
    privateKeyFile = config.sops.secrets.wireguard-private-key.path;
    peers =
      (lib.mapAttrsToList mkMeshPeer otherPeers)
      ++ (lib.mapAttrsToList mkClientPeer clients);
  };

  # If wg0 is down the run fails and the file freezes — covered by the
  # WireGuardMetricsStale alert on node_textfile_mtime_seconds rather than
  # retried into journal spam.
  systemd.services.wireguard-metrics = {
    description = "Export WireGuard peer state for node_exporter";
    after = [ "wg-quick-wg0.service" ];
    serviceConfig = {
      Type = "oneshot";
      ExecStart = "${collector}";
      StateDirectory = "node-exporter-textfiles";
    };
  };

  systemd.timers.wireguard-metrics = {
    wantedBy = [ "timers.target" ];
    timerConfig = {
      OnBootSec = "1min";
      OnUnitActiveSec = "30s";
    };
  };

  # The exporter itself is enabled by the k3s modules; this only points its
  # textfile collector at the directory the timer writes. Harmless on a
  # host where the exporter is off.
  services.prometheus.exporters.node.extraFlags = [
    "--collector.textfile.directory=${textfileDir}"
  ];
}
