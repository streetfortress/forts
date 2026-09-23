# Dev fort profile (#413): the hardened host with an `agent` instead of a
# `deployer`. hardened.nix + lab-host.nix, an unprivileged account that
# BUILDS (unlike deployer, which only observes), Home Manager with the dev
# toolchains and the dotfiles, nix-ld for the prebuilt binaries a dev box
# meets (uv-managed pythons, npm native modules). No k3s, no flux, no
# backups: a dev fort is transient, and nothing on it is precious
# (forts#12). Rendered by `fort scaffold` for a manifest with a [dev]
# table; mutually exclusive with production.nix and with the hand-managed
# interactive.nix + dotfiles.nix pair.
#
# What makes it a boundary rather than a laptop: the only credentials on
# the box are the fort's own (the agent bundle #414 mints per fort), SSH
# answers on the mesh only (hardened.nix), the agent has no wheel and no
# ops verbs — the pull loop is the only way config reaches this host —
# and the box is replaced, not repaired. That is what makes an unattended
# `claude -p` with permissions bypassed sane here (#418) and nowhere else.
#
# Toolchains come from the store, not from installers at runtime: nothing
# the SFI checkouts pin today needs a channel manager (go 1.24–1.26, node
# ≥22, python 3.13 via uv). Add rustup only when a repo grows a
# rust-toolchain.toml; don't build an FHS env unless something specific
# breaks after nix-ld.
{ config, lib, pkgs, ... }:

let
  # gitea's host key, from the one pin auto-rebuild.nix carries
  # ("[gitea.zen.lofi]:30022 ssh-rsa AAAA…" → type + material), so the
  # agent's first clone over ssh never meets a trust prompt.
  giteaParts = lib.splitString " " config.sfi.auto-rebuild.gitKnownHosts;
  giteaKey = "${lib.elemAt giteaParts 1} ${lib.elemAt giteaParts 2}";
in
{
  imports = [
    ./hardened.nix
    ./lab-host.nix
    ./dotfiles.nix # ~agent/.files, the checkout the HM symlinks point into
    ./auto-rebuild.nix # the gc window below is its option
  ];

  # The one hardened profile with a Home Manager user: agent's, and only
  # agent's — the flake's uniform module set still declares steven's HM
  # config, who does not exist here (same force as production.nix).
  home-manager.users = lib.mkForce { agent = import ../home/agent.nix; };
  sfi.dotfiles.user = "agent";
  programs.zsh.enable = true; # the login shell below

  users.users.agent = {
    isNormalUser = true;
    description = "SFI coding agent";
    shell = pkgs.zsh;
    # A user manager that outlives the login: `pickup` (#418) runs claude
    # under systemd-run --user so a dropped ssh session never kills a
    # task in flight.
    linger = true;
    # Read the host journal — the OBSERVE rung for a host with no other
    # operator account (same grant as deployer's, production.nix).
    extraGroups = [ "systemd-journal" ];
    # Mesh-only reach (hardened.nix); the key set matches deployer's for
    # now — trim to the dispatching devices when #415 settles it.
    openssh.authorizedKeys.keys = [
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBFSTKL7m9ViSstGGhgg1TBnrWEGkNptCCysU17Oxgfl"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIG0sCtYCOvdb/J7yluSSX9yixiG3pvhZo+OtVQWefjVj"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBuuLb9A36fWGYZexMb2soTxpHFB0HVQTHqR8vihvFMD"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINqO0N5rBif1+bFwnluWEmrkaoTAtqTrP/vONG6/fQKl"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBhrjrcm0Mb4gSMGM/GiWXvkZj3a2ej7/MOcw0Qujx+M"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFSfukFhjws+MpZu+qMqgVCoIitc43jGeqEnGMcF4ydQ"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEdgJK6LiKUZ+fQQZX28B1v82hCl1z4RxomJLn5pHiPH"
    ];
  };

  # agent builds: `nix develop`, `nix build` in the checkouts. hardened.nix
  # keeps root; this appends.
  nix.settings.allowed-users = [ "agent" ];

  # A build box churns generations and fills a store faster than a fort
  # that only receives merges: half the fleet window (#423).
  sfi.auto-rebuild.gc.olderThan = "14d";

  # Prebuilt dynamic binaries find a loader and the common libraries;
  # the set stays modest on purpose — grow it when a specific binary
  # names a missing one.
  programs.nix-ld = {
    enable = true;
    libraries = with pkgs; [
      stdenv.cc.cc.lib
      zlib
      openssl
      libffi
      libxml2
      icu
      curl
      expat
    ];
  };

  # The agent's git transport dials gitea over the mesh on 30022; pin the
  # host key system-wide so the first clone is hands-off.
  programs.ssh.knownHosts."gitea.zen.lofi" = {
    hostNames = [ "gitea.zen.lofi" "[gitea.zen.lofi]:30022" ];
    publicKey = giteaKey;
  };

  environment.systemPackages = [ pkgs.git ];
}
