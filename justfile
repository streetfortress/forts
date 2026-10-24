# Tag HEAD for release and push the tag. Accepts "1.2.3" or "v1.2.3" — any
# leading v is stripped before re-adding, so "vv1.2.3" can't happen. The final
# tag must match release.sh's preflight regex (vX.Y.Z, no prerelease/build).
# Fetches first (pruning tags deleted on the remote) and refuses to tag unless
# HEAD is exactly origin/main, so a stale checkout can't ship a release.
tag version:
  #!/usr/bin/env bash
  set -euo pipefail
  v="{{version}}"
  v="${v#v}"
  [[ "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] \
      || { echo "error: 'v$v' is not vX.Y.Z semver" >&2; exit 1; }
  git fetch origin --tags --prune --prune-tags
  [[ "$(git rev-parse HEAD)" == "$(git rev-parse origin/main)" ]] \
      || { echo "error: HEAD is not at origin/main — pull first" >&2; exit 1; }
  git tag "v$v" && git push origin "v$v"

