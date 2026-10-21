# tests/estate — a two-fort fixture estate

The smallest estate the framework's modules can evaluate against: two
plain forts, their manifests, the mesh peers file, the per-host sops
rules, the lab CA and a secrets file per host with the key names and no
values. Nothing here decrypts and nothing here is real.

The two forts are the two plain shapes (forts#142). `plain-fort` is a
lab VM: `[libvirt]`, the qcow runtime, a static LAN block.
`plain-cloud-fort` is a linode: `[linode]`, the linode runtime, no LAN
block. Neither has a `[cluster]` table, so neither runs k3s or flux. The
pair is what proves the two tables of a manifest decide two things — the
provider table picks the runtime module, the `[cluster]` table picks the
cluster.

`estate.toml` names a forge hub (`[hubs.forge]`) in another estate, and
`mesh/peers.json` carries that host as `remotes.forge`. The pair is what
`modules/lab-dns.nix` derives the forge pin from (forts#26), so the eval
below proves the derivation. `tests/init-estate.sh` is the other half:
the estate `fort init` renders names no forge host, and that fort gets
no pin.

`estate.toml` also names one operator device (`operator_keys`, a
throwaway key): the framework ships no operator key, so a fort of an
estate that names none does not evaluate (forts#81,
`modules/ops/deployer.nix`).

`nixos/hosts/<host>/` is what `fort scaffold <host> --write` renders into
this tree; `TestFixtureEstateIsScaffoldOutput` keeps it so, and that test
also names the hosts this file lists — a fort added here must gain a
`nixosConfigurations` entry in `flake.nix` and an eval line in the nix
job of `.gitea/workflows/checks.yaml`. CI evaluates each host, which is
how it proves the module outputs compose into a host — the eval, not a
build.
