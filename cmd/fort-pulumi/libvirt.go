package main

import (
	"fmt"
	"os"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gitea.zen.lofi/sfi/forts/internal/seed"
	"github.com/pulumi/pulumi-libvirt/sdk/go/libvirt"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// Libvirt realizes a host on the lab hypervisor (soundship) — the lab VM
// the create-nixos playbook used to define, as
// resources: a qcow2 volume in the `images` pool streamed from the
// bootstrap image through the libvirt API (no root hop — libvirtd writes
// it), and a domain from that volume with the lab's fixed shape
// (host-passthrough CPU, virtio disk, br0 bridge, serial + VNC console,
// autostart — which the playbook never set). No firewall and no DNS:
// the LAN bridge and the lab dnsmasq are the hypervisor's, and the
// host's own nftables is the only filter, as for every lab VM today.
//
// The birth-input discipline is the same as Linode's but the artifact
// is not consumed: the uploaded volume IS the root disk. `forts:imageId`
// records the image's path (provenance); the volume ignores changes to
// `source`, so the local file is deleted after the birth (hygiene — it
// holds no secret, #659) and a rebuilt image never proposes a disk
// replacement — a repave is `pulumi destroy` + provision, deliberately.
//
// The seed (#657; the model is #605's option 2): the host's ssh host key
// travels apart from the image, as a NoCloud cidata volume — the
// provider's cloud-init disk resource, an ISO in the same pool named
// <host>-seed.iso, attached to the domain as a CD-ROM (`hdd` on the
// `ide` bus, which is the i440FX machine's PIIX3 controller). The initrd
// mounts it and places the key before activation (nixos/modules/seed.nix,
// the nocloud channel). No cloud-init runs on the host; only the layout
// is borrowed. `fort provision` hands the seed over in seed.Env (base64,
// as for Linode) when the apply creates the domain, and records
// `forts:seed = nocloud` on the stack: the program declares the volume
// whenever that key is set, so a host born before the channel existed
// (the three adopted lab VMs, and the dev fort born from an image that
// carried the key) never sees a
// spurious ISO proposed on a day-2 apply. userData is a pulumi secret
// and ignored after creation, as the Linode instance ignores metadatas:
// a rotation of the host key is a repave, never a diff on a running
// domain. The volume stays in the pool for the domain's life — `pulumi
// destroy` removes the domain, then it — and it holds the key in plain
// text, as the root qcow2 beside it does: #605 accepts this trust in the
// hypervisor, docs/provision.md records it. The bridged provider
// (terraform-provider-libvirt 0.8.1) writes the ISO by exec'ing
// `mkisofs` on the machine that runs the apply; `fort provision` puts
// nixpkgs' cdrtools on PATH when the host has none.
//
// Sizing is the manifest's [shape] (vcpus, memory, disk) with the lab
// defaults. The provider refuses `size` next to
// `source` (the volume takes the image's virtual size), so `fort
// provision` grows the qcow2 to disk_size with qemu-img BEFORE the
// upload — the playbook's order — and the qcow's autoResize grows the
// root fs into it on first boot. The [libvirt] table is the static LAN
// address (baked into the image; exported for `fort provision`).
type Libvirt struct{}

const (
	libvirtPool   = "images"
	libvirtBridge = "br0"
	// seedChannel is the value of the stack's `forts:seed` config that
	// says the host was born with the cidata volume (#657). `fort
	// provision` sets it when the apply creates the domain; the name is
	// the channel's in nixos/modules/seed.nix.
	seedChannel = "nocloud"
)

func (l *Libvirt) Provision(ctx *pulumi.Context, name string, host manifest.Host) error {
	spec := host.Libvirt()
	if spec == nil {
		return fmt.Errorf("host %q: no [libvirt] table", name)
	}
	shape := host.Shape.WithLabDefaults()
	vcpus, memory := shape.Vcpus, shape.Memory

	// The bootstrap qcow2, set by `fort provision --image` — read at
	// creation only (ignoreChanges below).
	image := config.Require(ctx, "forts:imageId")

	var opts []pulumi.ResourceOption
	opts = append(opts, pulumi.Protect(true))

	vol, err := libvirt.NewVolume(ctx, name+"-disk", &libvirt.VolumeArgs{
		Name:   pulumi.String(name + ".qcow2"),
		Pool:   pulumi.String(libvirtPool),
		Source: pulumi.String(image),
		Format: pulumi.String("qcow2"),
	}, append(opts, pulumi.IgnoreChanges([]string{"source"}))...)
	if err != nil {
		return err
	}

	// The seed volume, for a host born with the channel (the type comment).
	// userData comes from the environment when this run carries the seed
	// (fort provision does, when the apply creates the domain; a bare
	// `pulumi preview` from provisioning/pulumi does not, and ignoreChanges keeps
	// the recorded value for an existing volume either way). Secret from
	// the first moment the engine sees it. metaData is the datasource's
	// one required file; the instance id is the hostname.
	var cloudinit pulumi.StringPtrInput
	if config.Get(ctx, "forts:seed") == seedChannel {
		var userData pulumi.StringPtrInput
		if ud := os.Getenv(seed.Env); ud != "" {
			text, err := seed.Decode(ud)
			if err != nil {
				return err
			}
			userData = pulumi.ToSecret(pulumi.String(text)).(pulumi.StringOutput)
		}
		iso, err := libvirt.NewCloudInitDisk(ctx, name+"-seed", &libvirt.CloudInitDiskArgs{
			Name:     pulumi.String(name + "-seed.iso"),
			Pool:     pulumi.String(libvirtPool),
			MetaData: pulumi.String("instance-id: " + name + "\n"),
			UserData: userData,
		}, append(opts,
			pulumi.IgnoreChanges([]string{"userData"}),
			pulumi.AdditionalSecretOutputs([]string{"userData"}),
		)...)
		if err != nil {
			return err
		}
		cloudinit = iso.ID()
	}

	dom, err := libvirt.NewDomain(ctx, name, &libvirt.DomainArgs{
		Name:   pulumi.String(name),
		Memory: pulumi.Int(memory * 1024), // MiB
		Vcpu:   pulumi.Int(vcpus),
		Cpu:    &libvirt.DomainCpuArgs{Mode: pulumi.String("host-passthrough")},
		// As a FILE disk, not a volume ref: the volume's id is its path,
		// and a `<disk type='volume'>` never starts here — Ubuntu's
		// virt-aa-helper puts no rule for pool-resolved sources in the
		// domain's AppArmor profile, so qemu gets EACCES on open. The
		// playbook's XML is a file disk too.
		Disks: libvirt.DomainDiskArray{
			&libvirt.DomainDiskArgs{File: vol.ID()},
		},
		// The seed volume as a CD-ROM (the provider appends it after the
		// disks, raw driver, serial "cloudinit"); nil for a host born
		// before the channel. The XSLT below leaves it alone: it matches
		// device='disk' only.
		Cloudinit: cloudinit,
		NetworkInterfaces: libvirt.DomainNetworkInterfaceArray{
			&libvirt.DomainNetworkInterfaceArgs{Bridge: pulumi.String(libvirtBridge)},
		},
		// Serial console so `virsh console <host>` works. It is the
		// lab's recovery path when the mesh is gone, and it shows two
		// things in turn. First the GRUB menu, for 3 s, because
		// nixos/modules/qcow-runtime.nix gives GRUB its own serial
		// terminal (#523) — a previous generation, or init=/bin/sh, on
		// a host that does not start. Then a `deployer` login, not a
		// root one, and only where the host imports
		// nixos/modules/ops/console.nix and carries
		// deployer-password-hash (#524). Without that import the
		// login prompt appears and no account can answer it.
		Consoles: libvirt.DomainConsoleArray{
			&libvirt.DomainConsoleArgs{Type: pulumi.String("pty"), TargetType: pulumi.String("serial"), TargetPort: pulumi.String("0")},
		},
		Graphics: &libvirt.DomainGraphicsArgs{
			Type:          pulumi.String("vnc"),
			ListenType:    pulumi.String("address"),
			ListenAddress: pulumi.String("127.0.0.1"),
			Autoport:      pulumi.Bool(true),
		},
		Autostart: pulumi.Bool(true),
		Running:   pulumi.Bool(true),
		// A file disk gets `<driver type='raw'>` from the provider (it
		// only knows the format of volume refs); the image is qcow2, so
		// patch the driver in the rendered XML — the provider's own
		// escape hatch for exactly this.
		Xml: &libvirt.DomainXmlArgs{Xslt: pulumi.String(qcow2DriverXSLT)},
		// Birth-time shape, ignored after creation: the provider reads a
		// pool-resident file disk back as a volume ref, and reads no
		// consoles/graphics/defaults back from a playbook-defined domain
		// (fort adopt) — every one of these is replace-forcing, and a
		// different shape IS a repave (destroy + provision), never an
		// in-place change. What stays managed live: autostart, running,
		// memory, vcpu (the last two also replace: a deliberate act).
		// cloudinit joins the list for the seed's reason: the key on the
		// volume is the one the host was born with, and the provider
		// would otherwise re-attach a changed ISO on the next reboot.
	}, append(opts, pulumi.IgnoreChanges([]string{"disks", "consoles", "graphics", "xml", "type", "fwCfgName", "qemuAgent", "cloudinit"}))...)
	if err != nil {
		return err
	}

	// Same outputs as the Linode backend, so `fort provision` gates the
	// image deletion on the same fact (the host exists).
	ctx.Export("ip", pulumi.String(spec.IP))
	ctx.Export("domain", dom.Name)
	ctx.Export("volume", vol.Name)
	return nil
}

// qcow2DriverXSLT rewrites every disk device's driver type to qcow2 —
// the format every fort image is built in (system.build.images.qemu).
const qcow2DriverXSLT = `<?xml version="1.0" ?>
<xsl:stylesheet version="1.0" xmlns:xsl="http://www.w3.org/1999/XSL/Transform">
  <xsl:output omit-xml-declaration="yes" indent="yes"/>
  <xsl:template match="node()|@*">
    <xsl:copy><xsl:apply-templates select="node()|@*"/></xsl:copy>
  </xsl:template>
  <xsl:template match="/domain/devices/disk[@device='disk']/driver/@type">
    <xsl:attribute name="type">qcow2</xsl:attribute>
  </xsl:template>
</xsl:stylesheet>`
