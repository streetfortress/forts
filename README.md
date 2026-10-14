# sfi/forts — the framework under an SFI estate

The modules a fort is made of, and the `fort` CLI that scaffolds, mints,
images, provisions and checks one. An **estate** (sfi/deployments, sfi/second-estate) is the other
half: the manifests, host files, secrets and flux trees of a set of
forts, built on this repo as a flake input. This repo holds no estate.

```
flake.nix          nixosModules.<name> (one per file under modules/), packages.fort (+ fort-pulumi,
                   wrapped with the CLI's tool set), devShells.default
modules/           the NixOS modules: the hardened profile, the mesh, k3s, flux, the ops verbs, ...
cmd/fort/          the CLI; templates/scaffold and templates/init (an estate's skeleton, the
                   shared flux layer included) are what it renders; a layer's templates are the layer's (docs/layers.md)
cmd/fort-pulumi/   the pulumi program fort provision execs
internal/          manifest, estate, seed, tools (the CLI's tool set: internal/tools/tools.json)
tests/estate/      a one-fort fixture estate the flake evaluates; tests/init-estate.sh creates one with fort init
docs/              the CLI handbook — start at docs/README.md
```

An estate takes this repo as `inputs.forts`; a host file imports
`inputs.forts.nixosModules.<name>` and sets `sfi.estate.root`
(modules/estate.nix) so the modules read the estate's files. Develop
against a local checkout with `--override-input forts path:../forts`.

---

## fort — cloud provisioning (#42/#87)

You manage *forts* with this: one fort = one host = base image + flux
workloads, declared by its manifest (`forts/<host>.toml`,
docs/manifest.md) and keyed by that hostname everywhere (nix host
dir, pulumi stack, peers.json, sops). `cmd/fort` is the CLI; `pulumi/` is
the resource program it wraps; `internal/manifest` is the shared schema.

The provider-side surface is deliberately three things — **instance,
firewall, DNS** — because the WireGuard mesh owns machine-to-machine
traffic and flux owns everything above the machine (no managed services:
a store or a database rides the cluster). Full design discussion on #87.

## Shape

- **One stack per host, stack name == the manifest's hostname.** Blast radius
  is one machine by construction; adding a host cannot churn the others.
  Zone-level resources (DNS zone, shared assets) get a `shared` stack
  when they materialize.
- **The program declares; the engine diffs; the human applies.** All
  apply paths go through the stock CLI — `fort provision` execs
  `pulumi up -s <host>` with inherited stdio, so you read the preview
  and confirm exactly as if run by hand, and running pulumi directly
  stays a first-class escape hatch. Never the automation API as a
  continuous applier.
- **State is the `file://` backend, committed here.** The owning repo is
  where state lives (a client buildout has no object store on day zero);
  secrets inside state are encrypted per-value by the passphrase
  provider. Moving to S3 later is `stack export | import`.
- **No renames, ever.** Hostname keys peers.json, sops, the flux cluster
  dir, and flake auto-discovery — rename = decommission + provision.
  Deleting a manifest touches no state; destruction is only ever
  `pulumi destroy -s <host>`, and every resource of every
  host carries `protect: true` so even that requires deliberate
  unprotecting first.

## The image is a provision input, not a resource

The bootstrap image is the plain build output of `fort image` (#42,
#659): built, uploaded, consumed by instance creation, then deleted on
both sides because nothing needs it afterwards. It carries no secret on
any variant — the host's key is `metadata.user_data` on the instance
(#606), or a NoCloud `cidata` volume attached to the domain (#657), set
by `fort provision` and read by the initrd (`nixos/modules/seed.nix`;
the model is #605's). Modeling the image as a pulumi resource is wrong
in both lifecycle directions:

- *Deletion*: a deliberately-deleted resource is permanent drift —
  `--expect-no-changes` red forever, or `RetainOnDelete` gymnastics.
- *Updates*: a rebuilt image would diff the instance's `image` input and
  ask to **replace the running host**. Day-2 updates are flux +
  auto-rebuild (#93); instance replacement is the deliberate DR drill,
  never an implicit apply.

Same shape as Packer→AMI→Terraform: the artifact is a pipeline step, IaC
consumes its ID. So `fort provision --image` uploads via the Linode API,
records the ID as stack config `forts:imageId` (committed provenance:
which image this host was provisioned from), execs `pulumi up`, and deletes the
image provider-side and locally once the instance exists — hygiene,
because Linode bills a custom image for as long as it exists, not
custody. The instance sets `ignoreChanges: ["image"]` — a provision input, not a
managed property — which keeps refresh and drift green after the image
is gone.

## Flow

The workflow — author verbs → PR → merge → apply verbs → commit the
pulumi state — and one page per verb are in
[`docs/`](../docs/README.md). `fort` with no arguments
prints the usage block, which is the same list.

Env: `LINODE_TOKEN` (shared with the pulumi provider) and the stack's
`PULUMI_CONFIG_PASSPHRASE` — taken from the environment when set, else
sops-decrypted from `provisioning/secrets.yaml` by every cloud command.
The libvirt backend needs neither: `LIBVIRT_DEFAULT_URI` defaults to
soundship over ssh with `~/.ssh/id_ed25519` in the URI (the provider
carries its own ssh client — no `~/.ssh/config`, no agent unless asked),
and `xsltproc` / `mkisofs` / `qemu-img` ride on the `fort` package's own
PATH (docs/README.md, "The tool set").

## The lab backend (`provider: libvirt`)

The lab VM as resources (what the create-nixos playbook's VM half used to
define): a qcow2 volume in soundship's `images` pool and a domain
with the lab's fixed shape (host-passthrough, virtio disk, br0, serial +
VNC, autostart). No firewall, no DNS — the LAN bridge and the lab
dnsmasq are the hypervisor's. Three provider facts shaped it (all found
on the probe, 2026-09-14): `size` is refused next to `source`, so the
qcow2 is grown with `qemu-img resize` before upload (the playbook's
order); a `<disk type='volume'>` never starts under Ubuntu's AppArmor
helper, so the domain uses a file disk on the volume's path; and a file
disk renders `driver type='raw'`, so an XSLT (the provider's escape
hatch) sets qcow2. The uploaded volume IS the root disk: not consumed,
`source` and `disks` ignored after the provision — a different disk is a
re-provision (`pulumi destroy` + provision), never an in-place change. The
host key rides beside it (#657): a cloud-init disk `<host>-seed.iso` in the
same pool, the NoCloud `cidata` layout, attached as an IDE CD-ROM and read
by the initrd; `user_data` is a pulumi secret, ignored after the provision,
and the provider needs `mkisofs` on the machine that runs the apply
(nixpkgs' `cdrtools`, which the `fort` package carries).

```
fort image lab1                                      # nix build (KVM: devbox); the key rides on the seed volume, not in the image
fort provision lab1 --image ~/.cache/forts/lab1-nixos.qcow2
```

Hosts without `provider` are untouched. A playbook-era VM joins without
a re-provision: set `provider: libvirt` on its entry and run `fort adopt
<host>` — the live domain and volume are imported (read-only against
the hypervisor), the program is previewed against them, and only live
properties (autostart, which the playbook never set) are allowed to
change; anything replace-forcing refuses. Provision-time shape (disk,
consoles, graphics, the driver XSLT, the provider's defaults) is ignored
after creation for exactly this reason — a different shape is a
re-provision. `fort destroy` for a lab host is the same `pulumi destroy` as
for a cloud one.

## Setup

None per host: `fort provision <host> --image …` creates the stack at
the first provision (`fort adopt` at adoption), and `fort destroy`
removes it.

`fort` commands pin `PULUMI_BACKEND_URL` to this checkout's
`provisioning/pulumi/state`, so no `pulumi login` is needed for them — and a
stale global login (e.g. from a worktree) cannot redirect where state is
written. Running pulumi by hand still honours whatever `pulumi login`
last recorded: prefer `fort preview` / `fort provision`, or export
`PULUMI_BACKEND_URL` yourself.

## Status

Demonstration shape (#87 decision PR): everything compiles against the
real SDKs but nothing has run against an account — the #42 checkboxes
(image upload contract, boot-config quirks, end-to-end first boot) are
the next session's spike. The libvirt backend
defines + starts the VM from the bootstrap qcow; every lab VM carries
`provider: libvirt` (the playbooks are gone — the three that predated
the backend were adopted in place). `fort
scaffold` renders the host's generated surface (nix host dir in the lab
or cloud shape, the flux cluster dir, the apps root kustomization; the
flake discovers the image variant from the host dir's runtime import);
`fort mint` is the identity step (host key → age → sops → wg peer →
deploy key → canary; private halves in memory, then only in the host's
sops file). A layer renders a chart's workload tree and its Secrets
(docs/layers.md; the apps layer lives in sfi/fort-layers). `fort dismantle` is the repo half
of decommissioning (#217) and `fort destroy` the side-effects half, gated
on the dismantle merge (docs/destroy.md). The public-face extraction (github.com/streetfortress/forts)
splits the generic tool from repo-specific manifests/state — later, and
a publish step.
