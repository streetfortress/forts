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
	case "init":
		err = initCmd(os.Args[2:])
	case "scaffold":
		err = scaffold(os.Args[2:])
	case "image":
		err = image(os.Args[2:])
	case "provision":
		err = provision(os.Args[2:])
	case "adopt":
		err = adopt(os.Args[2:])
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

  Verbs by intent — author verbs render repo changes staged for a PR,
  apply verbs act on the provider, read verbs change nothing. The read
  verbs split by source of truth (the kubectl mnemonic): check is the
  admission gate over the repo, preview the diff against pulumi, and
  the two not yet built — describe (declared, from the repo) and status
  (observed, from the live host) — are spec and status (#224).

  author  init, scaffold, mint, image, dismantle, ssh-config,
          link add, estate link, layer add, layer remove
  layers  fort <layer> <verb> … runs the layer's own binary, fort-<layer>,
          from PATH: what sits on top of the core (docs/layers.md)
  keys    console-password (the fort's getty password)
  apply   provision, adopt, destroy
  read    check, preview, wg-conf

  init --name <estate> --repo <git url> --admin <age1…> [--hub <role>=<fort>@<estate git url> …] [--write]
      create an estate: render into the root of a git checkout the files
      every other verb assumes — estate.toml (its hubs by role: the forge,
      and any role a layer or the estate serves, in this estate or
      another; docs/hub.md), the
      estate flake (sfi/forts as inputs.forts), the two sops rule files with the admin
      recipients, the empty mesh, the Pulumi project file, the shared
      flux infrastructure layer, the gate workflow, the directories. A
      pure render: no secrets, no network, nothing overwritten. Names
      what it cannot write (the lab CA, the edge Alertmanager webhook,
      provisioning/secrets.yaml). docs/init.md.

  scaffold <host> [--write]
      render the host's generated surface from its manifest,
      forts/<host>.toml: nixos/hosts/<host>/{default.nix,
      secrets.nix} in the lab shape (qcow runtime, static LAN) or, with a
      cloud provider table ([linode]), the cloud shape (linode runtime, zram); also flux/clusters/<host>/ (6 files) and the apps root
      kustomization. A layer's scaffold renders what its role asks of a
      hub (docs/layers.md). Prints the marked tree; --write lays it down, never
      overwriting a file that exists (a re-run fills gaps) — the manifest
      seeds the host, you refine it in place. Then fort mint.

  mint <host> [--write]
      mint the host's identity: its ssh host key (which doubles as its
      age identity), wireguard key + mesh peer, k3s token, the cluster age key, the flux deploy key (registered
      read-only on gitea — the one call outside the tree) and the sops
      canary; the console password where the host file enables the door
      (every scaffolded host does, sfi/forts#24: the hash beside the
      keys, the passphrase in provisioning/console-passwords.yaml); the
      sops rules that make the host a recipient of its own
      secrets. Private halves exist in memory and land only in
      nixos/secrets/<host>.yaml, encrypted. A re-run restores what that
      file holds and fills gaps only — rotation is deliberate, never a
      side effect. Runs after scaffold --write, before add. Prints the
      plan; --write mints, encrypts, registers, stages. fort dismantle is
      the inverse. Mint knows no account but the fort's own: an identity
      for a user the estate adds to a fort is the estate's to mint, into
      the same secrets file, and mint leaves keys it does not own alone.

  link add <fort> <estate url> [--address <ip>] [--port <udp>] [--write]
      put <fort> on another estate's mesh (docs/links.md): the local half
      of a link. Fetches that estate's repo and reads its estate.toml
      (the mesh, the interface, the zone), mesh/peers.json and manifests
      (the addresses taken); picks the highest free address, from .254
      down, and the lowest free port beside 51820 (--address, --port
      override; both are checked); mints the WireGuard key. Writes, in
      THIS estate: the private half into nixos/secrets/<fort>.yaml as
      wireguard-private-key-<name> (sops set), the [links.<name>] table
      into forts/<fort>.toml with the public half plaintext, and the
      sfi.links.<name> block into the host file (the one fort scaffold
      renders). The address is a claim; the other estate's peers.json
      is the record, and its 'fort estate link' verifies the claim. A
      re-run restores the key from the secrets file, keeps an address
      that is still free and picks one again when it is taken. Prints
      the plan; --write writes and stages. Then PR → merge → the other
      estate runs estate link.

  estate link <estate url> [--write]
      record the hosts of another estate that link to this one: the
      other half. Fetches that estate's repo, reads every forts/*.toml
      for a [links.<name>] table that names THIS estate, and verifies
      each claim against mesh/peers.json here — inside the mesh, held by
      no peer, client, remote or manifest, the interface and zone this
      estate's — before it writes the remotes entry (address, the host's
      endpoint, port, public key). A collision refuses and names the
      holder; the fix is 'fort link add' again on the other side. An
      entry already there with the same facts is current; one with the
      same host and other facts follows the manifest. Prints the plan;
      --write writes and stages; every fort of this estate peers with
      the host on its next rebuild, and the host's reconciler fills its
      own peer set on its next run.

  console-password <host> [--write] [--rotate] [--show]
      rotate (--rotate) and read (--show) the console password of the
      host's deployer account (#524); fort mint mints the first one with
      the identity, and --write alone mints it only for a host that
      enabled the door after the first provision. Two halves for two readers: the yescrypt hash into
      nixos/secrets/<host>.yaml as deployer-password-hash, which the
      HOST reads, and the passphrase into
      provisioning/console-passwords.yaml, the registry an OPERATOR
      reads. That file is one entry per fort, in the shape
      mesh/peers.json already has, encrypted to the admin age keys
      and NOT to the fort. nixos/modules/ops/console.nix consumes the
      hash, and only a getty accepts it — sshd refuses every password
      (nixos/modules/common.nix). The door exists for one failure: the
      mesh breaks, no operator reaches ssh, and sudo sfi-nixos-rollback
      is the remedy. The registry is why the credential survives that
      failure: a checkout and an admin age key read it with no network
      at all. --write prints NO value; --show decrypts one fort's
      passphrase, deliberately, one fort at a time. Every fort has the
      door — a plain fort carries deployer too (ops/deployer.nix, #647).
      A hash the fort already carries is never replaced without
      --rotate. fort dismantle withdraws the entry; fort check traces
      all three halves.

  image <host> [--from <built image>] [--out <dir>]
      build the host's bootstrap image: nix build ./nixos#<host>-{qcow|
      linode|baremetal} (the variant from the manifest's provider table;
      --from skips the build). The build output IS the artifact, on every
      variant, no secret inside (#659): the key that decrypts the host's
      secrets on first boot never enters the nix store or the image — it
      arrives at provision as metadata.user_data (#606) or on a NoCloud
      cidata volume (#657) and the initrd places it before activation;
      the baremetal channel is #658. linode's 6 GiB raw / 5 GiB gzip caps
      are asserted. Output: ~/.cache/forts/<host>-nixos.{qcow2,img.gz,
      img} — what provision --image consumes, or, for a [baremetal] host,
      what you write to the machine's disk from a live ISO (docs/
      baremetal.md). nix must be on PATH; nothing else.

  provision <host> [--image <path>] [--yes]
      apply the host's pulumi stack (execs 'pulumi up -s <host>'; you read
      the preview, or --yes confirms it — an agent, a script, CI; without
      a TTY and without --yes the verb refuses up front). With --image, the first provision: create the stack if the host
      has none, stage the bootstrap image and record it as the stack's
      forts:imageId — linode: upload the .img.gz; libvirt: grow the
      .qcow2 to disk_size, the program streams it into the pool as the
      root disk — and once the host exists, release it (provider-side
      where applicable, and the local copy): hygiene, Linode bills
      custom images; the image holds no secret (#42, #659). The
      apply that creates the host (the stack says, not the flag) also
      hands the host key seed to the program, read from nixos/secrets/
      <host>.yaml: metadata.user_data on the instance (#606) or a NoCloud
      cidata volume on the domain (#657), and records forts:seed. Without
      --image a host with no stack is refused. The state (and at the
      first provision, Pulumi.<host>.yaml) is staged for its own PR. A [baremetal] host is
      refused outright: no backend raises it.

  adopt <host> [--yes]
      bring a playbook-era lab VM (a [libvirt] manifest, no stack yet) under the backend WITHOUT a re-provision: rescan the pool, import the
      live domain + volume into a new stack (reads the hypervisor, writes
      local state only), preview the program against them and refuse
      unless the only changes are live properties (autostart, running),
      then one 'pulumi up' (you confirm, or --yes) converges those. Stages the new
      stack for its own PR.

  preview (<host> | --all) [--refresh] [--expect-no-changes]
      the read-only half: 'pulumi preview -s <host> --diff'. --refresh
      reconciles state with the provider first (drift); --expect-no-changes
      turns any proposed change into a nonzero exit. --all walks every
      host with a manifest, one OK/DRIFT/FAILED line per stack (SKIP for
      a [baremetal] host: no stack by design) — with --refresh
      --expect-no-changes that is 'just iac-drift'.

  check (<fort>... | --all)
      the merge gate over the core's tree. Fleet: the fort's nix host
      dir, flux cluster dir and apps tree exist; flux/apps/<fort>,
      flux-system and the shared infrastructure layers kustomize-build
      (a listed-but-unseeded sops file fails it — the guard working);
      every ${VAR} they carry is a cluster-vars key (flux's envsubst is
      strict), and CLUSTER_NAME and ESTATE_NAME are the fort's and the
      estate's names. A layer's check (an app's contract against its
      Secrets, docs/layers.md) is the layer's own gate step after this
      one. and warns on host dirs no manifest
      claims. Strict on
      every fort; exits nonzero on any error; annotates for CI when
      GITEA_ACTIONS is set.

  ssh-config [--write]
      render the client side of the mesh from the tree (#434):
      mesh/ssh_config — one Host block per peer in peers.json, the
      fort name plus a role alias where one fort holds the role (sfi-dev,
      prod, stage), its mesh address, and the account its shape answers
      on (a fort deployer, a hand-managed peer the ssh_user its peers.json
      entry names) —
      and mesh/known_hosts, one line per
      fort with its ssh host key, keyed by fort name (HostKeyAlias) and
      mesh address. Both are committed; consumers Include the first and
      list the second as a UserKnownHostsFile. mint and dismantle keep
      them current as they add or remove a peer; this is the whole-fleet
      render, and the one that backfills a fort's key line, which is in
      its sops file — with no age key here the committed line is kept
      and said so. fort check --all fails on a stale ssh_config and
      warns on a missing key line. Prints the plan; --write writes and
      stages.

  wg-conf <member>
      print a mesh member's own mesh interface the way the WireGuard app (or
      wg-quick) wants it pasted, from peers.json: Address, ListenPort
      (peers, not roaming clients), every other peer with its Endpoint
      where it has one and PersistentKeepalive, every client without.
      No PrivateKey line — the app generated that key on the device and
      its public half is what peers.json holds; the member must already
      be registered there (docs/mesh.md, "Adding a peer"). The way a
      Mac joins the mesh: register, wg-conf, paste. Writes nothing.

  dismantle <host> [--write]
      the inverse of scaffold + mint, as a repo mutation
      staged for a PR (#217): removes the manifest, nixos/hosts/
      <host>, nixos/secrets/<host>.yaml, both sops rule blocks and the
      shared-infrastructure recipient (then sops updatekeys on the files
      that lost it), the wireguard peer, flux/clusters/<host> and
      flux/apps/<host> — and prints the
      residue: every remaining mention of the host that the plan does not
      settle, for the reviewer to triage in the same PR, cross-fort sops
      refs into it. Touches no pulumi state: the report's last line is the one
      next step, fort destroy <host> after the merge.

  destroy <host> [--yes] [--no-stack]
      the half with side effects outside git, gated on the merge: refuses
      unless the host's manifest is ABSENT from origin/main (and the
      working tree), the tree is clean, and the stack exists. Then, with
      inherited stdio (you confirm each pulumi step, or --yes): power the instance
      off (linode), pulumi refresh, state unprotect --all (only ever
      right before the destroy), destroy, stack rm; clears the file
      backend's residue (.bak, history, backups), revokes the flux-<host>
      deploy key on gitea, stages the state removal for its own PR. A
      failure mid-way leaves the unprotected state in the tree and says
      "rerun". Provider comes from the checkpoint, not the manifest.
      --no-stack is for a [baremetal] host, which has no stack by design:
      the same gates minus the stack, then only the deploy key; the
      machine goes by hand (docs/baremetal.md).

  layer add <flake url>#<role> [--write] [-- <the layer's init flags>]
      install a layer (forts#144, docs/layers.md): the flake input named
      after the repo, the role's binary re-exported at that pin so the
      gate runs "nix run ./nixos#fort-<role>", and the layer's gate step
      after core's. Then the layer's own init runs with every flag after
      "--", passed through unread; it writes layers/<role>/ itself. Core
      reads nothing under layers/. Prints the plan; --write edits,
      stages and runs the init.

  layer remove <role> [--write]
      the reversal: layers/<role>/, the gate step, the re-export, and
      the input when no other role of it stays. The layer's own
      dismantle runs first, per fort.

  <layer> <verb> [args]
      any other first word is a layer's name: fort-<layer> on PATH takes
      the rest, in this process, with FORT_ESTATE_ROOT set. The order
      across the two is the operator's: core's verb, then the layer's —
      except dismantle and destroy, where the layer's runs first.

  Cloud commands take LINODE_TOKEN and PULUMI_CONFIG_PASSPHRASE from the
  environment, else sops-decrypt them from provisioning/secrets.yaml.
  [libvirt] hosts reach the hypervisor over ssh: LIBVIRT_DEFAULT_URI names it.`)
}
