# The challenge–response gate (#256): one verb invocation, one proof that
# a fleet admin age key is in the operator's hands RIGHT NOW.
#
# Why a second factor at all. A fort's ops ladder (ops-commands.nix) is
# held up by one credential: the deployer SSH key on the mesh. Rung 2
# verbs are safe under that credential alone because each one only
# retriggers machinery git already governs. A few verbs are not: they
# widen REACH — arbitrary kubectl against the admin kubeconfig, an
# unvetted branch switched onto the host as root. For those, "whoever
# holds the deployer key" is the wrong authorization, and the right one
# already exists: the fleet admin age key, which decrypts every fort's
# secrets and never touches a fort.
#
# The ceremony, per invocation:
#   1. The verb makes a random nonce and encrypts it to the admin age
#      PUBLIC keys. Nothing secret is at rest: the public keys are in
#      .sops.yaml, and the nonce lives in one root process for the
#      length of the prompt.
#   2. The operator copies the armored block to a machine that holds an
#      admin key (an operator machine), runs `age -d`, and pastes the nonce
#      back within the TTL.
#   3. The gate compares, writes the verb and its argv to the journal,
#      and only then lets the verb body run.
#
# Properties: the proof is live (a nonce, not a stored token), nothing
# replayable is typed on the host, and the host needs only `age` and the
# public keys. A static capability token — provision stamps a hash, the
# verb compares stdin — was rejected for exactly this: once typed on a
# host that is already listening, it is captured and good until rotation.
#
# What this does NOT control: a root-compromised host. The verb, the
# gate and the journal are all the host's, so a host with root already
# taken can fake the whole ceremony. This gate controls UNAUTHORIZED
# OPERATORS — someone with the deployer key and the mesh — and that is
# the threat the production profile (#86/#90) is shaped around. The
# README's key-decisions table says the same.
#
# The recipients come from `nixos/.sops.yaml`, not from a list kept
# beside it: the admin keys are the recipients every rule shares, which
# is the lines outside the `# BEGIN/END fort sops rule:` markers that
# `fort mint` maintains (the split cmd/fort/mint.go's
# sharedRecipients makes on the Go side). A rotated admin key therefore
# reaches every verb on the next rebuild, and a fort key never can.
#
# Two ways a verb opts in:
#   - the module that declares it sets `challenge = "<reason>"` on the
#     command (ops-commands.nix wraps the whole verb);
#   - a host names it in `sfi.ops.challenge.verbs`, which gates a verb
#     the module left open — per-fort policy, no module edit.
# A verb that needs the gate on ONE argument path calls
# `${config.sfi.ops.challenge.package}/bin/sfi-ops-challenge` itself.
#
# NO VERB IS GATED TODAY. This module lands the mechanics and one verb
# that exercises them, `sudo sfi-challenge` — it runs the ceremony,
# prints success or fail, and changes nothing. Gating a real verb is a
# separate decision per verb, and #256 keeps one candidate:
#   - sfi-admin-kubectl — arbitrary kubectl on the admin kubeconfig,
#     with no allowlist. The strongest candidate.
# #256's second candidate is retired, not gated: `sfi-nixos-rebuild
# --ref <branch>` was the argument that widened reach, and #76 removed
# it instead. A branch is an unvetted closure and every dev fort can
# push one, so the verb now builds main and nothing else; `--from <ip>`
# names another address for the same main, which is the path a stranded
# fort walks and must stay ungated — a gate there would lock the
# recovery behind the thing to recover.
# Two verbs that look like candidates and are not:
#   - sfi-admin-shell — the module that declares it also hands deployer
#     ALL NOPASSWD, so a person the gate stops types `sudo bash`. A gate
#     there would read as a control and be none.
#   - sfi-nixos-rollback — its own header already answers #256: it
#     narrows a menu of states git produced, it does not widen reach.
#
# `sudo sfi-challenge` is also the operational use of this module on a
# fort nobody has gated: it answers "does an admin key still reach this
# host, and does the operator still have one" BEFORE an incident asks.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.ops.challenge;

  # Fold over .sops.yaml: collect `- age1…` recipients, but only the ones
  # outside a fort's marker block. `.*` trails each pattern because
  # builtins.match anchors the whole line, and the lines carry comments.
  scanLine = acc: line:
    if builtins.match "[[:space:]]*# BEGIN fort sops rule:.*" line != null then
      acc // { inBlock = true; }
    else if builtins.match "[[:space:]]*# END fort sops rule:.*" line != null then
      acc // { inBlock = false; }
    else
      let m = builtins.match "[[:space:]]*- (age1[0-9a-z]+).*" line;
      in if m != null && !acc.inBlock then acc // { keys = acc.keys ++ m; } else acc;

  sharedRecipients = lib.unique
    (lib.foldl' scanLine { inBlock = false; keys = [ ]; }
      (lib.splitString "\n" (builtins.readFile (config.sfi.estate.root + "/nixos/.sops.yaml")))).keys;

  recipientFlags =
    lib.concatMapStringsSep " " (r: "-r ${lib.escapeShellArg r}") cfg.recipients;

  challengePkg = pkgs.writeShellApplication {
    name = "sfi-ops-challenge";
    runtimeInputs = [ pkgs.age pkgs.coreutils pkgs.util-linux ];
    text = ''
      # usage: sfi-ops-challenge <reason> -- <verb> [argv…]
      # Exit 0 = proved. Exit 77 (EX_NOPERM) = refused; the caller stops
      # there, because every verb runs under `set -euo pipefail`.
      if [ "$#" -lt 1 ]; then
        echo "usage: sfi-ops-challenge <reason> -- <verb> [argv...]" >&2
        exit 64
      fi
      reason=$1
      shift
      if [ "''${1-}" = "--" ]; then shift; fi
      verb=''${1-sfi-verb}
      shift || true
      argv="$*"

      who=''${SUDO_USER:-$(id -un)}
      ttl=${toString cfg.ttl}

      # auth.notice, so the proof sits in the journal next to the sudo
      # line that started the verb. Written BEFORE the verb body runs:
      # a killed session must still leave the whole story. The line IS
      # the audit, so the gate fails closed when it cannot be written.
      note() {
        logger -t sfi-ops-challenge -p auth.notice "$*" || {
          echo "error: the journal took no audit line — $verb refused" >&2
          exit 70
        }
      }

      # The ceremony is a conversation. No terminal means no operator —
      # refuse rather than find some other way to ask.
      if ! { : >/dev/tty; } 2>/dev/null; then
        echo "error: $verb needs the admin-key challenge and there is no terminal to ask on" >&2
        note "challenge REFUSED (no tty): user=$who verb=$verb argv=[$argv]"
        exit 77
      fi

      # 10 bytes of urandom, hex. od reads a fixed count, so no pipe
      # closes early under pipefail.
      nonce=$(od -An -tx1 -N10 /dev/urandom | tr -d ' \n')
      ciphertext=$(printf '%s\n' "$nonce" | age --armor ${recipientFlags})

      note "challenge ISSUED: user=$who verb=$verb argv=[$argv] ttl=''${ttl}s"

      {
        echo
        echo "== $verb needs the fleet admin age key"
        echo "   $reason"
        echo
        echo "   Decrypt this on a machine that holds an admin key:"
        echo
        printf '%s\n' "$ciphertext"
        echo
        echo "   age -d -i ~/.config/sops/age/keys.txt   # paste the block above, then ^D"
        echo
        echo "   Paste the line it prints. The challenge expires in ''${ttl}s."
        printf '   nonce: '
      } >/dev/tty

      answer=""
      if ! read -r -t "$ttl" answer </dev/tty; then
        echo >/dev/tty
        echo "error: no answer in ''${ttl}s — $verb refused" >&2
        note "challenge FAILED (timeout): user=$who verb=$verb argv=[$argv]"
        exit 77
      fi

      # Compare digests, not the values: a wrong paste then costs the
      # same time whatever its first characters were.
      want=$(printf '%s' "$nonce" | sha256sum)
      got=$(printf '%s' "$answer" | tr -d '[:space:]' | sha256sum)
      if [ "$want" != "$got" ]; then
        echo "error: wrong answer — $verb refused" >&2
        note "challenge FAILED (mismatch): user=$who verb=$verb argv=[$argv]"
        exit 77
      fi

      note "challenge PASSED: user=$who verb=$verb argv=[$argv] — the verb runs now"
      echo "   proved — $verb runs now (journal: -t sfi-ops-challenge)" >/dev/tty
    '';
  };
in
{
  imports = [ ../estate.nix ];

  options.sfi.ops.challenge = {
    recipients = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = sharedRecipients;
      defaultText = lib.literalMD "the recipients every rule in `nixos/.sops.yaml` shares";
      description = ''
        Age PUBLIC keys the challenge encrypts its nonce to — the fleet
        admin keys. The default reads them out of `nixos/.sops.yaml`, so
        a rotated admin key needs no edit here. An operator must hold one
        of these private halves to run a gated verb.
      '';
    };

    ttl = lib.mkOption {
      type = lib.types.ints.positive;
      default = 60;
      description = ''
        Seconds the operator has to answer a challenge. Short on
        purpose: the ceremony must feel like an event, and a prompt left
        open on a shared terminal is a second factor nobody holds.
      '';
    };

    verbs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "k8s-scale" "flux-suspend" ];
      description = ''
        Verb names (without the `sfi-` prefix) this host gates behind the
        challenge in addition to the ones their own modules gate. Per-fort
        policy: a fort that wants the ceremony on a verb the framework
        leaves open names it here.
      '';
    };

    package = lib.mkOption {
      type = lib.types.package;
      readOnly = true;
      default = challengePkg;
      defaultText = lib.literalMD "the generated `sfi-ops-challenge`";
      description = ''
        The gate itself, for a verb that must challenge on one argument
        path instead of every invocation. Call it as
        `sfi-ops-challenge <reason> -- <verb> <argv…>`; it exits 77 when
        the operator fails to prove the key.
      '';
    };
  };

  config = {
    assertions = [{
      assertion = cfg.recipients != [ ];
      message = "sfi.ops.challenge.recipients is empty: no admin age key was found in nixos/.sops.yaml, so a gated verb could never be run. Set it explicitly.";
    }];

    # The rehearsal verb. It holds no reach of its own: it runs the
    # ceremony and says what happened, so the mechanics can be exercised
    # on a fort without gating anything that matters. Deliberately NOT
    # declared with `challenge = …`, because a wrapped verb never reaches
    # its own body on a failure — this one has to report both outcomes.
    sfi.ops.commands.challenge = {
      description = "Rehearse the admin-key challenge: run the ceremony and print success or fail. Changes nothing";
      script = ''
        # `if cmd; then …; fi` would leave $? at 0 on the failing branch,
        # so the status is captured where it is still the gate's.
        rc=0
        ${cfg.package}/bin/sfi-ops-challenge \
          "a rehearsal — this verb changes nothing on this host" \
          -- sfi-challenge "$@" || rc=$?
        if [ "$rc" -eq 0 ]; then
          echo "success: the admin key answered. A gated verb would run its body now."
          exit 0
        fi
        echo "fail: the challenge was not answered (exit $rc). A gated verb would stop here." >&2
        exit "$rc"
      '';
    };
  };
}
