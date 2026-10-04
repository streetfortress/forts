# forts — cloud provisioning (#42/#87)

You manage *forts* with this: one fort = one host = base image + flux
workloads, declared by its manifest (`provisioning/forts/<host>.toml`,
docs/forts/manifest.md) and keyed by that hostname everywhere (nix host
dir, pulumi stack, peers.json, sops). `cmd/fort` is the CLI; `pulumi/` is
the resource program it wraps; `internal/manifest` is the shared schema.

The provider-side surface is deliberately three things — **instance,
firewall, DNS** — because the WireGuard mesh owns machine-to-machine
traffic and flux owns everything above the machine (no managed services:
rustfs/valkey/cnpg ride the clusters). Full design discussion on #87.

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

## The image is a birth input, not a resource

The bootstrap image is single-use by design (#42): built, uploaded,
consumed by instance creation, then deleted on both sides. A baremetal
image carries the host's pre-seeded key; a Linode or lab image no longer
does — the key is `metadata.user_data` on the instance (#606), or a
NoCloud `cidata` volume attached to the domain (#657), set by `fort
provision` and read by the initrd (`nixos/modules/seed.nix`; the model
is #605's). Modeling the image as a pulumi resource is wrong in both
lifecycle directions:

- *Deletion*: a deliberately-deleted resource is permanent drift —
  `--expect-no-changes` red forever, or `RetainOnDelete` gymnastics.
- *Updates*: a rebuilt image would diff the instance's `image` input and
  ask to **replace the running host**. Day-2 updates are flux +
  auto-rebuild (#93); instance replacement is the deliberate DR drill,
  never an implicit apply.

Same shape as Packer→AMI→Terraform: the artifact is a pipeline step, IaC
consumes its ID. So `fort provision --image` uploads via the Linode API,
records the ID as stack config `forts:imageId` (committed provenance:
which image this host was born from), execs `pulumi up`, and deletes the
image provider-side and locally once the instance exists. The instance
sets `ignoreChanges: ["image"]` — birth input, not managed property —
which keeps refresh and drift green after the image is gone.

## Flow

The workflow — author verbs → PR → merge → apply verbs → commit the
pulumi state — and one page per verb are in
[`docs/forts/`](../docs/forts/README.md). `fort` with no arguments
prints the usage block, which is the same list.

Env: `LINODE_TOKEN` (shared with the pulumi provider) and the stack's
`PULUMI_CONFIG_PASSPHRASE` — taken from the environment when set, else
sops-decrypted from `provisioning/secrets.yaml` by every cloud command.
The libvirt backend needs neither: `LIBVIRT_DEFAULT_URI` defaults to
soundship over ssh with `~/.ssh/id_ed25519` in the URI (the provider
carries its own ssh client — no `~/.ssh/config`, no agent unless asked),
and `xsltproc` / `qemu-img` come from nixpkgs when the host has none.

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
`source` and `disks` ignored after birth — a different disk is a repave
(`pulumi destroy` + provision), never an in-place change. The host key
rides beside it (#657): a cloud-init disk `<host>-seed.iso` in the same
pool, the NoCloud `cidata` layout, attached as an IDE CD-ROM and read by
the initrd; `user_data` is a pulumi secret, ignored after birth, and the
provider needs `mkisofs` on the machine that runs the apply (nixpkgs'
`cdrtools` when the host has none).

```
fort image lab1                                      # nix build (KVM: devbox); no graft — the key rides on the seed volume
fort provision lab1 --image ~/.cache/forts/lab1-nixos.qcow2
```

Hosts without `provider` are untouched. A playbook-era VM joins without
a repave: set `provider: libvirt` on its entry and run `fort adopt
<host>` — the live domain and volume are imported (read-only against
the hypervisor), the program is previewed against them, and only live
properties (autostart, which the playbook never set) are allowed to
change; anything replace-forcing refuses. Birth-time shape (disk,
consoles, graphics, the driver XSLT, the provider's defaults) is ignored
after creation for exactly this reason — a different shape is a repave.
`fort destroy` for a lab host is the same `pulumi destroy` as for a
cloud one.

## Setup

None per host: `fort provision <host> --image …` creates the stack at
birth (`fort adopt` at adoption), and `fort destroy` removes it.

`fort` commands pin `PULUMI_BACKEND_URL` to this checkout's
`forts/pulumi/state`, so no `pulumi login` is needed for them — and a
stale global login (e.g. from a worktree) cannot redirect where state is
written. Running pulumi by hand still honours whatever `pulumi login`
last recorded: prefer `fort preview` / `fort provision`, or export
`PULUMI_BACKEND_URL` yourself.

## The B2 spike (`cmd/b2probe`, `internal/b2`)

`internal/b2` is the Backblaze client the B2 half of `fort mint` will use
(docs/infra/backups.md, "Minting keys"): authorize, list buckets, and
create / list / delete application keys against the v4 API, hand-rolled
on net/http the way the gitea deploy-key client in `mint.go` is. Its
`Minting` / `Writer` / `Pruning` capability sets are the design's three
credential roles spelled once.

`cmd/b2probe` is the throwaway half — a one-shot verification, against a
real account, of the three things the API reference does not state and
that decide the shape: whether a **non-master** key may create keys at
all, whether a new key's capabilities must be a **subset** of the
creator's, and whether `bucketIds` really takes an **array**. It also
proves the guard mint will use (`b2_list_keys` echoes the restrictions,
so a recorded keyID can be checked against its *authority*, not just its
existence) and, with `--data-plane`, that a writer key cannot write
outside its prefix or delete at all.

```
sops --config .sops.yaml set provisioning/secrets.yaml '["b2-key-id"]' '"<keyID>"'
sops --config .sops.yaml set provisioning/secrets.yaml '["b2-app-key"]' '"<applicationKey>"'
go run ./cmd/b2probe [--data-plane <scratch bucket>]
```

Credentials come from `B2_KEY_ID` / `B2_APP_KEY` when set, else from
`b2-key-id` / `b2-app-key` in `provisioning/secrets.yaml` — the
precedence `cloudEnv` gives `LINODE_TOKEN` and the gitea token. The real
minting credential is *not* a master key; that the scoped one works is
the thing the probe is verifying, so the master key is the first run's
exception and should be replaced once it answers.

Every key it makes is named `b2probe-*`, expires in an hour, and is
deleted before exit; it never touches a bucket. It exits non-zero if an
assumption did not hold, and the report says what each one decides.
Delete the command once the findings are recorded — `internal/b2` stays.

## Status

Demonstration shape (#87 decision PR): everything compiles against the
real SDKs but nothing has run against an account — the #42 checkboxes
(image upload contract, boot-config quirks, end-to-end first boot) are
the next session's spike. The libvirt backend
defines + starts the VM from the injected qcow; every lab VM carries
`provider: libvirt` (the playbooks are gone — the three that predated
the backend were adopted in place). `fort
scaffold` renders the host's generated surface (nix host dir in the lab
or cloud shape, the flux cluster dir, the apps root kustomization; the
flake discovers the image variant from the host dir's runtime import);
`fort mint` is the identity step (host key → age → sops → wg peer →
deploy key → canary; private halves in memory, then only in the host's
sops file). `fort add` renders a chart's workload tree from the
contract the chart ships and `fort seed` produces the Secrets
(docs/forts/fort-configuration-contracts.md — status section there). `fort dismantle` is the repo half
of decommissioning (#217) and `fort destroy` the side-effects half, gated
on the dismantle merge (docs/forts/destroy.md). The public-face extraction (github.com/streetfortress/forts)
splits the generic tool from repo-specific manifests/state — later, and
a publish step.
