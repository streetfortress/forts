# fort provision — birth and day-2 applies

```
fort provision <host> [--image <path>] [--yes]
fort preview  (<host> | --all) [--refresh] [--expect-no-changes]
fort adopt    <host> [--yes]
```

`provision` is `pulumi up -s <host>` with the credentials, backend and
image lifecycle handled for you, and nothing hidden: the stock CLI runs
with inherited stdio, you read the preview and type `yes`. The program
(`forts/pulumi`) realises the [manifest](manifest.md) through a per-provider
backend — `linode` for cloud forts, `libvirt` for the lab (soundship's
`images` pool) — and the stack is named after the host, so `pulumi
<anything> -s <host>` from `forts/pulumi` under the same environment is
always a first-class escape hatch. A `[baremetal]` host has no backend
and is refused here: its image is written by hand
([baremetal.md](baremetal.md)).

## The image is a birth input, not a resource

The host boots from a single-use image that already carries its ssh host
key (grafted in by `fort image` from the sops file [`mint`](mint.md)
wrote), so the host can decrypt its secrets on first
boot. That image is deliberately **not** a pulumi resource: pulumi owns
only the durable surface (instance, disks, boot config, firewall, DNS;
or volume + domain in the lab). `--image` runs the birth flow around the
apply:

0. **The stack.** A host with no checkpoint under
   `forts/pulumi/state/` gets `pulumi stack init` (passphrase secrets
   provider); its `Pulumi.<host>.yaml` is born here. Only a birth
   creates a stack: without `--image` a stackless host is refused, so a
   typo can never put an empty stack on `preview --all`.
1. **Stage.** linode: upload the `.img.gz` through the Images API and
   wait until it is usable. libvirt: `qemu-img resize` the `.qcow2` to
   the manifest's `[shape] disk` (the provider refuses `size` next to
   `source`, so the growth happens before upload; the root fs
   auto-resizes on first boot) — the file's absolute path is the id.
2. **Record** the id as the stack's `forts:imageId` config — committed
   provenance of which image the host was born from. The program reads
   it at creation only and ignores later changes.
3. **`pulumi up`.** You confirm.
4. **Gate** on the `ip` stack output — a declined preview exits 0, so
   the artifact is only released once the host demonstrably exists.
5. **Release.** linode: delete the provider-side image, then the local
   file. libvirt: the local file only — the uploaded volume *is* the
   root disk (a different disk is a repave, never an in-place change).

If the apply fails, the image stays where it was and the message says
so: rerun **without** `--image` (the id is already set). Without
`--image`, `provision` is the plain day-2 apply.

After any apply, `forts/pulumi` is staged (state; at birth also
`Pulumi.<host>.yaml`); commit it as its own PR. The instance is protected (`protect: true` on
every fort) from that point until [`destroy`](destroy.md).

## Building the image

```
fort image <host>                              # nix build ./nixos#<host>-{qcow|linode|baremetal} + the graft
fort image <host> --from result/nixos.qcow2    # an image built by hand; skips the build
fort provision <host> --image ~/.cache/forts/<host>-nixos.qcow2   # or .img.gz
```

Any x86_64 Nix host with KVM builds it (devbox does; soundship needs no
shell); `fort image` needs `nix` and `sops` on PATH and takes `guestfish`
from PATH or, failing that, from nixpkgs. The flake picks the variant from the runtime module the host file
imports, so scaffold's choice of shape is the only switch. Details in
[`docs/nixos/operations.md`](../nixos/operations.md).

## preview

The read-only half: `pulumi preview -s <host> --diff`, same environment.

- Read a day-2 change (a firewall rule, a DNS record, a resize) here
  before the PR, and again from merged main before `provision`.
- `--refresh` reconciles state with the provider first: **drift**. The
  refresh block prints how the provider differs from the *checkpoint*'s
  recorded outputs — a fort born unbooted with no disks shows its disks,
  boot config and `running` status there forever, because a preview never
  writes state. Only the program diff below it counts; a committed
  `pulumi refresh` quiets the block.
- `--expect-no-changes` turns any proposed change into a nonzero exit.
- `--all` walks every host with a provider, one line each:

  ```
  OK      luxor
  OK      sfi-prod-439bf72
  DRIFT   staging-k3s
  PENDING web3   destroy pending: state without a manifest — fort destroy web3
  ```

  `just iac-drift` = `--all --refresh --expect-no-changes`, the fleet
  drift check. `PENDING` is the in-between state after a dismantle merge.

## adopt

```
fort adopt <host>          # a [libvirt] manifest, no stack yet
```

Brings a lab VM that predates the backend under it **without a repave**:
refresh the pool (older images were copied in behind libvirt's back),
import the live domain and volume into a new stack (`pulumi import` —
reads the hypervisor, writes local state only), preview the program
against them and refuse unless the only changes are the live properties
(`autostart`, `running`), then one `pulumi up` you confirm converges
those. The domain ignores its birth-time shape after creation (disks,
consoles, graphics, the driver XSLT, the provider's defaults — each
replace-forcing, none read back), while memory and vCPUs stay live so a
resize still shows as the replace it is. `forts:imageId` records
`adopted-<date>`, not an image. Stage the stack for its own PR
(`<host>: adopted`).

All three playbook-era lab VMs were adopted this way; a new lab fort is
born through `provision --image` like a cloud one.

## What the backends realise

**linode** (`[linode]` `region` + `type`, `[dns]` `zone` + `name`): the
instance created unbooted, root + swap disks from the
uploaded image, the boot config (grub2, the two disks) which is what
boots it, a firewall (default-deny in, 80/443 + wireguard), the A record
and a wildcard `*.<dns.name>` in the zone. `fort destroy` powers the
instance off first: Linode refuses disk deletion on a running instance.

**libvirt** (`[libvirt]` `ip`, `[shape]` `vcpus` / `memory` / `disk`): a
qcow2 volume in soundship's `images` pool streamed from the injected
image through the libvirt API (no root hop), and a domain in the lab's
fixed shape — host-passthrough CPU, virtio file disk on the volume's
path (a pool-resolved `<disk type='volume'>` never starts under Ubuntu's
AppArmor), `br0`, serial + VNC, autostart. The driver type is set to
qcow2 through the provider's XSLT hook (`xsltproc` from nixpkgs when the
host has none). No firewall, no DNS: the bridge and the lab dnsmasq are
the hypervisor's; the host's own nftables is the only filter.

## When birth stalls

Two failure shapes from sfi-stage-d95cc36's birth (2026-09-22, #408).
Both now self-heal; this is what the journal looks like if they recur,
and the hand recovery each guard automates.

**k3s restarts every ~11 s, `cloud-controller-manager exited: … configmaps
"extension-apiserver-authentication" is forbidden: User
"k3s-cloud-controller-manager"`.** The embedded CCM started before the
last object of k3s's own `ccm.yaml` (the kube-system RoleBinding to
`extension-apiserver-authentication-reader`) was applied, its exit took
k3s down mid-apply, and every restart re-runs the race with the loser
fixed: `journalctl -u k3s | grep -c "Kind=Addon controller"` stays at 1
however many starts there are. `k3s-ccm-rbac-guard.service`
(k3s-server.nix) watches for the signature and re-applies the manifest
in an up-window; by hand it is

```
until sudo sfi-kubectl apply -f /var/lib/rancher/k3s/server/manifests/ccm.yaml; do sleep 1; done
```

from a fort that has the disposable-fort `sfi-kubectl` verb. Upstream:
k3s-io/k3s#7328.

**`flux-bootstrap.service` failed: `UPGRADE FAILED: another operation
(install/upgrade/rollback) is in progress`.** A previous bootstrap run
died mid `helm install` (it shares k3s's fate on day 0) and left the
release record at `pending-install`; helm refuses everything while a
release is pending. The unit now clears any `pending-*` record before
installing and retries on failure a minute apart; by hand it is

```
sudo sfi-kubectl -n flux-system delete secret sh.helm.release.v1.flux-operator.v1
sudo sfi-systemd-restart flux-bootstrap
```

The chart's resources survive the delete and the fresh install adopts
them.

## Environment

Every verb here forces `PULUMI_BACKEND_URL` to this checkout's
`forts/pulumi/state` and `PULUMI_IGNORE_AMBIENT_PLUGINS=true` (the
nixpkgs bundle's ambient providers mis-shape outputs against the pinned
SDK). `PULUMI_CONFIG_PASSPHRASE` and `LINODE_TOKEN` come from the
environment, else sops-decrypted from `provisioning/secrets.yaml`.
`LIBVIRT_DEFAULT_URI` defaults to soundship over ssh with
`~/.ssh/id_ed25519` in the URI — the provider carries its own ssh client
and reads neither `~/.ssh/config` nor an agent.

`--yes` passes pulumi's `--yes --non-interactive` through: the apply
runs unprompted (an agent, a script, CI). Without a TTY and without
`--yes`, provision refuses before staging anything — no half-run to
replay.
