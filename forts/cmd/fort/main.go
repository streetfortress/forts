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
	case "seed":
		err = seed(os.Args[2:])
	case "check":
		err = check(os.Args[2:])
	case "dismantle":
		err = dismantle(os.Args[2:])
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

  seed <fort> [<dir>...] [--set KEY] [--chart-dir path] [--write]
      produce the fort's sops-encrypted Secrets from contracts: the
      committed full-form secrets.contract.yaml where there is one, else
      the chart's own contract (pulled at the pinned tag) + the fort's
      overlay, resolved against the fort's providers by convention.
      Dirs default to every contract/chart dir of the fort, in
      dependency order. Existing files are never rewritten: missing keys
      are added (sops set); --set KEY takes one value from stdin for an
      external key. Prints the ◆ plan; --write seeds. No value is ever
      printed.

  check (<fort>... | --all) [--chart-dir path]
      the merge gate: every app dir's committed Secret carries the keys
      its effective contract requires (plaintext key names, no
      decryption), every provider var resolves to a seeded file+key,
      every required provider is present and its wire: values are set
      in the HelmRelease. Charts are pulled at the pinned tag once per
      run. Strict on every fort (all are production-hardened); exits
      nonzero on any error; annotates for CI when GITEA_ACTIONS is set.

  dismantle <host> [--write]
      the inverse of scaffold + preseed-identity, as a repo mutation
      staged for a PR (#217): removes the inventory entry, nixos/hosts/
      <host>, nixos/secrets/<host>.yaml, both sops rule blocks and the
      shared-infrastructure recipient (then sops updatekeys on the files
      that lost it), the wireguard peer, fleet/clusters/<host> and
      fleet/apps/<host>, the flake's cloudHosts entry — and prints the
      residue: every remaining mention of the host for the reviewer to
      triage in the same PR, and cross-fort sops refs into it. Touches
      no pulumi state: fort destroy is the second, gated step after the
      merge (forts/DECOMISSIONING.md until it lands).

  Cloud commands take LINODE_TOKEN and PULUMI_CONFIG_PASSPHRASE from the
  environment, else sops-decrypt them from provisioning/secrets.yaml.`)
}
