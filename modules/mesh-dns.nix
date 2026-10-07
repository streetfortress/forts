# The names of the mesh: mesh/peers.json is the zone (forts#45).
#
# A fort needs the names of the hosts on its mesh: the forge, the
# monitoring hub, the CA. It knows every one of them at build time — they
# are the peers and remotes of mesh/peers.json — so this module renders
# them, and no resolver is on the fort's path. Every host on the mesh is
# <host>.<zone> at its mesh address, where the zone is the estate's
# (estate.toml `zone`, else <estate>.mesh — estate.nix): luxor.staging.mesh
# is luxor as sfi/staging reaches it, luxor.infra.mesh is luxor on its own
# mesh, and the two cannot be confused. The names are rendered twice from
# the one file:
#
#   1. /etc/hosts (networking.hosts): the auto-rebuild flake fetch and
#      flux-bootstrap's clone work with every DNS down;
#   2. the cluster's coredns-custom ConfigMap, as a k3s add-on manifest
#      (services.k3s.manifests → /var/lib/rancher/k3s/server/manifests),
#      applied by k3s itself at start, i.e. before flux exists. The zone
#      is a zone file (<zone>.db, read by the `file` plugin from the
#      ConfigMap's mount at /etc/coredns/custom/): <host> and *.<host>
#      per entry, so <service>.<host>.<zone> answers that host's mesh
#      address — the convention a provider's service hostnames assume
#      — and a name that is not there is NXDOMAIN, with a SOA. Its
#      serial is a hash of the records: a peers.json change rolls a new
#      ConfigMap, and the plugin reloads on the serial.
#
# This module owns the ConfigMap. k3s mounts exactly one, by name, into
# its CoreDNS pod and imports every `*.server` key of it, and the k3s
# manifest option merges shallowly (types.attrs), so two modules cannot
# each set `data`. Anything else the cluster DNS must answer comes in
# through `sfi.mesh.coredns`, one key per file: lab-dns.nix adds the
# .lofi block (the forge pin, the LAN forward), links.nix a linked
# estate's zone.
{ config, lib, ... }:

let
  cfg = config.sfi.mesh;
  zone = cfg.zone;
  selfName = config.networking.hostName;
  peersData = builtins.fromJSON (builtins.readFile (config.sfi.estate.root + "/mesh/peers.json"));
  # The hosts of the zone: every fort of this estate (peers) and every
  # host of another estate that links here (remotes). Not the clients: a
  # roaming device is dialed by nobody and named by nothing.
  zoneHosts = (peersData.peers or { }) // (peersData.remotes or { });

  records = lib.concatLists (lib.mapAttrsToList
    (n: p: [ "${n} IN A ${p.wg_vpn_ip}" "*.${n} IN A ${p.wg_vpn_ip}" ])
    zoneHosts);
  # A 32-bit serial from the records: nine digits, the first a 1 so the
  # number never starts with a zero, the rest from the hash.
  serial =
    let digits = lib.concatStrings (builtins.filter (c: builtins.match "[0-9]" c != null) (lib.stringToCharacters (builtins.hashString "sha256" (lib.concatStringsSep "\n" records))));
    in lib.toInt ("1" + builtins.substring 0 8 digits);
  zoneFile = lib.concatStringsSep "\n" ([
    "$ORIGIN ${zone}."
    "$TTL 60"
    "@ IN SOA ${selfName}.${zone}. hostmaster.${zone}. ${toString serial} 3600 600 86400 60"
    "@ IN NS ${selfName}.${zone}."
  ] ++ records) + "\n";
in
{
  imports = [ ./estate.nix ];

  options.sfi.mesh.coredns = lib.mkOption {
    type = lib.types.attrsOf lib.types.lines;
    default = { };
    example = lib.literalExpression ''{ "staging.mesh.server" = "staging.mesh:53 { forward . 10.1.0.5 }"; }'';
    description = ''
      More files for the coredns-custom ConfigMap, by name: a
      `<name>.server` is a server block k3s's CoreDNS imports, any
      other name is a file at /etc/coredns/custom/<name>. The zone of
      this mesh is rendered here; lab-dns.nix adds the .lofi block and
      links.nix a linked estate's zone this way.
    '';
  };

  config = {
    networking.hosts = lib.mkMerge
      (lib.mapAttrsToList (n: p: { ${p.wg_vpn_ip} = [ "${n}.${zone}" ]; }) zoneHosts);

    services.k3s.manifests.coredns-custom.content = {
      apiVersion = "v1";
      kind = "ConfigMap";
      metadata = {
        name = "coredns-custom";
        namespace = "kube-system";
        annotations = {
          # Rendered by the host (sfi/forts modules/mesh-dns.nix), not the fleet.
          "kustomize.toolkit.fluxcd.io/prune" = "disabled";
        };
      };
      data = {
        "${zone}.db" = zoneFile;
        "${zone}.server" = ''
          ${zone}:53 {
            errors
            file /etc/coredns/custom/${zone}.db
          }
        '';
      } // cfg.coredns;
    };
  };
}
