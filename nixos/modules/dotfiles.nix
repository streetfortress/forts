# Clone sfi/.fortfiles — the agent's operating config: CLAUDE.md, the
# skills (kb, pickup, scout, review) and herdr.toml — to ~/.fortfiles for
# every user this host carries (`sfi.dotfiles.users`, which the importing
# profile must set: one account per role on a dev fort, from dev-fort.nix
# — #413, #546, #532). Home Manager points three out-of-store symlinks
# into that checkout (home/base.nix); this module only provides the
# working checkout the symlinks expect, and keeps it current at boot. One
# unit per user (clone-dotfiles-<user>), because the clone runs AS the
# user and each account has its own home.
#
# Why a runtime clone and not nix content: these three paths change often,
# and a corrected skill must reach the next session with a `git pull` and
# no rebuild (sfi/.fortfiles#1). Everything else a fort's shell needs is
# nix (modules/shell.nix). Why this repo and not a personal one: a gitea
# team cannot hold a repo of a second owner, so `sfi-agentic-dev` could
# never reach sf1tzp/.files, and an agent could not propose a change to the
# skills that direct it. A fort clones no personal repo (#546).
#
# Read over https, unauthenticated: the repo is readable that way, and the
# checkout is read-only by design — an agent that wants to change a skill
# clones the repo under ~/src through a dispatch and opens a PR against
# its guarded main, like any other repo. Unauthenticated is also what makes
# one module serve every account: the clone carries no identity, so the
# three checkouts differ in nothing but their owner.
#
# Requires:
#   - the lab CA trusted        so TLS to gitea.zen.lofi validates — from
#                               modules/lab-host.nix (imported by k3s-server.nix).
#   - DNS resolving *.zen.lofi  (10.0.0.2, via the k8s dnsmasq forward).
#
# A lab host reaches gitea over the LAN; a host with no LAN path reaches it
# over the WireGuard mesh, so the unit orders itself after wg-quick-wg0 when
# that interface is present. Best-effort: an unreachable gitea logs and
# exits 0 rather than failing activation.
{ config, lib, pkgs, ... }:

let
  repo = "https://gitea.zen.lofi/sfi/.fortfiles.git";
  clone = user: {
    description = "Clone/update ${user}'s ~/.fortfiles from ${repo}";
    after = [ "network-online.target" "wg-quick-wg0.service" ];
    wants = [ "network-online.target" ];
    wantedBy = [ "multi-user.target" ];

    path = [ pkgs.git ];
    # git validates the gitea chain against the system bundle, which
    # security.pki (via lab-host.nix) rebuilds to include the lab CA.
    environment.SSL_CERT_FILE = "/etc/ssl/certs/ca-certificates.crt";

    serviceConfig = {
      Type = "oneshot";
      User = user;
      Group = "users";
    };

    # `timeout`: git over https has no connect timeout of its own, and a
    # gitea whose mesh address resolves but does not answer (wg0 down
    # mid-switch — see wireguard.nix on stopIfChanged) costs the kernel's
    # SYN retry window, ~2 min per connect, before it fails. A switch
    # blocks on this unit's job; a bounded miss keeps the switch short.
    script = ''
      set -eu
      dir="${config.users.users.${user}.home}/.fortfiles"
      if [ -d "$dir/.git" ]; then
        timeout 60 git -C "$dir" pull --ff-only \
          || echo "fortfiles: pull failed, keeping existing checkout" >&2
      else
        timeout 60 git clone "${repo}" "$dir" \
          || echo "fortfiles: clone failed (gitea unreachable?)" >&2
      fi
    '';
  };
in
{
  # No default on purpose: the profile that imports this names its
  # accounts, and an import without one is a mistake the eval should
  # report here.
  options.sfi.dotfiles.users = lib.mkOption {
    type = lib.types.listOf lib.types.str;
    description = "The accounts whose ~/.fortfiles this host clones and keeps current.";
  };

  config.systemd.services = lib.listToAttrs
    (map (user: lib.nameValuePair "clone-dotfiles-${user}" (clone user))
      config.sfi.dotfiles.users);
}
