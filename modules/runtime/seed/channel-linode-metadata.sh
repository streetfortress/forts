# shellcheck shell=bash
# The Linode channel of the seed reader (modules/runtime/seed.nix, #606):
# the seed is metadata.user_data on the instance, which `fort provision`
# sets from the sops file, and which the Metadata service serves to the
# instance alone. Two steps the service requires: a token (PUT /v1/token,
# the expiry in a header) and the read (GET /v1/user-data with the token
# in a header). The service answers on the link-local address
# 169.254.169.254 over IPv4; its IPv6 address (fd00:a9fe:a9fe::1) would
# need SLAAC in the initrd, so this channel does not use it. user_data
# comes back base64, as it was stored, and coreutils decodes it.
#
# The link: eth0 (net.ifnames=0 from linode-config.nix), one DHCP lease
# from busybox udhcpc under the same lease script as initrd-network.nix.
# Whether the lease's routes alone reach the link-local address is an
# open fact of #606: if the token request fails the first time, the
# channel adds the /32 on-link and tries once more, and the console says
# which path answered. The interface is flushed and taken down afterwards
# (initrd-network's flushBeforeStage2), so stage 2's dhcpcd starts clean.

sfi_seed_mds=169.254.169.254
sfi_seed_iface=eth0

sfi_seed_curl() {
  curl --silent --show-error --fail --max-time 5 \
    --retry 5 --retry-connrefused --retry-all-errors "$@"
}

sfi_seed_mds_token() {
  sfi_seed_curl --request PUT --header "Metadata-Token-Expiry-Seconds: 300" \
    "http://$sfi_seed_mds/v1/token"
}

sfi_seed_link_down() {
  ip address flush dev "$sfi_seed_iface" 2>/dev/null || true
  ip link set dev "$sfi_seed_iface" down 2>/dev/null || true
}

sfi_seed_channel() {
  # udev names the device; give it a moment to appear.
  waited=0
  while [ ! -e "/sys/class/net/$sfi_seed_iface" ] && [ "$waited" -lt 30 ]; do
    sleep 1
    waited=$((waited + 1))
  done
  echo "sfi-seed: bringing up $sfi_seed_iface with DHCP" >&2
  if ! ip link set dev "$sfi_seed_iface" up; then
    echo "sfi-seed: no $sfi_seed_iface in the initrd (virtio_net not loaded?)" >&2
    return 1
  fi
  # -t tries per attempt, -T seconds between tries, -A seconds before the
  # next attempt; --quit --now: one lease, then exit (initrd-network's shape).
  if ! udhcpc --quit --now -i "$sfi_seed_iface" -O staticroutes -t 10 -T 3 -A 1 \
      --script "@udhcpcScript@" >&2; then
    echo "sfi-seed: no DHCP lease on $sfi_seed_iface" >&2
    sfi_seed_link_down
    return 1
  fi
  ip -4 address show dev "$sfi_seed_iface" >&2 || true
  ip -4 route show >&2 || true
  token=$(sfi_seed_mds_token) || token=""
  if [ -z "$token" ]; then
    echo "sfi-seed: $sfi_seed_mds not reached through the lease's routes; adding it on-link" >&2
    ip route add "$sfi_seed_mds/32" dev "$sfi_seed_iface" || true
    token=$(sfi_seed_mds_token) || token=""
  fi
  if [ -z "$token" ]; then
    echo "sfi-seed: no token from the Metadata service at $sfi_seed_mds" >&2
    sfi_seed_link_down
    return 1
  fi
  echo "sfi-seed: token received; reading /v1/user-data" >&2
  if ! data=$(sfi_seed_curl --header "Metadata-Token: $token" "http://$sfi_seed_mds/v1/user-data"); then
    data=""
  fi
  sfi_seed_link_down
  if [ -z "$data" ]; then
    echo "sfi-seed: the instance has no user_data (did fort provision create it?)" >&2
    return 1
  fi
  printf '%s' "$data" | base64 -d
}
