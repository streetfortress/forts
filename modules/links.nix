# sfi.links: a host on another estate's mesh (deployments#674).
#
# A fort is on one mesh, its estate's (wireguard.nix: wg0, from
# mesh/peers.json). Some hosts serve more than one estate — the forge
# every estate clones from, a monitoring hub — and those LINK to the
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
# only; a link interface serves exactly `openTCPPorts`), forward between
# meshes (a DROP rule in FORWARD for every pair of mesh interfaces: a hub
# never bridges), or read anything but mesh/peers.json. For a private
# estate the clone needs a read credential: a read-only deploy key on
# that repo, registered by that estate's admin. The default is this
# host's own flux deploy key, which gitea lets one register on several
# repos. A public repo needs none.
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
# Limits: an interface name is at most 15 characters from
# [A-Za-z0-9_=+.-] (the kernel's); one UDP port per link, never 51820
# (this host's own mesh port). Both are eval-time assertions here.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.links;
  names = lib.attrNames cfg;
  ifaces = map (n: cfg.${n}.interface) names;
  # Every mesh interface on this host: its own (estate.toml's name, else
  # wg0 — estate.nix), and one per link.
  own = config.sfi.mesh.interface;
  meshIfaces = [ own ] ++ ifaces;
  pairs = lib.filter (p: p.a != p.b) (lib.cartesianProduct { a = meshIfaces; b = meshIfaces; });

  linkOpts = { name, ... }: {
    options = {
      repo = lib.mkOption {
        type = lib.types.str;
        example = "ssh://git@gitea.zen.lofi:30022/sfi/second-estate.git";
        description = "The other estate's git URL: the repo whose mesh/peers.json is the source of truth for this interface's peers. https:// for a public repo; ssh:// with `sshKeyFile` for a private one.";
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
        default = [ ];
        example = [ 30022 443 ];
        description = "TCP ports the other estate's forts may reach on this interface. Nothing else answers there — not ssh, not the exporters.";
      };
      secret = lib.mkOption {
        type = lib.types.str;
        default = "wireguard-private-key-${name}";
        description = "The sops secret (in this host's secrets file) holding the interface's private key. Declared here with mode 0400; the key is sops-set by hand, its public half goes into the other estate's `remotes` entry.";
      };
      sshKeyFile = lib.mkOption {
        type = lib.types.nullOr lib.types.str;
        default = if config.sops.secrets ? flux-git-deploy-key then config.sops.secrets.flux-git-deploy-key.path else null;
        defaultText = "this host's flux deploy key, when it has one";
        description = "The private key the clone uses for an ssh:// repo. Null for a public https:// repo.";
      };
      knownHosts = lib.mkOption {
        type = lib.types.lines;
        default = if config.sfi ? auto-rebuild then config.sfi.auto-rebuild.gitKnownHosts else "";
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
    ./textfile-metrics.nix # wg-link-<name>.prom
    ./ops/systemd.nix # sfi-systemd-restart wg-quick-<iface>, wg-link-<name>
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
      ];

    sops.secrets = lib.listToAttrs (map
      (n: lib.nameValuePair cfg.${n}.secret { mode = "0400"; owner = "root"; })
      names);

    networking.firewall.allowedUDPPorts = map (n: cfg.${n}.port) names;
    networking.firewall.interfaces = lib.listToAttrs (map
      (n: lib.nameValuePair cfg.${n}.interface { allowedTCPPorts = cfg.${n}.openTCPPorts; })
      names);

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
        })
      names);

    sfi.ops.restartableUnits =
      (map (n: "wg-quick-${cfg.${n}.interface}") names)
      ++ (map (n: "wg-link-${n}") names);

    systemd.services = lib.mkMerge (map
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
            description = "Reconcile ${l.interface} with ${l.repo} (mesh/peers.json)";
            after = [ "network-online.target" "wg-quick-${l.interface}.service" ];
            wants = [ "network-online.target" ];
            requires = [ "wg-quick-${l.interface}.service" ];
            path = with pkgs; [ git openssh jq wireguard-tools coreutils gnugrep ];
            environment = {
              LINK_NAME = n;
              LINK_IFACE = l.interface;
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
