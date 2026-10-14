# The VM-host runtime contract: virtio guest profile, root-by-label, grub
# on /dev/vda, serial console. Two consumers share it:
#
#   1. The bare `nixosConfigurations.<host>` — what `nixos-rebuild switch`
#      and therefore the auto-rebuild loop (#93) evaluates. Without this
#      module it has no root filesystem or bootloader and cannot switch.
#   2. The `<host>-qcow` image (#95): the upstream `images.qemu` variant
#      layers virtualisation/disk-image.nix onto this config via
#      extendModules. The variant owns the disk layout (msdos, single
#      root partition sda1, ext4 labeled `nixos`) but omits the
#      qemu-guest profile, console=ttyS0, and a grub timeout; this module
#      supplies them. Without the profile's virtio initrd modules the
#      image can't find /dev/vda at boot.
#
# Keeping both consumers on one module is what guarantees a switched
# generation converges to the same shape the image booted with.
#
# The seed reader (./seed.nix, #657) rides with this contract: a lab
# fort's image carries no host key; the key arrives on the cidata volume
# pulumi-libvirt attaches to the domain (cmd/fort-pulumi/libvirt.go), placed
# by the initrd before activation. A host whose key is already on the
# root (a host provisioned before the channel, and every switch of a running
# host) boots unchanged — the reader finds the key and returns.
#
# Priorities: mkDefault wherever the image variant (or grub.nix's
# internal device→devices wiring) defines the same value, so the extended
# config merges cleanly. kernelParams stays at normal priority: a
# mkDefault list is DROPPED (not merged) whenever anything else defines
# the option normally — common's loglevel/lsm params do, and losing
# console=ttyS0 would kill the virsh-console break-glass path.
#
# The serial line carries TWO stages, and the kernel parameter is only the
# second one (#523). GRUB has its own terminal set, and it defaults to the
# VGA screen alone: without the `serial` + `terminal_*` block below, an
# operator on `virsh console` sees nothing until the getty starts — which
# is precisely the moment a host with a broken boot never reaches. Upstream
# linode-config.nix writes all three parts (timeout, GRUB serial, kernel
# console), which is why Lish break-glass works on the Linode fort; this
# module used to write one of them.
{ lib, modulesPath, ... }:

{
  imports = [ ./platform.nix ./seed.nix "${toString modulesPath}/profiles/qemu-guest.nix" ];
  sfi.platform = "qemu";
  sfi.seed.enable = lib.mkDefault true;

  fileSystems."/" = {
    device = lib.mkDefault "/dev/disk/by-label/nixos";
    autoResize = lib.mkDefault true;
    fsType = lib.mkDefault "ext4";
  };

  boot.growPartition = lib.mkDefault true;
  boot.kernelParams = [ "console=ttyS0" ]; # normal priority — see header
  boot.loader.grub.device = lib.mkDefault "/dev/vda";

  # Three seconds of menu, not zero. install-grub.pl writes `set timeout=0`
  # with `set timeout_style=menu`: GRUB starts the default entry at once and
  # polls for no key, so nobody can pick a previous generation or append
  # init=/bin/sh. Three seconds is the whole break-glass window for a host
  # that does not start far enough to offer the console login
  # (ops/console.nix, #524), and it costs every qcow fort three seconds per
  # boot. mkDefault: a host that wants a longer window sets its own.
  boot.loader.timeout = lib.mkDefault 3;

  # GRUB's serial terminal — the first stage of the console path (#523).
  # install-grub.pl appends extraConfig after its gfxterm block and before
  # the menu entries, so this is the last word on the terminal set.
  #
  # `--append` rather than upstream's bare `terminal_output serial`: the
  # libvirt domain (cmd/fort-pulumi/libvirt.go) attaches a VNC screen as well
  # as the serial pty, and the menu belongs on both. The speed is nominal —
  # qemu's pty chardev ignores the baud rate — and 9600n8 is both GRUB's own
  # default and what an option-less `console=ttyS0` gives the kernel, so the
  # two ends still agree if this config ever reaches a real UART.
  boot.loader.grub.extraConfig = ''
    serial --speed=9600 --unit=0 --word=8 --parity=no --stop=1
    terminal_input --append serial
    terminal_output --append serial
  '';

  # Pin the image file name to what the image flow expects
  # (result/nixos.qcow2); the upstream default embeds the NixOS label,
  # which would churn every caller on every nixpkgs bump.
  image.modules.qemu = {
    image.baseName = "nixos";
  };
}
