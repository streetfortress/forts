# Shared base configuration for every SFI NixOS host — interactive AND
# production, so only genuinely user-agnostic settings belong here. steven's
# account lives in modules/interactive.nix; production hosts get `deployer`
# from modules/production.nix instead.
# Host-specific settings live in hosts/<name>/; opt-in role modules in modules/.
{ pkgs, ... }:

{
  # Nix (allowed-users is per-profile: interactive.nix / production.nix)
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
  services.openssh = {
    enable = true;
    settings = {
      PermitRootLogin = "no";
      PasswordAuthentication = false;
    };
  };

  # Every host is reachable over SSH.
  networking.firewall.allowedTCPPorts = [ 22 ];
}
