# Interactive-host add-on: steven's account and the shell it expects. Split out
# of common.nix so that "common" stays true for every host — production nodes
# (modules/production.nix) deliberately never import this and carry the
# unprivileged `deployer` account instead.
{ pkgs, ... }:

{
  nix.settings.allowed-users = [ "steven" ];

  # Enable ZSH system-wide (required for setting it as a user's default shell)
  programs.zsh.enable = true;

  # Primary user. Host modules extend extraGroups (e.g. docker, networkmanager).
  users.users.steven = {
    isNormalUser = true;
    description = "Steven Fitzpatrick";
    shell = pkgs.zsh;
    extraGroups = [ "wheel" "kubeops" ]; # kubeops: read the ops-ro kubeconfig (#90)
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
}
