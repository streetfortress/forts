# fort provision — the first provision and day-2 applies

```
fort provision <host> [--image <path>] [--yes]
fort preview  (<host> | --all) [--refresh] [--expect-no-changes]
fort adopt    <host> [--yes]
```

`provision` is `pulumi up -s <host>` with the credentials, backend,
image upload and seed handled for you, and nothing hidden: the stock CLI runs
with inherited stdio, you read the preview and type `yes`. The program
(`forts/pulumi`) realises the [manifest](manifest.md) through a per-provider
backend — `linode` for cloud forts, `libvirt` for the lab (soundship's
`images` pool) — and the stack is named after the host, so `pulumi
<anything> -s <host>` from `provisioning/pulumi` under the same environment is
always a first-class escape hatch. A `[baremetal]` host has no backend
and is refused here: its image is written by hand
([baremetal.md](baremetal.md)).

## The image is a provision input, not a resource

The host boots from the image `fort image` built: the plain build
output, no secret inside, on every variant (#659). Its ssh host key —
the age identity that decrypts the sops file [`mint`](mint.md) wrote —
arrives through the platform's channel at first boot: the Metadata
service, or a NoCloud `cidata` volume on the domain
([the seed](#the-seed-the-host-key-arrives-through-the-platforms-channel),
#606, #657). The host decrypts its secrets at its first activation.
That image is deliberately **not** a pulumi resource: pulumi owns
only the durable surface (instance, disks, boot config, firewall, DNS;
or volume + domain in the lab). `--image` runs the provision flow around the
apply:

0. **The stack.** A host with no checkpoint under
   `provisioning/pulumi/state/` gets `pulumi stack init` (passphrase secrets
   provider); `pulumi stack init` writes its `Pulumi.<host>.yaml`. Only
   the first provision creates a stack: without `--image` a stackless
   host is refused, so a typo can never put an empty stack on
   `preview --all`.
1. **Stage.** linode: upload the `.img.gz` through the Images API and
   wait until it is usable. libvirt: `qemu-img resize` the `.qcow2` to
   the manifest's `[shape] disk` (the provider refuses `size` next to
   `source`, so the growth happens before upload; the root fs
   auto-resizes on first boot) — the file's absolute path is the id.
2. **Record** the id as the stack's `forts:imageId` config — committed
   provenance of which image the host was provisioned from. The program
   reads it at creation only and ignores later changes.
3. **`pulumi up`.** You confirm.
4. **Gate** on the `ip` stack output — a declined preview exits 0, so
   the artifact is only released once the host demonstrably exists.
5. **Release.** linode: delete the provider-side image, then the local
   file. libvirt: the local file only — the uploaded volume *is* the
   root disk (a different disk is a re-provision, never an in-place change).
   This is hygiene, not custody: Linode bills a custom image for as long as it
   exists, the local copy is a few GiB, and the host needs neither once it has
   booted. The image holds no secret, so a copy that outlives the delete — the
   provider's own, or one left by a failed apply — gives an attacker nothing
   current (#605).

If the apply fails, the image stays staged and the message says so:
rerun **without** `--image` (the id is already set). A Linode image a
failed provision leaves behind costs money until a rerun or a hand delete
removes it (#608). Without `--image`, `provision` is the plain day-2
apply.

After any apply, `provisioning/pulumi` is staged (state; at the first
provision also `Pulumi.<host>.yaml`); commit it as its own PR. The
instance is protected (`protect: true` on every fort) from that point
until [`destroy`](destroy.md).

## Building the image

```
fort image <host>                              # nix build ./nixos#<host>-{qcow|linode|baremetal}
fort image <host> --from result/nixos.qcow2    # an image built by hand; skips the build
fort provision <host> --image ~/.cache/forts/<host>-nixos.qcow2   # or .img.gz
```

Any x86_64 Nix host with KVM builds it (devbox does; soundship needs no
shell); `fort image` needs `nix` on PATH and nothing else — it reads no
secret. The artifact is the plain build output on every variant:
Linode's caps are asserted and the file is copied (the lab copy is what
`provision` then grows to the manifest's disk; a baremetal `.img` is
what a human writes to the disk, [baremetal.md](baremetal.md)). The
flake picks the variant from the runtime module the host file imports, so
scaffold's choice of shape is the only switch. Details in
[`docs/nixos/operations.md`](../nixos/operations.md).

## The seed: the host key arrives through the platform's channel

This is the custody model of the first provision (#605, decided
2026-10-02). The sops file rides in the nix store, encrypted; the host
key that decrypts it never enters the image. Until #659 `fort image`
grafted the key in after the build, which made the image a copy of every
secret the fort holds, and the provider keeps copies of an image we
cannot delete (#605 has the facts). So **the image carries no secret,
the host key travels through a channel the platform provides, and one
NixOS module reads it before activation.** The repo stays the key store
([mint](mint.md)); the channel is the only road from it to the host. Two
channels exist: the Linode Metadata service (#606) and the lab's NoCloud
volume (#657). The baremetal channel — the same `cidata` layout on a USB
stick — is #658, not built; a baremetal provision waits for it
([baremetal.md](baremetal.md)).

**The seed** is the ed25519 host key as `fort mint` wrote it into
`nixos/secrets/<host>.yaml`: `ssh-host-key`, then `ssh-host-key.pub`, the
two files concatenated (`internal/seed`). Nothing else travels. The
image still carries the encrypted sops file, which is already in git.

**The channel** is `metadata.user_data` on the Linode instance, served by
the Metadata service to that instance alone. When a Linode apply of `fort
provision` creates the instance, it reads the pair from the sops file,
base64-encodes the seed and hands it to the pulumi program in
`FORT_SEED_USER_DATA`. The program (`cmd/fort-pulumi/linode.go`) sets it as
the instance's `metadata.user_data`, wrapped as a pulumi secret, and
ignores changes to it afterwards: Linode's metadata cannot change on a
running instance (the rebuild endpoint takes new metadata with new disks,
and a rebuild is a re-provision), the public API never returns `user_data`
(only `has_user_data`, so a refresh reads it back empty), and a re-minted
key must read as a re-provision, never as a diff on a running host.

**The lab channel** is a NoCloud `cidata` volume: the layout cloud-init's
NoCloud datasource defines — an ISO 9660 image labelled `cidata` with a
`user-data` and a `meta-data` file — borrowed for its reader, because no
cloud-init runs on a fort. When a libvirt apply of `fort provision`
creates the domain, the program (`cmd/fort-pulumi/libvirt.go`) declares the
provider's cloud-init disk resource: `<host>-seed.iso` in soundship's
`images` pool, `user_data` the seed text itself (no base64 on this
channel; the program decodes the environment value), `meta_data` the one
required line, `instance-id: <host>`. The domain attaches it through its
`cloudinit` input, which the provider renders as a CD-ROM — `hdd` on the
`ide` bus, the PIIX3 controller of the i440FX machine libvirt defaults
to. `user_data` is a pulumi secret and ignored after creation, as the
instance's `metadatas` are; the domain ignores `cloudinit` too. A rotation
of the host key is therefore a re-provision, never a diff on a running
domain. The volume stays in the pool for the domain's life — `destroy`
removes the domain, then it — and it holds the key in plain text, as the
root qcow2 beside it does: #605 accepts this trust in the hypervisor, and
this sentence records it. Two things follow from the bridged provider
(terraform-provider-libvirt 0.8.1): it writes the ISO by exec'ing `mkisofs`
on the machine that runs the apply, so `provision` checks for it before
pulumi starts — the `fort` package carries it (nixpkgs' `cdrtools`), a
checkout run without it stops and says so (as for `xsltproc`); and the ISO's
`user-data` is the resource's input, so the committed checkpoint holds it as
ciphertext, under [the state copy](#the-state-copy) below.

The provision also records `forts:seed` on the stack (`linode-metadata`
or `nocloud`): committed provenance of the channel the host was
provisioned through, next to `forts:imageId`. The libvirt program
declares the seed volume only when that key says `nocloud`, so a host
provisioned before the channel existed — the three adopted lab VMs, and
the dev fort provisioned from an image that carried the key — never sees
an empty ISO proposed on a day-2 apply. Those hosts get the channel when
they are re-provisioned.

Whether this apply creates the host is the stack's answer, not the
flags': `provision` runs `pulumi stack export` and looks for a resource
of the host's type — the Linode instance, the libvirt domain. None means
this apply creates one, so the seed is read; a missing or incomplete sops
file then stops the verb before the apply, because a provision without a seed
is a boot that stops in the initrd, and the console is a worse place to
learn that than the terminal. A host already in state means the sops
file is not touched and the key never reaches the environment of pulumi
and its plugins on a day-2 apply. The question is not "was `--image`
given": a retry of a failed provision runs without it and may be the run
that creates the host. It is also not "will this apply create a host",
because a replacement never succeeds on either backend — the root disk is
built from the uploaded image and the provision flow deleted it — so a region
change, a resize, or a host deleted behind pulumi's back all end in the
same place: destroy and provision, which reads the seed again. The read
needs the admin age key, as the token lookups in
[the environment](#environment) already do.

**The initrd placement** is `sfi-seed.service` in the systemd initrd
(`nixos/modules/seed.nix`, imported and enabled by
`modules/linode-runtime.nix` and `modules/qcow-runtime.nix`, so every
Linode and every lab fort has it; the channel follows `sfi.platform`). It
runs after the root is mounted on `/sysroot` and is required by
`initrd.target`, so before the switch to stage 2. Activation runs in the
initrd too on this nixpkgs — `initrd-nixos-activation.service`, after
`initrd-switch-root.target`, which is after `initrd.target` — so the
seed unit finishes before activation starts, and sops-nix finds the key
where it always was (the #656 journal below shows the quarter second
between them). A
`ConditionPathExists` skips the unit when the key is on the root, so a
host provisioned before the channel boots unchanged, and so does every later
boot of a seeded one. Otherwise the channel runs. On a Linode fort the
unit brings `eth0` up with one `udhcpc` lease, asks the service for a
token (`PUT http://169.254.169.254/v1/token`, the expiry in a header),
reads `GET /v1/user-data` with it, decodes the base64 and flushes the
link so stage 2's dhcpcd starts clean (IPv4 only; the service's IPv6
address would need SLAAC in the initrd). On a lab fort the unit waits up
to 30 s for `/dev/disk/by-label/cidata`, mounts it read-only as iso9660,
reads `/user-data` — the text itself — and unmounts it; the initrd carries
`isofs` (the iso9660 driver) and `sr_mod` loaded, and `ata_piix` and
`ahci` for udev to load from the controller's PCI id. Then the shared half writes
`/etc/ssh/ssh_host_ed25519_key` (0600) and `.pub` (0644) onto the root
and exits. Its output goes to the journal and the console
(`journal+console`), so Lish or `virsh console` shows every step and
`journalctl -b -u sfi-seed.service` shows it afterwards.

If the channel has no seed and the key is absent, the unit fails,
`initrd.target` fails with it, and systemd stops the boot in the initrd's
emergency target with the reader's message above the prompt: the root has
no key, sops-nix cannot decrypt, a fort that `fort provision` created
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
`ip route get 169.254.169.254` must answer `blackhole`.

**The NoCloud channel has no close step**, and `seed.nix` says so next
to the Linode close, so the pair is visible. A block device is root-only:
a pod or the unprivileged `agent` user cannot open `/dev/sr0`, and the
initrd unmounts the volume before stage 2. The Linode channel is open to
every process on the host until stage 2 blackholes it; this one was never
open to anyone but root.

**The state copy.** `user_data` is a resource input on both backends, so
the key now has a second home: the committed checkpoint under
`provisioning/pulumi/state/`, as a pulumi secret. The passphrase provider
encrypts it, and the passphrase is sops-managed in
`provisioning/secrets.yaml`, so the same admin keys gate this copy as
gate the sops file — a second encryption scheme on the same secret,
accepted in #605 and recorded here. `Pulumi.<host>.yaml` holds no seed:
it goes through the environment, not through stack config (`forts:seed`
names the channel, not the value). After the first provision, before the
state PR, confirm the checkpoint holds ciphertext and not the key:

```
f=provisioning/pulumi/state/.pulumi/stacks/forts/<host>.json
# linode: the instance's metadatas; libvirt: the cloud-init disk's userData
jq '.checkpoint.latest.resources[] | select(.type=="linode:index/instance:Instance") | .inputs.metadatas, .outputs.metadatas' "$f"
jq '.checkpoint.latest.resources[] | select(.type=="libvirt:index/cloudInitDisk:CloudInitDisk") | .inputs.userData, .outputs.userData' "$f"
# each userData is {"4dabf18193072939515e22adb298388d": "1b47061264138c4ac30d75fd1eb44270", "ciphertext": "v1:…"}
grep -c 'BEGIN OPENSSH' "$f"                                   # 0: no plaintext key
grep -c 'LS0tLS1CRUdJTiBPUEVOU1NIIFBSSVZBVEUgS0VZLS0tLS0K' "$f"  # 0: no base64 of it either
```

A rotation of the host key (`sops unset`, re-mint) therefore recreates
the instance — the metadata cannot change on a running one. That matches
[`mint`](mint.md)'s restore-not-rotate rule: a re-run restores the same
identity, and a rotation is a deliberate re-provision.

**What remains.** The baremetal stick (#658), and the re-provision of
production (#660) and of the lab forts provisioned before the channel, are
#605's open steps. The root disk and the recovery image (#607) still
hold every secret in plain text, and so does the seed volume in the lab
pool: the channel narrows the trust to the platform's metadata store
and the hypervisor, it does not remove it.

### Verification results from [linode-metadata-test](https://gitea.zen.lofi/sfi/deployments/pulls/656)

```
┌────────────────────────────────────────────────────────┬──────────────────────────────────────────────────────────────────────────────────┐
│                         fact                           │                               evidence on the host                               │
├────────────────────────────────────────────────────────┼──────────────────────────────────────────────────────────────────────────────────┤
│ Metadata service answers a host provisioned explicitly │ token received; reading /v1/user-data, then placed /etc/ssh/ssh_host_ed25519_key │
├────────────────────────────────────────────────────────┼──────────────────────────────────────────────────────────────────────────────────┤
│ DHCP lease alone routes the link-local address         │ no adding it on-link line in the journal                                         │
├────────────────────────────────────────────────────────┼──────────────────────────────────────────────────────────────────────────────────┤
│ user-data comes back base64 as stored                  │ the decoded seed parsed as a valid key and sops imported it                      │
├────────────────────────────────────────────────────────┼──────────────────────────────────────────────────────────────────────────────────┤
│ channel closed in stage 2                              │ both blackhole routes present, a PUT to the token endpoint fails at connect      │
└────────────────────────────────────────────────────────┴──────────────────────────────────────────────────────────────────────────────────┘
```
Placement and ordering. On this nixpkgs, activation runs in the initrd, not stage 2: the sops import appears under the initrd activation unit. That is fine, and it is ordered rather than lucky. The seed unit is required by initrd.target, the activation unit runs after the switch-root target, and that target comes after initrd.target. In the journal the seed finished a quarter second before activation started. The key landed with modes 0600 and 0644, sops imported it, and all four secrets are in place under /run/secrets. The whole read, from DHCP to key on disk, took under a second.

The state and the provider. The committed checkpoint holds the seed as ciphertext in both inputs and outputs, with no plaintext and no base64 of the key anywhere in the file. Linode reports the instance with user data set. No bootstrap image was left behind, so the release step did its job. The only private images on the account are the two automatic recovery images of web2 and db2, which expire on 2026-10-07.

### Verification results from [libvirt-nocloud-test](https://gitea.zen.lofi/sfi/deployments/pulls/664)

```
┌───────────────────────┬────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│         fact          │                                                         result on the host                                                         │
├───────────────────────┼────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ device and label      │ /dev/sr0, labelled, present before the unit started                                                                                │
├───────────────────────┼────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ modules               │ ata_piix virtio_blk libata sr_mod isofs cdrom, on a pc-i440fx-noble machine                                                        │
├───────────────────────┼────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ mount to key          │ 15 ms, 29 ms from the unit's first line                                                                                            │
├───────────────────────┼────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ mkisofs on macmini    │ not on PATH; built from nixpkgs for darwin by the tool fallback and used by the provider                                           │
├───────────────────────┼────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ volume and attachment │ libvirt-nocloud-test-seed.iso in the pool, attached as a read-only IDE CD-ROM with the cloudinit serial                            │
├───────────────────────┼────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┤
│ checkpoint            │ user data is ciphertext in inputs and outputs, no plaintext or base64 of the key anywhere, the stack config names the channel only │
└───────────────────────┴────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
```

The ordering matched the Linode host: the seed unit finished before the initrd activation unit, sops imported the key from there, the four secrets are in place, and the volume is not mounted in stage 2. k3s is Ready, the five flux pods run, and the mesh is up. The DNS coupling from #441 did not bite because a lab host reaches soundship over the LAN.

## preview

The read-only half: `pulumi preview -s <host> --diff`, same environment.

- Read a day-2 change (a firewall rule, a DNS record, a resize) here
  before the PR, and again from merged main before `provision`.
- `--refresh` reconciles state with the provider first: **drift**. The
  refresh block prints how the provider differs from the *checkpoint*'s
  recorded outputs — a fort provisioned unbooted with no disks shows its disks,
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

Brings a lab VM that predates the backend under it **without a
re-provision**: refresh the pool (older images were copied in behind
libvirt's back), import the live domain and volume into a new stack (`pulumi
import` — reads the hypervisor, writes local state only), preview the
program against them and refuse unless the only changes are the live
properties (`autostart`, `running`), then one `pulumi up` you confirm
converges those. The domain ignores its provision-time shape after creation
(disks, the seed CD-ROM, consoles, graphics, the driver XSLT, the provider's
defaults — each replace-forcing, none read back), while memory and vCPUs
stay live so a resize still shows as the replace it is. `forts:imageId`
records `adopted-<date>`, not an image, and no `forts:seed` is set: an
adopted host has no seed volume and gets one at its re-provision. Stage the
stack for its own PR (`<host>: adopted`).

All three playbook-era lab VMs were adopted this way; `provision --image`
creates a new lab fort, like a cloud one.

## What the backends realise

**linode** (`[linode]` `region` + `type`, `[dns]` `zone` + `name`): the
instance created unbooted, root + swap disks from the
uploaded image, the boot config (grub2, the two disks) which is what
boots it, a firewall (default-deny in, 80/443 + wireguard), the A record
and a wildcard `*.<dns.name>` in the zone. `fort destroy` powers the
instance off first: Linode refuses disk deletion on a running instance.

**libvirt** (`[libvirt]` `ip`, `[shape]` `vcpus` / `memory` / `disk`): a
qcow2 volume in soundship's `images` pool streamed from the bootstrap
image through the libvirt API (no root hop); for a host provisioned with
the channel, the seed volume `<host>-seed.iso` in the same pool ([the
seed](#the-seed-the-host-key-arrives-through-the-platforms-channel)); and
a domain in the lab's fixed shape — host-passthrough CPU, virtio file
disk on the volume's path (a pool-resolved `<disk type='volume'>` never
starts under Ubuntu's AppArmor), the seed volume as an IDE CD-ROM, `br0`,
serial + VNC, autostart. The driver type is set to qcow2 through the
provider's XSLT hook (`xsltproc` from nixpkgs when the host has none).
No firewall, no DNS: the bridge and the lab dnsmasq are the hypervisor's;
the host's own nftables is the only filter.

## When the bootstrap stalls

Two failure shapes from the bootstrap of sfi-stage-d95cc36 (2026-09-22, #408).
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
until kubectl apply -f /var/lib/rancher/k3s/server/manifests/ccm.yaml; do sleep 1; done
```

as deployer on a fort that enables ops/admin.nix: the first `kubectl`
on the terminal runs the admin-key challenge and mints the cluster-admin
credential, the loop rides it. Upstream: k3s-io/k3s#7328.

**`flux-bootstrap.service` failed: `UPGRADE FAILED: another operation
(install/upgrade/rollback) is in progress`.** A previous bootstrap run
died mid `helm install` (it shares k3s's fate on day 0) and left the
release record at `pending-install`; helm refuses everything while a
release is pending. The unit now clears any `pending-*` record before
installing and retries on failure a minute apart; by hand it is

```
kubectl -n flux-system delete secret sh.helm.release.v1.flux-operator.v1   # the admin rung (ops/admin.nix)
sudo systemctl restart flux-bootstrap                                      # the same rung: sudo behind the challenge
```

The chart's resources survive the delete and the fresh install adopts
them.

## Environment

Every verb here forces `PULUMI_BACKEND_URL` to this checkout's
`provisioning/pulumi/state` and `PULUMI_IGNORE_AMBIENT_PLUGINS=true` (the
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
