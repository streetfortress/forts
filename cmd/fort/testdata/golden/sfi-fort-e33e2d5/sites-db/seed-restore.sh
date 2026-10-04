#!/bin/sh
# Load the promoted symbology seed (sfi-seeds/symbology/latest.*, made by
# sfi-prod's seed-symbology job) into this cluster's EMPTY symbology
# database. Runs after the fetch container has staged the dump and its
# sidecar in /work.
#
# No dollar-brace anywhere: strict envsubst. Longhand only.
set -eu
db=symbology
dump=/work/latest.dump
meta=/work/latest.json

echo "== the seed"
cat "$meta"
echo

echo "== checksum"
want=$(sed -n 's/.*"sha256": "\([0-9a-f]*\)".*/\1/p' "$meta")
have=$(sha256sum "$dump" | cut -d' ' -f1)
test "$want" = "$have" || { echo "sha256 mismatch: sidecar $want, file $have"; exit 1; }
echo "   ok $have"

echo "== versions (the seed's vs this cluster's)"
echo "   seed:    $(sed -n 's/.*"postgres": "\([^"]*\)".*/\1/p' "$meta")"
echo "   cluster: $(psql -XAtc "select current_setting('server_version')")"

echo "== the target must be virgin"
n=$(psql -XAtd "$db" -c "select count(*) from pg_tables where schemaname not in ('pg_catalog','information_schema')")
echo "   user tables in $db: $n"
test "$n" = "0" || { echo "REFUSING: $db is not empty — this cluster already holds data. A lab fort repaves; it does not get loaded twice."; exit 1; }

# Extensions come from the sidecar, created here by the superuser before
# the restore, and their TOC entries are filtered out of the restore —
# CREATE EXTENSION in the archive would either fail as a non-superuser
# or collide with one the Database CR already made (vector).
echo "== extensions"
exts=$(sed -n 's/.*"extensions": "\([^"]*\)".*/\1/p' "$meta")
timescale=no
echo "$exts" | tr ',' '\n' | while read -r e; do
  name=$(echo "$e" | awk '{print $1}')
  [ -n "$name" ] || continue
  [ "$name" = plpgsql ] && continue
  echo "   create extension if not exists $name"
  psql -XAtd "$db" -c "create extension if not exists \"$name\""
done
if echo "$exts" | grep -q timescaledb; then timescale=yes; fi

pg_restore --list "$dump" | grep -v -E ' (EXTENSION|COMMENT - EXTENSION) ' > /work/toc.list
echo "   $(grep -c -v '^;' /work/toc.list) TOC entries to restore ($(pg_restore --list "$dump" | grep -c -E ' (EXTENSION|COMMENT - EXTENSION) ') extension entries filtered)"

if [ "$timescale" = yes ]; then
  echo "== timescaledb_pre_restore"
  psql -XAtd "$db" -c "select timescaledb_pre_restore()"
fi

echo "== pg_restore (as the superuser, objects owned by $db)"
pg_restore --no-owner --no-acl --exit-on-error --role="$db" \
  --use-list=/work/toc.list --dbname "$db" "$dump"

if [ "$timescale" = yes ]; then
  echo "== timescaledb_post_restore"
  psql -XAtd "$db" -c "select timescaledb_post_restore()"
fi

echo "== result: counts only"
psql -XAtd "$db" <<'SQL'
select 'tables          ' || count(*) from pg_tables where schemaname = 'public';
select 'alembic_version ' || version_num from alembic_version;
select 'auth.user       ' || count(*) || ' (stripped: expect 0)' from auth."user";
select 'watchlist       ' || count(*) || ' (stripped: expect 0)' from watchlist;
SQL
rm -f "$dump"
echo "== done"
