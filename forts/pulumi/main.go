// Cloud provisioning (#42/#87): one Pulumi micro-stack per host, stack
// name == the manifest's hostname (provisioning/forts/<host>.toml). The program is a declaration — it registers
// the host's desired resources and the engine diffs against this stack's
// state (file:// backend, committed). Apply is always the stock CLI —
// `fort provision` execs it with inherited stdio, so a human still reads
// the preview:
//
//	fort scaffold web3                  # render generated files → PR
//	fort provision web3 --image <path>  # upload → pulumi up → delete image
//	just iac-drift                      # preview --refresh --expect-no-changes
//
// Design (settled on #87): no renames ever (rename = decommission +
// provision); deleting the manifest is inert; destruction only by
// explicit `pulumi destroy`, double-gated by protect on production
// hosts. The bootstrap image is deliberately NOT a resource here — it is
// a birth input passed as stack config (see forts/README.md).
// Machine-to-machine traffic rides the WireGuard mesh — the only
// provider-side surface is instance, firewall, DNS.
package main

import (
	"fmt"

	"gitea.zen.lofi/sfi/deployments/forts/internal/manifest"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// backends realize a manifest's intent per provider (#87: "the manifest
// declares intent, a per-provider backend realizes it"). Adding a cloud =
// one more implementation of this interface + its Spec in
// internal/manifest. libvirt is the lab hypervisor (soundship) — lab hosts
// carry a [libvirt] table; the manifest loader refuses a host without
// exactly one provider table.
var backends = map[string]Backend{
	"linode":  &Linode{},
	"libvirt": &Libvirt{},
}

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		host, err := manifest.Load("../..", ctx.Stack())
		if err != nil {
			return err
		}
		backend, ok := backends[host.Provider]
		if !ok {
			return fmt.Errorf("no backend for provider %q", host.Provider)
		}
		return backend.Provision(ctx, ctx.Stack(), host)
	})
}
