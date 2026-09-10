# Shared wiring for node_exporter textfile collectors (#121, #130).
# Imported by every module that writes metrics into the textfile
# directory (wireguard.nix, disk-usage-metrics.nix, auto-rebuild.nix);
# module imports dedupe, so the flag is defined exactly once however many
# collectors a host carries.
#
# Contract for writers: create files via a systemd unit with
# StateDirectory = "node-exporter-textfiles" (systemd owns creation of
# /var/lib/node-exporter-textfiles), write to a temp file in the same
# directory, `chmod 0644` it, then rename over `<name>.prom` — mktemp
# defaults to 0600 and node_exporter runs unprivileged, so a missed chmod
# turns every file in the directory into node_textfile_scrape_error 1
# (found live on staging-k3s after the #127 rollout).
{ ... }:

{
  # The exporter itself is enabled by the k3s modules; harmless where off.
  services.prometheus.exporters.node.extraFlags = [
    "--collector.textfile.directory=/var/lib/node-exporter-textfiles"
  ];

  # Writers using StateDirectory get the directory from systemd; this
  # covers writers that run before any of them on a fresh boot (the
  # auto-rebuild gate writes its revision metric from ExecCondition).
  systemd.tmpfiles.rules = [
    "d /var/lib/node-exporter-textfiles 0755 root root -"
  ];
}
