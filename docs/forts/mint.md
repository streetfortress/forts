# fort mint — the host's identity

```
fort mint <host> [--write]
```

`mint` gives a host everything that makes it *itself*: the keys other
systems will recognise it by, the sops rules that let it read its own
secrets, the mesh peer entry, and — for a flux cluster — the cluster's
age key, the deploy key gitea trusts, and the canary that proves the
whole chain works. It runs after [`scaffold --write`](scaffold.md) and
before [`add`](add.md); [`dismantle`](dismantle.md) is its exact inverse.

## The custody model

**The repo is the key store.** Every private half is generated in memory
and lands only in `nixos/secrets/<host>.yaml`, sops-encrypted to the admin
recipients and to the host itself. Nothing is written in the clear except
that file for the instant before sops encrypts it (mode 0600, removed if
encryption fails). No key ever exists as a file on the operator's
machine, so there is nothing to shred and nothing to lose; the admin age
key is the sole root of trust.

Consequences worth knowing:

- **A re-run restores, then fills gaps.** Mint first decrypts what the
  secrets file already holds and derives the public halves from it; it
  mints only the keys that are missing. So a re-mint after `dismantle` +
  `scaffold` gives a host back its identity, and a re-run can never mint an
  orphan key. Rotation is a deliberate act — remove the key from the sops
  file first — never a side effect.
- **The host's ssh key doubles as its age identity** (ssh-to-age): the
  recipient in the sops rule is derived from it, and the image inject step
  reads the same key back so the host boots already able to decrypt.
- **Nothing is ever printed but public material**: the host's age
  recipient, the cluster's, the wireguard public key.

## What it does, only if absent

| | artifact | when |
|---|---|---|
| ● | `nixos/secrets/<host>.yaml` — `k3s-token`, `wireguard-private-key`, `ssh-host-key` + `.pub`, `flux-sops-age-key`, `flux-git-deploy-key` | new file written whole and encrypted once; missing keys added with `sops set` |
| ○ | `nixos/.sops.yaml` — a `# BEGIN/END ANSIBLE sops rule: <host>` block for `secrets/<host>.yaml`, recipients = the shared admin set + the host | always |
| ○ | `wireguard/peers.json` — `.peers.<host>` with `wg_vpn_ip`, `public_key`, and `wg_endpoint_ip` (the inventory's, else `dns_name.dns_zone`) | `wg_vpn_ip` set |
| ○ | `.sops.yaml` — the fleet rule for `fleet/(clusters\|apps)/<host>/` (admin + the cluster key) and a `# cluster: <host>` recipient line in the shared `fleet/infrastructure` rule | `flux_cluster: true` |
| ◆ | `fleet/infrastructure/**/*.sops.yaml` — `sops updatekeys`, so the shared files gain the new recipient | that line was added |
| ◆ | `fleet/apps/<host>/sops-canary.yaml` — encrypted under the new rule | `flux_cluster: true` |
| · | gitea deploy key `flux-<host>`, read-only, on `sfi/deployments` — **the one call outside the tree** | `flux_cluster: true`; registered under `--write` only |

The marker blocks keep their historical `ANSIBLE` name: dismantle keys on
them, and renaming them is a separate, mechanical change.

**The shared admin set is read from the configs, not hardcoded**: the
recipient lines every existing host rule shares ("readable by whoever can
read every other host"); with a single host rule the catch-all rule joins
the intersection; with none — a fresh fork — the catch-all is the answer.
Adding or removing an admin is a hand edit of the existing rules and new
hosts follow.

## Guards

Mint refuses, before writing anything, when the repo already names an
identity that the secrets file does not derive:

- the nixos rule exists but its host recipient is not the one
  `ssh-host-key` derives;
- the fleet rule exists but lacks the recipient `flux-sops-age-key`
  derives;
- the peer exists with a different public key than
  `wireguard-private-key` derives;
- gitea holds a key titled `flux-<host>` that differs from
  `flux-git-deploy-key` — every key under that title must match (a
  timed-out POST can leave duplicates; matching duplicates are accepted
  and reported, `destroy` revokes them all).

Each of these means the real key is not in sops (a host whose key lived
elsewhere) or the rule is stale — either way a re-key is yours to choose,
not the tool's to paper over. The message names the import command.

## Output

```
mint sfi-x   (cloud: linode · flux cluster · mesh 10.1.0.55)
  host age recipient age1…
  cluster recipient  age1…
● nixos/secrets/sfi-x.yaml             k3s-token, wireguard-private-key, … — minted, sops-encrypted
○ nixos/.sops.yaml                     + rule secrets/sfi-x.yaml → 3 shared recipient(s) + host age1…
○ wireguard/peers.json                 + .peers.sfi-x (10.1.0.55, endpoint sfi-x.streetfortress.cloud)
○ .sops.yaml                           + rule fleet/(clusters|apps)/sfi-x/ → 1 shared + cluster age1…; + `# cluster: sfi-x` recipient …
◆ fleet/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml   sops updatekeys (gains the sfi-x recipient)
◆ fleet/apps/sfi-x/sops-canary.yaml    encrypted under the new rule
· gitea deploy key flux-sfi-x: would register (read-only)

dry run — nothing minted (mint --write).
```

A complete host reports `complete — identity restored from it` and the
deploy key `registered (id N, verified match)`: that is the acceptance
check after importing a key by hand.

## Needs

The admin age key (to encrypt and to read the existing file), and for a
flux cluster the `gitea-token` in `provisioning/secrets.yaml`. A lab-only
host needs no gitea credential. `jq` (the peers file is edited through it
so the formatting and key order stay byte-for-byte, as dismantle removes
it).
