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
//	fort add web3 oci://…/symbology     # render a chart's fort-side tree
//	    --version 0.1.5 --write         # from its contract (#144) → seed → PR
//
// The create-nixos / delete playbooks that raised lab VMs on soundship
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
	case "add":
		err = add(os.Args[2:])
	case "seed":
		err = seed(os.Args[2:])
	case "mint":
		err = mint(os.Args[2:])
	case "b2-key":
		err = b2Key(os.Args[2:])
	case "console-password":
		err = consolePassword(os.Args[2:])
	case "b2-buckets":
		err = b2BucketsCmd(os.Args[2:])
	case "check":
		err = check(os.Args[2:])
	case "ssh-config":
		err = sshConfigCmd(os.Args[2:])
	case "wg-conf":
		err = wgConf(os.Args[2:])
	case "dismantle":
		err = dismantle(os.Args[2:])
	case "destroy":
		err = destroy(os.Args[2:])
	case completeVerb:
		// The shell's callback, not an operator's verb: hidden from
		// usage(), and it answers nothing when it cannot (complete.go).
		err = complete(os.Args[2:])
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

  Verbs by intent — author verbs render repo changes staged for a PR,
  apply verbs act on the provider, read verbs change nothing. The read
  verbs split by source of truth (the kubectl mnemonic): check is the
  admission gate over the repo, preview the diff against pulumi, and
  the two not yet built — describe (declared, from the repo) and status
  (observed, from the live host) — are spec and status (#224).

  author  init, scaffold, mint, image, add, seed, dismantle, ssh-config
  keys    b2-key (the B2 credentials no other verb creates),
          console-password (the fort's getty password)
  apply   provision, adopt, destroy
  read    check, preview, wg-conf

  init --name <estate> --repo <git url> --admin <age1…> [--hub <role>=<fort>@<estate git url> …] [--write]
      create an estate: render into the root of a git checkout the files
      every other verb assumes — estate.toml (its hubs by role: the forge,
      the monitoring hub, in this estate or another; docs/hub.md), the
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
      cloud provider table ([linode]), the cloud shape (linode runtime, zram); also flux/clusters/<host>/ (6 files), the apps root
      kustomization, and the fort's file for its monitoring hub under
      flux/hub/<hub host>/ (docs/hub.md); on the hub host itself, the
      subscriptions under flux/apps/<host>/serves/. Prints the marked tree; --write lays it down, never
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

  b2-key <minting|pruning|audit|pv <host>|db-physical <host>|db-logical <host>|restore <host>> [--write]
      mint a B2 credential no verb may create for itself, because these
      live outside the repo and the web console cannot express their
      capability sets. minting (sfi-b2-key-minting-key)
      is what fort mint exports to create per-fort keys: writeKeys, no
      deleteFiles. pruning (sfi-b2-pruning-key) is the fleet's ONLY
      delete-capable credential, scoped to sfi-pv-backups, and belongs in
      luxor's host secrets for sfi.pv-prune. audit (sfi-b2-audit-key)
      is the s3-exporter's: list on all three backup buckets, no
      readFiles — the one key that stands in a Secret for good, because
      it reads nothing. The host roles are ONE KEY
      PER HOST PER BUCKET, namePrefix <host>/, no deleteFiles, the role
      word being the key's stem and the bucket's: pv (sfi-pv-<host>) is
      the restic credential the HOST holds — the grant fort mint gives a
      fort, so it is refused for a host that has a manifest; db-physical
      (sfi-db-physical-<host>) is what the host's CNPG cluster ships
      barman backups with, held in-cluster; db-logical
      (sfi-db-logical-<host>) is what a databasus instance writes that
      host's dumps with. They are separate because their holders are, and
      because a writer can destroy whatever it can reach (overwrite, then
      the lifecycle rule deletes the hidden original). restore
      (sfi-restore-<host>) is the credential a RESTORE runs with:
      list/read on all three buckets under <host>/ and nothing that
      writes. It is the one grant that crosses a prefix, because the
      machine restoring a host is never that host; it is NOT refused for
      a fort, fort destroy leaves it standing, and it belongs in the
      password manager for the length of the restore.
      Prompts for the credential it needs, or reads B2_APP_KEY: the
      account master key for minting, and sfi-b2-key-minting-key for
      everything else. The minting key's id is not a secret and sits
      in provisioning/secrets.yaml as b2-minting-key-id, so only the
      key is pasted (B2_KEY_ID overrides the id; the master key's id
      is always asked for). Prints the plan and the grant; --write
      creates it and prints the secret ONCE — one of the two verbs that
      print a value (console-password is the other), because there is
      nowhere else for it to go. An existing key is never replaced (its
      secret is unrecoverable) and any drift from the role's grant is
      named.

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

  b2-buckets [--write]
      reconcile the B2 backup buckets with their declaration: each
      exists, is allPrivate, and carries exactly one lifecycle rule in
      the API's raw fields — daysFromHidingToDeleting on all three,
      daysFromUploadingToHiding on the two append-only db buckets and
      NEVER on sfi-pv-backups, where it would shred a restic repo. The
      console's lifecycle presets abstract over those two fields, which
      is why this is a verb. Rules are replaced wholesale; buckets it
      does not declare are reported and never touched. The plan runs on
      the minting key; --write needs the account master key
      (writeBuckets), prompted for or from B2_KEY_ID / B2_APP_KEY.
      destroy asks for the minting key only when the retired
      nixos/secrets/<host>.yaml shows the fort held a B2 key.

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

  add <fort> <oci-ref> (--version X | --follow-pr N | --follow-main)
      [--name app] [--hostname fqdn] [--http service:port]
      [--without type,...] [--chart-dir path] [--write]
      render a chart's fort-side tree from the forts.contract.yaml it
      ships (docs/fort-configuration-contracts.md): app dir + HelmRelease with the
      contract's wire: values filled by convention, gateway listener +
      certificate, provider instances (forts/postgres, forts/valkey,
      forts/rustfs — one namespace, forts) on first need and tenant
      registration after that, the seeding contract.
      New files + appended list entries only; prints the marked tree.
      --write lays it down for a PR, then seed the ◆ files. --follow-pr
      pins a PR's per-commit preview charts via a semver filter, and
      --follow-main pins main's — the staging pin, which names no
      version. A chart that ships no contract declares nothing: add
      renders the app dir alone and says so; --http names the Service
      to route to.

  seed <fort> [<dir>...] [--set KEY] [--chart-dir path] [--write]
      produce the fort's sops-encrypted Secrets from contracts: the
      committed full-form forts.contract.yaml where there is one, else
      the chart's own contract (pulled at the pinned tag) + the fort's
      overlay, resolved against the fort's providers by convention.
      A <dir> is the app's name (symbology), a provider's (postgres,
      valkey, rustfs — under forts/) or its repo-relative path;
      dirs default to every contract/chart dir of the fort, in
      dependency order. Existing files are never rewritten: missing keys
      are added (sops set); --set KEY takes one value from stdin for an
      external key. Prints the ◆ plan; --write seeds. No value is ever
      printed.

  check (<fort>... | --all) [--chart-dir path]
      the merge gate. Fleet: the fort's nix host dir, flux cluster dir
      and apps tree exist; flux/apps/<fort>, flux-system and the shared
      infrastructure layers kustomize-build (a listed-but-unseeded sops
      file fails it — the guard working); every ${VAR} they carry is a
      cluster-vars key (flux's envsubst is strict), and CLUSTER_NAME and
      ESTATE_NAME are the fort's and the estate's names. Contracts: every app
      dir's committed Secret carries the keys its effective contract
      requires (plaintext key names, no decryption), every provider var
      resolves to a seeded file+key, every required provider is present
      and its wire: values are set in the HelmRelease. Charts are pulled
      at the pinned tag once per run. The B2 pruning key's holder
      (#400): a host that sweeps a tier of sfi.pv-prune names that
      tier's restic-prune-s3-env key in its sops file (else an error:
      the activation fails), and a host that names one sweeps the tier
      (else a warning: a deleting credential nothing reads); --all
      also wants exactly one host naming the b2 key. --all also
      validates every manifest and warns on host dirs no manifest
      claims. Strict on
      every fort; exits nonzero on any error; annotates for CI when
      GITEA_ACTIONS is set.

  ssh-config [--write]
      render the client side of the mesh from the tree (#434):
      mesh/ssh_config — one Host block per peer in peers.json, the
      fort name plus a role alias where one fort holds the role (sfi-dev,
      sfi-prod, sfi-stage), its mesh address, and the account its shape
      answers on (a fort deployer, a peer with no manifest steven) —
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
      refs into it, and whether the host is the fleet's B2 pruner (#400:
      its sops file names restic-prune-s3-env-b2; destroy revokes the
      key). Touches no pulumi state: the report's last line is the one
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
      the same gates minus the stack, then only the identities (deploy
      key, B2 keys); the machine goes by hand (docs/baremetal.md).
      The B2 keys are the fort's own and, when the retired secrets file
      names restic-prune-s3-env-b2, sfi-b2-pruning-key too (#400): the
      host was the fleet's pruner, and the output says the fleet now
      has none.

  Cloud commands take LINODE_TOKEN and PULUMI_CONFIG_PASSPHRASE from the
  environment, else sops-decrypt them from provisioning/secrets.yaml.
  [libvirt] hosts reach soundship over ssh (LIBVIRT_DEFAULT_URI).`)
}
