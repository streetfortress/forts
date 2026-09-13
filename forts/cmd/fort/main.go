// fort — manage forts: the codegen and lifecycle half of the
// provisioning story (#42/#87). A fort is one host: base image + flux
// workloads, keyed by its inventory hostname everywhere (nix host dir,
// pulumi stack, peers.json, sops).
//
// Scaffolding writes source of truth into this repo (nix host files, flux
// cluster dirs, sops rules, wg peers) — it is a code generator, reviewed
// like any other change, and deliberately NOT part of any Pulumi resource
// graph. Provisioning wraps the stock pulumi CLI and owns the single-use
// image's whole lifecycle around it. The full flow:
//
//	fort scaffold web3                  # render files → branch → PR
//	                                    # (merge; build + inject the image)
//	fort provision web3 --image <path>  # upload → pulumi up → delete image
//	                                    # flux owns everything above it
//	fort preview web3 [--refresh]       # read the diff (day-2 edits, drift)
//	fort add web3 oci://…/symbology     # render a chart's fort-side tree
//	    --version 0.1.5 --write         # from its contract (#144) → seed → PR
//
// Phase A parity is incremental: scaffold currently renders the host nix
// file; the identity steps (host key → age recipient → sops rule →
// encrypted secrets → wg peer → flux cluster-init) remain on
// provisioning/vms/create-nixos.yaml until ported. create-nixos stays
// authoritative for lab VMs throughout.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "scaffold":
		err = scaffold(os.Args[2:])
	case "provision":
		err = provision(os.Args[2:])
	case "preview":
		err = preview(os.Args[2:])
	case "add":
		err = add(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: fort <command> [args]

  scaffold <host> [--write]
      render the host's generated files from provisioning/inventory.yaml
      (stdout by default; --write creates nixos/hosts/<host>/default.nix,
      refusing to overwrite an existing file — inventory seeds the host,
      you refine it in place)

  provision <host> [--image <path>]
      apply the host's pulumi stack (execs 'pulumi up -s <host>'; you read
      the preview). With --image, first upload the injected single-use
      bootstrap image and record it as the stack's forts:imageId; once the
      instance exists, delete the image provider-side and locally (#42).

  preview <host> [--refresh] [--expect-no-changes]
      the read-only half: 'pulumi preview -s <host> --diff'. --refresh
      reconciles state with the provider first (drift, as 'just iac-drift'
      does); --expect-no-changes turns any proposed change into a nonzero
      exit.

  add <fort> <oci-ref> (--version X | --follow-pr N) [--name app]
      [--hostname fqdn] [--without type,...] [--chart-dir path] [--write]
      render a chart's fort-side tree from the secrets.contract.yaml it
      ships (forts/CONTRACTS.md): app dir + HelmRelease with the
      contract's wire: values filled by convention, gateway listener +
      certificate, provider instances (valkey) on first need and tenant
      registration (sites-db, valkey) after that, the seeding contract.
      New files + appended list entries only; prints the marked tree.
      --write lays it down for a PR, then seed the ◆ files. --follow-pr
      pins a PR's per-commit preview charts via a semver filter.

  Cloud commands take LINODE_TOKEN and PULUMI_CONFIG_PASSPHRASE from the
  environment, else sops-decrypt them from provisioning/secrets.yaml.`)
}
