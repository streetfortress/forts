# The runtime half of nixos-generators' qcow format (#93): virtio guest
# profile, root-by-label, grub on /dev/vda. The format module injects these
# only into the `<host>-qcow` image build, so the bare
# `nixosConfigurations.<host>` — the thing `nixos-rebuild switch --flake`
# and therefore auto-rebuild.nix evaluates — failed its fileSystems/
# bootloader assertions and could never switch. (Known quirk since the
# image-build era; it only became load-bearing when #93 made switch the
# steady-state update path.)
#
# Values mirror nixos-generators formats/qcow.nix (rev 8946737), at
# mkDefault priority so the format module's own definitions win outright in
# the image build. The one exception is kernelParams: it's a list option,
# so a mkDefault list is DROPPED (not merged) whenever anything else
# defines the option at normal priority — which common's loglevel/lsm
# params do, and losing console=ttyS0 on switch would kill the
# virsh-console break-glass path. Defined at normal priority instead; the
# image build gets the param twice (format + here), which the kernel
# treats as one. A switch on a provisioned host otherwise converges to the
# same shape the image booted with.
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
}
