package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"gitea.zen.lofi/sfi/forts/internal/seed"
	"github.com/pulumi/pulumi-linode/sdk/v4/go/linode"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

// linodePlanDiskMB looks up an instance type's total disk (MB) from the
// public, unauthenticated types endpoint. See the comment at its call
// site for why this isn't the provider's GetInstanceType invoke.
func linodePlanDiskMB(instanceType string) (int, error) {
	resp, err := http.Get("https://api.linode.com/v4/linode/types/" + url.PathEscape(instanceType))
	if err != nil {
		return 0, fmt.Errorf("looking up instance type %q: %w", instanceType, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("looking up instance type %q: %s", instanceType, resp.Status)
	}
	var out struct {
		Disk int `json:"disk"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return 0, fmt.Errorf("decoding instance type %q: %w", instanceType, err)
	}
	if out.Disk <= swapMB {
		return 0, fmt.Errorf("instance type %q reports %d MB disk — refusing to size root disk", instanceType, out.Disk)
	}
	return out.Disk, nil
}

// Linode realizes a host on Linode: boot an instance from the bootstrap
// image, attach a default-deny firewall at creation, hand it its ssh host
// key through the Metadata service, and point DNS at it. The host comes up
// holding its sops identity before activation (nixos/modules/seed.nix
// reads metadata.user_data in the initrd, #606) and flux-bootstraps itself
// — no SSH in the provisioning path.
//
// The image itself is NOT a resource here. It is a provision input (#42):
// `fort provision --image` uploads it, sets `forts:imageId` on the stack,
// and deletes it (provider-side and local) once the instance exists. The
// root disk carries ignoreChanges on `image` — a provision input, not a
// managed property, so neither the image's deletion nor a later rebuild
// ever diffs a running host. Since #606 the image carries no secret;
// the delete is hygiene (Linode bills custom images), not custody (#659).
//
// The seed (#605 option 2): metadata.user_data on the instance, the one
// value the image used to carry. `fort provision` reads the key pair from
// nixos/secrets/<host>.yaml and hands the base64 value over in seed.Env
// on every Linode apply; the program wraps it as a pulumi secret, so the
// committed checkpoint holds ciphertext under the passphrase provider —
// a second copy of the key, under the same admin keys, accepted and
// recorded in docs/provision.md. The instance ignores changes to
// `metadatas`: Linode's metadata cannot change on a running instance (the
// rebuild endpoint takes new metadata with new disks, and a rebuild is a
// re-provision), the API never returns user_data (only has_user_data, so a
// refresh reads it back empty), and a re-minted key must show as a
// deliberate re-provision, not as a diff on a running host.
//
// The instance is built the explicit way — empty instance + disks +
// config profile — rather than instance-from-image, because the implicit
// config profile Linode creates would boot their `linode/latest-64bit`
// kernel and run Network Helper, both wrong for NixOS (#87's boot
// quirks): the image owns GRUB and eth0 DHCP (upstream linode-config.nix
// baked in at build), so the config profile must be grub2 + all helpers
// off. Swap is a real /dev/sdb disk, matching the image's default
// `swapDevices` expectation.
type Linode struct{}

const swapMB = 512

func (l *Linode) Provision(ctx *pulumi.Context, name string, host manifest.Host) error {
	spec := host.Linode()
	if spec == nil {
		return fmt.Errorf("host %q: no [linode] table", name)
	}

	// Set by `fort provision --image` after uploading the artifact. It
	// stays in Pulumi.<host>.yaml as committed provenance:
	// the record of which image this host was provisioned from.
	imageID := config.Require(ctx, "forts:imageId")

	// protect applies to every fort and to the whole host surface, not just
	// the instance: an un-gated firewall delete would strip provider-side
	// filtering from a running host. `pulumi destroy` refuses until each
	// resource is explicitly unprotected — the "gated manual delete".
	// Deleting the manifest alone never reaches this state at all.
	var opts []pulumi.ResourceOption
	opts = append(opts, pulumi.Protect(true))

	// Default-deny + exactly the production profile's public surface:
	// 80/443 and the WireGuard transport — IPv4 ONLY, by declaration:
	// only A records are published, LE challenges over v4, wg rides v4,
	// so open v6 inbounds would be surface serving nothing (#86: the
	// firewall is exactly the served surface). Going dual-stack later is
	// an in-place rule update + AAAA, no replacement. Outbound v6 stays
	// allowed. SSH is absent on purpose — the mesh is the only admin
	// plane.
	anyV4 := pulumi.StringArray{pulumi.String("0.0.0.0/0")}
	firewall, err := linode.NewFirewall(ctx, name+"-fw", &linode.FirewallArgs{
		Label:          pulumi.Sprintf("%s-fw", name),
		InboundPolicy:  pulumi.String("DROP"),
		OutboundPolicy: pulumi.String("ACCEPT"),
		Inbounds: linode.FirewallInboundArray{
			&linode.FirewallInboundArgs{
				Label:    pulumi.String("web"),
				Action:   pulumi.String("ACCEPT"),
				Protocol: pulumi.String("TCP"),
				Ports:    pulumi.String("80,443"),
				Ipv4s:    anyV4,
			},
			&linode.FirewallInboundArgs{
				Label:    pulumi.String("wireguard"),
				Action:   pulumi.String("ACCEPT"),
				Protocol: pulumi.String("UDP"),
				Ports:    pulumi.String("51820"),
				Ipv4s:    anyV4,
			},
		},
	}, opts...)
	if err != nil {
		return err
	}

	// Root disk fills the plan minus swap; the image's ext4 autoResizes
	// into it on first boot. Plan disk size comes from the PUBLIC types
	// endpoint (no auth) rather than the linode.GetInstanceType invoke,
	// which is broken in provider v4.39 both ways we tried it: the
	// matching plugin rejects the invoke (SDK marshals computed attrs,
	// framework says read-only: price/addons), and nixpkgs pulumi-bin's
	// ambient v6 plugin mis-shapes the result (Disk=0). Revisit on the
	// next provider major bump.
	diskMB, err := linodePlanDiskMB(spec.Type)
	if err != nil {
		return err
	}

	// The seed, when this run carries one (fort provision does; a bare
	// `pulumi preview` from provisioning/pulumi does not, and ignoreChanges keeps
	// the recorded value for an existing instance either way). Secret from
	// the first moment the engine sees it.
	var metadatas linode.InstanceMetadataArrayInput
	if ud := os.Getenv(seed.Env); ud != "" {
		metadatas = linode.InstanceMetadataArray{
			&linode.InstanceMetadataArgs{
				UserData: pulumi.ToSecret(pulumi.String(ud)).(pulumi.StringOutput),
			},
		}
	}

	instance, err := linode.NewInstance(ctx, name, &linode.InstanceArgs{
		Label:          pulumi.String(name),
		Region:         pulumi.String(spec.Region),
		Type:           pulumi.String(spec.Type),
		FirewallId:     firewall.ID().ApplyT(func(id string) (int, error) { return strconv.Atoi(id) }).(pulumi.IntOutput),
		PrivateIp:      pulumi.Bool(false),
		BackupsEnabled: pulumi.Bool(false), // backups are a layer's job, not the provider's
		Metadatas:      metadatas,
		// Empty shell: disks + boot config below own what runs on it.
		Booted: pulumi.Bool(false),
		// …and `booted` is therefore the InstanceConfig's to manage: a
		// refresh reads the running host back as booted=true, and without
		// this the next apply would "correct" it to false — which the
		// provider implements as a shutdown (caught on the first
		// `preview --refresh` after the fort went live, 2026-09-13).
		// `metadatas` for the reasons in the type comment: fixed for the
		// instance's life, never read back, and a rotation is a re-provision.
	}, append(opts,
		pulumi.IgnoreChanges([]string{"booted", "metadatas"}),
		pulumi.AdditionalSecretOutputs([]string{"metadatas"}),
	)...)
	if err != nil {
		return err
	}
	linodeID := instance.ID().ApplyT(func(id string) (int, error) { return strconv.Atoi(id) }).(pulumi.IntOutput)

	rootDisk, err := linode.NewInstanceDisk(ctx, name+"-root", &linode.InstanceDiskArgs{
		Label:    pulumi.String("nixos"),
		LinodeId: linodeID,
		Size:     pulumi.Int(diskMB - swapMB),
		Image:    pulumi.String(imageID),
	}, append(opts, pulumi.IgnoreChanges([]string{"image"}))...)
	if err != nil {
		return err
	}
	swapDisk, err := linode.NewInstanceDisk(ctx, name+"-swap", &linode.InstanceDiskArgs{
		Label:      pulumi.String("swap"),
		LinodeId:   linodeID,
		Size:       pulumi.Int(swapMB),
		Filesystem: pulumi.String("swap"),
	}, opts...)
	if err != nil {
		return err
	}

	diskID := func(d *linode.InstanceDisk) pulumi.IntPtrInput {
		return d.ID().ApplyT(func(id string) (int, error) { return strconv.Atoi(id) }).(pulumi.IntOutput)
	}
	_, err = linode.NewInstanceConfig(ctx, name+"-boot", &linode.InstanceConfigArgs{
		LinodeId:   linodeID,
		Label:      pulumi.String("nixos"),
		Kernel:     pulumi.String("linode/grub2"),
		RootDevice: pulumi.String("/dev/sda"),
		Device: linode.InstanceConfigDeviceArray{
			&linode.InstanceConfigDeviceArgs{DeviceName: pulumi.String("sda"), DiskId: diskID(rootDisk)},
			&linode.InstanceConfigDeviceArgs{DeviceName: pulumi.String("sdb"), DiskId: diskID(swapDisk)},
		},
		Helpers: linode.InstanceConfigHelperArray{
			&linode.InstanceConfigHelperArgs{
				// Network Helper can't rewrite NixOS network config; the
				// image handles eth0 DHCP itself. The rest assume a
				// mutable distro filesystem.
				Network:           pulumi.Bool(false),
				Distro:            pulumi.Bool(false),
				ModulesDep:        pulumi.Bool(false),
				DevtmpfsAutomount: pulumi.Bool(false),
				UpdatedbDisabled:  pulumi.Bool(true),
			},
		},
		Booted: pulumi.Bool(true),
	}, opts...)
	if err != nil {
		return err
	}

	// Public DNS for the production hostname → the new instance (#87).
	if host.DNS.Zone != "" {
		domain, err := linode.LookupDomain(ctx, &linode.LookupDomainArgs{Domain: &host.DNS.Zone})
		if err != nil {
			return err
		}
		_, err = linode.NewDomainRecord(ctx, name+"-dns", &linode.DomainRecordArgs{
			DomainId:   pulumi.Int(*domain.Id),
			Name:       pulumi.String(host.DNS.Name),
			RecordType: pulumi.String("A"),
			Target:     instance.IpAddress,
		}, opts...)
		if err != nil {
			return err
		}
		// Per-fort service hostnames (umami.<host>.<zone> today; the
		// fleet's <service>.<host> convention) resolve via one wildcard
		// rather than a record per service — the gateway listener +
		// Certificate are already keyed that way in flux/apps/<host>.
		_, err = linode.NewDomainRecord(ctx, name+"-dns-wildcard", &linode.DomainRecordArgs{
			DomainId:   pulumi.Int(*domain.Id),
			Name:       pulumi.String("*." + host.DNS.Name),
			RecordType: pulumi.String("A"),
			Target:     instance.IpAddress,
		}, opts...)
		if err != nil {
			return err
		}
	}

	ctx.Export("ip", instance.IpAddress)
	ctx.Export("mesh_ip", pulumi.String(host.MeshIP))
	return nil
}
