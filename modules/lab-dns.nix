# The forge's name, and the LAN's zone. Nothing in here is a property of
# the mesh — that is mesh-dns.nix — and the file contributes two things
# through mesh-dns.nix's extension point, each as a CoreDNS server block
# of its own.
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
# The pin is in /etc/hosts and in the cluster DNS, as a `hosts` line in
# a server block whose zone is the forge's NAME, whole (forts#172): a
# block answers the names under its zone, and the forge's own name has
# none under it, so the block answers that one name and no other.
# CoreDNS picks the longest matching block, so a forge under the LAN
# zone (gitea.zen.lofi under .lofi) still hits its own block first, and
# a forge under a public domain (forge.streetfortress.cloud) takes
# nothing from the rest of that domain: every other name under it
# resolves as every public name does. The block was the name's TLD once,
# which is right for a lab TLD and wrong for .cloud.
#
# The LAN zone is the second thing, and it is the homelab's (.lofi:
# llm.lofi, chat.lofi), not a fort name and not the forge's. A lab fort
# forwards it to its LAN resolvers — the same ones its static LAN block
# names — so a pod there still reaches the LAN; a cloud fort has nowhere
# to send it and renders no block: it knows the forge pin and no LAN
# name. A LAN resolver gates no fort. An estate with no LAN zone sets
# `lanZone = null` and renders no forward block either.
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
  # mesh (forts#1). The pin's block below is that name, whole.
  forgeName = ((estateToml.hubs or { }).forge or { }).name or null;

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
  forwards = cfg.lofiForward != [ ] && cfg.lanZone != null;

  # The forge's block: its name as the zone, the pin as the one answer.
  # `hosts` without `fallthrough` is the end of the line, and nothing
  # else is under this zone to fall through to.
  forgeServer = ''
    ${forgeName}:53 {
      errors
      cache 30
      hosts {
        ${cfg.giteaAddress} ${forgeName}
      }
    }
  '';

  # The LAN zone's block: the forward, where there is one. No pin in
  # here — the forge has its own block, whatever zone its name is under.
  lanServer = ''
    ${cfg.lanZone}:53 {
      errors
      cache 30
      forward . ${lib.concatStringsSep " " cfg.lofiForward} {
        policy sequential
      }
    }
  '';
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
    lanZone = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = "lofi";
      description = ''
        The homelab LAN's DNS zone, which a lab fort's cluster DNS
        forwards to its LAN resolvers (lofiForward). The homelab's zone,
        not a fort name and not the forge's: the forge's name has a block
        of its own whatever zone it is under. Null: no LAN zone, and no
        forward block is rendered.
      '';
    };
    lofiForward = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = lanDNS;
      defaultText = "the host's static LAN resolvers (systemd.network dns): a lab fort's lan_dns; empty on a cloud fort";
      description = ''
        Where the cluster DNS forwards the LAN zone's names (lanZone):
        the homelab's LAN resolvers, which only a lab fort can reach.
        Tried in order. Empty: nothing is forwarded, and the cluster
        answers no LAN name.
      '';
    };
  };

  config = lib.mkMerge [
    (lib.mkIf pinned {
      assertions = [{
        assertion = builtins.match ".+\\..+" forgeName != null;
        message = "sfi.lab-dns: [hubs.forge] name ${forgeName} has one label — the pin's DNS block would be a whole TLD; name the forge under a domain (forge.example). fort check refuses this too.";
      }];
      networking.hosts.${cfg.giteaAddress} = [ forgeName ];
      sfi.mesh.coredns."${forgeName}.server" = forgeServer;
    })
    (lib.mkIf forwards {
      sfi.mesh.coredns."${cfg.lanZone}.server" = lanServer;
    })
  ];
}
