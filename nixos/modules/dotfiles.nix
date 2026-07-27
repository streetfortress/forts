# Clone steven's dotfiles from the internal Gitea over the homelab network, so
# ~/.files exists at runtime. Home Manager builds the dotfiles themselves into
# the Nix store at image-build time (home/steven.nix); this only provides the
# working checkout that the `nix-rebuild` alias (--flake ~/.files/nixos) and the
# shell profiles sourced by home/steven.nix expect.
#
# Requires:
#   - the lab CA trusted        so TLS to gitea.zen.lofi validates. staging-k3s
#                               already gets this from modules/k3s-server.nix.
#   - DNS resolving *.zen.lofi  (10.0.0.2, via the k8s dnsmasq forward).
#
# staging-k3s reaches gitea over the LAN (it sits on 10.0.0.0/24); production
# hosts with no LAN path reach it over the WireGuard mesh, so the unit orders
# itself after wg-quick-wg0 when that interface is present. Best-effort: an
# unreachable gitea logs and exits 0 rather than failing activation.
{ config, pkgs, ... }:

let
  user = "steven";
  home = config.users.users.${user}.home;
  repo = "https://gitea.zen.lofi/sf1tzp/.files.git";
in
{
  systemd.services.clone-dotfiles = {
    description = "Clone/update ${user}'s dotfiles from ${repo}";
    after = [ "network-online.target" "wg-quick-wg0.service" ];
    wants = [ "network-online.target" ];
    wantedBy = [ "multi-user.target" ];

    path = [ pkgs.git ];
    # git validates the gitea chain against the system bundle, which
    # security.pki (modules/homelab-ca.nix) rebuilds to include the lab CA.
    environment.SSL_CERT_FILE = "/etc/ssl/certs/ca-certificates.crt";

    serviceConfig = {
      Type = "oneshot";
      User = user;
      Group = "users";
    };

    script = ''
      set -eu
      dir="${home}/.files"
      if [ -d "$dir/.git" ]; then
        git -C "$dir" pull --ff-only \
          || echo "dotfiles: pull failed, keeping existing checkout" >&2
      else
        git clone "${repo}" "$dir" \
          || echo "dotfiles: clone failed (gitea unreachable?)" >&2
      fi
    '';
  };
}
