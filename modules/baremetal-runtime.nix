# The physical-host runtime contract (#287): an image that boots the
# machine rather than a description of it. The same two consumers as
# qcow-runtime.nix share it:
#
#   1. The bare `nixosConfigurations.<host>` — what `nixos-rebuild switch`
#      and the auto-rebuild loop (#93) evaluate on the host.
#   2. The `<host>-baremetal` image: the upstream `images.raw-efi` variant
#      layers virtualisation/disk-image.nix onto this config via
#      extendModules. The variant owns the disk layout — GPT, an ESP as
#      partition 1 (label `ESP`) and the root as partition 2 (ext4, label
#      `nixos`) — and systemd-boot on the ESP. This module supplies what
#      a machine needs that a VM does not. The image carries no host key
#      (#659); the channel that places one on this platform is #658.
#
# What makes the image hardware-neutral, so scaffold and mint need to
# know nothing about the box (the four facts a hand-generated
# hardware.nix carries, each answered without the machine):
#
#   - the disk: mounted by LABEL, never UUID — the labels are written by
#     the image build, so they are the same on every disk it is written to
#   - reaching the disk from the initrd: hardware.enableAllHardware, the
#     installer ISO's own profile — every storage controller module in the
#     initrd, plus the redistributable firmware (the installer boots
#     everything for the same reason). Fatter than a VM's initrd; the
#     price of not knowing the box
#   - the CPU: both vendors' microcode packages; the kernel applies the
#     one that matches, the other is a harmless miss
#   - the firmware: UEFI. systemd-boot is installed to the ESP's fallback
#     path (\EFI\BOOT\BOOTX64.EFI) at image-build time, where no EFI
#     variables exist to touch — so the image boots on any UEFI firmware
#     with no boot entry registered, and canTouchEfiVariables stays off on
#     the host too (a rebuild must not depend on efivars being writable).
#     A BIOS-only box is #287's hybrid image, not built until one exists.
#
# No bridge, no NetworkManager, no console=ttyS0: the LAN block is the
# host's own (scaffold renders it from [baremetal], matched by MAC where
# the manifest names one), and a physical box has a screen.
#
# Priorities: mkDefault wherever the image variant defines the same
# value, so the extended config merges cleanly (as qcow-runtime.nix).
{ lib, ... }:

{
  imports = [ ./platform.nix ];
  sfi.platform = "baremetal";

  hardware.enableAllHardware = true;
  hardware.cpu.amd.updateMicrocode = lib.mkDefault true;
  hardware.cpu.intel.updateMicrocode = lib.mkDefault true;

  fileSystems."/" = {
    device = lib.mkDefault "/dev/disk/by-label/nixos";
    autoResize = lib.mkDefault true;
    fsType = lib.mkDefault "ext4";
  };
  fileSystems."/boot" = {
    device = lib.mkDefault "/dev/disk/by-label/ESP";
    fsType = lib.mkDefault "vfat";
    options = [ "fmask=0077" "dmask=0077" ];
  };
  boot.growPartition = lib.mkDefault true;

  boot.loader.systemd-boot.enable = lib.mkDefault true;
  boot.loader.efi.canTouchEfiVariables = lib.mkDefault false;
  boot.loader.timeout = lib.mkDefault 1;

  # Pin the image file name to what the image flow expects
  # (result/nixos.img); the upstream default embeds the NixOS label.
  image.modules.raw-efi = {
    image.baseName = "nixos";
  };
}
