# Today's wiring of the homelab into a fort: the forge's name, and the
# LAN's zone. Nothing in here is a property of the mesh — that is
# mesh-dns.nix — and the file goes when the hub-estate model (forts#38)
# and the forge's entry in peers.json (forts#26) make it redundant. Until
# then it contributes two things through mesh-dns.nix's extension point:
#
# The forge is the one exception to <host>.<zone>: gitea.zen.lofi is an
# identity, not a location — it is in every git remote, every OCI
# reference, every flux source and the registry certificate, and one
# gitea is on several meshes. So the forge keeps its one name on every
# mesh, and this module pins that name to the forge's address on THIS
# mesh (`giteaAddress`; forts#26 derives it from the forge host's
# peers.json entry, and a rename is forts#1). Until then the default is
# the sfi/deployments mesh address of sfi-fort-zenbook, and an estate on
# another mesh sets it in the host file. The pin is in /etc/hosts and in
# the cluster DNS, as a hosts line of the .lofi block: not a record of
# the zone file, because the .lofi block also forwards on a lab fort and
# the file plugin does not fall through.
#
# .lofi is the homelab's LAN zone (llm.lofi, chat.lofi), not a fort
# name. A lab fort forwards it to its LAN resolvers — the same ones its
# static LAN block names — so a pod there still reaches the LAN; a cloud
# fort has nowhere to send it and forwards nothing: it knows the forge
# pin and no other .lofi name. The dnsmasq on soundship (10.1.0.1) gates
# no fort any more.
{ config, lib, ... }:

let
  cfg = config.sfi.lab-dns;
  forgeName = "gitea.zen.lofi";

  # The host's static LAN resolvers, as its systemd.network block names
  # them ([libvirt] / [baremetal] lan_dns, rendered by fort scaffold).
  # A cloud host has no such block, and the list is empty.
  lanDNS = lib.unique (lib.concatMap (n: n.dns or [ ]) (lib.attrValues config.systemd.network.networks));

  # The .lofi block: the forge pin, and the LAN forward where there is
  # one. `hosts` without `fallthrough` is the end of the line: a cloud
  # fort answers no other .lofi name.
  lofiServer = lib.concatStringsSep "\n" ([
    "lofi:53 {"
    "  errors"
    "  cache 30"
    "  hosts {"
    "    ${cfg.giteaAddress} ${forgeName}"
  ] ++ lib.optionals (cfg.lofiForward != [ ]) [
    "    fallthrough"
    "  }"
    "  forward . ${lib.concatStringsSep " " cfg.lofiForward} {"
    "    policy sequential"
  ] ++ [
    "  }"
    "}"
  ]) + "\n";
in
{
  imports = [ ./mesh-dns.nix ];

  options.sfi.lab-dns = {
    giteaAddress = lib.mkOption {
      type = lib.types.str;
      # Hop two (#350): sfi-fort-zenbook on the sfi/deployments mesh. An
      # estate on another mesh sets the forge's address there (forts#26
      # renders it from the forge host's peers.json entry).
      default = "10.1.0.4";
      description = "The forge's (gitea.zen.lofi's) address on this mesh: the one name that is not <host>.<zone>, pinned in /etc/hosts and the cluster DNS.";
    };
    lofiForward = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = lanDNS;
      defaultText = "the host's static LAN resolvers (systemd.network dns): a lab fort's lan_dns; empty on a cloud fort";
      description = ''
        Where the cluster DNS forwards the .lofi names that are not the
        forge: the homelab's LAN zone, which only a lab fort can reach.
        Tried in order. Empty: nothing is forwarded, and the cluster
        answers no .lofi name but the forge's.
      '';
    };
  };

  config = {
    networking.hosts.${cfg.giteaAddress} = [ forgeName ];
    sfi.mesh.coredns."lofi.server" = lofiServer;
  };
}
