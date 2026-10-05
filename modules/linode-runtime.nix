# Linode cloud-host runtime contract: upstream linode-config.nix (GRUB +
# LISH serial, eth0 DHCP, qemu-guest profile, diag tools) hardened with
# by-label device addressing — the qcow-runtime.nix discipline.
#
# Upstream mounts root as /dev/sda and swaps on /dev/sdb, but virtio-scsi
# enumeration order is NOT guaranteed: web3's first boot (#142) raced it,
# the kernel named the swap disk sda, and stage-1 found "no ext4" on a
# perfectly healthy root disk. Host-side GRUB kept working throughout —
# it addresses the config-profile SLOT, not the kernel name, which is
# why kernel+initrd loaded from the very filesystem the initrd then
# "couldn't find". Labels are stable on both disks: the image build
# labels root "nixos" (make-disk-image), and Linode formats swap disks
# it creates as "linode-swap".
#
# The seed reader (./seed.nix, #606) rides with this contract: a Linode
# fort's image carries no host key; the key arrives through the Metadata
# service at first boot, placed by the initrd before activation. A host
# whose key is already on the root (a host provisioned before the channel)
# boots unchanged — the reader finds the key and returns.
{ lib, modulesPath, ... }:

{
  imports = [ ./platform.nix ./seed.nix "${toString modulesPath}/virtualisation/linode-config.nix" ];
  sfi.platform = "linode";
  sfi.seed.enable = lib.mkDefault true;

  # mkForce: upstream sets device = "/dev/sda" at normal priority.
  # autoResize rides along unchanged (first boot grows the deployed fs
  # into the full disk).
  fileSystems."/".device = lib.mkForce "/dev/disk/by-label/nixos";

  # Overrides upstream's mkDefault [ { device = "/dev/sdb"; } ].
  swapDevices = [ { device = "/dev/disk/by-label/linode-swap"; } ];
}
