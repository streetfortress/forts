<p align="center">
  <img src="readme-images/forts-banner.png" alt="Forts!" width="440" height="120">
</p>

<p align="center"><strong>A framework for bootstrapping GitOps environments, centered around nixOS and FluxCD</strong></p>

# forts

Use the `fort` cli to scaffold a git repository (called an _estate_) and lifecycle machines (called _forts_):

- An **Estate** is a git repository that declares one or more **forts**. 
- A **Fort** is a nixOS machine and its associated workloads.
- Forts in an estate sit on a wireguard **Mesh**. 

After scaffolding a **fort**, `fort image` builds a bootable disk image and `fort provision` uses **pulumi** to deploy it.

> The image is bootable wherever images are bootable. Currently the fort CLI supports the Libvirt and Linode APIs. Baremetal deployments work but are not yet automated.

Since everything about a fort is managed in the estate, the resulting **I.A.C.** surface is tiny:
- A Compute Instance and Disk
- A Firewall and Domain Name

Once deployed, the fort begins a continuous reconciliation against the estate repo:
- **It's "Just NixOS:"** System changes stem from `<estate>/nixos/hosts/<fort>/default.nix`. Configure a fort to your heart's content, even installing desktop environments and advanced dotfile configurations if you have them. Changes here are automatically pulled down and applied. nixOS revisions can easily _be rolled back_ if anything goes wrong.
- **It's "Just Kubernetes:"** Workload changes go in `<estate>/flux/apps/<fort>/...`. You can run almost any service on a fort. Changes here are automatically reconciled by Flux, and you get all of the familiar conveniences of kubernetes for configuration management.

**Forts is meant to get these environments up and running.** Once deployed, it's just "nixOS and kubernetes", and you can manage them just like you'd expect. The `fort` cli _takes care of the tedious manifest work_ that's required to build out a GitOps environment.

<div align="center">

[Getting Started](docs/README.md) | [Contributing](CONTRIBUTING.md) | [Security](SECURITY.md) | [SFI's AI Disclosure](https://github.com/streetfortress/ai-disclosure) | [MIT License](LICENSE)

</div>

---

## What's in this repo

This repository has three main parts:
- The nixOS flake: The host level configurations for the **Mesh**, **kubernetes**, and **ops**.
- The templates of manifests to render in an estate: boilerplate `.toml`, `.yaml`, `.nix` files
- The fort cli codebase: application logic for __templating__ and __secrets management__.

---

<p align="center">⛫ <em>Built on the Shoulders of Giants</em> ⛫ </p>
<p align="center"><em>Forts!</em> by <em><a href="https://streetfortress.com">Streetfortress Industries</a></em></p>

