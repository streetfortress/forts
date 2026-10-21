# tests/github-estate — a one-fort fixture estate on github.com

The second fixture estate (forts#1): the smallest estate whose forge is
a hosted one with a public name. `estate.toml` puts `repo` on
github.com and declares nothing else about the forge — no
`[hubs.forge]`, no mesh pin, no lab CA under `nixos/certs/`. That is
tier one of the two tiers of forge: the forge kind derives from the
host of `repo` (`github`), the deploy key goes through api.github.com
with `github-token`, and the host key every clone pins is github.com's
own. Nothing here decrypts and nothing here is real but that key.

The fort is a cloud cluster fort — a Linode with k3s and flux
(`forts/cloud-fort.toml`) — so the eval covers the two modules that
once defaulted to the lab forge's pin: `flux-bootstrap.nix` (the
git-auth secret's known_hosts) and `auto-rebuild.nix` (the rebuild
loop's clone). `tests/estate` is the other fixture: a plain lab fort
with a forge hub in another estate, tier two.

`nixos/hosts/cloud-fort/`, `flux/clusters/cloud-fort/` and
`flux/apps/cloud-fort/kustomization.yaml` are what `fort scaffold
cloud-fort --write` renders into this tree; the test
`TestGitHubFixtureEstateIsScaffoldOutput` keeps them so.
`flux/infrastructure/` and the two `.sops.yaml` rule files are what
`fort init` renders. The sops files — the host's secrets, the canary,
the console registry, the Alertmanager webhook — carry the key names
and opaque values, the shape sops leaves. `mesh/ssh_config` and
`mesh/known_hosts` are `fort ssh-config --write` with no age key.

`flake.nix` evaluates the host as `nixosConfigurations.cloud-fort` and
CI evaluates its toplevel derivation; `tests/check-github-estate.sh`
runs `fort check --all` on a copy of this tree, which must pass with no
error. Unlike `tests/estate`, this fixture is a whole estate, because
the gate builds the flux tree.
