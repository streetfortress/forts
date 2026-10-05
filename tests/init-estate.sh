#!/usr/bin/env bash
# The exit test of fort init (deployments#385, step 5): the estate it
# creates, with one plain fort scaffolded in, evaluates against THIS
# checkout as its framework input. No build: the eval of the host's
# toplevel derivation is the proof the pieces compose.
#
#   tests/init-estate.sh            builds fort with nix, then runs
#   FORT=/path/to/fort tests/init-estate.sh
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
fort=${FORT:-$(nix build --no-link --print-out-paths "$repo#fort")/bin/fort}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cd "$tmp"
git init -q -b main
"$fort" init --name test-estate --repo ssh://git@gitea.zen.lofi:30022/sfi/test-estate \
  --admin age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpq0d5e6m --write
# The estate's own files init names: the lab CA. And what mint would
# write for the fort: its peer, its secrets file (names, no values).
cp "$repo/tests/estate/forts/plain-fort.toml" forts/
cp "$repo/tests/estate/nixos/certs/step-ca-root.pem" nixos/certs/
cp "$repo/tests/estate/nixos/secrets/plain-fort.yaml" nixos/secrets/
cp "$repo/tests/estate/mesh/peers.json" mesh/peers.json
"$fort" scaffold plain-fort --write
git add -A
git -c user.email=t@t -c user.name=t commit -qm "init + plain-fort"
echo "== lock against $repo"
nix flake lock ./nixos --override-input forts "path:$repo"
echo "== eval plain-fort"
nix eval --raw ./nixos#nixosConfigurations.plain-fort.config.system.build.toplevel.drvPath --override-input forts "path:$repo"
echo
echo "ok: an estate that fort init created evaluates against this checkout"
