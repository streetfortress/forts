# The metrics a fort publishes, on any provider and any estate (forts#60):
# the node exporter on 9100, and the textfile collectors the framework
# writes into. This is what a monitoring hub scrapes over the mesh
# (forts#38): a cluster fort's edge Prometheus and a plain fort's pulled
# ScrapeConfig read the same exporter under the same job name.
#
# Split out of lab-host.nix, which tied the exporter to the lab CA and the
# homelab's DNS. A fort on a cloud estate publishes these metrics and is
# not in the lab.
{ ... }:

{
  imports = [
    # Directory-size metrics for the host dashboard (#130): node_exporter
    # knows filesystems, not directories.
    ./disk-usage-metrics.nix
    # The textfile collector those metrics ride on, and the wireguard
    # timer (wireguard.nix) and the rebuild gate (auto-rebuild.nix) with
    # them. Module imports dedupe, so the flag is defined once.
    ./textfile-metrics.nix
  ];

  # openFirewall opens 9100 on every interface; hardened.nix forces it off
  # and re-opens 9100 on the mesh interface only. A fort that is not
  # hardened keeps the open port.
  services.prometheus.exporters.node = {
    enable = true;
    openFirewall = true;
  };
}
