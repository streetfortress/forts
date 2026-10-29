<p align="center">
  <img src="doc-images/ops-primer-banner.png" alt="Forts Operational Primer" height="160">
</p>

# Fort Ops - An Operational Primer

Forts was designed around a few strong operational practices - lessons from years of operating enterprise cloud and hyperscaler environments. They may not be what you're used to, so the core precepts are laid out below.

More specific documentation can be found in [the reference docs](reference/ops.md) or the codebase comments (AI Generated).

**Contents**

- [Forts are 'Cattle' Machines](#forts-are-cattle-machines)
- [Least Privilege Rules Apply](#least-privilege-rules-apply)
- [Ops is about Recovery - Not Repairs](#ops-is-about-recovery---not-repairs)
  - [Commit SHAs: The "version" of config sets](#commit-shas-the-version-of-config-sets)
  - [Rolling back a change in `nixos/`](#rolling-back-a-change-in-nixos)
  - [Rolling back a change in `flux/`](#rolling-back-a-change-in-flux)
  - [Pro Tip: Use Squash Style Merge in an Estate](#pro-tip-use-the-squash-style-merge-in-an-estate)
- [The Estate Mesh](#the-estate-mesh)
- [Secrets in an Estate](#secrets-in-an-estate)
  - [Rotating Secrets](#rotating-secrets)
- [Accessing A Fort](#accessing-a-fort)
- [Escalating to Admin](#escalating-to-admin)

## Forts are 'Cattle' Machines

Think of a fort as an ephemeral compute resource. It runs a declared configuration that you generate, identically, across estates. A fort's lifecycle is automation-friendly and consists of clearly encapsulated CLI actions, so you can spin them up and tear them down _very quickly_. The longest part of a fort deployment is the image build and upload steps, **not setting up boilerplate**.

## Least Privilege Rules Apply

There is _no_ public login path, _no_ normal privilege escalation to root, _no_ ability to imperatively make changes (read only kubeconfig).

In an estate, _All_ changes to the system go through source control and code review. This enables a few interesting techniques:
  - The entire configuration state of a deployment is captured in the git log.
  - Each configuration is a reference that can be reverted, cherry-picked, scripted as a drill, performance tested, and more.
  - Triage can live in the codebase as comments, documentation remains close to the code, not scattered across wikis etc.
  - A purely declarative infrastructure is easily understood by agents and can be extended with purpose-built monitoring, utility scripts, and runbook automation.

However, because _All_ changes must go through git, your abilities as an operator are rather limited. More on this below.

---

## Ops is about Recovery - Not Repairs

Forts is centered around NixOS and FluxCD: two systems that _continuously_ attempt to apply the declared state. If you try to change something manually, the forts machinery will _fight you_, automatically undoing your changes. Therefore if something goes wrong, your first and usually best/quickest option to resume service is to _roll back to a known good config_. Recovering the service first allows operators to defer triaging and root-causing until an opportune time (ideally within a day or two - not at 3 am on a random Sunday).

### Commit SHAs: The "version" of config sets

The forts machinery uses **git commits** in the estate repo to "version" configurations. Estate commit `abcd1234` describes _everything_ about that environment at that point in time - nixos, flux workloads. If the next commit `efgh5678` breaks something, your ops goal is usually to get the system back to `abcd1234` (Some application changes, like database schema migrations, can complicate this process. Test accordingly before changing an important environment).

### Rolling back a change in `nixos/`

<p align="center">
  <img src="doc-images/ops-nixos-rollback.jpeg" alt="Roll back a NixOS change with a built in ops command">
</p>

> NixOS keeps its history on disk. Generations are complete past states sitting locally, so nixos-rollback can choose one offline, with no source repo, no network, no build. 

It's possible for a nixos/ change to **sever the mesh**, and the way back from that situation is: out-of-band console access + `sudo nixos-rollback` (the menu lists each generation and the commit it was built from; pick the good one — mesh restored), then revert / fix main and `sudo nixos-pull`.

### Rolling back a change in `flux/`

<p align="center">
  <img src="doc-images/ops-k8s-rollback.jpeg" alt="Roll back a FluxCD change with a PR">
</p>

> Flux has no generation store — past cluster states exist only in git, so "roll back" means re-applying old manifests, which needs the source and an apply path. Flux's native rollback therefore is the revert commit.

A Flux change shouldn't ever _break the fort_, so the PR -> FluxCD reconciliation loop should always remain active. Therefore the fort ops module does not have a built-in `flux-rollback` command like it does for nixos. Recovery for a flux app is achieved through git operations.

### Pro Tip: Use the Squash Style Merge in an Estate

Sometimes a pull request is composed of several individual commits. When operating an estate, it's **unlikely** that you would want to roll back to a sub-commit of a pull request. Using the 'Squash' style merge results in a linear history of CI-passing configuration versions that greatly simplifies estate operations.

---

## The Estate Mesh

A fort only exposes its **Wireguard Mesh** port(s) by default. Enabling `[cluster]` in `forts/<fort>.toml` opens 80/443 as a part of the k3s server scaffold.

### There is no public facing "front door" on a fort

The _only_ way onto a fort is SSH _over the mesh_, or an out-of-band console session.

### Leverage the Estate mesh for fort-to-fort communication

One property of an _estate_ is its **Mesh**. Forts are addressable on the mesh network at `<fort>.<estate>.mesh`

**Expose mesh services as kubernetes `NodePorts`, and open the port _only on the mesh interface_**. This keeps the service channel private while maintaining the public face for HTTP/S.

---

## Secrets in an Estate

NixOS, and FluxCD both integrate cleanly with [SOPS](https://getsops.io), so forts makes use of it heavily.

- Each fort gets a unique AGE recipient (derived from the fort's host SSH key identity).
- Each fort has two primary SOPS channels:
  - `nixos/secrets/<fort>.yaml`: host-level secrets, decryptable by the fort's host AGE key (and the estate admins).
  - `flux/.../secrets.sops.yaml`: cluster secrets for workloads and infrastructure, decryptable by the fort's cluster AGE key (and the estate admins).
- A compromised workload cannot decrypt the fort's secrets with the cluster AGE key, a compromised host AGE key cannot decrypt a cluster secret
- Secrets are stored, encrypted-at-rest via SOPS, directly in the estate repository.

### Rotating Secrets

Because every secret's location is declared, secrets are a rotatable inventory — a rotation is a PR with automatic rollout, and "what do we rotate if X leaks" is a grep, not archaeology.

> Almost every secret in an estate is rotatable. The one exception is **a fort's identity secrets**. A fort's host key, wireguard key, etc are intrinsically tied to the lifecycle of that fort. If you need to change a fort's identity, provision a new one and tear down the old.

#### The Secrets that can be easily rotated:

1. Workload secret values (seeded or hand-set): change the value (sops set / edit), PR, merge — flux applies the new Secret.
2. Host-side secret values (sops-nix delivered): merge → pull loop → activation puts the new value on disk.
3. The console passphrase: the one the fort CLI owns end-to-end — `fort console-password <host> --rotate`
4. The recipient set (adding or removing an admin or other secret readers): `just updatekeys`. We've implemented this as a justfile recipe for now, it calls `sops updatekeys` on all sops-encrypted secrets in the estate.

---

## Accessing A Fort

In order to get onto a fort you must login by:

- SSH on the **Mesh**
- "Out of band" via the machine provider's **console interface**

### SSH over the Mesh

In `estate.toml`, add your SSH **public key** to `operator_keys`. Then, add your machine to the estate's `mesh/peers.json` `clients` section:

```
# mesh/peers.json
{ ...
  "clients": {
    "macmini": {
      "wg_vpn_ip": "<a valid mesh IP address>",
      "public_key": "<your wireguard public key>"
    },
  },
}
```

Commit `peers.json` and merge it to main in the estate. Forts will pick up the configuration automatically, and you can then SSH to them at their mesh addresses.

### Out of Band Console Access

If _the mesh ever breaks_ (wireguard fails, invalid system configuration (interface typo, etc)), you can still recover a fort.

Compute providers provide a **console interface** which requires no network connectivity on the fort. In order to login with this method, you must use a fort's **generated console password**, stored encrypted in `provisioning/console-passwords.yaml`.

```
fort console-password <fort> --show
virsh console <fort> # (deployer user & decrypted console password)
```

---

## Escalating to Admin

A fort's `deployer` user does not have root access by default. You can enable it in the fort's nix config:

```nix
# In <estate>/nixos/hosts/<fort>/default.nix

sfi.ops.admin.enable = true;
```

Once merged and applied on the fort:

- You can use `sudo` and `sudo -i` to escalate to root privileges
- `kubectl` will use the admin kubeconfig (for write and delete actions you cannot normally do as ops)

> By default, forts will _challenge_ you to prove possession of the estate's admin AGE key before allowing escalation. You can disable that for a "passwordless sudo" experience with `sfi.ops.admin.challenge.enable = false;`, or declare your own user management freely because **"it's just NixOS"**.

### The Challenge-Response Semantics

Instead of just broadly allowing root to anyone with access to the 'deployer' user, the forts flake provides a mechanism to **prove Admin AGE Key Possession** first.

The challenge integrates **natively** with the operating system's sudo mechanics as an PAM module.

Running `sudo` or `kubectl` for the first time as admin will invoke the challenge. An encrypted 'number used once' (nonce)  and instructions will be displayed.

1. **Decrypt** the nonce with the estate's admin AGE Key
2. **Paste** the decrypted nonce in the fort's terminal

A valid nonce is accepted and the terminal session behaves exactly as if you'd just authenticated with 'sudo ... types password ...'.

You can configure `sfi.ops.admin.challenge.{timeout,ttl}` (Default: 15minutes, 60s to complete challenge).

