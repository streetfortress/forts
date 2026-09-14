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
fort scaffold web3                  # renders generated files → branch → PR
                                    # (merge — generated code is reviewed;
                                    #  build + inject the bootstrap image)
fort provision web3 --image <path>  # upload → pulumi up (you confirm) →
                                    # image deleted both sides
                                    # flux bootstraps and owns everything above
fort preview web3                   # read the diff first (day-2 edits);
                                    # --refresh = drift against the provider
fort provision web3                 # later day-2 applies (firewall/DNS edits)
just iac-drift                      # preview --refresh --expect-no-changes
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
the next session's spike. The libvirt backend is a stub until it reaches
create-nixos parity; lab VMs stay on `just create-nixos`. `fort
scaffold` renders the host nix file today; the identity steps (host key
→ age → sops → wg peer → flux cluster-init) remain on the playbook
until ported. `fort add` renders a chart's workload tree from the
contract the chart ships and `fort seed` produces the Secrets
(CONTRACTS.md — status section there). `fort dismantle` is the repo half
of decommissioning (#217); the pulumi/deploy-key half is still the manual
runbook in DECOMISSIONING.md until `fort destroy` lands. The public-face extraction (github.com/streetfortress/forts)
splits the generic tool from repo-specific inventory/state — later, and
a publish step.
