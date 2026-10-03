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

The host boots from a single-use image. For a lab or baremetal host the
image already carries its ssh host key (grafted in by `fort image` from
the sops file [`mint`](mint.md) wrote); for a Linode host the image
carries no secret and the key arrives through the Metadata service at
first boot ([the seed](#the-seed-the-host-key-arrives-through-the-platforms-channel),
#606). Either way the host decrypts its secrets at its first activation.
That image is deliberately **not** a pulumi resource: pulumi owns
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
fort image <host>                              # nix build ./nixos#<host>-{qcow|linode|baremetal}; + the graft for qcow and baremetal
fort image <host> --from result/nixos.qcow2    # an image built by hand; skips the build
fort provision <host> --image ~/.cache/forts/<host>-nixos.qcow2   # or .img.gz
```

Any x86_64 Nix host with KVM builds it (devbox does; soundship needs no
shell); `fort image` needs `nix` on PATH, and for a grafted variant (qcow,
baremetal) also `sops`, plus `guestfish` from PATH or, failing that, from
nixpkgs. A Linode host's artifact is the plain build output: no graft, no
sops, no guestfish — the caps are asserted and the file is copied. The
flake picks the variant from the runtime module the host file imports, so
scaffold's choice of shape is the only switch. Details in
[`docs/nixos/operations.md`](../nixos/operations.md).

## The seed: the host key arrives through the platform's channel

The bootstrap image used to be a copy of every secret the fort holds: the
sops file rides in the nix store, and the grafted host key decrypts it.
The provider keeps copies of that image we cannot delete (#605 has the
facts). The decision there (2026-10-02) is **option 2 for every
platform**: the image carries no secret, the host key travels through a
channel the platform provides, and one NixOS module reads it before
activation. This section is the Linode channel, the first one built
(#606). The libvirt and baremetal channels (a NoCloud `cidata` volume)
follow in #605's order; until they land, `fort image` grafts for those
two variants exactly as before.

**The seed** is the ed25519 host key as `fort mint` wrote it into
`nixos/secrets/<host>.yaml`: `ssh-host-key`, then `ssh-host-key.pub`, the
two files concatenated (`forts/internal/seed`). Nothing else travels. The
image still carries the encrypted sops file, which is already in git.

**The channel** is `metadata.user_data` on the Linode instance, served by
the Metadata service to that instance alone. When a Linode apply of `fort
provision` creates the instance, it reads the pair from the sops file,
base64-encodes the seed and hands it to the pulumi program in
`FORT_SEED_USER_DATA`. The program (`forts/pulumi/linode.go`) sets it as
the instance's `metadata.user_data`, wrapped as a pulumi secret, and
ignores changes to it afterwards: Linode's metadata cannot change on a
running instance (the rebuild endpoint takes new metadata with new disks,
and a rebuild is a repave), the public API never returns `user_data`
(only `has_user_data`, so a refresh reads it back empty), and a re-minted
key must read as a repave, never as a diff on a running host.

Whether this apply creates the instance is the stack's answer, not the
flags': `provision` runs `pulumi stack export` and looks for a resource
of the instance type. None means this apply creates one, so the seed is
read; a missing or incomplete sops file then stops the verb before the
apply, because a birth without a seed is a boot that stops in the initrd,
and Lish is a worse place to learn that than the terminal. An instance
already in state means the sops file is not touched and the key never
reaches the environment of pulumi and its plugins on a day-2 apply. The
question is not "was `--image` given": a retry of a failed birth runs
without it and may be the run that creates the instance. It is also not
"will this apply create an instance", because a replacement never
succeeds on this backend — the root disk is built from the uploaded image
and the birth flow deleted it — so a region change or an instance deleted
in the Linode console both end in the same place: destroy and provision,
which reads the seed again. The read needs the admin age key, as the
token lookups in [the environment](#environment) already do.

**The initrd placement** is `sfi-seed.service` in the systemd initrd
(`nixos/modules/seed.nix`, imported and enabled by
`modules/linode-runtime.nix`, so every Linode fort has it). It runs after
the root is mounted on `/sysroot` and is required by `initrd.target`, so
before the switch to stage 2; stage 2 then runs activation and sops-nix
finds the key where it always was. A `ConditionPathExists` skips the unit
when the key is on the root, so a host born from a grafted image boots
unchanged. Otherwise the unit brings `eth0` up with one `udhcpc` lease,
asks the service for a token (`PUT http://169.254.169.254/v1/token`, the
expiry in a header), reads `GET /v1/user-data` with it, decodes the
base64, writes `/etc/ssh/ssh_host_ed25519_key` (0600) and `.pub` (0644)
onto the root, flushes the link so stage 2's dhcpcd starts clean, and
exits. IPv4 only; the service's IPv6 address would need SLAAC in the
initrd. Its output goes to the journal and the console
(`journal+console`), so Lish shows every step and `journalctl -b -u
sfi-seed.service` shows it afterwards.

If the channel has no seed and the key is absent, the unit fails,
`initrd.target` fails with it, and systemd stops the boot in the initrd's
emergency target with the reader's message above the prompt: the root has
no key, sops-nix cannot decrypt, a fort born through `fort provision`
gets its key from this channel, place it by hand from a rescue boot or
destroy and provision again. Emergency access in the initrd is off
(`boot.initrd.systemd.emergencyAccess`), so the prompt refuses a login;
the message is the whole point.

**The channel closes in stage 2.** The seed stays readable from inside the
instance for its life: Linode has no call to disable the Metadata service,
`user_data` cannot be cleared on a running instance, and the service
answers any request from the instance's network — the token step stops a
forged request from outside, not code that runs on the host. Without a
block, a pod, or the unprivileged `agent` user on a dev fort, could ask
for a token and read the host key, which today is a root-only file and
decrypts the whole sops file (and so the fleet-wide entries of #609).
Root does not need the endpoint after the initrd. So `sfi-seed-close.service`
(the same `linode-metadata` block of `seed.nix`: the channel opens and
closes in one place) installs a blackhole route to `169.254.169.254/32`
and `fd00:a9fe:a9fe::1/128` before `network-pre.target`, as the firewall
is ordered. A route rather than a firewall rule because forwarded pod
traffic uses the host's routing table too, and a CNI that rewrites the
filter chains cannot open it; metric 0 beats any route a DHCP lease could
push for the same prefix. It is a unit and not
`networking.interfaces.eth0.ipv4.routes` because that option excludes
`blackhole` (its description says why: not a per-device type). The initrd
read is unaffected — the route exists in stage 2 only. On a live host
`ip route get 169.254.169.254` must answer `blackhole`. The NoCloud
channels that follow need no close step: a block device is root-only.

**The state copy.** `user_data` is a resource input, so the key now has a
second home: the committed checkpoint under `forts/pulumi/state/`, as a
pulumi secret. The passphrase provider encrypts it, and the passphrase is
sops-managed in `provisioning/secrets.yaml`, so the same admin keys gate
this copy as gate the sops file — a second encryption scheme on the same
secret, accepted in #605 and recorded here. `Pulumi.<host>.yaml` holds no
seed: it goes through the environment, not through stack config. After a
birth, before the state PR, confirm the checkpoint holds ciphertext and
not the key:

```
f=forts/pulumi/state/.pulumi/stacks/forts/<host>.json
jq '.checkpoint.latest.resources[] | select(.type=="linode:index/instance:Instance") | .inputs.metadatas, .outputs.metadatas' "$f"
# each userData is {"4dabf18193072939515e22adb298388d": "1b47061264138c4ac30d75fd1eb44270", "ciphertext": "v1:…"}
grep -c 'BEGIN OPENSSH' "$f"                                   # 0: no plaintext key
grep -c 'LS0tLS1CRUdJTiBPUEVOU1NIIFBSSVZBVEUgS0VZLS0tLS0K' "$f"  # 0: no base64 of it either
```

A rotation of the host key (`sops unset`, re-mint) therefore recreates
the instance — the metadata cannot change on a running one. That matches
[`mint`](mint.md)'s restore-not-rotate rule: a re-run restores the same
identity, and a rotation is a deliberate repave.

**What this does not change yet.** `provision --image` still runs the
single-use lifecycle (stage, record, release) and still deletes the
Linode image once the host exists; for a Linode host that delete is now
hygiene, not custody. Removing the lifecycle, the graft for the other two
variants, and the repave of production are #605's steps 2–4. The root
disk and the recovery image (#607) still hold every secret in plain text.

**Four facts to record on the test host.** This section was written with
the module built and inspected (the production host's initrd carries the
unit, `curl`, `udhcpc` and the modules) but before a Linode birth ran it.
A throwaway `[linode]` manifest proves it: scaffold, mint, `fort image`,
`fort provision --image`, then read `journalctl -b -u sfi-seed.service`
and `ip route get 169.254.169.254` over the mesh (or watch Lish), then
`fort destroy` and delete the recovery image (#607). Replace the answer
cells below with what the host says.

| fact | how the host answers it | answer |
|---|---|---|
| Does the Metadata service answer a host born the explicit way (empty instance, disks, GRUB2 config profile, Network Helper off)? | `sfi-seed: token received; reading /v1/user-data` followed by `sfi-seed: placed /etc/ssh/ssh_host_ed25519_key` | *not yet run* |
| Does the initrd reach the link-local address with the DHCP lease alone? | no `adding it on-link` line: the lease's routes sufficed. With it: the unit needed the `/32` on `eth0`, which it adds itself | *not yet run* |
| Does `/v1/user-data` return the value base64-encoded, as stored? | the reader decodes the body, as cloud-init's Akamai datasource does. `placed …` means yes. `the seed is not an OpenSSH private key` with user_data set means the service returned it decoded, and the `base64 -d` step must go | *not yet run* |
| Is the channel closed in stage 2? | `ip route get 169.254.169.254` on the live host prints `blackhole 169.254.169.254` | *not yet run* |

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
