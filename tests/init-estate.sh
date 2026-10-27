#!/usr/bin/env bash
# The exit test of fort init (deployments#385, step 5): the estate it
# creates, with one plain fort scaffolded in, evaluates against THIS
# checkout as its framework input. No build: the eval of the host's
# toplevel derivation is the proof the pieces compose.
#
# Once per clone route (forts#167): an ssh:// repo with the forge's host
# key, and an https:// repo with none — the anonymous route, where init
# must ask for no --known-hosts and the host must evaluate without one.
#
#   tests/init-estate.sh            builds fort with nix, then runs
#   FORT=/path/to/fort tests/init-estate.sh
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
fort=${FORT:-$(nix build --no-link --print-out-paths "$repo#fort")/bin/fort}

init_and_eval() {
  local route=$1; shift
  local tmp
  tmp=$(mktemp -d)
  # shellcheck disable=SC2064
  trap "rm -rf '$tmp'" RETURN
  cd "$tmp"
  git init -q -b main
  # --operator-key: the framework ships no operator key (forts#81), so the
  # estate names the devices that hold `deployer`. A fort of an estate that
  # names none does not evaluate, which is what the eval below would show.
  "$fort" init --name test-estate "$@" \
    --admin age1qyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpqyqszqgpq0d5e6m \
    --operator-key "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJDZXdVYO58sXRAwLieovSB3fHDAhMF96rjyfCx+vLuL test-operator" \
    --forts git+https://example.invalid/forts
  # The estate's own files init names: the lab CA. And what mint would
  # write for the fort: its peer, its secrets file (names, no values).
  cp "$repo/tests/estate/forts/plain-fort.toml" forts/
  cp "$repo/tests/estate/nixos/certs/ca-root.pem" nixos/certs/
  cp "$repo/tests/estate/nixos/secrets/plain-fort.yaml" nixos/secrets/
  cp "$repo/tests/estate/mesh/peers.json" mesh/peers.json
  "$fort" scaffold plain-fort
  git add -A
  git -c user.email=t@t -c user.name=t commit -qm "init + plain-fort"
  echo "== lock against $repo ($route route)"
  nix flake lock ./nixos --override-input forts "path:$repo"
  echo "== eval plain-fort ($route route)"
  nix eval --raw ./nixos#nixosConfigurations.plain-fort.config.system.build.toplevel.drvPath --override-input forts "path:$repo"
  echo
  echo "== the route the host evaluates with: $(nix eval --raw ./nixos#nixosConfigurations.plain-fort.config.sfi.estate.authRoute --override-input forts "path:$repo")"
}

init_and_eval ssh --repo ssh://git@gitea.zen.lofi:30022/sfi/test-estate \
  --known-hosts "[gitea.zen.lofi]:30022 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJDZXdVYO58sXRAwLieovSB3fHDAhMF96rjyfCx+vLuL"
# The anonymous route: no --known-hosts, and no fort waits for one.
init_and_eval anonymous --repo https://github.com/acme/test-estate
echo "ok: an estate that fort init created evaluates against this checkout, on both routes"
