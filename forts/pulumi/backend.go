package main

import (
	"gitea.zen.lofi/sfi/deployments/forts/internal/inventory"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Backend realizes one host's inventory intent on one provider. The whole
// provider-side surface is deliberately three things — instance, firewall,
// DNS — because the mesh owns machine-to-machine traffic, flux owns
// everything above the machine (no managed services: rustfs/valkey/cnpg
// ride the clusters), and the bootstrap image is a birth input owned by
// `fort provision`, not a resource.
type Backend interface {
	Provision(ctx *pulumi.Context, name string, host inventory.Host) error
}
