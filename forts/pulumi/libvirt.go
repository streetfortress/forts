package main

import (
	"fmt"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
	"github.com/pulumi/pulumi-libvirt/sdk/go/libvirt"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// Libvirt realizes a host on the lab hypervisor (soundship) — the lab VM
// the create-nixos playbook used to define, as
// resources: a qcow2 volume in the `images` pool streamed from the
// injected image through the libvirt API (no root hop — libvirtd writes
// it), and a domain from that volume with the lab's fixed shape
// (host-passthrough CPU, virtio disk, br0 bridge, serial + VNC console,
// autostart — which the playbook never set). No firewall and no DNS:
// the LAN bridge and the lab dnsmasq are the hypervisor's, and the
// host's own nftables is the only filter, as for every lab VM today.
//
// The birth-input discipline is the same as Linode's but the artifact
// is not consumed: the uploaded volume IS the root disk. `forts:imageId`
// records the injected image's path (provenance); the volume ignores
// changes to `source`, so the local single-use file is deleted after
// the birth and a rebuilt image never proposes a disk replacement — a
// repave is `pulumi destroy` + provision, deliberately.
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
)

func (l *Libvirt) Provision(ctx *pulumi.Context, name string, host manifest.Host) error {
	spec := host.Libvirt()
	if spec == nil {
		return fmt.Errorf("host %q: no [libvirt] table", name)
	}
	shape := host.Shape.WithLabDefaults()
	vcpus, memory := shape.Vcpus, shape.Memory

	// The injected qcow2, set by `fort provision --image` — read at
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
	}, append(opts, pulumi.IgnoreChanges([]string{"disks", "consoles", "graphics", "xml", "type", "fwCfgName", "qemuAgent"}))...)
	if err != nil {
		return err
	}

	// Same outputs as the Linode backend, so `fort provision` gates the
	// single-use image deletion on the same fact (the host exists).
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
