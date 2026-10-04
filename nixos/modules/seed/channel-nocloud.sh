# shellcheck shell=bash
# The NoCloud channel of the seed reader (nixos/modules/seed.nix, #657):
# the seed is /user-data on a volume labelled `cidata` — the layout of
# cloud-init's NoCloud datasource, which pulumi-libvirt's cloud-init disk
# resource writes (an ISO 9660 image, `mkisofs -volid cidata`, attached to
# the domain as a CD-ROM) and which `fort provision` fills from the sops
# file (forts/pulumi/libvirt.go). No cloud-init runs on a fort: the layout
# is borrowed, this script is the reader. The file is the seed text
# itself — no base64 on this channel, unlike the Metadata service's.
#
# The device: udev links it as /dev/disk/by-label/cidata once the
# controller driver (ata_piix on the i440FX machine libvirt defaults to,
# ahci on q35), sr_mod and the iso9660 filesystem (the module is isofs)
# are in the initrd — seed.nix lists them. The wait is bounded: a domain born without the
# volume must stop in the initrd with the reader's message, not hang. The
# mount is read-only and undone before the seed is returned, so nothing of
# the initrd's view of the volume survives into stage 2.
#
# The same layout serves the baremetal channel (#658): a second USB stick
# labelled cidata, with a longer wait for USB enumeration.

sfi_seed_dev=/dev/disk/by-label/cidata
sfi_seed_mnt=/run/sfi-seed

# The storage modules the device needed, for the record (#657 asks for
# the list): read from /proc/modules with the shell alone.
sfi_seed_modules() {
  mods=""
  while read -r name _; do
    case $name in
      isofs|sr_mod|cdrom|ata_piix|ahci|libata|virtio_blk|virtio_scsi|usb_storage|uas) mods="$mods $name" ;;
    esac
  done < /proc/modules
  echo "sfi-seed: storage modules loaded:$mods" >&2
}

sfi_seed_channel() {
  # udev links the label once sr_mod has the device and blkid has read
  # it; give the controller and the drive a moment.
  waited=0
  while [ ! -e "$sfi_seed_dev" ] && [ "$waited" -lt 30 ]; do
    sleep 1
    waited=$((waited + 1))
  done
  if [ ! -e "$sfi_seed_dev" ]; then
    echo "sfi-seed: no volume labelled cidata after ${waited}s (is the seed ISO attached to the domain?)" >&2
    sfi_seed_modules
    return 1
  fi
  node=$(readlink -f "$sfi_seed_dev") || node="$sfi_seed_dev"
  echo "sfi-seed: cidata is $node (after ${waited}s)" >&2
  sfi_seed_modules
  mkdir -p "$sfi_seed_mnt"
  if ! mount -t iso9660 -o ro "$sfi_seed_dev" "$sfi_seed_mnt"; then
    echo "sfi-seed: mount of $node as iso9660 failed" >&2
    return 1
  fi
  echo "sfi-seed: mounted $node read-only; reading /user-data" >&2
  data=$(cat "$sfi_seed_mnt/user-data" 2>/dev/null) || data=""
  if ! umount "$sfi_seed_mnt"; then
    echo "sfi-seed: umount of $sfi_seed_mnt failed; stage 2 will find it mounted" >&2
  fi
  if [ -z "$data" ]; then
    echo "sfi-seed: the cidata volume has no /user-data (was it made by fort provision?)" >&2
    return 1
  fi
  printf '%s\n' "$data"
}
