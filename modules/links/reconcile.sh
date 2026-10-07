# The reconciler of one link (modules/links.nix): keep the link
# interface's peer set equal to the other estate's mesh/peers.json, and
# the link's zone file equal to its names (forts#45). Env:
# LINK_NAME LINK_IFACE LINK_ZONE LINK_REPO LINK_REF LINK_ADDRESS LINK_PORT
# LINK_SSH_KEY (may be empty) LINK_KNOWN_HOSTS (a file) STATE_DIRECTORY.
set -euo pipefail

state=/var/lib/wg-link-$LINK_NAME
cache=$state/peers.json
zonefile=$state/zone.db
textfile=/var/lib/node-exporter-textfiles
now=$(date +%s)
fetched=0

metrics() {
  # $1 = peers applied (or 0). Atomic write; the exporter is unprivileged.
  tmp=$(mktemp "$textfile/.wg-link-$LINK_NAME.prom.XXXXXX")
  {
    echo "# TYPE sfi_link_fetch_success gauge"
    echo "sfi_link_fetch_success{link=\"$LINK_NAME\",interface=\"$LINK_IFACE\"} $fetched"
    echo "# TYPE sfi_link_last_run_seconds gauge"
    echo "sfi_link_last_run_seconds{link=\"$LINK_NAME\",interface=\"$LINK_IFACE\"} $now"
    echo "# TYPE sfi_link_peers gauge"
    echo "sfi_link_peers{link=\"$LINK_NAME\",interface=\"$LINK_IFACE\"} $1"
  } > "$tmp"
  chmod 0644 "$tmp"
  mv "$tmp" "$textfile/wg-link-$LINK_NAME.prom"
}

# 1. Fetch the other estate's peers.json; on failure, the last good copy.
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
if [ -n "${LINK_SSH_KEY:-}" ]; then
  export GIT_SSH_COMMAND="ssh -i $LINK_SSH_KEY -o UserKnownHostsFile=$LINK_KNOWN_HOSTS -o IdentitiesOnly=yes -o BatchMode=yes"
fi
if git clone -q --depth 1 --branch "$LINK_REF" "$LINK_REPO" "$work/repo" 2>"$work/clone.err" \
   && jq -e '.peers | type == "object"' "$work/repo/mesh/peers.json" >/dev/null 2>&1; then
  install -m 0644 "$work/repo/mesh/peers.json" "$cache"
  # estate.toml's `interface`, the name every host gives that mesh, and
  # its zone — `zone`, else <name>.mesh (forts#45). Three top-level TOML
  # keys, so a line match is enough.
  toml_key() { sed -nE 's/^[[:space:]]*'"$1"'[[:space:]]*=[[:space:]]*"([^"]+)".*/\1/p' "$work/repo/estate.toml" 2>/dev/null | head -1 || true; }
  theirs=$(toml_key interface)
  echo "${theirs:-}" > "$state/interface"
  theirzone=$(toml_key zone)
  if [ -z "$theirzone" ]; then
    theirname=$(toml_key name)
    [ -n "$theirname" ] && theirzone="$theirname.mesh"
  fi
  echo "${theirzone:-}" > "$state/zone"
  fetched=1
else
  echo "fetch of $LINK_REPO ($LINK_REF) failed: $(tr '\n' ' ' < "$work/clone.err" 2>/dev/null || true)" >&2
  if [ ! -s "$cache" ]; then
    echo "no cached peers.json either: the link stays empty until the first fetch succeeds" >&2
    metrics 0
    exit 1
  fi
  echo "using the cached copy from $(date -r "$cache" -u +%FT%TZ)" >&2
fi

# 2. One name per mesh: when the other estate names its interface, this
#    link is called the same. (An estate on the default wg0 names none;
#    a link cannot be wg0 and is not checked.)
theirs=$(cat "$state/interface" 2>/dev/null || true)
if [ -n "$theirs" ] && [ "$theirs" != "$LINK_IFACE" ]; then
  echo "estate.toml of $LINK_REPO names its mesh interface $theirs; this link is $LINK_IFACE — one name per mesh, fix one side, applying nothing" >&2
  metrics 0
  exit 1
fi

#    The same for the zone: the names of that mesh are <host>.<zone>
#    on every host on it, this one included (links.nix serves them).
theirzone=$(cat "$state/zone" 2>/dev/null || true)
if [ -n "$theirzone" ] && [ "$theirzone" != "$LINK_ZONE" ]; then
  echo "estate.toml of $LINK_REPO makes its zone $theirzone; this link is $LINK_ZONE — one zone per mesh, fix one side, applying nothing" >&2
  metrics 0
  exit 1
fi

# 3. This host's own entry in the other estate's remotes: it must exist
#    (the forts dial what it says), and it must agree with the link.
mypub=$(wg show "$LINK_IFACE" public-key)
me=$(jq -r --arg k "$mypub" '(.remotes // {}) | to_entries[] | select(.value.public_key == $k) | "\(.key) \(.value.wg_vpn_ip) \(.value.wg_endpoint_port // 51820)"' "$cache")
if [ -z "$me" ]; then
  echo "this host's key ($mypub) is in no remotes entry of $LINK_REPO — the other estate has not added this link yet (docs/links.md)" >&2
  metrics 0
  exit 1
fi
read -r myname myip myport <<< "$me"
if [ "$myip" != "$LINK_ADDRESS" ] || [ "$myport" != "$LINK_PORT" ]; then
  echo "remotes.$myname in $LINK_REPO says $myip:$myport; this link is configured as $LINK_ADDRESS:$LINK_PORT — fix one side, applying nothing" >&2
  metrics 0
  exit 1
fi

# 4. Desired peers: every peer of that estate (keepalive, endpoint where
#    stable), every roaming client (neither), every other linked host
#    (endpoint + port, keepalive). Exactly what a fort there would carry.
desired=$(jq -r --arg k "$mypub" '
  def line(kind): to_entries[] | select(.value.public_key != $k)
    | "\(.value.public_key) \(.value.wg_vpn_ip) \(.value.wg_endpoint_ip // "-") \(.value.wg_endpoint_port // 51820) \(kind)";
  ((.peers // {}) | line("peer")),
  ((.clients // {}) | line("client")),
  ((.remotes // {}) | line("remote"))' "$cache")

declare -A keep=()
n=0
while read -r pub ip ep port kind; do
  [ -n "$pub" ] || continue
  keep[$pub]=1
  n=$((n + 1))
  case $kind in
    client) wg set "$LINK_IFACE" peer "$pub" allowed-ips "$ip/32" ;;
    *)
      if [ "$ep" != "-" ]; then
        wg set "$LINK_IFACE" peer "$pub" allowed-ips "$ip/32" endpoint "$ep:$port" persistent-keepalive 25
      else
        wg set "$LINK_IFACE" peer "$pub" allowed-ips "$ip/32" persistent-keepalive 25
      fi ;;
  esac
done <<< "$desired"

# 5. Remove what the file no longer names.
for pub in $(wg show "$LINK_IFACE" peers); do
  if [ -z "${keep[$pub]:-}" ]; then
    echo "removing peer $pub (no longer in $LINK_REPO)" >&2
    wg set "$LINK_IFACE" peer "$pub" remove
  fi
done

# 6. The zone: every peer and remote of that estate as <host>.<zone> at
#    its address on that mesh, <host> and *.<host> each, the shape
#    mesh-dns.nix renders for a fort's own mesh. The local CoreDNS reads
#    this file (links.nix) and reloads it when the serial changes; the
#    serial is the fetch time, so a run that changes nothing changes
#    nothing. Written whole and renamed into place.
records=$(jq -r '((.peers // {}) + (.remotes // {})) | to_entries | sort_by(.key)[] | "\(.key) IN A \(.value.wg_vpn_ip)\n*.\(.key) IN A \(.value.wg_vpn_ip)"' "$cache")
if [ ! -s "$zonefile" ] || [ "$(sed -n '5,$p' "$zonefile")" != "$records" ]; then
  tmp=$(mktemp "$state/.zone.db.XXXXXX")
  {
    echo "\$ORIGIN $LINK_ZONE."
    echo "\$TTL 60"
    echo "@ IN SOA $myname.$LINK_ZONE. hostmaster.$LINK_ZONE. $now 3600 600 86400 60"
    echo "@ IN NS $myname.$LINK_ZONE."
    echo "$records"
  } > "$tmp"
  chmod 0644 "$tmp"
  mv "$tmp" "$zonefile"
fi
hosts=$(jq -r '((.peers // {}) + (.remotes // {})) | length' "$cache")

metrics "$n"
echo "$LINK_IFACE: $n peer(s) from $LINK_REPO ($LINK_REF), fetched=$fetched, as remotes.$myname $myip:$myport; $hosts name(s) in $LINK_ZONE"
