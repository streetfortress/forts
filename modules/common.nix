# Shared base configuration for every SFI NixOS host — cluster forts,
# dev forts and hubs alike — so only genuinely user-agnostic settings
# belong here. Accounts are the profile's: `deployer` from
# modules/production.nix, `agent` from the estate's dev role.
# Host-specific settings live in hosts/<name>/; opt-in role modules in modules/.
{ pkgs, ... }:

{
  # Nix (allowed-users is per-profile: hardened.nix keeps it root-only,
  # a role module of the estate appends its accounts)
  nix.settings.experimental-features = [ "nix-command" "flakes" ];
  nixpkgs.config.allowUnfree = true;

  # sops-nix CLI tooling (per-host sops wiring lives in hosts/<name>/secrets.nix)
  environment.systemPackages = with pkgs; [ sops age ssh-to-age ];

  # Timezone and locale
  time.timeZone = "America/Los_Angeles";
  i18n.defaultLocale = "en_US.UTF-8";
  i18n.extraLocaleSettings = {
    LC_ADDRESS = "en_US.UTF-8";
    LC_IDENTIFICATION = "en_US.UTF-8";
    LC_MEASUREMENT = "en_US.UTF-8";
    LC_MONETARY = "en_US.UTF-8";
    LC_NAME = "en_US.UTF-8";
    LC_NUMERIC = "en_US.UTF-8";
    LC_PAPER = "en_US.UTF-8";
    LC_TELEPHONE = "en_US.UTF-8";
    LC_TIME = "en_US.UTF-8";
  };


  # OpenSSH: key-only auth, no root login.
  #
  # A password reaches sshd by two paths, and both must say no. The
  # second one is why KbdInteractiveAuthentication is here (#78):
  # NixOS defaults it to true and defaults UsePAM to true, and PAM's
  # keyboard-interactive path reads the same /etc/shadow hash that
  # ops/console.nix installs for `deployer`. PasswordAuthentication
  # alone therefore left the console password good over the mesh.
  services.openssh = {
    enable = true;
    settings = {
      PermitRootLogin = "no";
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;
    };
  };

  # Every host is reachable over SSH.
  networking.firewall.allowedTCPPorts = [ 22 ];
}
