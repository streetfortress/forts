# WireGuard mesh membership. Reusable across hosts: the local peer is picked by
# hostname, so any host whose networking.hostName matches a key in
# wireguard/peers.json can import this. Requires the sops secret
# `wireguard-private-key` (declared in the host's secrets module).
#
# Hosts that route the k8s LAN into the mesh (the bridge host) additionally
# import ./wireguard-lan-gateway.nix.
{ config, lib, ... }:

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
}
