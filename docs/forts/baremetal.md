# A fort on hardware — `[baremetal]`

```toml
[baremetal]
ip = "10.0.0.6"                # the static LAN address baked into the image
mac = "f8:ce:72:02:05:07"      # the NIC it goes on (optional — see below)
disk = "/dev/nvme0n1"          # the writer's target (informational)
```

A physical machine on the lab LAN is a fort like any other: the same
hardened profile, flux, the mesh, the same verbs. What differs is who
raises it. A VM's image is handed to a hypervisor or a cloud API;
hardware needs something running *on the machine* to put the image on
its disk. Until that writer is automated (#287 — a netboot ramdisk, a
BMC), the writer is you, at the keyboard, with a live ISO. Everything
else in the verb sequence is unchanged, and `fort provision` /
`preview` / `destroy` know the host has no backend rather than
pretending it has one.

## What the image knows about the machine: nothing

The point of an image is that it boots anywhere. The four facts a
hand-generated `hardware.nix` carries each have a hardware-neutral answer,
and `modules/baremetal-runtime.nix` gives them:

| `hardware.nix` fact | what it is for | the image's answer |
|---|---|---|
| `fileSystems."/"` / `"/boot"` by UUID | mounting the right disk | **by label** (`nixos`, `ESP`) — written by the image build, identical on every disk it is written to |
| `boot.initrd.availableKernelModules = [ nvme xhci_pci thunderbolt … ]` | reaching the root disk from the initrd | `hardware.enableAllHardware` — the installer ISO's own profile: every storage controller, plus the redistributable firmware |
| `boot.kernelModules = [ kvm-amd ]`, `hardware.cpu.amd.updateMicrocode` | virtualization, microcode | both vendors' microcode; the kernel applies the one that matches |
| BIOS vs UEFI, the boot disk | where the bootloader goes | **UEFI**: the upstream `raw-efi` variant — GPT, an ESP, systemd-boot on the ESP's *fallback path* (`\EFI\BOOT\BOOTX64.EFI`), so any UEFI firmware boots it with no boot entry registered. A BIOS-only box is #287's hybrid image, not built until one exists |

So `scaffold` and `mint` need nothing beyond the table. The image is
fatter than a VM's (the all-hardware initrd and the firmware bundle);
that is the price of not knowing the box; the numbers are at the end of
this page.

## The NIC: match by MAC, not by name

A lab VM's LAN block matches `en*` — right for a machine with exactly
one wired NIC. On hardware the interface name is often not a fact:
zenbook's NIC is a USB adapter in a Thunderbolt dock, and its name
(`enp116s0f4u1u1`) encodes the PCI bus of the dock's USB controller and
the port path below it — it changes whenever the dock moves ports. And
`en*` would claim any second adapter too.

`mac` fixes both: scaffold renders `matchConfig.MACAddress` instead of
the name glob. Read it off the box once with `ip link` (the burned-in
address; the kernel's `altname enx<mac>` is the same fact spelled
differently). Leave `mac` unset on a box with one onboard NIC and the
`en*` match is what you get.

## The verbs

```
provisioning/forts/<host>.toml ← [baremetal] ip, mac, disk; fort check --all
fort scaffold <host> --write  ← baremetal-runtime.nix, the LAN block by MAC
fort mint <host> --write      ← keys, sops rules, mesh peer, deploy key, canary
fort add / fort seed          ← as for any fort
                              → PR → merge
fort image <host>             ← nix build ./nixos#<host>-baremetal + the host
                                 key grafted in (guestfish on partition 2)
                                 → ~/.cache/forts/<host>-nixos.img
                              ← WRITE IT (below); delete the artifact
fort check <host>             ← flux reconciles; the host rebuilds itself from main
```

`fort provision <host>` refuses a `[baremetal]` host by name — there is
no backend and no stack. `fort preview --all` lists it as `SKIP` so the
walk is visibly complete. At retirement, `fort dismantle` is the same
PR as for any fort; `fort destroy <host> --no-stack` then revokes the
identities mint registered (deploy key, B2 keys) with the same
merged-main gate, skipping the pulumi steps — the machine itself is
powered off and its disk wiped by hand.

## Writing the image — the runbook

The writer is the stock NixOS minimal ISO on a USB stick and `dd`. It
keeps the birth identical to a VM's: the artifact `fort image` produced,
host key grafted, single-use, is what boots — nothing is placed by hand
on the machine, and the image is deleted after one use like any other.

**Before the machine is touched**: if it is being *repaved* (an existing
host becoming a fort), everything on its disk is gone at step 4. Its data
must already be somewhere else and verified there. For zenbook that is
the gitea interim's hop one (#338); the write happens only after the
interim has served through the name.

1. **The stick.** Any NixOS minimal ISO of a recent release
   (`nixos-minimal-*-x86_64-linux.iso`); `dd` it to a USB stick from
   devbox. The ISO's kernel carries every driver the image's does, so a
   box the ISO boots is a box the image boots.
2. **Boot it.** UEFI boot from the stick (the firmware's one-time boot
   menu). Plug in ethernet: the installer DHCPs every NIC it finds, and
   all it needs is a route to devbox. `ip -br addr` to see it came up.
3. **Fetch the artifact** onto the installer's tmpfs, from wherever
   `fort image` left it — with `dd` running from devbox over ssh, nothing
   is copied to the stick:
   ```
   # on the machine, as root (the installer's default shell):
   passwd            # set a throwaway password for the next step
   # on devbox:
   ssh root@<installer's DHCP address> 'cat > /dev/<disk>' < ~/.cache/forts/<host>-nixos.img
   ```
   or, with the image on the stick or fetched by `curl`, plain
   `dd if=<host>-nixos.img of=/dev/<disk> bs=4M status=progress conv=fsync`.
   `<disk>` is the manifest's `disk` — check with `lsblk` first that it
   is the internal one and not the stick.
4. **Verify the write** before rebooting: `lsblk -f /dev/<disk>` shows
   two partitions, `ESP` (vfat) and `nixos` (ext4). The root partition is
   sized to the image; it grows to the disk on first boot
   (`boot.growPartition` + `autoResize`).
5. **Reboot** into the disk (pull the stick). The firmware finds
   `\EFI\BOOT\BOOTX64.EFI` on the ESP without an entry. The first boot
   is the ordinary fort first boot: sops decrypts with the grafted host
   key, wg0 comes up, k3s and flux bootstrap.
6. **Delete the artifact** on devbox (`shred -u`): it carries the host's
   private key. With a provider this is `fort provision`'s release step;
   here it is yours.
7. **Verify** from a mesh member: `ssh deployer@<mesh_ip>`, `sudo
   sfi-kubectl get nodes` if the host carries ops/kubectl.nix, `kubectl
   get gitrepository -n flux-system` through the ops-ro kubeconfig.

Nothing is registered anywhere: no stack, no `Pulumi.<host>.yaml`, no
state PR. `git log` on the manifest is the provenance of the birth.

## What is proven, and where

The runtime module and the variant were proven without hardware
(#287's sequencing step 1, 2026-09-18), on devbox under OVMF with a
throwaway `[baremetal]` host scaffolded and imaged by the real verbs:

- EDK II found the disk and started systemd-boot from the ESP's fallback
  path — no boot entry, no EFI variables touched.
- `ESP` and `nixos` mounted by label; on a disk grown from 6 G to 20 G
  the root partition and filesystem grew on the next boot (19.7 G).
- The grafted host key decrypted the sops file: wg0 came up with the
  mesh address, k3s and flux-bootstrap started, 4 pods running.
- A virtio NIC carrying the manifest's `mac` got `[baremetal] ip`. Then
  the dock case: booted with **no** matching NIC, one hot-plugged 30 s
  later under a *different name* (`ens5`, not `ens3`) still got the
  address by MAC; wg0 and k3s had waited for it (`network-online` at
  37 s, k3s at 47 s) rather than starting without a route. A second NIC
  with another MAC stayed `unmanaged`.
- The weight of not knowing the box: closure 2.68 GiB vs 2.08 GiB for
  the qcow fort (the firmware bundle is 0.73 GiB of it), initrd 43 MiB
  vs 27 MiB, the raw image 6.5 GB. `enableAllHardware` is acceptable.

What only the first machine can prove: this firmware's fallback-path
behavior, this dock's NIC under this kernel, the write itself.

## Later: the writer as a verb (#287)

The `[baremetal]` table is the manifest half; the image half is done.
What is not built: a netboot ramdisk that fetches and writes the image
(PXE from soundship's dnsmasq, or Redfish virtual media on a box with a
BMC), and a backend that owns power and boot order. Until a headless
box with a BMC exists, a human at the keyboard is the honest writer —
and `bmc`, `boot_mac`, `firmware` stay out of the table until something
reads them.
