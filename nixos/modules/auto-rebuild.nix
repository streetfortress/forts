# Pull-based OS updates (#93): a root-side timer watches main and runs
# `nixos-rebuild switch --flake` when the host's config changes — flux for
# the k8s layer, this loop for the nix layer. No interactive root, no new
# SSH surface; deployer stays read-only on production hosts (the pinned
# sudo carve-out comes from the ops framework, ops/ops-commands.nix) and
# the no-SSH end-state stays viable.
#
# Motivating incident: luxor's running WireGuard peer set silently drifted
# from wireguard/peers.json (production-test added, luxor never rebuilt),
# which broke the mesh path to a new cluster while every LAN path kept
# working. Config drift on hosts nobody rebuilds is invisible until
# something depends on the stale part.
#
# Mechanics:
#   - system.autoUpgrade with a git+ssh flake ref. Flakes copy the whole
#     repo tree (dir= only locates flake.nix), so the modules' relative
#     reads like ../../wireguard/peers.json work from the fetched source —
#     verified by eval'ing a peers-derived attr from the git URL (#93's
#     "wrinkle" checkbox). The bare nixosConfigurations entry is made
#     switch-capable by qcow-runtime.nix in the flake's vmModules.
#   - Minutely timer (flux's cadence) + an ExecCondition gate that is
#     PATH-granular: `git ls-remote` first (one round trip; unchanged tip
#     = free skip, which is nearly every tick), then, when the tip moved,
#     a shallow bare clone in the state dir answers whether the trees the
#     host closure actually reads — nixos/, wireguard/, fleet/clusters/
#     — changed since the last attempt. Most commits are fleet-layer
#     manifests: those are recorded as attempted+applied at the new tip
#     (the host honestly runs main@tip's nixos tree) and skipped without
#     an eval — "applied" is the rev the host has caught up to, not the
#     rev it was built from; that is "effective" (below), which only a
#     real switch advances. Only a closure-relevant commit buys the flake eval (~2.5s /
#     0.8 GB on devbox; tens of seconds on the VMs) and a switch — the
#     cost that used to justify an hours-long cadence. Merging a config
#     change is the rollout; sfi-nixos-rebuild remains the "now" lever
#     and the retry for a failed attempt.
#   - Gating on last-attempted (not last-succeeded) rev means a failing
#     commit gets exactly one try — no eval-crash loop across ticks — and
#     the NixosUpgradeFailed alert fires once. The next push to main (or
#     `sfi-nixos-rebuild`) retries. If gitea is unreachable the gate lets
#     the run proceed and fail loudly rather than skipping silently
#     forever.
#   - Escape hatch: `sudo sfi-nixos-rebuild` as deployer clears the
#     attempted-rev marker and starts nixos-upgrade.service immediately —
#     the moral equivalent of `flux reconcile --with-source`. Declared via
#     the ops framework (ops/ops-commands.nix), which generates the same
#     pinned NOPASSWD sudoers rule this module used to carry directly; the
#     host still only ever pulls vetted main, so deployer gains no write
#     path. The old name, `sfi-rebuild-now`, survives as a deprecation
#     shim (below) that execs the new verb.
#   - Auth reuses the host's flux-git-deploy-key (read-only deploy key,
#     already in every flux host's sops file) via GIT_SSH_COMMAND; the
#     pinned gitea host key matches flux-bootstrap.nix.
#   - allowReboot off: kernel/major bumps go through the repave path. This
#     module is the hybrid's "patch" leg — routine changes land within a
#     minute or two of merging; majors are deliberate.
#   - A failed switch fails nixos-upgrade.service and stays on the old
#     generation (atomic) — and so does an unreachable gitea (gate exit
#     255; nix would otherwise rebuild the cached clone and report
#     success); the failure is visible in journald → Loki
#     ({job="systemd"} SYSTEMD_UNIT="nixos-upgrade.service"). The
#     NixosUpgradeFailed rule in fleet/apps/luxor/monitoring/loki-rules.yaml
#     alerts on it.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.auto-rebuild;
  repoUrl = "ssh://git@gitea.zen.lofi:30022/sfi/deployments";
  textfileDir = "/var/lib/node-exporter-textfiles"; # textfile-metrics.nix
  flakeRef = "git+${repoUrl}?ref=main&dir=nixos#${config.networking.hostName}";
  knownHostsFile = pkgs.writeText "auto-rebuild-known-hosts" cfg.gitKnownHosts;

  # ExecCondition: exit 1 = clean "condition failed" skip; exit 0 = run;
  # exit 255 = the UNIT fails (systemd.service(5)). Inherits the service's
  # GIT_SSH_COMMAND and $STATE_DIRECTORY. Recording the rev here (not
  # after the switch) is what caps failing commits at one attempt each.
  # Echoes land in the unit's journal (→ Loki) so gate decisions are
  # visible next to the rebuild output they explain.
  #
  # An unreachable gitea is a unit FAILURE, not a pass-through: nix's git
  # fetcher only warns ("could not update local clone …; continuing with
  # the most recent version") and rebuilds the cached clone, so a host
  # that cannot reach gitea would otherwise report a green "Finished NixOS
  # Upgrade" forever while silently frozen — luxor did exactly that for
  # 12h+ on 2026-09-13 (resolver stuck, gitea.zen.lofi NXDOMAIN). Exit 255
  # trips NixosUpgradeFailed (loki-rules) the same as a failed switch.
  #
  # Path granularity: the trees below are everything the host closure
  # reads from the repo (the flake under nixos/, peers.json, and the
  # cluster's flux-instance.yaml). A tip that leaves them untouched is a
  # fleet-only commit: markers advance, no eval.
  closureTrees = [ "nixos" "wireguard" "fleet/clusters" ];
  gate = pkgs.writeShellScript "auto-rebuild-gate" ''
    git=${pkgs.git}/bin/git
    remote=$($git ls-remote ${repoUrl} refs/heads/main | ${pkgs.coreutils}/bin/cut -f1)
    if [ -z "$remote" ]; then
      echo "gate: cannot read main's tip from ${repoUrl} — failing the unit (a rebuild from the cached clone would hide this)"
      exit 255
    fi
    marker="$STATE_DIRECTORY/last-attempted-rev"
    tree_marker="$STATE_DIRECTORY/last-attempted-trees"
    last=""
    if [ -e "$marker" ]; then last=$(${pkgs.coreutils}/bin/cat "$marker"); fi
    if [ "$remote" = "$last" ]; then
      echo "gate: main unchanged at ''${remote:0:7}; skipping"
      exit 1
    fi

    # Shallow bare mirror of main, just enough to read tree ids.
    repo="$STATE_DIRECTORY/main.git"
    if [ ! -d "$repo" ]; then
      $git clone --quiet --bare --depth=1 --no-tags --branch main ${repoUrl} "$repo"
    else
      $git -C "$repo" fetch --quiet --depth=1 --no-tags origin +refs/heads/main:refs/heads/main
    fi
    trees=$($git -C "$repo" rev-parse ${lib.concatMapStringsSep " " (t: "\"$remote:${t}\"") closureTrees} \
      | ${pkgs.coreutils}/bin/sha256sum | ${pkgs.coreutils}/bin/cut -c1-16)
    last_trees=""
    if [ -e "$tree_marker" ]; then last_trees=$(${pkgs.coreutils}/bin/cat "$tree_marker"); fi

    record_attempt() {
      printf '%s\n' "$remote" > "$marker"
      printf '%s\n' "$trees" > "$tree_marker"
      tmp=$(${pkgs.coreutils}/bin/mktemp ${textfileDir}/.nixos-attempted.prom.XXXXXX)
      printf 'node_nixos_attempted_revision_info{revision="%s"} 1\n' "$remote" > "$tmp"
      ${pkgs.coreutils}/bin/chmod 0644 "$tmp"
      ${pkgs.coreutils}/bin/mv "$tmp" ${textfileDir}/nixos-attempted.prom
    }

    if [ -n "$last" ] && [ "$trees" = "$last_trees" ]; then
      echo "gate: main at ''${remote:0:7} leaves ${lib.concatStringsSep "/" closureTrees} untouched (fleet-only commit); recording as applied, no rebuild"
      record_attempt
      ${recordApplied}
      exit 1
    fi
    echo "gate: main at ''${remote:0:7} (last attempted: ''${last:-none}); closure inputs changed — rebuilding"
    record_attempt
    exit 0
  '';

  # Companion to the gate's attempted-rev metric: the rev the host has
  # caught up to. Recorded when the switch SUCCEEDS (ExecStartPost is
  # skipped on failure) AND by the gate's fleet-only skip, so applied !=
  # attempted reads as "last run failed" and applied vs flux's gotk
  # revision reads as nix/k8s drift — the #130 update-status row. A
  # no-op switch still "applies" its rev: the config content was
  # identical, so the rev is honestly what the host runs. Deliberately NOT
  # system.configurationRevision: embedding the rev in the closure would
  # churn a new generation on every commit, including fleet-only no-ops.
  # Carries no timestamp on purpose: with the minutely gate, applied
  # advances on every merge to main, and a "time since" derived from it
  # read as "this host just rebuilt" all day (host-fleet's stat sat at
  # <15 min through 8–11 fleet-only merges/host/day) — see recordEffective.
  recordApplied = pkgs.writeShellScript "auto-rebuild-record-applied" ''
    rev=$(${pkgs.coreutils}/bin/cat /var/lib/sfi-auto-rebuild/last-attempted-rev 2>/dev/null) || exit 0
    tmp=$(${pkgs.coreutils}/bin/mktemp ${textfileDir}/.nixos-applied.prom.XXXXXX)
    printf 'node_nixos_applied_revision_info{revision="%s"} 1\n' "$rev" > "$tmp"
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp"
    ${pkgs.coreutils}/bin/mv "$tmp" ${textfileDir}/nixos-applied.prom
  '';

  # The rev the running closure was actually built from, plus when the
  # switch that built it finished: ONLY after a successful ExecStart, never
  # from the gate. effective != applied is normal (fleet-only merges since
  # the last closure-relevant one); effective's timestamp is the honest
  # "time since this host last ran nixos-rebuild" — the soak-window stat
  # on host-fleet keys on it.
  recordEffective = pkgs.writeShellScript "auto-rebuild-record-effective" ''
    rev=$(${pkgs.coreutils}/bin/cat /var/lib/sfi-auto-rebuild/last-attempted-rev 2>/dev/null) || exit 0
    tmp=$(${pkgs.coreutils}/bin/mktemp ${textfileDir}/.nixos-effective.prom.XXXXXX)
    {
      printf 'node_nixos_effective_revision_info{revision="%s"} 1\n' "$rev"
      printf 'node_nixos_effective_timestamp_seconds %s\n' "$(${pkgs.coreutils}/bin/date +%s)"
    } > "$tmp"
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp"
    ${pkgs.coreutils}/bin/mv "$tmp" ${textfileDir}/nixos-effective.prom
  '';

  # Old-name compatibility shim: muscle memory and existing runbooks say
  # `sudo sfi-rebuild-now`; point them at the framework name and run it
  # anyway. Gets its own pinned sudoers rule below so the old invocation
  # keeps working for deployer.
  rebuildNowShim = pkgs.writeShellApplication {
    name = "sfi-rebuild-now";
    text = ''
      echo "sfi-rebuild-now is deprecated — use 'sudo sfi-nixos-rebuild' (running it for you)" >&2
      exec /run/current-system/sw/bin/sfi-nixos-rebuild "$@"
    '';
  };
in
{
  options.sfi.auto-rebuild = {
    enable = lib.mkEnableOption "pull-based nixos-rebuild from the deployments repo";

    dates = lib.mkOption {
      type = lib.types.str;
      default = "minutely";
      description = ''
        systemd.time(7) calendar for the watch timer. Minutely is flux's
        cadence: the gate's ls-remote makes an unchanged tip a free skip,
        and path granularity makes fleet-only commits a marker update —
        only closure-relevant commits cost an eval + switch.
      '';
    };

    randomizedDelaySec = lib.mkOption {
      type = lib.types.str;
      default = "15s";
      description = "Jitter so the fleet's ticks (and their ls-remotes against gitea) don't line up.";
    };

    gitKnownHosts = lib.mkOption {
      type = lib.types.str;
      # Same pin as flux-bootstrap.nix gitKnownHosts.
      default = "[gitea.zen.lofi]:30022 ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAACAQClPt06+mIzU3p5SGeGELvfWqoxgLAtvnG4yAbLz/jvReNLyj44aoKC3ciIKQc+yH1UZIaghznzZ+k8ChdoVX6vnV1qQWa9270/tGcpFT9UcYsgIdfkGfWyy9Kh8SGw56qldK+wcX3tWzbZU1ZyzS5d0RJjDu62MqkomVJ4nbnMuZIdw6v3qiOm7q1VdorqtVgn/0QbcLmT8kHiGopqR/BUoCG4+b7utmgrxTuHoYvpfLNIJcYKu8DLEPbLAjSlt38Z+qK25usWxuS4ZHIE7+q4yGZfSYfyG768594OE7OKlcUrlH5nArvI9eCi0U2L+/sWSAKkSE5P8UEmlXiaVl6+XLQVI036w/t6VJO4+Z4gM2FaukH5+XMhZcngdoMY5h46rUC1zV6fepd/MpyaEh5JO9Kntfkbx7Q+mvqvJBMh5+2hLQx5Umm6dmv+y9oXll/PATzxVFxe5+D3O03u0R0cAqp010ddSqHPRS3FNhuyLfiW53L0t9kGKWJQJoMKLSZuCEBt+F+iTe3bVeaIbSu4RBClFA/cHC1+CAWb3OsScdkP7vNObB5dDjMqfd/BaAw8CEJp0wwbR/gXjK4RHQ0WzSFi7++09zbaEZ4cYJwsNuePrhTrFpOMWVM1RdO70eCihMH7aqgTsa95LxD8dCdifiECQuz877gCZ3uEk8ekbQ==";
      description = "SSH known_hosts line(s) for the git host.";
    };
  };

  imports = [
    ./textfile-metrics.nix # nixos-{attempted,applied,effective}.prom
    ./ops/ops-commands.nix # sfi-nixos-rebuild is an ops verb
    ./ops/systemd.nix # sfi-systemd-restart nixos-upgrade (#212)
  ];

  config = lib.mkIf cfg.enable {
    # Also declared by flux-bootstrap.nix on flux hosts; identical empty
    # definitions merge cleanly, and non-flux hosts get it from here.
    sops.secrets.flux-git-deploy-key = { };

    # sfi-nixos-rebuild is the curated path (clears the attempted-rev
    # marker first); the bare restart is the escape hatch when that verb
    # itself is what misbehaves.
    sfi.ops.restartableUnits = [ "nixos-upgrade" ];

    system.autoUpgrade = {
      enable = true;
      flake = flakeRef;
      dates = cfg.dates;
      randomizedDelaySec = cfg.randomizedDelaySec;
      allowReboot = false;
    };
    # systemd's default 1-minute AccuracySec would coalesce a minutely
    # timer into "sometime this minute"; tighten so the cadence is real.
    systemd.timers.nixos-upgrade.timerConfig.AccuracySec = "5s";

    systemd.services.nixos-upgrade = {
      # Upstream's script interpolates the flake ref UNQUOTED
      # (`--flake ''${cfg.flake}` joined by toString, tasks/auto-upgrade.nix),
      # so the `&` in ?ref=main&dir=nixos backgrounds a half command, turns
      # `dir=nixos#<host>` into a bare env assignment, and executes
      # `--upgrade` as a command — the unit fails on every run (caught live
      # by the rebuild-now escape hatch + NixosUpgradeFailed on staging's
      # first repave).
      # The allowReboot=false branch upstream is this single line, so
      # replace it wholesale with a properly escaped ref; --refresh bypasses
      # the flake fetcher's cache so every run sees main's tip (upstream
      # adds it too, but to the flags this override ignores).
      script = lib.mkForce ''
        ${config.system.build.nixos-rebuild}/bin/nixos-rebuild switch --refresh --flake ${lib.escapeShellArg flakeRef}
      '';
      environment.GIT_SSH_COMMAND = lib.concatStringsSep " " [
        "${pkgs.openssh}/bin/ssh"
        "-i ${config.sops.secrets.flux-git-deploy-key.path}"
        "-o UserKnownHostsFile=${knownHostsFile}"
        "-o IdentitiesOnly=yes"
      ];
      serviceConfig = {
        StateDirectory = "sfi-auto-rebuild"; # /var/lib/sfi-auto-rebuild
        ExecCondition = "${gate}";
        # Only on ExecStart success — see recordApplied / recordEffective.
        ExecStartPost = [ "${recordApplied}" "${recordEffective}" ];
      };
    };

    # The escape hatch for production-profile hosts, where deployer has no
    # wheel: run the same pull loop, now. The framework generates the
    # PATH wrapper, the pinned NOPASSWD sudoers rule, and the sfi-ops
    # index row. `--wait` makes it synchronous: the operator's prompt
    # blocks through eval+switch and the exit status reflects the unit's
    # result (fire-and-forget `start` reported nothing, which read as
    # success even when the unit failed).
    #
    # `--from <ip>` is the hatch under the hatch: pull and switch from a
    # gitea reached BY ADDRESS, bypassing the name. The name is pinned in
    # /etc/hosts (lab-dns.nix) and moves with gitea; a fort pinned to a
    # host that is gone — the interim never came up, or the flip has to
    # be rolled back to the previous host — cannot fetch the config that would
    # re-pin it, and without this verb the only way forward is the
    # provider console. The source is still authenticated: the deploy
    # key dials `git@<ip>:30022` with HostKeyAlias set to the pinned
    # known_hosts entry, so any address may be named but only the real
    # gitea's host key passes (an alias is matched verbatim, brackets
    # and port included — checked against gitea 2026-09-18). `--ref`
    # takes a branch so a fix can be tried before it is on main; the
    # next timer run converges back to main regardless.
    sfi.ops.commands.nixos-rebuild = {
      description = "Clear the last-attempted marker and pull/switch to main now via nixos-upgrade.service; --from <ip> [--ref <branch>] pulls from a gitea by address (the name is pinned and moves with gitea)";
      script = ''
        from="" ref=main
        while [ $# -gt 0 ]; do
          case "$1" in
            --from) from=$2; shift 2 ;;
            --ref) ref=$2; shift 2 ;;
            *) echo "usage: sfi-nixos-rebuild [--from <ip> [--ref <branch>]]" >&2; exit 2 ;;
          esac
        done
        if [ -n "$from" ]; then
          case "$from" in
            *[!0-9.]*|"") echo "--from: want an IPv4 address, got '$from'" >&2; exit 2 ;;
          esac
          case "$ref" in
            *[!A-Za-z0-9._/-]*|"") echo "--ref: want a branch name, got '$ref'" >&2; exit 2 ;;
          esac
          url="ssh://git@$from:30022/sfi/deployments"
          echo "pulling $ref from $url (host key checked as [gitea.zen.lofi]:30022) and switching..."
          export GIT_SSH_COMMAND="${lib.concatStringsSep " " [
            "${pkgs.openssh}/bin/ssh"
            "-i ${config.sops.secrets.flux-git-deploy-key.path}"
            "-o UserKnownHostsFile=${knownHostsFile}"
            "-o IdentitiesOnly=yes"
            "-o HostKeyAlias=[gitea.zen.lofi]:30022"
          ]}"
          ${config.system.build.nixos-rebuild}/bin/nixos-rebuild switch --refresh \
            --flake "git+$url?ref=$ref&dir=nixos#${config.networking.hostName}"
          echo "switched from $from ($ref); the timer converges back to main from wherever gitea.zen.lofi now points"
          exit 0
        fi
        if [ "$ref" != main ]; then
          echo "--ref needs --from (the unit pulls main by name)" >&2; exit 2
        fi
        marker=/var/lib/sfi-auto-rebuild/last-attempted-rev
        if [ -e "$marker" ]; then
          echo "cleared last-attempted marker ($(${pkgs.coreutils}/bin/cat "$marker")) — the gate will report 'none' and rebuild"
          ${pkgs.coreutils}/bin/rm -f "$marker"
        fi
        echo "pulling main via nixos-upgrade.service (logs: {job=\"systemd\"} SYSTEMD_UNIT=\"nixos-upgrade.service\")..."
        if ${pkgs.systemd}/bin/systemctl start --wait nixos-upgrade.service; then
          echo "nixos-upgrade: succeeded"
        else
          rc=$?
          echo "nixos-upgrade: FAILED (rc=$rc) — see the unit's logs"
          exit "$rc"
        fi
      '';
    };

    environment.systemPackages = [ rebuildNowShim ];

    # Pinned rule for the deprecation shim, same pattern the framework
    # generates for the real verb; drop once runbooks/muscle memory have
    # moved to `sudo sfi-nixos-rebuild`.
    security.sudo.extraRules = [{
      users = [ "deployer" ];
      commands = [{
        command = "/run/current-system/sw/bin/sfi-rebuild-now";
        options = [ "NOPASSWD" ];
      }];
    }];
  };
}
