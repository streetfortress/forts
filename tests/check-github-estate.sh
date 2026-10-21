#!/usr/bin/env bash
# The gate on the github.com fixture estate (tests/github-estate,
# forts#1): `fort check --all` on a tree whose repo is on github.com and
# that declares no hub — tier one of the two tiers of forge. An estate's
# own CI runs this gate against the framework it pins; here the
# framework runs it on the fixture, so a check that assumes a lab forge,
# a .lofi name or a mesh pin fails by name.
#
# The fixture is copied into a repo of its own: `fort` finds the estate
# root at the nearest .git, and in this checkout that is the framework.
# tests/estate is an eval fixture, not a gate fixture: it carries no flux
# tree and no ssh_config, and the gate reports both.
#
#   tests/check-github-estate.sh            builds fort with nix, then runs
#   FORT=/path/to/fort tests/check-github-estate.sh
set -euo pipefail
repo=$(cd "$(dirname "$0")/.." && pwd)
fort=${FORT:-$(nix build --no-link --print-out-paths "$repo#fort")/bin/fort}
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cp -R "$repo/tests/github-estate/." "$tmp/"
cd "$tmp"
git init -q -b main
git add -A
git -c user.email=t@t -c user.name=t commit -qm "fixture"
"$fort" check --all
echo "ok: the github.com fixture estate passes the gate"
