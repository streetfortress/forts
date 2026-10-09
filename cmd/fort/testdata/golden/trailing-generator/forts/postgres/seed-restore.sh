#!/usr/bin/env bash
# Loads the staging seed dump into an empty cluster. Stands in for the
# real script (deployments#455): this fixture only needs the
# configMapGenerator that carries it, after patches:.
set -euo pipefail
psql "$DATABASE_URL" -f /seed/seed.sql
