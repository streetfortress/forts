# The estate: the checkout a fort's host file lives in — the manifests at
# forts/, the mesh at mesh/, the flux tree at flux/, the host files, secrets
# and sops rules under nixos/ (docs/README.md, "The estate layout"). The
# framework's modules come from a flake input and so cannot reach the
# estate's files by a relative path: whatever a module reads from the
# estate, it reads through this one option, which the host file sets
# (`fort scaffold` renders `sfi.estate.root = ../../..;` — three levels
# up from nixos/hosts/<host>/). Keeping the layout here, not in every
# reader, is what lets the layout version alone when a path moves.
{ lib, ... }:

{
  options.sfi.estate.root = lib.mkOption {
    type = lib.types.path;
    description = ''
      The estate checkout this host is declared in. Set by the host file;
      the modules that read the estate's files (the mesh peers, the flux
      tree, the sops rules, the lab CA) read them under this root.
    '';
  };
}
