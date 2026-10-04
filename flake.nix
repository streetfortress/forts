{
  description = "forts — the framework under an SFI estate: the NixOS modules a fort is made of, the fort CLI, the chart templates";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    # The modules declare sops secrets (a host's secrets.nix, the mesh key,
    # the k3s token): sops-nix is the framework's dependency, and an
    # estate's flake follows this pin or its own.
    sops-nix = {
      url = "github:Mic92/sops-nix";
      inputs.nixpkgs.follows = "nixpkgs";
    };
  };

  outputs = { self, nixpkgs, sops-nix }:
    let
      lib = nixpkgs.lib;
      systems = [ "x86_64-linux" "aarch64-linux" "aarch64-darwin" "x86_64-darwin" ];
      forAll = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # Every module under modules/ is an output, by file name; a module in
      # a subdirectory carries the directory as a prefix (ops/deployer.nix
      # → ops-deployer). Modules import each other by relative path and
      # read the estate through sfi.estate.root (modules/estate.nix), so a
      # host file imports what it is — inputs.forts.nixosModules.k3s-server
      # — and the flake needs no edit when a module is added.
      modulesIn = dir: prefix:
        lib.concatMapAttrs (name: type:
          if type == "regular" && lib.hasSuffix ".nix" name then
            let base = lib.removeSuffix ".nix" name;
                # ops/ops-commands.nix is ops-commands, not ops-ops-commands
                attr = if lib.hasPrefix prefix base then base else "${prefix}${base}";
            in { "${attr}" = dir + "/${name}"; }
          else if type == "directory" then
            modulesIn (dir + "/${name}") "${prefix}${name}-"
          else
            { }) (builtins.readDir dir);
    in {
      nixosModules = modulesIn ./modules "";

      # The fort CLI and the pulumi program it execs (cmd/fort-pulumi,
      # named so it never shadows the pulumi CLI). One package, two
      # binaries, so an estate pins one input and gets a matching pair.
      packages = forAll (pkgs: rec {
        fort = pkgs.buildGoModule {
          pname = "fort";
          version = self.shortRev or self.dirtyShortRev or "dirty";
          src = ./.;
          subPackages = [ "cmd/fort" "cmd/fort-pulumi" ];
          vendorHash = "sha256-nl0SVyeEfyRK/LmoiGI3fY7nrIk2d9zbfvJE+8kUiDs=";
          # The mirror tests want an estate; the fixtures do not need the
          # network. Build-time tests are what CI runs on the checkout.
          doCheck = false;
          meta = {
            description = "the fort CLI: scaffold, mint, image, provision, add, seed, check, dismantle, destroy";
            mainProgram = "fort";
          };
        };
        default = fort;
      });

      checks = forAll (pkgs: {
        fort = self.packages.${pkgs.system}.fort;
      });

      # The fixture estate (tests/estate: one plain fort, rendered there by
      # `fort scaffold`) evaluated the way an estate's flake evaluates a
      # host: the common base, the host dir, sops-nix, and this flake as
      # `inputs.forts`. CI evaluates its toplevel derivation — the proof
      # that the module outputs compose into a host — and builds nothing.
      nixosConfigurations.plain-fort = lib.nixosSystem {
        system = "x86_64-linux";
        specialArgs = { inputs = { forts = self; inherit nixpkgs sops-nix; }; };
        modules = [
          self.nixosModules.common
          ./tests/estate/nixos/hosts/plain-fort
          sops-nix.nixosModules.sops
        ];
      };
    };
}
