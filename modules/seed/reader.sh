# shellcheck shell=bash
# The shared half of the seed reader (nixos/modules/seed.nix, #606). It is
# the body of sfi-seed.service in the systemd initrd, after the root is
# mounted on /sysroot and before the switch to stage 2. POSIX sh, run by
# bash under `set -e`: every command that may fail is guarded. The channel
# half defines sfi_seed_channel, which prints the seed on stdout and
# returns non-zero when the channel has none.
#
# The seed is the host's ed25519 ssh key as `fort mint` wrote it into
# nixos/secrets/<host>.yaml (ssh-host-key, then ssh-host-key.pub), the
# two files concatenated. The reader splits at the PEM trailer, so the
# public line may carry any comment. internal/seed is the Go side
# of the same format; its test is the contract between the two.
#
# A failure exits 1. initrd.target requires the unit, so systemd stops the
# boot in the initrd's emergency target with these lines on the console
# (the unit writes to journal+console); an operator on Lish reads them.

sfi_seed_fail() {
  echo
  echo "sfi-seed: $1"
  echo "sfi-seed: the root has no /etc/ssh/ssh_host_ed25519_key, and sops-nix cannot"
  echo "sfi-seed: decrypt this host's secrets without it. Stage 2 would activate with"
  echo "sfi-seed: no secret, so the boot stops here. fort provision gives a fort"
  echo "sfi-seed: its key through the @channel@ channel (docs/provision.md)."
  echo "sfi-seed: Place the key by hand from a rescue boot, or destroy and provision"
  echo "sfi-seed: the host again."
  echo
  exit 1
}

sfi_seed() {
  root="$1"
  key="$root/etc/ssh/ssh_host_ed25519_key"
  if [ -s "$key" ]; then
    echo "sfi-seed: host key present on the root, nothing to do"
    return 0
  fi
  echo "sfi-seed: no host key on the root; reading the seed from the @channel@ channel"
  seed=$(sfi_seed_channel) || sfi_seed_fail "the @channel@ channel returned no seed"
  private=$(printf '%s\n' "$seed" | sed -n '/^-----BEGIN OPENSSH PRIVATE KEY-----$/,/^-----END OPENSSH PRIVATE KEY-----$/p')
  public=$(printf '%s\n' "$seed" | sed -n '/^-----END OPENSSH PRIVATE KEY-----$/,$p' | sed '1d' | grep '^ssh-ed25519 ' | head -n 1) || public=""
  case $private in
    "-----BEGIN OPENSSH PRIVATE KEY-----"*"-----END OPENSSH PRIVATE KEY-----") ;;
    *) sfi_seed_fail "the seed is not an OpenSSH private key (no PEM header and trailer)" ;;
  esac
  [ -n "$public" ] || sfi_seed_fail "the seed has no ssh-ed25519 public line after the PEM trailer"
  mkdir -p "$root/etc/ssh"
  old_umask=$(umask)
  umask 077
  printf '%s\n' "$private" > "$key"
  chmod 0600 "$key"
  printf '%s\n' "$public" > "$key.pub"
  chmod 0644 "$key.pub"
  umask "$old_umask"
  echo "sfi-seed: placed /etc/ssh/ssh_host_ed25519_key (0600) and .pub (0644) on the root"
}
