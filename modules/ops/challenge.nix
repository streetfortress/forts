# The admin-key challenge (#256, #115): sudo's authentication on a fort.
# One proof that a fleet admin age key is in the operator's hands RIGHT
# NOW opens one terminal for a bounded window; inside it sudo asks nothing.
#
# Why a second factor at all. A fort's ops ladder (ops-commands.nix) is
# held up by one credential: the deployer SSH key on the mesh. The pinned
# verbs are safe under that credential alone because each one only
# retriggers machinery git already governs. The admin rung is not: a root
# shell, arbitrary kubectl against the cluster. For those, "whoever holds
# the deployer key" is the wrong authorization, and the right one already
# exists: the fleet admin age key, which decrypts every fort's secrets
# and never touches a fort.
#
# WHERE THE GATE SITS (#115, decided 2026-10-13). The ceremony used to be a
# wrapper inside one verb, per invocation. It is now the PAM stack of the
# sudo service — two pam_exec lines — so it gates whatever sudoers says
# needs a password, and sudo's own timestamp is the window:
#   - The pinned verbs (sfi.ops.commands, NOPASSWD) never reach PAM.
#   - A verb declared with `challenge = true`, or named by the host in
#     sfi.ops.challenge.verbs, gets a PASSWD line: the first such sudo on
#     a terminal runs the ceremony, the next ones inside the window do not.
#   - A host that enables the admin rung (ops/admin.nix) adds
#     `deployer ALL=(ALL) ALL` without NOPASSWD: `sudo -i` is the shell,
#     behind the same gate, and `admin-unlock` mints the kubectl
#     credential behind it.
# The framework builds no window of its own. The shell's window is sudo's
# timestamp (`Defaults:deployer timestamp_timeout`, timestamp_type=tty: one
# terminal, one window; `sudo -k` ends it). kubectl's window is a token's
# expiry, which the API server enforces (ops/admin.nix). One option,
# sfi.ops.challenge.timeout, sets both.
#
# THE CEREMONY, once per window:
#   1. sudo needs to authenticate and runs the auth stack. The first
#      pam_exec line (sfi-challenge-issue) makes a random nonce, encrypts
#      it to the admin age PUBLIC keys, stores the nonce per terminal
#      under /run (root-only) and prints the armored block. pam_exec's
#      `stdout` option relays every line through sudo's conversation to
#      the terminal, so the block lands on the tty and not in a pipe the
#      command's output may be going to.
#   2. sudo prompts for the answer — the PASSWD prompt, reworded by
#      `Defaults passprompt`. The operator decrypts the block on a machine
#      that holds an admin key and pastes the nonce.
#   3. The second pam_exec line (sfi-challenge-verify, `expose_authtok`)
#      reads the typed answer on stdin, compares its digest with the
#      stored nonce for this terminal, journals the outcome, and removes
#      the stored nonce whatever the outcome. A pass is sudo's success;
#      sudo then writes the timestamp for this tty and runs the command.
# The nonce is per terminal because pam_exec runs the two lines as two
# processes: the first prints, the second checks, and the store between
# them is keyed by PAM_TTY, so two sessions cannot share one nonce.
#
# Properties: the proof is live (a nonce, not a stored token), nothing
# replayable is typed on the host, and the host needs only `age` and the
# public keys. A static capability token — provision stamps a hash, the
# verb compares stdin — was rejected for exactly this: once typed on a
# host that is already listening, it is captured and good until rotation.
# A bounded window is a different trade from "good until rotation", and
# #115 accepted it: sudo's timestamp is the same window every Linux
# box's `sudo` already grants after a password.
#
# What this does NOT control: a root-compromised host. The scripts, the
# PAM stack and the journal are all the host's, so a host with root
# already taken can fake the whole ceremony. This gate controls
# UNAUTHORIZED OPERATORS — someone with the deployer key and the mesh —
# and that is the threat the hardened profile (#86/#90) is shaped
# around.
#
# What it changes for every account on the host: sudo's password
# authentication IS the challenge. pam_unix leaves the sudo service, so a
# shadow password (the console-only one ops/console.nix gives deployer,
# or any other) does not authenticate to sudo on a fort. That is the
# posture of hardened.nix made true for sudo: no account here has a
# password that reaches root.
#
# The recipients come from `nixos/.sops.yaml`, not from a list kept
# beside it: the admin keys are the recipients every rule shares, which
# is the lines outside the `# BEGIN/END fort sops rule:` markers that
# `fort mint` maintains (the split cmd/fort/mint.go's
# sharedRecipients makes on the Go side). A rotated admin key therefore
# reaches the gate on the next rebuild, and a fort key never can.
#
# `sudo challenge` is the rehearsal: a PASSWD verb whose body prints
# success, with `timestamp_timeout=0` for that one command so it always
# runs the ceremony and leaves no window behind. It answers "does an
# admin key still reach this host, and does the operator still have one"
# BEFORE an incident asks.
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

  # The nonce store: one file per terminal, root-only, on tmpfs. The
  # nonce is the secret of the ceremony — a reader of this directory
  # answers every challenge — so 0700 root is the whole point, and
  # pam_exec's `seteuid` is what makes the scripts run as root (sudo's
  # real uid is the operator's, and a shell drops privileges when the
  # two differ).
  store = "/run/sfi-challenge";

  # Shared by both scripts: the journal line IS the audit, so the gate
  # fails closed when it cannot be written; and the per-terminal key.
  common = ''
    who=''${PAM_RUSER:-''${PAM_USER:-?}}
    tty=''${PAM_TTY:-}
    note() {
      logger -t sfi-ops-challenge -p auth.notice "$*" || {
        echo "error: the journal took no audit line — refused" >&2
        exit 70
      }
    }
    key=$(printf '%s' "$tty" | tr -c 'A-Za-z0-9._-' '_')
  '';

  issue = pkgs.writeShellApplication {
    name = "sfi-challenge-issue";
    runtimeInputs = [ pkgs.age pkgs.coreutils pkgs.util-linux ];
    text = ''
      # The first pam_exec line of the sudo service (`stdout`, `seteuid`).
      # Exit 0: a challenge is on the terminal and its nonce is stored.
      # Any other exit fails the requisite line, and sudo says no.
      ${common}

      # The ceremony is a conversation. No terminal means no operator —
      # refuse rather than find some other way to ask.
      if [ -z "$tty" ] || ! { : >"$tty"; } 2>/dev/null; then
        echo "error: the admin-key challenge needs a terminal to ask on"
        note "challenge REFUSED (no tty): user=$who service=''${PAM_SERVICE:-?}"
        exit 1
      fi

      # 10 bytes of urandom, hex. od reads a fixed count, so no pipe
      # closes early under pipefail.
      nonce=$(od -An -tx1 -N10 /dev/urandom | tr -d ' \n')
      ciphertext=$(printf '%s\n' "$nonce" | age --armor ${recipientFlags})

      install -d -m 0700 -o root -g root ${store}
      umask 077
      printf '%s %s\n' "$who" "$nonce" > "${store}/$key.tmp"
      mv "${store}/$key.tmp" "${store}/$key"

      note "challenge ISSUED: user=$who tty=$tty service=''${PAM_SERVICE:-?} ttl=${toString cfg.ttl}s"

      echo
      echo "== sudo on ${config.networking.hostName} needs the fleet admin age key"
      echo "   This terminal is then open for ${toString cfg.timeout} minutes (sudo -k closes it)."
      echo
      echo "   Decrypt this on a machine that holds an admin key:"
      echo
      printf '%s\n' "$ciphertext"
      echo
      echo "   age -d -i ~/.config/sops/age/keys.txt   # paste the block above, then ^D"
      echo
      echo "   Paste the line it prints at the prompt. The challenge expires in ${toString cfg.ttl}s."
    '';
  };

  verify = pkgs.writeShellApplication {
    name = "sfi-challenge-verify";
    runtimeInputs = [ pkgs.coreutils pkgs.util-linux ];
    text = ''
      # The second pam_exec line of the sudo service (`expose_authtok`,
      # `seteuid`): the typed answer arrives on stdin, with no newline.
      # Exit 0 is the proof; anything else is a failed attempt, and sudo
      # offers a fresh challenge or gives up.
      ${common}

      answer=""
      IFS= read -r answer || true
      answer=$(printf '%s' "$answer" | tr -d '[:space:]')

      file="${store}/$key"
      if [ -z "$tty" ] || [ ! -f "$file" ]; then
        echo "error: no challenge was issued on this terminal" >&2
        note "challenge FAILED (no challenge issued): user=$who tty=$tty"
        exit 1
      fi

      # One nonce, one attempt: the stored half goes before the compare,
      # so a second read of the same answer meets no challenge.
      read -r issued_to nonce < "$file"
      issued_at=$(stat -c %Y "$file")
      rm -f "$file"

      if [ "$issued_to" != "$who" ]; then
        echo "error: the challenge on this terminal was issued to another user" >&2
        note "challenge FAILED (user mismatch): user=$who issued_to=$issued_to tty=$tty"
        exit 1
      fi
      if [ $(( $(date +%s) - issued_at )) -gt ${toString cfg.ttl} ]; then
        echo "error: the challenge expired after ${toString cfg.ttl}s" >&2
        note "challenge FAILED (timeout): user=$who tty=$tty"
        exit 1
      fi

      # Compare digests, not the values: a wrong paste then costs the
      # same time whatever its first characters were.
      want=$(printf '%s' "$nonce" | sha256sum)
      got=$(printf '%s' "$answer" | sha256sum)
      if [ "$want" != "$got" ]; then
        note "challenge FAILED (mismatch): user=$who tty=$tty"
        exit 1
      fi

      note "challenge PASSED: user=$who tty=$tty — sudo opens this terminal for ${toString cfg.timeout}m"
    '';
  };

  pam_exec = "${config.security.pam.package}/lib/security/pam_exec.so";
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
        of these private halves to open sudo on a fort.
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

    timeout = lib.mkOption {
      type = lib.types.ints.between 10 60;
      default = 15;
      description = ''
        Minutes one proof keeps a terminal open: sudo's
        `timestamp_timeout` for deployer (one terminal, one window —
        `timestamp_type=tty`; `sudo -k` ends it early), and the life of
        the kubectl credential `admin-unlock` mints (ops/admin.nix),
        which the API server enforces. At least 10, because the
        TokenRequest API issues no token for less; at most 60, because a
        longer window stops being an event.
      '';
    };

    verbs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      example = [ "nixos-rollback" ];
      description = ''
        Verb names (`sfi.ops.commands` attribute names) this host puts
        behind the challenge in addition to the ones their own modules declare with
        `challenge = true`: their sudoers line requires authentication,
        so the first one on a terminal runs the ceremony and the next
        ones ride the window. Per-fort policy: a fort that wants the
        gate on a verb the framework leaves open names it here.
      '';
    };
  };

  config = {
    assertions = [{
      assertion = cfg.recipients != [ ];
      message = "sfi.ops.challenge.recipients is empty: no admin age key was found in nixos/.sops.yaml, so sudo could never authenticate on this host. Set it explicitly.";
    }];

    # The sudo service: the two pam_exec lines in place of pam_unix.
    # `requisite` on the first: no challenge on the terminal, no prompt
    # worth asking. `sufficient` on the second: a pass is the answer, a
    # fail falls through to NixOS's pam_deny. The orders are relative to
    # that deny rule, as pam.nix asks, so a renumbering upstream cannot
    # move these two behind it.
    security.pam.services.sudo = {
      unixAuth = false;
      rules.auth = {
        sfi-challenge-issue = {
          order = config.security.pam.services.sudo.rules.auth.deny.order - 200;
          control = "requisite";
          modulePath = pam_exec;
          args = [ "seteuid" "stdout" "quiet" "${issue}/bin/sfi-challenge-issue" ];
        };
        sfi-challenge-verify = {
          order = config.security.pam.services.sudo.rules.auth.deny.order - 100;
          control = "sufficient";
          modulePath = pam_exec;
          args = [ "seteuid" "expose_authtok" "quiet" "${verify}/bin/sfi-challenge-verify" ];
        };
      };
    };

    # The window and the words. timestamp_type=tty is sudo's default and
    # is written out because the design rests on it. lecture=never: the
    # ceremony is the lecture. passprompt replaces "[sudo] password for",
    # which would be a lie here; badpass_message says what happens next
    # (sudo re-runs the stack, so a fresh block follows).
    security.sudo.extraConfig = ''
      Defaults:deployer timestamp_timeout=${toString cfg.timeout}, timestamp_type=tty
      Defaults lecture=never
      Defaults passprompt="   nonce for %u@%h: "
      Defaults badpass_message="that was not the nonce; a fresh challenge follows"
      # The rehearsal always runs the ceremony and leaves no window: a
      # zero timeout means sudo neither reads nor writes a timestamp for
      # this command.
      Defaults!/run/current-system/sw/bin/challenge timestamp_timeout=0
    '';

    systemd.tmpfiles.rules = [ "d ${store} 0700 root root -" ];

    # The rehearsal verb. It holds no reach of its own: sudo runs the
    # ceremony to reach it, and the body says it worked.
    sfi.ops.commands.challenge = {
      description = "Rehearse the admin-key challenge: sudo runs the ceremony to reach this verb, which prints success. Changes nothing, opens no window";
      challenge = true;
      script = ''
        echo "success: the admin key answered. This rehearsal left no window open; the next PASSWD sudo on this terminal asks again."
      '';
    };
  };
}
