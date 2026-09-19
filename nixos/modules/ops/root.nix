# sfi-root: a root shell for deployer, for hosts that opt in — rung 3
# through the front door, on the HOST rather than the cluster
# (ops/kubectl.nix is the same rung for the cluster).
#
#   sudo sfi-root            # root login shell, listed in `sfi-ops`
#   sudo <anything>          # the rule is ALL, NOPASSWD
#
# The framework's rule (ops-commands.nix) is that verbs nudge machinery
# and never desired state, and that a root shell is a console break-glass
# event. This module exists for a WINDOW where that rule is the wrong
# trade: a migration across a new OS, a new operational model and a new
# stack (the gitea move, #338/#350), where anything that goes wrong has
# to be dug into on the box and no host may be left stranded on a
# provider console. The curated verbs stay the first resort; this is the
# guarantee behind them.
#
# The guard is the import: a host gets this only by naming it in its own
# default.nix and setting enable, with the window in the comment beside
# it, and the same PR that ends the window removes both lines. Every
# sudo invocation is logged with its full argument list, which is the
# audit — there is no allowlist here and none is claimed. NOPASSWD
# because no account on a fort has a password (production.nix), so a
# password prompt would be a lockout, not a control.
{ config, lib, pkgs, ... }:
let
  cfg = config.sfi.ops.root;
in
{
  imports = [ ./ops-commands.nix ];

  options.sfi.ops.root.enable = lib.mkOption {
    type = lib.types.bool;
    default = false;
    description = "Grant deployer unrestricted NOPASSWD sudo and a `sudo sfi-root` shell — the ADMIN rung on the host. For a declared window only.";
  };

  config = lib.mkIf cfg.enable {
    security.sudo.extraRules = [{
      users = [ "deployer" ];
      commands = [{ command = "ALL"; options = [ "NOPASSWD" ]; }];
    }];

    sfi.ops.commands.root = {
      description = "ADMIN RUNG: a root login shell — this host is in a declared break-glass window (sudo is ALL for deployer)";
      script = ''
        echo "root shell on $(${pkgs.nettools}/bin/hostname) — every command here is outside git; sudo has logged this invocation" >&2
        exec ${pkgs.bashInteractive}/bin/bash -l
      '';
    };
  };
}
