#!/usr/bin/env bash
# The gate on a fixture estate: `fort check --all` on a tree copied into
# a repo of its own. An estate's own CI runs this gate against the
# framework it pins; here the framework runs it on its fixtures, so a
# check that assumes a lab forge, a .lofi name, a mesh pin or a deploy
# key fails by name.
#
#   tests/github-estate   the repo on github.com, no hub — tier one of the
#                         two tiers of forge (forts#1), on the ssh route
#   tests/public-estate   the repo public on github.com, cloned anonymously
#                         over https — the anonymous route (forts#167): no
#                         deploy key, no host key, no forge token
#
# The fixture is copied into a repo of its own: `fort` finds the estate
# root at the nearest .git, and in this checkout that is the framework.
# tests/estate is an eval fixture, not a gate fixture: it carries no flux
# tree and no ssh_config, and the gate reports both.
#
#   tests/check-fixture-estate.sh tests/github-estate      builds fort with nix, then runs
#   FORT=/path/to/fort tests/check-fixture-estate.sh tests/public-estate
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
fixture=${1:?usage: tests/check-fixture-estate.sh <fixture dir>}
fixture=$(cd "$fixture" && pwd)
fort=${FORT:-$(nix build --no-link --print-out-paths "$repo#fort")/bin/fort}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cp -R "$fixture/." "$tmp/"
cd "$tmp"
git init -q -b main
git add -A
git -c user.email=t@t -c user.name=t commit -qm "fixture"
"$fort" check --all
echo "ok: $(basename "$fixture") passes the gate"
