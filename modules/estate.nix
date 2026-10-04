# The estate: the checkout a fort's host file lives in — the manifests at
# forts/, the mesh at mesh/, the flux tree at flux/, the host files, secrets
# and sops rules under nixos/ (docs/README.md, "The estate layout"). The
# framework's modules come from a flake input and so cannot reach the
# estate's files by a relative path: whatever a module reads from the
# estate, it reads through this one option, which the host file sets
# (`fort scaffold` renders `sfi.estate.root = ../../..;` — three levels
# up from nixos/hosts/<host>/). Keeping the layout here, not in every
# reader, is what lets the layout version alone when a path moves.
{ config, lib, ... }:

let
  tomlPath = config.sfi.estate.root + "/estate.toml";
  estateToml = if builtins.pathExists tomlPath then builtins.fromTOML (builtins.readFile tomlPath) else { };
in
{
  options.sfi.estate.root = lib.mkOption {
    type = lib.types.path;
    description = ''
      The estate checkout this host is declared in. Set by the host file;
      the modules that read the estate's files (the mesh peers, the flux
      tree, the sops rules, the lab CA) read them under this root.
    '';
  };

  # The one estate fact a module reads from estate.toml itself: the name
  # of the mesh interface (deployments#674). One name per mesh, on every
  # host on it — this host's own (wireguard.nix) and a linked host's link
  # interface (links.nix) — so `wg show <name>` means the same thing
  # everywhere, and hardened.nix / production.nix open the admin ports on
  # it by that name. An estate that names none is on wg0, as before.
  options.sfi.mesh.interface = lib.mkOption {
    type = lib.types.strMatching "^[A-Za-z0-9_=+.-]{1,15}$";
    default = estateToml.interface or "wg0";
    defaultText = "estate.toml's `interface`, else wg0";
    description = "The WireGuard interface of this estate's mesh on this host.";
  };
}
