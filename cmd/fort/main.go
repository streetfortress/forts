// fort — manage forts: the codegen and lifecycle half of the
// provisioning story (#42/#87). A fort is one host: base image + flux
// workloads, declared by its manifest (forts/<host>.toml)
// and keyed by that hostname everywhere (nix host dir, pulumi stack,
// peers.json, sops).
//
// Scaffolding writes source of truth into this repo (nix host files, flux
// cluster dirs, sops rules, wg peers) — it is a code generator, reviewed
// like any other change, and deliberately NOT part of any Pulumi resource
// graph. Provisioning wraps the stock pulumi CLI and owns the bootstrap
// image's lifecycle around it. The full flow:
//
//	fort scaffold web3                  # render files → branch → PR
//	fort image web3                     # (after the merge) nix build; the output is the artifact
//	fort provision web3 --image <path>  # stage image → pulumi up → release it
//	                                    # flux owns everything above it
//	fort preview web3 [--refresh]       # read the diff (day-2 edits, drift)
//	fort dismantle web3 --write         # retire: repo half → PR (merge)
//	fort destroy web3                   # then the side-effects half
//
// The create-nixos / delete playbooks that raised lab VMs on the hypervisor
// are gone: scaffold + mint cover their Phase A and cluster-init, the
// libvirt backend their VM half, destroy their delete. Every manifest
// names exactly one provider table (fort check --all validates them).
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
	case "--version":
		err = version(os.Stdout)
	case "init":
		err = initCmd(os.Args[2:])
	case "scaffold":
		err = scaffold(os.Args[2:])
	case "image":
		err = image(os.Args[2:])
	case "provision":
		err = provision(os.Args[2:])
	case "preview":
		err = preview(os.Args[2:])
	case "mint":
		err = mint(os.Args[2:])
	case "console-password":
		err = consolePassword(os.Args[2:])
	case "check":
		err = check(os.Args[2:])
	case "ssh-config":
		err = sshConfigCmd(os.Args[2:])
	case "wg-conf":
		err = wgConf(os.Args[2:])
	case "link":
		err = linkCmd(os.Args[2:])
	case "estate":
		err = estateCmd(os.Args[2:])
	case "dismantle":
		err = dismantle(os.Args[2:])
	case "destroy":
		err = destroy(os.Args[2:])
	case "layer":
		err = layerCmd(os.Args[2:])
	case completeVerb:
		// The shell's callback, not an operator's verb: hidden from
		// usage(), and it answers nothing when it cannot (complete.go).
		err = complete(os.Args[2:])
	default:
		// A verb core does not have is a layer's name: fort-<name> on
		// PATH takes the rest (layer.go, forts#144). The error names
		// both when there is neither.
		err = execLayer(os.Args[1], os.Args[2:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `usage: fort <command> [args]
  Forts! A framework for building nixOS and fluxCD environments.
  Use the fort CLI to init an estate, scaffold, manage secrets and lifecyle a fort.

  init -> scaffold -> mint -> add -> seed -> provision -> dismantle -> destroy

  ---

  --version
	  Display version information about the fort cli and associated flake

  init --name <estate> --repo <git url> --admin <age1…> [--hub <role>=<fort>@<estate git url> …] [--write]
      Create an estate: render the files and directory structure expected by forts.
      See docs/init.md for more information.

  scaffold <fort> [--write]
	  Create a fort: render the files and directory structure for a single fort.
	  See docs/scaffold.md for more information.

  mint <fort> [--write]
      Mint a fort's identity: Generate host, cluster, wireguard keys.
	  See docs/mint.md for more information.

  image <fort> [--from <built image>] [--out <dir>]
	  Build a bootable disk image for a fort.
	  See docs/provision.md for more information.

  provision <fort> [--image <path>] [--yes]
	  Deploy a fort. Wrapper around 'pulumi up'
	  See docs/provision.md for more information.

  dismantle <fort> [--write]
	  Remove a fort's files that were rendered by fort cli. Used when preparing a fort for decomission.
	  See docs/provision.md for more information.

  destroy <fort> [--yes] [--no-stack]
	  Once a fort has been dismantled, this command deletes the associated compute resources.
	  Wrapper around 'pulumi destroy'
	  See docs/provision.md for more information.

  check (<fort>... | --all) [--eval]
	  Static validation of the estate repo:
	  - kustomize build of flux/
	  - validation of forts-managed configuration paths
	  - with --eval: nix-eval of each checked fort's nixos configuration
	  See docs/reference/check.md for more information.

  preview (<host> | --all) [--refresh] [--expect-no-changes]
	  Compare reality with what's declared in the estate.
	  Wrapper around 'pulumi preview'
	  See docs/provision.md for more information.

  console-password <fort> [--write] [--rotate] [--show]
	  Manage the out-of-band console access password for a fort.
	  See docs/ops.md for more information.

  ssh-config [--write]
	  Helper function: render an SSH config for accessing the forts of this estate.

  wg-conf <member>
	  Helper function: render a Wireguard configuration file for joining a client to the estate mesh.

  link add <fort> <estate url> [--address <ip>] [--port <udp>] [--write]
	  Configure a link between <fort> and a remote estate.
	  See docs/links.md for more information.

  estate link <estate url> [--write]
	  Configure this estate to accept peers in a remote estate.
	  See docs/links.md for more information.

  layer add <flake url>#<role> [--write] [-- <the layer's init flags>]
	  Add a layer configuration. Layers allow for arbitrary apps and configs to be applied in estates uniformly.
	  See docs/layers.md for more information.

  layer remove <role> [--write]
	  Remove a layer configuration.
	  See docs/layers.md for more information.
  `)
}
