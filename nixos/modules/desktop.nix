# Desktop environment module: GNOME, PipeWire, fonts
{ config, pkgs, ... }:

{
  # X11 and GNOME (display/desktop manager paths moved in 24.11+)
  services.xserver.enable = true;
  services.displayManager.gdm.enable = true;
  services.desktopManager.gnome.enable = true;

  # Keyboard layout (updated path for 24.05+)
  services.xserver.xkb = {
    layout = "us";
    variant = "";
    options = "caps:escape";
  };

  # PipeWire audio (sound.enable removed in 24.05)
  services.pulseaudio.enable = false;
  security.rtkit.enable = true;
  services.pipewire = {
    enable = true;
    alsa.enable = true;
    alsa.support32Bit = true;
    pulse.enable = true;
  };

  # Printing
  services.printing.enable = true;

  # Firefox
  programs.firefox.enable = true;

  # Steam
  programs.steam.enable = true;

  # Desktop packages
  environment.systemPackages = with pkgs; [
    gnome-tweaks  # was gnome.gnome-tweaks before 24.05
    alacritty
  ];

  # Fonts (nerdfonts restructured in 24.05 - now individual packages)
  fonts.packages = with pkgs; [
    nerd-fonts.commit-mono
    nerd-fonts.ubuntu
  ];
}
