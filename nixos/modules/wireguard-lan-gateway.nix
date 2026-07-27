# Bridge-host WireGuard gateway: routes the k8s LAN (10.0.0.0/24) into the mesh
# via this host, so microVM workers (and pods) reach 10.1.0.0/24 without each
# needing its own mesh peer. Imported only by the bridge host (zenbook); pairs
# with ./wireguard.nix.
{ pkgs, ... }:

{
  boot.kernel.sysctl."net.ipv4.ip_forward" = 1;

  networking.wg-quick.interfaces.wg0 = {
    postUp = ''
      ${pkgs.iptables}/bin/iptables -A FORWARD -i br0 -o wg0 -j ACCEPT
      ${pkgs.iptables}/bin/iptables -A FORWARD -i wg0 -o br0 -j ACCEPT
      ${pkgs.iptables}/bin/iptables -t nat -A POSTROUTING -s 10.0.0.0/24 -o wg0 -j MASQUERADE
      # Clamp TCP MSS to the WG path MTU. Without this, pod traffic (flannel
      # VXLAN, MTU 1450) crossing the tunnel produces oversized segments that
      # WG silently drops, breaking long TLS records with "bad record MAC".
      ${pkgs.iptables}/bin/iptables -t mangle -A FORWARD -o wg0 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
      ${pkgs.iptables}/bin/iptables -t mangle -A FORWARD -i wg0 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
      ${pkgs.iptables}/bin/iptables -t mangle -A OUTPUT  -o wg0 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1360
    '';
    preDown = ''
      ${pkgs.iptables}/bin/iptables -D FORWARD -i br0 -o wg0 -j ACCEPT
      ${pkgs.iptables}/bin/iptables -D FORWARD -i wg0 -o br0 -j ACCEPT
      ${pkgs.iptables}/bin/iptables -t nat -D POSTROUTING -s 10.0.0.0/24 -o wg0 -j MASQUERADE
      ${pkgs.iptables}/bin/iptables -t mangle -D FORWARD -o wg0 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
      ${pkgs.iptables}/bin/iptables -t mangle -D FORWARD -i wg0 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
      ${pkgs.iptables}/bin/iptables -t mangle -D OUTPUT  -o wg0 -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --set-mss 1360
    '';
  };
}
