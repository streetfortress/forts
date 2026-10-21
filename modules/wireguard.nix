# WireGuard mesh membership. Reusable across hosts: the local peer is picked by
# hostname, so any host whose networking.hostName matches a key in
# mesh/peers.json can import this. Requires the sops secret
# `wireguard-private-key` (declared in the host's secrets module).
#
# Mesh observability (#121): a root-side timer exports per-peer WireGuard
# state (`wg show <iface> dump`) as node_exporter textfile metrics — the
# deployer-safe replacement for `sudo wg show`, which no longer exists on
# the all-prod fleet. Labels use peer names from the same peers.json the
# config is built from, so drift alerts read as hostnames, not pubkeys.
# The WireGuardPeerSilent / WireGuardMetricsStale rules live in the edge
# rule set (flux/infrastructure/configs/monitoring/rules-edge.yaml):
# mesh-health must evaluate locally with the mesh down.
{ config, lib, pkgs, ... }:

let
  iface = config.sfi.mesh.interface; # estate.toml's `[mesh] interface`, else wg0 (estate.nix)
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
    ${pkgs.wireguard-tools}/bin/wg show ${iface} dump | ${pkgs.gawk}/bin/awk -v map=${peerNames} -v iface=${iface} '
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
        for (i = 1; i <= n; i++) printf "node_wireguard_latest_handshake_seconds{interface=\"%s\",peer=\"%s\"} %s\n", iface, peer[i], hs[i]
        print "# TYPE node_wireguard_peer_keepalive_seconds gauge"
        for (i = 1; i <= n; i++) printf "node_wireguard_peer_keepalive_seconds{interface=\"%s\",peer=\"%s\"} %s\n", iface, peer[i], ka[i]
        print "# TYPE node_wireguard_receive_bytes_total counter"
        for (i = 1; i <= n; i++) printf "node_wireguard_receive_bytes_total{interface=\"%s\",peer=\"%s\"} %s\n", iface, peer[i], rx[i]
        print "# TYPE node_wireguard_transmit_bytes_total counter"
        for (i = 1; i <= n; i++) printf "node_wireguard_transmit_bytes_total{interface=\"%s\",peer=\"%s\"} %s\n", iface, peer[i], tx[i]
      }' > "$tmp"
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp" # mktemp gives 0600; the exporter is unprivileged
    ${pkgs.coreutils}/bin/mv "$tmp" "$STATE_DIRECTORY/wireguard.prom"
  '';
in
{
  imports = [
    ./estate.nix
    # The names of the mesh (forts#45): every peer and remote of
    # peers.json as <host>.<zone>, in /etc/hosts and in the cluster DNS.
    # A host with a mesh interface gets the zone of that mesh — the names
    # and the interface are one fact, so one import carries both
    # (forts#60). The add-on half is a services.k3s.manifests entry,
    # inert on a host where k3s is disabled.
    ./mesh-dns.nix
    ./textfile-metrics.nix
  ];

  networking.firewall.allowedUDPPorts = [ 51820 ];

  networking.wg-quick.interfaces.${iface} = {
    # The prefix is the mesh's ([mesh] cidr, sfi.mesh.prefixLength;
    # forts#161): the one place the subnet's width is written on a fort.
    # `fort wg-conf` renders a client's the same.
    address = [ "${self.wg_vpn_ip}/${toString config.sfi.mesh.prefixLength}" ];
    listenPort = 51820;
    privateKeyFile = config.sops.secrets.wireguard-private-key.path;
    peers =
      (lib.mapAttrsToList mkMeshPeer otherPeers)
      ++ (lib.mapAttrsToList mkClientPeer clients)
      ++ (lib.mapAttrsToList mkMeshPeer remotes);
    # Public names never ride the mesh (sfi/deployments#441, forts#45).
    # wg-quick hands a `dns` setting to resolvconf in exclusive mode, and
    # systemd-resolved then routes every query of the host through the
    # mesh interface — day-0 pulls from ghcr.io and uploads off-site waited
    # on a mesh that was down. The mesh interface answers for the names
    # it is given (`~<domain>` routing domains) and for nothing else. A
    # host without resolved has no routing to correct; the mesh names a
    # fort needs are in /etc/hosts regardless (mesh-dns.nix).
    postUp = lib.mkIf config.services.resolved.enable
      "${config.systemd.package}/bin/resolvectl default-route ${iface} false || true";
  };

  # A peer whose endpoint is a DNS NAME (a cloud fort's public FQDN in
  # peers.json — a cloud fort's is one) makes `wg addconf` a DNS client at
  # boot, and a name that does not resolve is a "configuration parsing
  # error": wg-quick tears the interface down and exits 1, and the unit does not
  # retry. A fort that boots while DNS is not yet answering has no mesh,
  # therefore no SSH and no rebuild, until a human reboots it again —
  # the interim at 02:48 on 2026-09-19, mid router reboot (#350): k3s
  # and gitea came up, every fort's gitea pin pointed at a mesh address
  # that never answered. Retry until it works: DNS availability is the
  # only thing the unit is waiting on, and nothing else on the host can
  # stand in for the mesh.
  systemd.services."wg-quick-${iface}" = {
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
    # dial the forge over the mesh this unit has just torn down: its name
    # still resolves (the /etc/hosts pin, lab-dns.nix), the packets leave
    # by the LAN default route and black-hole, and each connect burns the
    # kernel's SYN retry window (~2 min) before it fails. A lab machine
    # removal (#646, 2026-10-02) held sfi-dev-831c637's switch for 13 min
    # that way, and the mesh never came back up. Restart instead: the restart
    # phase runs AFTER the user phase, so the mesh stays up through
    # activation and the agents' oneshots, and a peers.json rollout is one
    # down/up on every fort alike.
    stopIfChanged = false;
  };

  # If the interface is down the run fails and the file freezes — covered by the
  # WireGuardMetricsStale alert on node_textfile_mtime_seconds rather than
  # retried into journal spam.
  systemd.services.wireguard-metrics = {
    description = "Export WireGuard peer state for node_exporter";
    after = [ "wg-quick-${iface}.service" ];
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
