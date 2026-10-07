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

  # The two estate facts a module reads from estate.toml itself: the name
  # of the mesh interface (deployments#674) and the zone of the mesh
  # (forts#45). One name per mesh, on every host on it — this host's own
  # (wireguard.nix) and a linked host's link interface (links.nix) — so
  # `wg show <name>` means the same thing everywhere, and hardened.nix /
  # production.nix open the admin ports on it by that name. An estate
  # that names none is on wg0, as before.
  options.sfi.mesh.interface = lib.mkOption {
    type = lib.types.strMatching "^[A-Za-z0-9_=+.-]{1,15}$";
    default = estateToml.interface or "wg0";
    defaultText = "estate.toml's `interface`, else wg0";
    description = "The WireGuard interface of this estate's mesh on this host.";
  };

  # The zone: every host on the mesh is <host>.<zone> (mesh-dns.nix), and
  # a host linked to this mesh from another estate names it the same
  # (links.nix). The default derives from the estate's name — estate
  # staging is the zone staging.mesh — so a mesh-local name says which
  # mesh it is on. .mesh is the fleet's one private TLD.
  options.sfi.mesh.zone = lib.mkOption {
    type = lib.types.strMatching "^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)*$";
    default = estateToml.zone or (
      if estateToml ? name then "${estateToml.name}.mesh"
      else throw "sfi.mesh.zone: ${toString tomlPath} names no estate (`name`) — fort init renders estate.toml; or set the zone in the host file"
    );
    defaultText = "estate.toml's `zone`, else <name>.mesh";
    description = "The DNS zone of this estate's mesh: every peer and remote of mesh/peers.json is <host>.<zone> at its mesh address.";
  };

  config = {
    # The estate's CA root, trusted where the estate carries one
    # (forts#60). An estate that runs a step-ca issues the TLS of its own
    # services from it — the registry certificate the rebuild fetches
    # over, cert-manager's ClusterIssuer, the *.lofi names of the lab —
    # so every host of that estate must trust it. The file is the
    # estate's to provide (`fort init` says so and cannot write it), and
    # an estate with no CA has no file here and carries no extra trust.
    # The ca role (forts#46) owns this trust after the roles land.
    security.pki.certificateFiles =
      let caRoot = config.sfi.estate.root + "/nixos/certs/step-ca-root.pem";
      in lib.optional (builtins.pathExists caRoot) caRoot;
  };
}
