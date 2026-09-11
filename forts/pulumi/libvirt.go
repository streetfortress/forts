package main

import (
	"fmt"

	"gitea.zen.lofi/sfi/deployments/forts/internal/inventory"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Libvirt is the soundship lab backend — a stub until it reaches parity
// with provisioning/vms/create-nixos.yaml (define VM from the qcow,
// qemu-img resize to disk_size, virsh autostart). When it lands, the
// "one tech from local KVM to any cloud" unification is complete and the
// ansible VM-lifecycle half retires; the identity/scaffold half is the
// fort CLI's job either way.
type Libvirt struct{}

func (l *Libvirt) Provision(ctx *pulumi.Context, name string, host inventory.Host) error {
	return fmt.Errorf("libvirt backend not implemented — %q stays on `just create-nixos` until parity (#42)", name)
}
