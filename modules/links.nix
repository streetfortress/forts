# sfi.links: a host on another estate's mesh (deployments#674).
#
# A fort is on one mesh, its estate's (wireguard.nix: wg0, from
# mesh/peers.json). Some hosts serve more than one estate — the forge
# every estate clones from, a hub a layer serves — and those LINK to the
# other estate: one more WireGuard interface, with its own key, its own
# listen port and the address the other estate allocated. Linking puts
# the host on that mesh as if it were a fort there: it peers with every
# fort of that estate, and every one of them peers with it.
#
# The two halves of a link:
#
#   - here, the declaration: the other estate's repo, the interface, the
#     port, the address. The interface comes up with NO peers.
#   - the reconciler (links/reconcile.sh), a timer: clone the other
#     estate's repo, read mesh/peers.json, and make the interface's peer
#     set equal to it — adds and removes, `wg set` on this interface
#     alone. The last good copy is cached, so a boot with the forge down
#     still brings the peers up. The other estate names THIS host in its
#     `remotes` table (docs/mesh.md): the reconciler finds its own key
#     there and refuses to apply anything when that entry disagrees with
#     the link's address or port — fix one side, not silently.
#
# What the link does not do: open ssh (hardened.nix opens 22 on wg0
# only; a link interface serves exactly the ports of the roles this host
# serves that estate — `roles`, forts#38: the forge's 30022 and 443, and
# for every other role the `ports` its [[serves.<role>]] entry declares
# (forts#105); `openTCPPorts` derives from them and is overridable), forward between meshes (a DROP rule in FORWARD for
# every pair of mesh interfaces: a hub never bridges), or read anything
# but mesh/peers.json. For a private estate the clone needs a read
# credential: a read-only deploy key on that repo, registered by that
# estate's admin. The default is this host's own flux deploy key, which
# gitea lets one register on several repos. A public repo needs none.
#
# The link is the network half of the hub relationship this estate's
# estate.toml declares in [[serves.<role>]] (forts#38): the entries that
# name THIS host and an estate whose URL's last label is the link's name
# supply `roles` and `repo` by default, so a link to an estate this host
# serves needs only the port and the address — the two facts chosen on
# this side.
#
# One name per mesh: the other estate's estate.toml names the interface
# every host gives its mesh (`interface`, estate.nix), and a link to it
# uses that name — so `wg show wg-second` means the same thing on a fort
# of sfi/second-estate and on the host linked to it. The name is set
# here (the unit needs it at build time; a pure eval fetches nothing)
# and the reconciler checks it against the fetched estate.toml, refusing
# to apply on a mismatch as it does for the address and the port. An
# estate that names no interface (wg0, the default) is not checked: a
# link cannot be called wg0 — that is this host's own mesh.
#
# One zone per mesh, the same way (forts#45): the other estate's names
# are <host>.<zone> — its estate.toml's `zone`, else <estate>.mesh — on
# every host on that mesh, this one included. A fort renders its own
# mesh's zone at build time (mesh-dns.nix); a linked host learns the other
# estate's peers at run time, so the reconciler renders that zone into a
# zone file beside the cached peers.json, and a CoreDNS on this host
# serves every link's zone from those files — on loopback for the host
# (systemd-resolved routes `~<zone>` there, per link interface) and on
# this host's own mesh address for its pods (the coredns-custom block
# forwards the zone there). The zone is set here like the interface, and
# the reconciler checks it against the fetched estate.toml the same way.
#
# Limits: an interface name is at most 15 characters from
# [A-Za-z0-9_=+.-] (the kernel's); one UDP port per link, never 51820
# (this host's own mesh port). Both are eval-time assertions here.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.links;
  # The host's config, by a name the link submodule (whose own `config`
  # is the link) can still reach: its sops secrets, its auto-rebuild pin.
  hostConfig = config;
  names = lib.attrNames cfg;
  ifaces = map (n: cfg.${n}.interface) names;
  zones = map (n: cfg.${n}.zone) names;
  # Every mesh interface on this host: its own (estate.toml's name, else
  # wg0 — estate.nix), and one per link.
  own = config.sfi.mesh.interface;
  meshIfaces = [ own ] ++ ifaces;
  pairs = lib.filter (p: p.a != p.b) (lib.cartesianProduct { a = meshIfaces; b = meshIfaces; });

  # This host's address on its own mesh: where its pods reach the zone
  # server below. Null on a host that is in no peers.json of its own.
  peersData = builtins.fromJSON (builtins.readFile (config.sfi.estate.root + "/mesh/peers.json"));
  ownIP = ((peersData.peers or { }).${config.networking.hostName} or { }).wg_vpn_ip or null;

  # The roles (forts#38): what a hub opens on a link to the estate it
  # serves, per role. The forge is the one role the core knows: gitea's
  # ssh and https. Every other role is a name the estate chooses and the
  # `ports` its [[serves.<role>]] entry declares (forts#105) — a layer's
  # role (a monitoring hub's remote_write and push ports) or the estate's
  # own; the core renders nothing for it.
  forgePorts = [ 30022 443 ];

  # This estate's serves tables ([[serves.<role>]] fort, estates), read
  # for THIS host: the roles whose entry names this host and lists an
  # estate whose URL's last label is the link's name (ssh://…/sfi/staging
  # → staging), and that estate's URL for the clone.
  tomlPath = config.sfi.estate.root + "/estate.toml";
  estateToml = if builtins.pathExists tomlPath then builtins.fromTOML (builtins.readFile tomlPath) else { };
  lastLabel = url: lib.removeSuffix ".git" (lib.last (lib.splitString "/" (lib.removeSuffix "/" url)));
  servesTables = estateToml.serves or { };
  servedHere = name: role: lib.concatMap
    (entry: lib.filter (url: lastLabel url == name) (entry.estates or [ ]))
    (lib.filter (entry: (entry.fort or null) == config.networking.hostName) (servesTables.${role} or [ ]));
  rolesFor = name: lib.filter (role: servedHere name role != [ ]) (lib.attrNames servesTables);
  # The ports those entries declare for this host and that estate, plus
  # the forge's when the forge is one of the roles.
  portsFor = name: lib.unique (lib.concatMap
    (role: lib.concatMap (entry: if role == "forge" then forgePorts else (entry.ports or [ ]))
      (lib.filter (entry: (entry.fort or null) == config.networking.hostName && lib.any (url: lastLabel url == name) (entry.estates or [ ])) (servesTables.${role} or [ ])))
    (lib.attrNames servesTables));
  estateURLFor = name:
    let urls = lib.unique (lib.concatMap (role: servedHere name role) (lib.attrNames servesTables));
    in if urls == [ ] then null else lib.head urls;

  # The zone file a link starts with, until the reconciler's first run
  # writes the real one: a SOA and nothing else, so CoreDNS comes up.
  emptyZone = n: pkgs.writeText "wg-link-${n}-zone.db" ''
    $ORIGIN ${cfg.${n}.zone}.
    $TTL 60
    @ IN SOA ${config.networking.hostName}.${cfg.${n}.zone}. hostmaster.${cfg.${n}.zone}. 1 3600 600 86400 60
    @ IN NS ${config.networking.hostName}.${cfg.${n}.zone}.
  '';
  zoneRE = "^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$";

  # `config` here is the link's own (openTCPPorts reads roles); the
  # host's is hostConfig.
  linkOpts = { name, config, ... }: {
    options = {
      repo = lib.mkOption {
        type = lib.types.str;
        default =
          if estateURLFor name != null then estateURLFor name
          else throw "sfi.links.${name}.repo: no [[serves.<role>]] entry of estate.toml names this host the hub of an estate called ${name} — set repo in the host file, or declare the entry (forts#38)";
        defaultText = "the URL of the estate named after this link, from the serves tables that name this host";
        example = "ssh://git@forge.example:22/acme/second-estate.git";
        description = "The other estate's git URL: the repo whose mesh/peers.json is the source of truth for this interface's peers. https:// for a public repo; ssh:// with `sshKeyFile` for a private one. Defaults to the [[serves]] entry of estate.toml whose estate's last label is this link's name.";
      };
      roles = lib.mkOption {
        type = lib.types.listOf lib.types.str;
        default = rolesFor name;
        defaultText = "the roles whose [[serves.<role>]] entry names this host and the estate named after this link, else none";
        example = [ "forge" "monitoring" ];
        description = "The roles this host serves the other estate (forts#38): what `openTCPPorts` derives from — forge: 30022 and 443; any other role: the `ports` its [[serves.<role>]] entry declares (forts#105). Defaults to the roles whose entry in estate.toml names this host and that estate.";
      };
      ref = lib.mkOption {
        type = lib.types.str;
        default = "main";
        description = "The branch the reconciler reads.";
      };
      interface = lib.mkOption {
        type = lib.types.str;
        default = "wg-${name}";
        description = "The WireGuard interface for this link: the name the other estate's estate.toml gives its mesh (`interface`), which the reconciler checks. At most 15 characters; never this host's own mesh interface.";
      };
      zone = lib.mkOption {
        type = lib.types.str;
        default = "${name}.mesh";
        description = "The DNS zone of the other estate's mesh: its estate.toml's `zone`, else <estate>.mesh, which the reconciler checks. Every peer and remote of that estate is <host>.<zone>, served on this host from the reconciler's zone file.";
      };
      port = lib.mkOption {
        type = lib.types.port;
        description = "The UDP port this interface listens on — what the other estate's `remotes` entry for this host carries as wg_endpoint_port. Never 51820.";
      };
      address = lib.mkOption {
        type = lib.types.str;
        example = "10.2.0.4";
        description = "This host's address on the other estate's mesh, allocated by that estate: its `remotes` entry's wg_vpn_ip. The reconciler refuses to apply when the two disagree.";
      };
      prefixLength = lib.mkOption {
        type = lib.types.int;
        default = 24;
        description = "The other mesh's prefix length (every estate's mesh is a /24).";
      };
      openTCPPorts = lib.mkOption {
        type = lib.types.listOf lib.types.port;
        default = portsFor name;
        defaultText = "the ports of `roles`: the forge's pair, and the `ports` of every other role's serves entry";
        example = [ 30022 443 ];
        description = "TCP ports the other estate's forts may reach on this interface: the ports of the roles this host serves it, by default. Nothing else answers there — not ssh, not the exporters.";
      };
      secret = lib.mkOption {
        type = lib.types.str;
        default = "wireguard-private-key-${name}";
        description = "The sops secret (in this host's secrets file) holding the interface's private key. Declared here with mode 0400; the key is sops-set by hand, its public half goes into the other estate's `remotes` entry.";
      };
      sshKeyFile = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = if hostConfig.sops.secrets ? flux-git-deploy-key then hostConfig.sops.secrets.flux-git-deploy-key.path else null;
        defaultText = "this host's flux deploy key, when it has one";
        description = "The private key the clone uses for an ssh:// repo. Null for a public https:// repo.";
      };
      knownHosts = lib.mkOption {
        type = lib.types.lines;
        default = if hostConfig.sfi ? auto-rebuild then hostConfig.sfi.auto-rebuild.gitKnownHosts else "";
        defaultText = "sfi.auto-rebuild.gitKnownHosts";
        description = "known_hosts line(s) for the other estate's git host.";
      };
      interval = lib.mkOption {
        type = lib.types.str;
        default = "5min";
        description = "How often the reconciler re-reads the other estate's peers.json (systemd OnUnitActiveSec).";
      };
    };
  };

  ifaceRE = "^[A-Za-z0-9_=+.-]{1,15}$";
in
{
  imports = [
    ./estate.nix # sfi.mesh.interface: this host's own mesh interface
    ./mesh-dns.nix # sfi.mesh.coredns: the linked zones in this host's cluster DNS
    ./textfile-metrics.nix # wg-link-<name>.prom
    ./ops/systemd.nix # sfi-systemd-restart wg-quick-<iface>, wg-link-<name>, coredns
  ];

  options.sfi.links = lib.mkOption {
    type = lib.types.attrsOf (lib.types.submodule linkOpts);
    default = { };
    description = "The other estates this host is on the mesh of, by link name (the other estate's name, by convention).";
  };

  config = lib.mkIf (cfg != { }) {
    assertions =
      (lib.concatMap
        (n:
          let l = cfg.${n}; in
          [
            {
              assertion = builtins.match ifaceRE l.interface != null;
              message = "sfi.links.${n}.interface = \"${l.interface}\": an interface name is at most 15 characters from [A-Za-z0-9_=+.-] (the kernel's limit)";
            }
            {
              assertion = l.interface != own;
              message = "sfi.links.${n}: ${own} is this host's own mesh (wireguard.nix, estate.toml); a link gets an interface of its own";
            }
            {
              assertion = l.port != 51820;
              message = "sfi.links.${n}.port = 51820 is this host's own mesh port (wireguard.nix); a link listens beside it";
            }
            {
              assertion = builtins.match zoneRE l.zone != null;
              message = "sfi.links.${n}.zone = \"${l.zone}\": want a DNS name of lowercase labels (second-estate.mesh)";
            }
            {
              assertion = l.zone != config.sfi.mesh.zone;
              message = "sfi.links.${n}.zone = \"${l.zone}\" is this host's own mesh zone (estate.toml); the other estate's names need a zone of their own";
            }
            {
              assertion = builtins.match "^[0-9]+\\.[0-9]+\\.[0-9]+\\.[0-9]+$" l.address != null;
              message = "sfi.links.${n}.address = \"${l.address}\": want this host's IPv4 address on the other mesh";
            }
            {
              assertion = !(lib.hasPrefix "ssh://" l.repo) || l.sshKeyFile != null;
              message = "sfi.links.${n}: an ssh:// repo needs sshKeyFile (a read-only deploy key on that repo)";
            }
          ])
        names)
      ++ [
        {
          assertion = lib.length (lib.unique ifaces) == lib.length ifaces;
          message = "sfi.links: two links share an interface name";
        }
        {
          assertion = lib.length (lib.unique (map (n: cfg.${n}.port) names)) == lib.length names;
          message = "sfi.links: two links share a listen port";
        }
        {
          assertion = lib.length (lib.unique zones) == lib.length zones;
          message = "sfi.links: two links share a zone";
        }
      ];

    sops.secrets = lib.listToAttrs (map
      (n: lib.nameValuePair cfg.${n}.secret { mode = "0400"; owner = "root"; })
      names);

    networking.firewall.allowedUDPPorts = map (n: cfg.${n}.port) names;
    networking.firewall.interfaces = lib.listToAttrs (map
      (n: lib.nameValuePair cfg.${n}.interface { allowedTCPPorts = cfg.${n}.openTCPPorts; })
      names)
    # The zone server, for this host's pods: they reach the host's own
    # mesh address through cni0. Not open on any mesh interface — the
    # forts of the other estate resolve their names themselves.
    // lib.optionalAttrs (ownIP != null) {
      cni0 = { allowedUDPPorts = [ 53 ]; allowedTCPPorts = [ 53 ]; };
    };

    # A hub never bridges: no forwarding between any two mesh interfaces,
    # whatever else (k3s's ip_forward, flannel's ACCEPTs) is on the host.
    # Inserted at the top of FORWARD so nothing appended later outranks it.
    networking.firewall.extraCommands = lib.concatMapStrings
      (p: "iptables -I FORWARD -i ${p.a} -o ${p.b} -j DROP\n")
      pairs;
    networking.firewall.extraStopCommands = lib.concatMapStrings
      (p: "iptables -D FORWARD -i ${p.a} -o ${p.b} -j DROP 2>/dev/null || true\n")
      pairs;

    networking.wg-quick.interfaces = lib.listToAttrs (map
      (n:
        let l = cfg.${n}; in
        lib.nameValuePair l.interface {
          address = [ "${l.address}/${toString l.prefixLength}" ];
          listenPort = l.port;
          privateKeyFile = config.sops.secrets.${l.secret}.path;
          # No peers at build time: the reconciler owns the set.
          peers = [ ];
          # The host's own resolution of the other estate's names: the
          # zone, and only the zone, routes to the zone server on
          # loopback — set explicitly, not through wg-quick's `dns`,
          # whose exclusive mode would route every name of the host
          # there (wireguard.nix). Nothing to route without resolved.
          postUp = lib.mkIf config.services.resolved.enable
            (lib.concatMapStringsSep "\n" (c: "${config.systemd.package}/bin/resolvectl ${c} || true") [
              "dns ${l.interface} 127.0.0.1"
              "domain ${l.interface} ~${l.zone}"
              "default-route ${l.interface} false"
            ]);
        })
      names);

    # The zone server: one CoreDNS for every link's zone, from the zone
    # file the reconciler keeps beside its cached peers.json (`file`
    # reloads it on a serial change). On loopback for this host and on
    # its own mesh address for its pods; the firewall opens it to the
    # pods and nobody else. It binds that address, so it starts after
    # this host's own mesh is up and retries until it is.
    services.coredns = {
      enable = true;
      config = lib.concatMapStrings
        (n: ''
          ${cfg.${n}.zone}:53 {
            bind 127.0.0.1${lib.optionalString (ownIP != null) " ${ownIP}"}
            errors
            file /var/lib/wg-link-${n}/zone.db {
              reload 10s
            }
          }
        '')
        names;
    };
    systemd.tmpfiles.rules = lib.concatMap
      (n: [
        "d /var/lib/wg-link-${n} 0755 root root -"
        "C /var/lib/wg-link-${n}/zone.db - - - - ${emptyZone n}"
      ])
      names;

    # The same zones for this host's pods: the cluster DNS forwards each
    # one to the zone server at the host's mesh address (mesh-dns.nix
    # renders the ConfigMap; inert on a host without k3s).
    sfi.mesh.coredns = lib.optionalAttrs (ownIP != null) (lib.listToAttrs (map
      (n: lib.nameValuePair "${cfg.${n}.zone}.server" ''
        ${cfg.${n}.zone}:53 {
          errors
          cache 30
          forward . ${ownIP}
        }
      '')
      names));

    sfi.ops.restartableUnits =
      (map (n: "wg-quick-${cfg.${n}.interface}") names)
      ++ (map (n: "wg-link-${n}") names)
      ++ [ "coredns" ];

    systemd.services = lib.mkMerge ([{
      # The zone server (above): after this host's own mesh, retried
      # until its address is there to bind.
      coredns = {
        after = [ "wg-quick-${own}.service" ];
        wants = [ "wg-quick-${own}.service" ];
        unitConfig.StartLimitIntervalSec = 0;
        serviceConfig.RestartSec = "5s";
      };
    }] ++ map
      (n:
        let
          l = cfg.${n};
          knownHostsFile = pkgs.writeText "wg-link-${n}-known-hosts" l.knownHosts;
        in
        {
          # The same retry shape as wg0 (wireguard.nix): a link that fails
          # to come up is retried, and a switch restarts it late.
          "wg-quick-${l.interface}" = {
            unitConfig.StartLimitIntervalSec = 0;
            serviceConfig = {
              Restart = "on-failure";
              RestartSec = "15s";
            };
            stopIfChanged = false;
          };
          "wg-link-${n}" = {
            description = "Reconcile ${l.interface} and the ${l.zone} zone with ${l.repo} (mesh/peers.json)";
            after = [ "network-online.target" "wg-quick-${l.interface}.service" ];
            wants = [ "network-online.target" ];
            requires = [ "wg-quick-${l.interface}.service" ];
            path = with pkgs; [ git openssh jq wireguard-tools coreutils gnugrep ];
            environment = {
              LINK_NAME = n;
              LINK_IFACE = l.interface;
              LINK_ZONE = l.zone;
              LINK_REPO = l.repo;
              LINK_REF = l.ref;
              LINK_ADDRESS = l.address;
              LINK_PORT = toString l.port;
              LINK_SSH_KEY = if l.sshKeyFile == null then "" else l.sshKeyFile;
              LINK_KNOWN_HOSTS = "${knownHostsFile}";
            };
            serviceConfig = {
              Type = "oneshot";
              ExecStart = "${pkgs.writeShellScript "wg-link-${n}" (builtins.readFile ./links/reconcile.sh)}";
              StateDirectory = [ "wg-link-${n}" "node-exporter-textfiles" ];
            };
          };
        })
      names);

    systemd.timers = lib.listToAttrs (map
      (n: lib.nameValuePair "wg-link-${n}" {
        wantedBy = [ "timers.target" ];
        timerConfig = {
          OnBootSec = "30s";
          OnUnitActiveSec = cfg.${n}.interval;
          RandomizedDelaySec = "30s";
        };
      })
      names);
  };
}
