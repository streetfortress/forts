# Forts — Getting Started

## Configuration

We tried to minimize the amount of new configuration when designing forts. Typically, when you're building out an environment, you're repeating a lot of manifest content with only a handful of differentiating factors: names, ip addresses, that kind of thing.

To that end, there's only two fort configuration files, and they both use `.toml` to distinguish themselves from the Flux and nixOS configs. These files are kept intentionally brief: 

**Estate.toml** holds the core estate-level facts:
```
name = "estate"
operator_keys = [ "ssh-ed25519 AAAA...", ] # ssh public keys for operator access after bootstrapping; configure later in default.nix

[source]
repo = "https://forge.streetfortress.cloud/sfi/estate" # where the estate's forts will pull their configurations from

[mesh]
cidr = "10.1.0.0/16" # the mesh network topology
```

**\<fort\>.toml** holds the core fort-level facts:
```
mesh_ip = "10.1.0.2"


[libvirt] # other backends: baremetal and linode (more to come)
uri = "qemu+ssh://my-hypervisor/system"
ip = "10.0.0.2" # LAN IP

[shape] # some backend providers use a 'sku' instead of a shape
vcpus = 4
memory = 8
disk = "80G"

[cluster] # enable k3s if you want to leverage flux for workload management
datastore = "sqlite"
audit_logging = true
```

There's a bit more when **Estate Linking** gets involved, but that's the core of it. 

Run `fort init` inside an empty directory with `estate.toml` to render out _the estate's_ boilerplate. Then, `fort scaffold <fort>` renders out _that fort's_ boilerplate.

## Deploying a fort

In this GitOps environment, _everything_ about a fort involves a git commit; doing _anything_ usually requires a handful of manifest files and edits.

The `fort` cli provides a few commands to help facilitate this boilerplate:

- `scaffold`: renders the fort's nixos and flux trees.
- `mint`: generates _identity_ secrets for the fort.
- `add`: render flux HelmRelease, Gateway API CRs, etc.
- `seed`: generates _application_ secrets for the fort workloads.
- `image`: builds the fort's bootable disk image.
- `provision`: deploys the fort's disk image using **pulumi**.
- `dismantle`: removes the rendered manifests.
- `destroy`: destroys the _pulumi stack_ and associated resources _like keys_.

### The rhythm: author → PR → merge → apply → commit the state

Every change follows the same process: prepare the change in git, review and merge to main, the change rolls out automatically across the estate.

The 'PR for every change' practice might feel like overkill, but it has a few upsides that make it interesting:
- it builds a recorded history of infrastructure configurations
- everything being declared makes it easy to reason about during incidents
- committed revisions make rollback and recovery straightforward, drill-able steps.

## Your first 5 minutes with forts:

### Create an Estate

Start by creating an empty git repo. Write an `estate.toml` file describing the `[source]` repository, `[mesh]` network, and operator SSH public keys.

```
fort init --write        ← once per estate: estate.toml, the flake, the rules, the layer (init.md)
```

This command will render out _the estate's_ directory tree. Commit this and push to the repo.

### Scaffold a Fort

Write a `<fort>.toml` file in the `forts/` directory. The fort file is where you define its mesh IP, resource allocations, and provider (eg libvirt).

Then run:

```
fort scaffold <host> --write  ← nixos/hosts/<host>/, flux/clusters/<host>/, apps root
```

Like `fort init`, `fort scaffold` will render out _the fort's_ nixos and flux directory trees.


#### Mint the fort's identity

One last step before we deploy. The fort has manifests, but is missing important sensitive configurations:

- Its **host SSH key**. This is used to identify the fort to other machines AND doubles as the **host age key** used to decrypt secrets on the fort.
- Its **wireguard key** and peer configuration. Without these the fort cannot communicate on the mesh.
- Its **kubernetes cluster token** and **cluster age key** used to decrypt secrets in the cluster.

Forts relies heavily on [sops](https://getsops.io) for secrets management. Secrets sit encrypted at rest _in the estate repo_ and are made accessible by effective key management:
- A fort cannot read secrets encrypted to another fort
- A _cluster workload_ cannot read secrets encrypted in the fort's _own host_ secrets

Because of this exposure it's common to keep estate repos private/unlisted (to avoid "collect now, decrypt later" style attacks.) SOPS uses AGE by default when encrypting secrets.


Run `fort mint <fort> --write` and commit the changes.

### Provision the Fort

Once a fort has been scaffolded and minted, it's ready to deploy. `fort image <fort>` will build a bootable disk image in the format required by the fort's **provider** (libvirt: qcow2, linode: .img.gz, etc). The build is its own verb, not a step inside `provision`: images build where a builder lives (an x86_64 nix host), and an operator can build a batch ahead of a deployment.

That image is pre-configured to look up the estate repo and begin reconciling. No step 2, just boot it and you're ready to go.

#### Compute Providers
`fort provision <fort>` takes that image and deploys it on a _compute provider_:

| provider | notes |
| --- | --- |
| `libvirt` | any libvirt hypervisor |
| `linode` | the only cloud provider supported atm. Any that allow upload of custom VM disk images should be supportable in the future |
| `baremetal` | Works in practice but not automated yet |

#### Fort cli is a pulumi wrapper

`fort provision` and its companions `fort destroy` and `fort preview` call [**pulumi**](https://github.com/pulumi/pulumi) under the hood. Everything above the fort is reconciled from the estate - but everything below it (the compute instance, disk, firewall) are not.

Thankfully, that's a short list, and **pulumi** serves as a good backend for deploying forts.

Each fort gets an independent _pulumi stack_ that tracks deployment state in a _pulumi history file_.  **`fort provision` leaves the pulumi history uncommitted** in the repo. Commit this after provisioning and merge it to main.


### Define a Kubernetes Workload

Once your fort is deployed, it continuously reconciles both the `flux/` and `nixos/` trees. These work _exactly_ like a vanilla flux or nixos environment. You can define your own `HelmReleases`, custom nix configs, etc just like normal.

The fort CLI has a few more commands that can help with the workload boilerplate:

- `fort add <fort> oci://…/<chart>`: Render a HelmRelease and optionally the associated Gateway API CRs
- `fort seed <fort>`: Generate and encrypt strong secrets for your workloads
- `fort check <host>`: Run `kustomize build` and static validation against the `flux/` tree and the manifests; `--eval` adds a full nix evaluation of the host ([reference/check](reference/check.md)).

## Day 2 Stuff

Eventually you will need to get onto a fort for some operations work. When that day comes, your options are:

- SSH over the estate's **Mesh**
- Console access via the **provider backend**

See [ops.md](ops.md) for more information.

### Updating Forts

We're still working on the update cadence for forts. The nixOS flake will require more updates than the templates and CLI. To update the flake, the process looks like:

1. Move the tag in `nixos/flake.nix`
2. Run `nix flake lock ./nixos`
3. Commit and merge to main. The forts will reconcile the change shortly afterwards.

## Retirement

When it's time to decommission a fort, these CLI commands take care of the removal boilerplate:

- `fort dismantle <fort>` removes the files created by `fort scaffold`. Commit and merge their removal.
- `fort destroy <fort>` runs the equivalent `pulumi destroy` against the fort and _deletes the keys associated with the fort_. 

## More Reading

- [manifest](manifest.md) — the fort's declaration: neutral intent + one provider table
- [scaffold](scaffold.md) — the host's generated surface, from its manifest
- [mint](mint.md) — the host's identity, sops-encrypted in-repo
- [provision](provision.md) — the first provision and day-2 applies; `preview`
- [baremetal](baremetal.md) — a fort on hardware: the `[baremetal]` table, the hardware-neutral image, the provision by hand
- [init](init.md) — create an estate: the files every other verb assumes
- [ops](ops.md) — the host side: the three rungs of the ops ladder, the verb each rung has, and how an estate or a layer declares one of its own
- [dismantle](dismantle.md) — the repo half of retirement
- [hub](hub.md) — the hub as a relationship between estates: `[hubs.<role>]` and `[[serves.<role>]]` with its `ports`; the forge is the one role the core renders for
- [links](links.md) — a host on another estate's mesh: `[links.<name>]` in the manifest, `fort link add` and `fort estate link`, the reconciler, the `remotes` entry; the roles it opens
- [layers](layers.md) — what sits on top of the core: `layers/<layer>/` in the estate, `fort <layer> <verb>` execs `fort-<layer>`, `fort layer add` installs one and passes its flags through, the verb order
- [mesh](mesh.md) — `peers.json`: the subnet, the three tables, linking a host of another estate
- [ssh-config](ssh-config.md) — the client side of the mesh: `ssh_config` +
  `known_hosts` rendered from peers.json and the manifests; `wg-conf` for a
  Mac joining the mesh
- [destroy](destroy.md) — the side-effects half, gated on the merge
- [tools](reference/tools.md) — the CLI's tool set: what the package carries, what the host owns, and why
- [check](reference/check.md) — the merge gate: what it checks per fort and fleet-wide, reading the output, CI annotations
- [release](release.md) — a release is the flake at a tag: the contract, the two kinds of tag, nixpkgs as the channel, templates as bootstrap only, how an estate bumps

## Appendix

### Reading a verb's report

Every author verb prints the same report twice: without `--write` it is a dry
run (a plan, nothing touched), with `--write` it is the record of what was
laid down. One line per file, led by a mark:

| mark | meaning |
| --- | --- |
| `●` | created — a file this verb writes new |
| `○` | edited — an existing file this verb adds to or confirms current; never a rewrite |
| `◆` | regenerated — rendered wholesale from its sources (`ssh_config`, `known_hosts`, sops updatekeys); regenerate, never hand-edit |
| `✗` | removed — or, in `fort check` and the `fort destroy` gates, a failure |
| `✓` | a passed check or gate |
| `·` | a note — nothing was done; context you may want |
| `!` | attention — something this verb cannot fix; the line names the repair |

Author verbs stage what they write and stop there: the commit and the PR stay
with you. That is the whole contract behind "staged — commit the rendered
files."

### Credentials and environment

| need | comes from | verbs |
|---|---|---|
| admin age key (`~/.config/sops/age/keys.txt`) | you | mint, seed, dismantle (updatekeys), everything that reads `provisioning/secrets.yaml` |
| `gitea-token` or `github-token` — the forge's kind, derived from the host of `estate.toml`'s `repo` | `provisioning/secrets.yaml` | mint (register deploy key), destroy (revoke) — on the ssh route; an estate whose `repo` is `https://` clones anonymously and needs neither ([init.md](init.md)) |
| `PULUMI_CONFIG_PASSPHRASE` | env, else `pulumi-passphrase` in `provisioning/secrets.yaml` | provision, preview, destroy |
| `LINODE_TOKEN` | env, else `linode-token` in `provisioning/secrets.yaml` | the same, for `provider: linode` |
| the hypervisor | the fort's `[libvirt] uri`; `LIBVIRT_DEFAULT_URI` in the environment wins, and a destroy reads `forts:libvirtUri` off the stack | the same, for `provider: libvirt` |
| the tools the verbs exec (`git`, `helm`, `jq`, `kustomize`, `mkisofs`, `mkpasswd`, `qemu-img`, `sops`, `xsltproc`) | the `fort` package, which carries them on its own PATH ([reference/tools](reference/tools.md)) | every verb that shells out |
| `nix`, `pulumi` | the host | `image` (nix), the apply verbs (pulumi) |
| nothing | — | `check` (CI runs it without any key), `scaffold`, `wg-conf`, `ssh-config` (the `ssh_config` half; a fort's `known_hosts` line needs the admin age key) |


## The estate layout 

Here's what an estate typically looks like. These directories and files are generated by `fort init` and `fort scaffold` etc.


```
forts/<host>.toml                the manifests: one file per fort, the declaration
nixos/
  hosts/<host>/                  default.nix + secrets.nix, rendered by scaffold, then the estate's
  secrets/<host>.yaml            the fort's sops file, written by mint
  modules/                       the estate's own modules (the framework's come from the flake input)
  .sops.yaml                     the per-host rules, a marker block per fort
flux/
  clusters/<host>/               the FluxInstance and the cluster's Kustomizations
  apps/<host>/                   the apps root: the estate's flux, or what a layer renders into it
  infrastructure/                the shared layer every cluster applies
layers/<layer>/                  a layer's estate.toml and forts/<fort>.toml (layers.md); the core reads nothing here
mesh/
  peers.json                     who is on the mesh and where (mesh.md): mint adds, dismantle removes; remotes by hand
  ssh_config, known_hosts        rendered from peers.json + the manifests
provisioning/
  secrets.yaml                   the fleet secrets (gitea token, pulumi passphrase, ...)
  console-passwords.yaml         the console registry: mint adds a fort's passphrase, console-password rotates it, dismantle withdraws it
  pulumi/                        the project file, Pulumi.<host>.yaml per stack, and state/ (the file:// backend)
.sops.yaml                       the flux rules: one per cluster, plus the shared infrastructure rule
```
