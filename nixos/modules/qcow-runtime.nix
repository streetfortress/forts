# The VM-host runtime contract: virtio guest profile, root-by-label, grub
# on /dev/vda, serial console. Two consumers share it:
#
#   1. The bare `nixosConfigurations.<host>` — what `nixos-rebuild switch`
#      and therefore the auto-rebuild loop (#93) evaluates. Without this
#      module it has no root filesystem or bootloader and cannot switch.
#   2. The `<host>-qcow` image (#95): the upstream `images.qemu` variant
#      layers virtualisation/disk-image.nix onto this config via
#      extendModules. The variant owns the disk layout (msdos, single
#      root partition sda1, ext4 labeled `nixos` — mirrored by
#      create-nixos.yaml's guestfish nixos_root_dev) but omits the
#      qemu-guest profile, console=ttyS0, and a grub timeout; this module
#      supplies them. Without the profile's virtio initrd modules the
#      image can't find /dev/vda at boot.
#
# Keeping both consumers on one module is what guarantees a switched
# generation converges to the same shape the image booted with.
#
# Priorities: mkDefault wherever the image variant (or grub.nix's
# internal device→devices wiring) defines the same value, so the extended
# config merges cleanly. kernelParams stays at normal priority: a
# mkDefault list is DROPPED (not merged) whenever anything else defines
# the option normally — common's loglevel/lsm params do, and losing
# console=ttyS0 would kill the virsh-console break-glass path.
{ lib, modulesPath, ... }:

{
  imports = [ "${toString modulesPath}/profiles/qemu-guest.nix" ];

  fileSystems."/" = {
    device = lib.mkDefault "/dev/disk/by-label/nixos";
    autoResize = lib.mkDefault true;
    fsType = lib.mkDefault "ext4";
  };

  boot.growPartition = lib.mkDefault true;
  boot.kernelParams = [ "console=ttyS0" ]; # normal priority — see header
  boot.loader.grub.device = lib.mkDefault "/dev/vda";
  boot.loader.timeout = lib.mkDefault 0;

  # Pin the image file name to what create-nixos.yaml copies
  # (result/nixos.qcow2); the upstream default embeds the NixOS label,
  # which would churn the playbook on every nixpkgs bump.
  image.modules.qemu = {
    image.baseName = "nixos";
  };
}
