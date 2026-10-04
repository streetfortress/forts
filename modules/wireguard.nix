# WireGuard mesh membership. Reusable across hosts: the local peer is picked by
# hostname, so any host whose networking.hostName matches a key in
# mesh/peers.json can import this. Requires the sops secret
# `wireguard-private-key` (declared in the host's secrets module).
#
# Mesh observability (#121): a root-side timer exports per-peer WireGuard
# state (`wg show wg0 dump`) as node_exporter textfile metrics — the
# deployer-safe replacement for `sudo wg show`, which no longer exists on
# the all-prod fleet. Labels use peer names from the same peers.json the
# config is built from, so drift alerts read as hostnames, not pubkeys.
# The WireGuardPeerSilent / WireGuardMetricsStale rules live in the edge
# rule set (flux/infrastructure/configs/monitoring/rules-edge.yaml):
# mesh-health must evaluate locally with the mesh down.
{ config, lib, pkgs, ... }:

let
  peersData = builtins.fromJSON (builtins.readFile (config.sfi.estate.root + "/mesh/peers.json"));
  selfName = config.networking.hostName;
  self = peersData.peers.${selfName};
  otherPeers = lib.filterAttrs (n: _: n != selfName) peersData.peers;
  clients = peersData.clients or {};
  # The hosts of OTHER estates that link to this mesh (deployments#674):
  # dialed like a peer, at the endpoint and the port the link gave them
  # (a linked host listens for this mesh on a port beside its own mesh's).
  # Not forts of this estate: no ssh, no manifest — docs/mesh.md.
  remotes = peersData.remotes or {};

  mkMeshPeer = _name: p: {
    publicKey = p.public_key;
    allowedIPs = [ "${p.wg_vpn_ip}/32" ];
    persistentKeepalive = 25;
  } // lib.optionalAttrs (p ? wg_endpoint_ip) {
    endpoint = "${p.wg_endpoint_ip}:${toString (p.wg_endpoint_port or 51820)}";
  };

  mkClientPeer = _name: c: {
    publicKey = c.public_key;
    allowedIPs = [ "${c.wg_vpn_ip}/32" ];
  };

  # pubkey → name lookup for metric labels, from the same source of truth
  # as the peer config itself (mesh peers + roaming clients).
  peerNames = pkgs.writeText "wireguard-peer-names" (lib.concatStringsSep "\n"
    (lib.mapAttrsToList (n: p: "${p.public_key}\t${n}")
      (otherPeers // clients // remotes)));

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
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp" # mktemp gives 0600; the exporter is unprivileged
    ${pkgs.coreutils}/bin/mv "$tmp" "$STATE_DIRECTORY/wireguard.prom"
  '';
in
{
  imports = [
    ./estate.nix
    ./textfile-metrics.nix
    ./ops/systemd.nix # sfi-systemd-restart wg-quick-wg0 (#212)
  ];

  # A peer rollout that wg-quick did not pick up is one restart away.
  sfi.ops.restartableUnits = [ "wg-quick-wg0" ];

  networking.firewall.allowedUDPPorts = [ 51820 ];

  networking.wg-quick.interfaces.wg0 = {
    address = [ "${self.wg_vpn_ip}/24" ];
    listenPort = 51820;
    privateKeyFile = config.sops.secrets.wireguard-private-key.path;
    peers =
      (lib.mapAttrsToList mkMeshPeer otherPeers)
      ++ (lib.mapAttrsToList mkClientPeer clients)
      ++ (lib.mapAttrsToList mkMeshPeer remotes);
  };

  # A peer whose endpoint is a DNS NAME (a cloud fort's public FQDN in
  # peers.json — sfi-prod's is one) makes `wg addconf` a DNS client at
  # boot, and a name that does not resolve is a "configuration parsing
  # error": wg-quick tears wg0 down and exits 1, and the unit does not
  # retry. A fort that boots while DNS is not yet answering has no mesh,
  # therefore no SSH and no rebuild, until a human reboots it again —
  # the interim at 02:48 on 2026-09-19, mid router reboot (#350): k3s
  # and gitea came up, every fort's gitea pin pointed at a mesh address
  # that never answered. Retry until it works: DNS availability is the
  # only thing the unit is waiting on, and nothing else on the host can
  # stand in for the mesh.
  systemd.services.wg-quick-wg0 = {
    unitConfig.StartLimitIntervalSec = 0;
    serviceConfig = {
      Restart = "on-failure";
      RestartSec = "15s";
    };
    # A changed service is, by default, STOPPED at the start of a switch
    # and started again at its end (X-StopIfChanged), with activation and
    # the per-user unit phase in between. On a cluster fort those two
    # moments are milliseconds apart. On a dev fort the user phase walks
    # every lingering agent manager and re-runs its oneshots, and those
    # dial gitea over the mesh this unit has just torn down: gitea.zen.lofi
    # still resolves (the /etc/hosts pin, lab-dns.nix), the packets leave
    # by the LAN default route and black-hole, and each connect burns the
    # kernel's SYN retry window (~2 min) before it fails. The devbox
    # removal (#646, 2026-10-02) held sfi-dev-831c637's switch for 13 min
    # that way, and wg0 never came back up. Restart instead: the restart
    # phase runs AFTER the user phase, so the mesh stays up through
    # activation and the agents' oneshots, and a peers.json rollout is one
    # down/up on every fort alike.
    stopIfChanged = false;
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
}
