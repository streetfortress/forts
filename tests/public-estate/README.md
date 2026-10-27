# tests/public-estate — a one-fort fixture estate on the anonymous route

The third fixture estate (forts#167): the smallest estate that clones
its own repo with no credential. `estate.toml` puts `repo` on
github.com as an `https://` URL and declares nothing else about the
forge — no `known_hosts`, no `[hubs.forge]`, no mesh pin, no lab CA
under `nixos/certs/`. That is the anonymous route: the scheme of
`repo` derives `auth = "none"`, flux's source-controller and the
rebuild loop clone a public repo over https with TLS from the system
roots, `fort mint` mints and registers no deploy key and reads no
`github-token`, and `fort destroy` revokes nothing. Nothing here
decrypts and nothing here is real.

The fort is a cloud cluster fort — a Linode with k3s and flux
(`forts/public-fort.toml`) — so the eval covers the two modules that
branch on the route: `flux-bootstrap.nix` (no git-auth secret, no
deploy-key declaration) and `auto-rebuild.nix` (an https repoUrl, no
`GIT_SSH_COMMAND`, `nixos-pull --from` refused). `tests/github-estate`
is the same shape on the ssh route — the deploy key and github.com's
host key — and keeps that coverage; `tests/estate` is a plain lab fort
with a forge hub in another estate.

`nixos/hosts/public-fort/`, `flux/clusters/public-fort/` and
`flux/apps/public-fort/kustomization.yaml` are what `fort scaffold
public-fort` renders into this tree; the test
`TestPublicFixtureEstateIsScaffoldOutput` keeps them so, and reads off
them that the FluxInstance names no `pullSecret`.
`flux/infrastructure/` and the two `.sops.yaml` rule files are what
`fort init` renders. The sops files — the host's secrets (no
`flux-git-deploy-key`), the canary, the console registry, the
Alertmanager webhook — carry the key names and opaque values, the shape
sops leaves. `mesh/ssh_config` and `mesh/known_hosts` are `fort
ssh-config` with no age key.

`flake.nix` evaluates the host as `nixosConfigurations.public-fort` and
CI evaluates its toplevel derivation, then reads the rendered
`nixos-upgrade` unit and the `nixos-pull` verb off it;
`tests/check-fixture-estate.sh tests/public-estate` runs `fort check
--all` on a copy of this tree, which must pass with no error.
