# The one fact the flake needs per auto-discovered host that the host
# dir itself must declare: which runtime contract (and therefore which
# image variant) it is built for. Set by modules/qcow-runtime.nix
# ("qemu" → `<host>-qcow`), modules/linode-runtime.nix ("linode" →
# `<host>-linode`) and modules/baremetal-runtime.nix ("baremetal" →
# `<host>-baremetal`); `fort scaffold` renders the matching import from
# the manifest's provider table ([libvirt] → qemu, [linode] → linode,
# [baremetal] → baremetal). No default on purpose: a host dir that
# imports no runtime module has no root filesystem either, and the eval
# should say so here rather than deep in the bootloader config.
{ lib, ... }:

{
  options.sfi.platform = lib.mkOption {
    type = lib.types.enum [ "qemu" "linode" "baremetal" ];
    description = "Runtime/image target of this host: qemu (qcow2 VM), linode (cloud image) or baremetal (raw UEFI disk image).";
  };
}
