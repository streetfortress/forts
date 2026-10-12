# tests/estate — a one-fort fixture estate

The smallest estate the framework's modules can evaluate against: one
plain fort, its manifest, the mesh peers file, the per-host sops rules,
the lab CA and a secrets file with the key names and no values. Nothing
here decrypts and nothing here is real.

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

`nixos/hosts/plain-fort/` is what `fort scaffold plain-fort --write`
renders into this tree; `TestFixtureEstateIsScaffoldOutput` keeps it so.
`flake.nix` evaluates it as `nixosConfigurations.plain-fort`, which is
how CI proves the module outputs compose into a host — the eval, not a
build.
