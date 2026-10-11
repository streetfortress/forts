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

      # The CLI's tool dependencies as one declared set (#25).
      # internal/tools/tools.json names every binary the CLI execs and
      # the nixpkgs attribute that carries it; this flake is its second
      # reader, so the wrapper's PATH and the Go code's check can
      # never name different tools. The set's `ambient` half (nix,
      # pulumi) is deliberately absent here — tools.json says why of
      # each one, and the CLI checks them instead of carrying them.
      toolAttrs = lib.attrValues (lib.importJSON ./internal/tools/tools.json).packaged;
      # A tool nixpkgs does not support on this system is left out
      # rather than breaking the package there: the CLI names it as
      # missing before the verb starts, and the operator supplies it.
      toolset = pkgs:
        lib.filter (p: lib.meta.availableOn pkgs.stdenv.hostPlatform p)
          (map (attr: lib.getAttrFromPath (lib.splitString "." attr) pkgs) toolAttrs);

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
      # binaries, so an estate pins one input and gets a matching pair —
      # and, since #25, a matching set of tools under them.
      packages = forAll (pkgs: rec {
        # The Go build alone. An output of its own because the wrapper
        # below is a second derivation over it, and because `nix build
        # .#fort-unwrapped` is the compile check without the tools.
        fort-unwrapped = pkgs.buildGoModule {
          pname = "fort-unwrapped";
          version = self.shortRev or self.dirtyShortRev or "dirty";
          src = ./.;
          subPackages = [ "cmd/fort" "cmd/fort-pulumi" ];
          vendorHash = "sha256-nl0SVyeEfyRK/LmoiGI3fY7nrIk2d9zbfvJE+8kUiDs=";
          # The mirror tests want an estate; the fixtures do not need the
          # network. Build-time tests are what CI runs on the checkout.
          doCheck = false;
          meta = {
            description = "the fort CLI, without its tools on PATH";
            mainProgram = "fort";
          };
        };

        # What an operator installs and `nix run <estate>/nixos#fort`
        # runs: the two binaries with the packaged tool set ahead of them
        # on PATH, from this flake's nixpkgs. Each verb used to discover
        # its tools one at a time, at the moment of the apply (#25:
        # `fort console-password` wanted mkpasswd on a Mac that had none;
        # `fort provision` built cdrtools with the provider waiting). A
        # prefix, not a suffix: the point is the version this flake pins,
        # not whatever the machine happens to carry.
        fort = pkgs.runCommand "fort-${fort-unwrapped.version}"
          {
            nativeBuildInputs = [ pkgs.makeWrapper ];
            passthru = { inherit fort-unwrapped; };
            meta = {
              description = "the fort CLI: scaffold, mint, image, provision, add, seed, check, dismantle, destroy";
              mainProgram = "fort";
            };
          }
          ''
            mkdir -p $out/bin
            for b in fort fort-pulumi; do
              makeWrapper ${fort-unwrapped}/bin/$b $out/bin/$b \
                --prefix PATH : ${lib.makeBinPath (toolset pkgs)}
            done
            # The completion scripts (forts#113), in the two directories
            # the shells already read: compinit walks share/zsh/site-
            # functions of every profile, bash-completion loads
            # share/bash-completion/completions/<command> on demand. Both
            # call `fort __complete`, so they ride with the binary and
            # cannot be a version apart from it.
            install -Dm444 ${./completions/_fort} $out/share/zsh/site-functions/_fort
            install -Dm444 ${./completions/fort.bash} $out/share/bash-completion/completions/fort
          '';
        default = fort;
      });

      checks = forAll (pkgs: {
        fort = self.packages.${pkgs.system}.fort;
      });

      # `nix develop` in this repo: the Go toolchain and the same
      # declared tools, so `go run ./cmd/fort <verb>` finds every one.
      devShells = forAll (pkgs: {
        default = pkgs.mkShell {
          packages = [ pkgs.go ] ++ toolset pkgs;
        };
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

      # The same fort linked to a second estate (modules/links.nix,
      # deployments#674): the fixture host plus one link, as the forge of
      # that estate (forts#38: the roles say what the link opens). CI
      # evaluates it so the module's assertions, the interface, the
      # firewall rules and the reconciler unit compose with a hardened
      # host.
      nixosConfigurations.linked-fort = lib.nixosSystem {
        system = "x86_64-linux";
        specialArgs = { inputs = { forts = self; inherit nixpkgs sops-nix; }; };
        modules = [
          self.nixosModules.common
          ./tests/estate/nixos/hosts/plain-fort
          sops-nix.nixosModules.sops
          self.nixosModules.links
          {
            sfi.links.other-estate = {
              repo = "https://gitea.zen.lofi/sfi/other-estate.git";
              interface = "wg-other";
              port = 51821;
              address = "10.3.0.96";
              roles = [ "forge" ];
            };
          }
        ];
      };
    };
}
