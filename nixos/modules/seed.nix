# The seed reader (#606; the decision is #605's comment of 2026-10-02).
#
# A fort decrypts its secrets with sops-nix at activation, with the age
# identity derived from /etc/ssh/ssh_host_ed25519_key (hosts/<host>/
# secrets.nix). Until #659 `fort image` grafted that key into the
# bootstrap image, which made the image a copy of every secret the fort
# holds, kept in a provider's object storage we cannot wipe. This module
# is where the key lives instead: a channel the platform provides. The
# image is a plain build output, and the key arrives at first boot,
# before activation.
#
# Where it runs: the systemd initrd (the fleet's nixpkgs pin defaults to
# it on every host), as sfi-seed.service — after the root is mounted on
# /sysroot, required by initrd.target, so before the switch to stage 2.
# Activation does not run in stage 2 on this nixpkgs: it runs in the
# initrd too, as initrd-nixos-activation.service, after
# initrd-switch-root.target, which is after initrd.target. The seed unit
# is required by initrd.target, so it finishes before activation starts
# and sops-nix finds the key where it is today — the #656 journal shows
# the seed done a quarter second before activation. The sops wiring and
# the unit order do not change. A stage-2 unit would write the key after
# activation had run once without it, and every unit that needs a secret
# would have failed by then; this is why the reader is in the initrd
# (#605, "Why the initrd and not a stage-2 unit").
#
# What it does: if the key is on the root, nothing (a ConditionPathExists
# on the unit, and the script checks again). Otherwise it reads the seed
# from the channel that sfi.platform selects, writes the key (0600) and
# its .pub (0644) to the root, and exits. If the channel has no seed, the
# unit fails, initrd.target fails with it, and systemd stops the boot in
# the initrd's emergency target with the reader's message on the console
# (journal+console). The shell is in ./seed/: reader.sh is the shared
# half, channel-*.sh one per channel, POSIX sh run by bash under `set -e`
# (shellcheck -s bash passes on all of them).
#
# Channels, by platform (#605's table):
#   linode    → linode-metadata  metadata.user_data on the instance, read
#               over one DHCP lease on eth0 from the Metadata service's
#               link-local address. `fort provision` sets the value
#               (forts/pulumi/linode.go). The initrd gains virtio_net,
#               af_packet, iproute2, busybox's udhcpc and a minimal curl
#               (the token request is a PUT, which busybox wget cannot
#               send). No systemd-networkd in the initrd: the lease is
#               the unit's own, and it flushes the link before stage 2.
#               Stage 2 then CLOSES the channel: a blackhole route to the
#               service's two addresses (sfi-seed-close.service, below).
#   qemu      → nocloud          /user-data on a volume labelled cidata,
#               the layout of cloud-init's NoCloud datasource (#657). On
#               a lab fort it is the ISO that pulumi-libvirt's cloud-init
#               disk resource writes into the images pool and attaches to
#               the domain as a CD-ROM (forts/pulumi/libvirt.go); `fort
#               provision` fills it from the sops file. The file is the
#               seed text, no base64. The initrd gains isofs (the iso9660
#               driver), sr_mod (cdrom with it) and the two IDE/SATA
#               controller drivers libvirt may attach the CD to. No
#               network.
#               No close step. A block device is root-only — a pod or
#               the unprivileged agent user cannot open /dev/sr0 — and
#               the mount is undone in the initrd. The pair of the
#               Linode close is therefore nothing, and it is written
#               here so the pair is visible: the Linode channel is open to
#               every process on the host until stage 2 blackholes it;
#               this one was never open to anyone but root.
#   baremetal → not yet: the same cidata layout on a USB stick follows
#               as #658. The image carries no key on this variant either
#               (#659), so a baremetal birth waits for that channel;
#               enabling this module on it is an eval error until then.
#
# Each channel is a pair — open in the initrd, close in stage 2 — and both
# halves live here, so nothing platform-specific reaches a generic module.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.seed;

  channels = {
    linode = "linode-metadata";
    qemu = "nocloud";
  };
  channel = channels.${config.sfi.platform} or null;

  # The lease script, as initrd-network.nix writes it: address, mtu, the
  # classless static routes (-O staticroutes) and the default route.
  udhcpcScript = pkgs.writeScript "sfi-seed-udhcpc" ''
    #!${pkgs.runtimeShell}
    if [ "$1" = bound ]; then
      ip address add "$ip/$mask" dev "$interface"
      if [ -n "$mtu" ]; then
        ip link set mtu "$mtu" dev "$interface"
      fi
      if [ -n "$staticroutes" ]; then
        echo "$staticroutes" \
          | sed -r "s@(\S+) (\S+)@ ip route add \"\1\" via \"\2\" dev \"$interface\" ; @g" \
          | sed -r "s@ via \"0\.0\.0\.0\"@@g" \
          | ${pkgs.runtimeShell}
      fi
      if [ -n "$router" ]; then
        ip route add "$router" dev "$interface"
        ip route add default via "$router" dev "$interface"
      fi
    fi
  '';

  readerScript = lib.replaceStrings [ "@channel@" ] [ (toString channel) ] (builtins.readFile ./seed/reader.sh);
  channelScript = {
    linode-metadata = lib.replaceStrings [ "@udhcpcScript@" ] [ "${udhcpcScript}" ] (builtins.readFile ./seed/channel-linode-metadata.sh);
    nocloud = builtins.readFile ./seed/channel-nocloud.sh;
  }.${toString channel} or "";
in
{
  imports = [ ./platform.nix ];

  options.sfi.seed.enable = lib.mkEnableOption "the seed reader: place the ssh host key from the platform's channel in the initrd, before activation";

  config = lib.mkIf cfg.enable (lib.mkMerge [
    {
      assertions = [
        {
          assertion = channel != null;
          message = "sfi.seed: no channel for sfi.platform = \"${config.sfi.platform}\" yet — the NoCloud stick for baremetal is #658; a birth on this platform waits for it";
        }
        {
          assertion = config.boot.initrd.systemd.enable;
          message = "sfi.seed: the reader is a unit of the systemd initrd (boot.initrd.systemd.services.sfi-seed); the scripted initrd would need a boot.initrd.postMountCommands hook instead";
        }
      ];

      # sed and grep for the reader's split of the seed, whatever the
      # channel; coreutils (base64, readlink, cut) is already there.
      boot.initrd.systemd.initrdBin = [ pkgs.gnused pkgs.gnugrep ];

      boot.initrd.systemd.services.sfi-seed = {
        description = "Place the ssh host key from the ${toString channel} channel";
        unitConfig = {
          DefaultDependencies = false;
          RequiresMountsFor = [ "/sysroot" ];
          ConditionPathExists = "!/sysroot/etc/ssh/ssh_host_ed25519_key";
        };
        after = [ "initrd-root-fs.target" "systemd-modules-load.service" ];
        before = [ "initrd.target" "shutdown.target" ];
        conflicts = [ "shutdown.target" ];
        requiredBy = [ "initrd.target" ];
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
          # The console is where the operator reads a stopped boot (Lish,
          # or virsh console).
          StandardOutput = "journal+console";
          StandardError = "journal+console";
        };
        script = ''
          export PATH=/bin:/sbin
          ${readerScript}
          ${channelScript}
          sfi_seed /sysroot
        '';
      };
    }

    (lib.mkIf (channel == "linode-metadata") {
      # virtio_net so eth0 exists in the initrd (linode-config.nix loads it
      # in stage 2 only); af_packet for udhcpc's raw socket (initrd-network.nix
      # adds the same).
      boot.initrd.kernelModules = [ "virtio_net" "af_packet" ];

      boot.initrd.systemd = {
        # ip for the lease and the routes. curlMinimal: libcurl + openssl +
        # zlib, no http2/idn/psl/ssh stacks — HTTP only here; the service
        # is link-local and unencrypted by design.
        initrdBin = [ pkgs.iproute2 pkgs.curlMinimal ];
        # One applet of busybox, by name: the multicall binary dispatches
        # on argv[0], and the symlink's name is udhcpc.
        extraBin.udhcpc = "${pkgs.busybox}/bin/udhcpc";
        # make-initrd-ng follows ELF dependencies and symlinks, not the store
        # references of a script's text: the lease script the channel names
        # has to be listed, or udhcpc finds no script at the path.
        storePaths = [ udhcpcScript ];
      };

      # The close half (PR #654 review). The seed stays readable from inside the
      # instance for its life: Linode has no call to disable the
      # Metadata service and user_data cannot be cleared on a running
      # instance, and the service answers any request from the instance's
      # network — a pod, or the unprivileged agent user on a dev fort,
      # could PUT for a token and read the host key, which today is a
      # root-only file. Root does not need the endpoint after the initrd.
      # A blackhole route covers pods, local users and root alike, because
      # forwarded traffic uses the host's routing table too; it is a
      # routing-table entry, not a firewall rule, so a CNI that rewrites
      # the filter chains cannot open it. Metric 0 beats any route a DHCP
      # lease could push for the same prefix (dhcpcd's carry a metric).
      #
      # Not networking.interfaces.eth0.ipv4.routes: that option excludes
      # blackhole (its description says why — the type is not per device),
      # so the route is a unit, ordered like the firewall: before
      # network-pre.target, so it exists before any interface comes up.
      # ip route replace: a re-run is a no-op.
      systemd.services.sfi-seed-close = {
        description = "Close the Linode Metadata channel: blackhole the service's addresses";
        wantedBy = [ "sysinit.target" ];
        wants = [ "network-pre.target" ];
        before = [ "network-pre.target" ];
        after = [ "systemd-modules-load.service" ];
        unitConfig.DefaultDependencies = false;
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
        };
        path = [ pkgs.iproute2 ];
        script = ''
          ip -4 route replace blackhole 169.254.169.254/32
          if [ -e /proc/sys/net/ipv6 ]; then
            ip -6 route replace blackhole fd00:a9fe:a9fe::1/128
          fi
        '';
      };
    })

    (lib.mkIf (channel == "nocloud") {
      # The filesystem and the CD-ROM class driver, loaded by
      # systemd-modules-load (the unit orders after it): isofs is the
      # iso9660 driver — the module's name, not the filesystem type;
      # `iso9660` here would match nothing, and only the kernel's alias
      # fs-iso9660 resolves it (the local boot of #657 showed exactly
      # that: the list said iso9660, /proc/modules said nothing, and the
      # mount loaded isofs on demand). sr_mod gives the drive its
      # /dev/sr0 and pulls cdrom in as a dependency. Loaded, not merely
      # available, so the channel does not depend on request_module.
      boot.initrd.kernelModules = [ "isofs" "sr_mod" ];
      # The controllers the CD can hang off, loaded by udev from the PCI
      # id: pulumi-libvirt attaches the cloud-init disk as `hdd` on the
      # `ide` bus, which is the PIIX3 IDE controller (ata_piix) on the
      # i440FX `pc` machine libvirt defaults to and a SATA port (ahci) on
      # q35. The qemu-guest profile lists both already; listed here so
      # the channel does not depend on the profile (the baremetal stick,
      # #658, adds usb_storage and uas in the same place).
      boot.initrd.availableKernelModules = [ "ata_piix" "ahci" "cdrom" ];

      # No close half: see the header. A block device is root-only, and
      # the initrd unmounts the volume before stage 2.
    })
  ]);
}
