# What makes a host a member of the SFI lab estate, on any provider (#417):
# it trusts the lab CA, knows the names of its mesh (and the forge's), and
# publishes the metrics luxor scrapes. No k3s in here — k3s-server.nix imports this, and
# so does a role module of the estate, such as the dev role (a fort with no
# cluster). Lifted out of
# k3s-server.nix, where dotfiles.nix used to have to go looking for the CA.
{ config, ... }:

{
  imports = [
    ./estate.nix
    # The names of the mesh (forts#45): every peer and remote of
    # peers.json as <host>.<zone>, rendered into /etc/hosts and the
    # coredns-custom add-on. The add-on half is a services.k3s.manifests
    # entry, inert on a host where k3s is disabled.
    ./mesh-dns.nix
    # Today's wiring of the homelab: the gitea.zen.lofi pin and the .lofi
    # forward, through mesh-dns.nix's extension point. Goes with the
    # hub-estate model (forts#38).
    ./lab-dns.nix
    # Directory-size metrics for the host dashboard (#130), and the
    # textfile collector they ride on.
    ./disk-usage-metrics.nix
  ];

  # Trust the lab step-ca root (ACME/TLS for *.lofi services).
  security.pki.certificateFiles = [ (config.sfi.estate.root + "/nixos/certs/step-ca-root.pem") ];

  # Node metrics. openFirewall opens 9100 everywhere on a lab host;
  # hardened.nix narrows that to wg0.
  services.prometheus.exporters.node = {
    enable = true;
    openFirewall = true;
  };
}
