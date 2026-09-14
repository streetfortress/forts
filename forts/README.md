# forts — cloud provisioning (#42/#87)

You manage *forts* with this: one fort = one host = base image + flux
workloads, keyed by its inventory hostname everywhere (nix host dir,
pulumi stack, peers.json, sops). `cmd/fort` is the CLI; `pulumi/` is the
resource program it wraps; `internal/inventory` is the shared schema.

The provider-side surface is deliberately three things — **instance,
firewall, DNS** — because the WireGuard mesh owns machine-to-machine
traffic and flux owns everything above the machine (no managed services:
rustfs/valkey/cnpg ride the clusters). Full design discussion on #87.

## Shape

- **One stack per host, stack name == inventory hostname.** Blast radius
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
  Removing an inventory entry touches no state; destruction is only ever
  `pulumi destroy -s <host>`, and every resource of a `production: true`
  host carries `protect: true` so even that requires deliberate
  unprotecting first.

## The image is a birth input, not a resource

The bootstrap image carries the host's pre-seeded key and is single-use
by design (#42): built, injected, uploaded, consumed by instance
creation, then deleted on both sides. Modeling that as a pulumi resource
is wrong in both lifecycle directions:

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

```
fort scaffold web3 --write          # host nix files (cloud shape) + the flux
                                    # cluster dir + apps root kustomization
./provisioning/preseed-identity.sh web3   # keys, sops rules, deploy key, canary
                                    # → branch → PR (merge — generated code is
                                    #  reviewed; build + inject the bootstrap image)
fort provision web3 --image <path>  # linode: upload → pulumi up (you confirm)
                                    # → image deleted both sides; libvirt: the
                                    # qcow2 (grown to disk_size) streams into
                                    # soundship's pool as the root disk, local
                                    # copy deleted. flux owns everything above
fort preview web3                   # read the diff first (day-2 edits);
                                    # --refresh = drift against the provider
fort provision web3                 # later day-2 applies (firewall/DNS edits)
fort preview --all --refresh \
    --expect-no-changes             # fleet drift check (= just iac-drift)
fort add web3 oci://…/symbology \
    --version 0.1.5 [--write]       # render a chart's fort-side tree from
                                    # its contract (CONTRACTS.md)
fort seed web3 [--write]            # produce its sops Secrets (dependency
                                    # order derived) → PR
fort check --all                    # the merge gate: Secrets vs contracts
                                    # (CI runs it; no age key needed)
fort dismantle web3 [--write]       # retire: remove exactly what scaffold +
                                    # preseed generated, report the residue
                                    # → PR; then (DECOMISSIONING.md) destroy
```

Env: `LINODE_TOKEN` (shared with the pulumi provider) and the stack's
`PULUMI_CONFIG_PASSPHRASE` — taken from the environment when set, else
sops-decrypted from `provisioning/secrets.yaml` by every cloud command.
The libvirt backend needs neither: `LIBVIRT_DEFAULT_URI` defaults to
soundship over ssh with `~/.ssh/id_ed25519` in the URI (the provider
carries its own ssh client — no `~/.ssh/config`, no agent unless asked),
and `xsltproc` / `qemu-img` come from nixpkgs when the host has none.

## The lab backend (`provider: libvirt`)

The same surface `provisioning/vms/create-nixos.yaml`'s VM half defines,
as resources: a qcow2 volume in soundship's `images` pool and a domain
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
(`pulumi destroy` + provision), never an in-place change.

```
nix build ./nixos#lab1-qcow                          # KVM: devbox or soundship
./provisioning/inject-host-key.sh lab1 result/nixos.qcow2
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

## One-time setup

```
PULUMI_CONFIG_PASSPHRASE=... pulumi stack init web3 --secrets-provider passphrase
```

`fort` commands pin `PULUMI_BACKEND_URL` to this checkout's
`forts/pulumi/state`, so no `pulumi login` is needed for them — and a
stale global login (e.g. from a worktree) cannot redirect where state is
written. Running pulumi by hand still honours whatever `pulumi login`
last recorded: prefer `fort preview` / `fort provision`, or export
`PULUMI_BACKEND_URL` yourself.

## Status

Demonstration shape (#87 decision PR): everything compiles against the
real SDKs but nothing has run against an account — the #42 checkboxes
(image upload contract, boot-config quirks, end-to-end first boot) are
the next session's spike. The libvirt backend
reaches create-nixos's VM half (define + start from the injected qcow);
lab VMs opt in with `provider: libvirt`, the rest stay on `just
create-nixos`. `fort
scaffold` renders the host's generated surface (nix host dir in the lab
or cloud shape, the flux cluster dir, the apps root kustomization; the
flake discovers the image variant from the host dir's runtime import);
the identity steps (host key → age → sops → wg peer → deploy key →
canary) are `provisioning/preseed-identity.sh` until it becomes a verb. `fort add` renders a chart's workload tree from the
contract the chart ships and `fort seed` produces the Secrets
(CONTRACTS.md — status section there). `fort dismantle` is the repo half
of decommissioning (#217); the pulumi/deploy-key half is still the manual
runbook in DECOMISSIONING.md until `fort destroy` lands. The public-face extraction (github.com/streetfortress/forts)
splits the generic tool from repo-specific inventory/state — later, and
a publish step.
