# The forge's name, and the LAN's zone. Nothing in here is a property of
# the mesh — that is mesh-dns.nix — and the file contributes two things
# through mesh-dns.nix's extension point:
#
# The forge is the one exception to <host>.<zone>: its name is an
# identity, not a location — it is in every git remote, every OCI
# reference, every flux source and the registry certificate, and one
# forge is on several meshes. So the forge keeps its one name on every
# mesh, and this module pins that name to the forge's address on THIS
# mesh. The address is an estate fact, derived, not written here
# (forts#26): estate.toml's `[hubs.forge]` names the forge HOST
# (docs/hub.md), and mesh/peers.json gives that host's address on this
# mesh — its `peers` entry when the forge is a fort of this estate, its
# `remotes` entry when it links here from another estate (docs/mesh.md).
# The address is written in that entry and nowhere else, so a forge that
# moves, or an estate on a second mesh, changes one line in one file and
# every fort rebuilds. An estate that names no forge host gets no pin:
# `giteaAddress` is null, nothing is rendered, and `fort check` says so.
# The forge's NAME is an estate fact too: `[hubs.forge] name`, read
# below. An estate whose forge is a hosted one with a public name
# (github.com, forts#1) declares no [hubs.forge] and gets no pin: the
# name resolves by public DNS, as every other public name does.
#
# The pin is in /etc/hosts and in the cluster DNS, as a hosts line of
# the .lofi block: not a record of the zone file, because the .lofi
# block also forwards on a lab fort and the file plugin does not fall
# through.
#
# .lofi is the homelab's LAN zone (llm.lofi, chat.lofi), not a fort
# name. A lab fort forwards it to its LAN resolvers — the same ones its
# static LAN block names — so a pod there still reaches the LAN; a cloud
# fort has nowhere to send it and forwards nothing: it knows the forge
# pin and no other name under the forge's TLD. A LAN resolver gates no
# fort.
{ config, lib, ... }:

let
  cfg = config.sfi.lab-dns;

  # The forge host: estate.toml's [hubs.forge] fort. Read here the way
  # estate.nix reads `interface` and `zone` and links.nix reads the
  # serves tables — the file is the estate's, and a module that needs
  # one fact from it reads that fact.
  tomlPath = config.sfi.estate.root + "/estate.toml";
  estateToml = if builtins.pathExists tomlPath then builtins.fromTOML (builtins.readFile tomlPath) else { };
  forgeHost = ((estateToml.hubs or { }).forge or { }).fort or null;
  # Its name: [hubs.forge] name, the one name the forge keeps on every
  # mesh (forts#1). The block below serves that name's TLD.
  forgeName = ((estateToml.hubs or { }).forge or { }).name or null;
  forgeTLD = if forgeName == null then null else lib.last (lib.splitString "." forgeName);

  # Its address on this mesh: the peers and remotes of mesh/peers.json,
  # the same two tables that are the zone (mesh-dns.nix). A forge named
  # by [hubs.forge] and absent from both is a declaration this module
  # cannot honour, so it throws by name rather than pin the wrong
  # address; `fort check` reports it before nix does.
  peersData = builtins.fromJSON (builtins.readFile (config.sfi.estate.root + "/mesh/peers.json"));
  meshHosts = (peersData.peers or { }) // (peersData.remotes or { });
  forgeAddress =
    if forgeHost == null then null
    else if meshHosts ? ${forgeHost} then meshHosts.${forgeHost}.wg_vpn_ip
    else throw "sfi.lab-dns.giteaAddress: ${toString tomlPath} names ${forgeHost} in [hubs.forge], and mesh/peers.json has neither peers.${forgeHost} nor remotes.${forgeHost} — the forge's address on this mesh is that entry (docs/mesh.md, \"Linking\"); add it, or set the address in the host file";

  # The host's static LAN resolvers, as its systemd.network block names
  # them ([libvirt] / [baremetal] lan_dns, rendered by fort scaffold).
  # A cloud host has no such block, and the list is empty.
  lanDNS = lib.unique (lib.concatMap (n: n.dns or [ ]) (lib.attrValues config.systemd.network.networks));

  pinned = cfg.giteaAddress != null && forgeName != null;
  forwards = cfg.lofiForward != [ ];

  # The .lofi block: the forge pin, and the LAN forward where there is
  # one. `hosts` without `fallthrough` is the end of the line: a cloud
  # fort answers no other .lofi name. With neither half there is no
  # block at all (config, below).
  lofiServer = lib.concatStringsSep "\n" ([
    "${forgeTLD}:53 {"
    "  errors"
    "  cache 30"
  ] ++ lib.optionals pinned ([
    "  hosts {"
    "    ${cfg.giteaAddress} ${forgeName}"
  ] ++ lib.optionals forwards [
    "    fallthrough"
  ] ++ [
    "  }"
  ]) ++ lib.optionals forwards [
    "  forward . ${lib.concatStringsSep " " cfg.lofiForward} {"
    "    policy sequential"
    "  }"
  ] ++ [
    "}"
  ]) + "\n";
in
{
  imports = [ ./mesh-dns.nix ];

  options.sfi.lab-dns = {
    giteaAddress = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = forgeAddress;
      defaultText = "the [hubs.forge] host's address in mesh/peers.json (its peers entry, else its remotes entry); null where estate.toml names no forge host";
      description = ''
        The forge's address on this mesh ([hubs.forge] name): the one name
        that is not <host>.<zone>, pinned in /etc/hosts and the cluster
        DNS. An estate fact — estate.toml's [hubs.forge] names the host
        and mesh/peers.json gives its address — so a host file sets this
        only to override the estate. Null: no pin, and the fort resolves
        the forge's name by whatever other DNS it has.
      '';
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

  config = lib.mkMerge [
    (lib.mkIf pinned {
      networking.hosts.${cfg.giteaAddress} = [ forgeName ];
    })
    (lib.mkIf (pinned || (forwards && forgeTLD != null)) {
      sfi.mesh.coredns."${forgeTLD}.server" = lofiServer;
    })
  ];
}
