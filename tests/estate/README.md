# tests/estate — a one-fort fixture estate

The smallest estate the framework's modules can evaluate against: one
plain fort, its manifest, the mesh peers file, the per-host sops rules,
the lab CA and a secrets file with the key names and no values. Nothing
here decrypts and nothing here is real.

`nixos/hosts/plain-fort/` is what `fort scaffold plain-fort --write`
renders into this tree; `TestFixtureEstateIsScaffoldOutput` keeps it so.
`flake.nix` evaluates it as `nixosConfigurations.plain-fort`, which is
how CI proves the module outputs compose into a host — the eval, not a
build.
