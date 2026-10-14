package main

import (
	"gitea.zen.lofi/sfi/forts/internal/manifest"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Backend realizes one host's manifest on one provider. The whole
// provider-side surface is deliberately three things — instance, firewall,
// DNS — because the mesh owns machine-to-machine traffic, flux owns
// everything above the machine (no managed services: a store or a
// database rides the cluster), and the bootstrap image is a provision input owned by
// `fort provision`, not a resource.
type Backend interface {
	Provision(ctx *pulumi.Context, name string, host manifest.Host) error
}
