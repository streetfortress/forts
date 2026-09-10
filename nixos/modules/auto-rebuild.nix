# Pull-based OS updates (#93): a root-side timer watches main and runs
# `nixos-rebuild switch --flake` when the host's config changes — flux for
# the k8s layer, this loop for the nix layer. No interactive root, no new
# SSH surface; deployer stays read-only on production hosts (one pinned
# sudo carve-out below) and the no-SSH end-state stays viable.
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
#   - Every-6h timer + an ExecCondition gate (a single `git ls-remote`)
#     that skips the run unless main's tip moved since the last ATTEMPT.
#     The gate is SHA-granular, not path-granular: any commit to the
#     monorepo — and most commits are fleet-layer manifests that don't
#     change this host's closure — moves the tip and buys a full refetch +
#     flake eval (~2.5s / 0.8 GB peak RSS on devbox; expect tens of
#     seconds on the shared-hypervisor VMs) ending in a no-op switch.
#     That's why the cadence is hours, not flux's minutes: at active-dev
#     commit rates a short timer degenerates into eval-per-commit ×3 VMs
#     for mostly-nix-irrelevant changes. Immediacy comes from
#     sfi-rebuild-now instead; quiet-period ticks are skipped free by the
#     gate. Skips are systemd "condition" results: one "gate: ...skipping"
#     journal line, no failure state, no Loki alert.
#   - Gating on last-attempted (not last-succeeded) rev means a failing
#     commit gets exactly one try — no eval-crash loop across ticks — and
#     the NixosUpgradeFailed alert fires once. The next push to main (or
#     `sfi-rebuild-now`) retries. If gitea is unreachable the gate lets the
#     run proceed and fail loudly rather than skipping silently forever.
#   - Escape hatch: `sudo sfi-rebuild-now` as deployer clears the
#     attempted-rev marker and starts nixos-upgrade.service immediately —
#     the moral equivalent of `flux reconcile --with-source`. The sudoers
#     rule is NOPASSWD on exactly that store-linked script; the host still
#     only ever pulls vetted main, so deployer gains no write path.
#   - Auth reuses the host's flux-git-deploy-key (read-only deploy key,
#     already in every flux host's sops file) via GIT_SSH_COMMAND; the
#     pinned gitea host key matches flux-bootstrap.nix.
#   - allowReboot off: kernel/major bumps go through the repave path. This
#     module is the hybrid's "patch" leg — routine changes land within the
#     6h window, or immediately via sfi-rebuild-now; majors are deliberate.
#   - A failed switch fails nixos-upgrade.service and stays on the old
#     generation (atomic); the failure is visible in journald → Loki
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

  # ExecCondition: exit 1 = clean "condition failed" skip; exit 0 = run.
  # Inherits the service's GIT_SSH_COMMAND and $STATE_DIRECTORY. Recording
  # the rev here (not after the switch) is what caps failing commits at
  # one attempt each. Echoes land in the unit's journal (→ Loki) so gate
  # decisions are visible next to the rebuild output they explain.
  gate = pkgs.writeShellScript "auto-rebuild-gate" ''
    remote=$(${pkgs.git}/bin/git ls-remote ${repoUrl} refs/heads/main | ${pkgs.coreutils}/bin/cut -f1)
    if [ -z "$remote" ]; then
      echo "gate: cannot read main's tip from ${repoUrl}; proceeding (a fetch failure will alert)"
      exit 0
    fi
    marker="$STATE_DIRECTORY/last-attempted-rev"
    last=""
    if [ -e "$marker" ]; then last=$(${pkgs.coreutils}/bin/cat "$marker"); fi
    if [ "$remote" = "$last" ]; then
      echo "gate: main unchanged at ''${remote:0:7}; skipping"
      exit 1
    fi
    echo "gate: main at ''${remote:0:7} (last attempted: ''${last:-none}); rebuilding"
    printf '%s\n' "$remote" > "$marker"
    tmp=$(${pkgs.coreutils}/bin/mktemp ${textfileDir}/.nixos-attempted.prom.XXXXXX)
    printf 'node_nixos_attempted_revision_info{revision="%s"} 1\n' "$remote" > "$tmp"
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp"
    ${pkgs.coreutils}/bin/mv "$tmp" ${textfileDir}/nixos-attempted.prom
    exit 0
  '';

  # Companion to the gate's attempted-rev metric: recorded only when the
  # switch SUCCEEDS (postStart ≙ ExecStartPost, skipped on failure), so
  # applied != attempted reads as "last run failed" and applied vs flux's
  # gotk revision reads as nix/k8s drift — the #130 update-status row.
  # A no-op switch still "applies" its rev: the config content was
  # identical, so the rev is honestly what the host runs. Deliberately NOT
  # system.configurationRevision: embedding the rev in the closure would
  # churn a new generation on every commit, including fleet-only no-ops.
  recordApplied = pkgs.writeShellScript "auto-rebuild-record-applied" ''
    rev=$(${pkgs.coreutils}/bin/cat /var/lib/sfi-auto-rebuild/last-attempted-rev 2>/dev/null) || exit 0
    tmp=$(${pkgs.coreutils}/bin/mktemp ${textfileDir}/.nixos-applied.prom.XXXXXX)
    {
      printf 'node_nixos_applied_revision_info{revision="%s"} 1\n' "$rev"
      printf 'node_nixos_applied_timestamp_seconds %s\n' "$(${pkgs.coreutils}/bin/date +%s)"
    } > "$tmp"
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp"
    ${pkgs.coreutils}/bin/mv "$tmp" ${textfileDir}/nixos-applied.prom
  '';

  # `--wait` makes the escape hatch synchronous: the operator's prompt
  # blocks through eval+switch and the exit status reflects the unit's
  # result (fire-and-forget `start` reported nothing, which read as
  # success even when the unit failed).
  rebuildNow = pkgs.writeShellScriptBin "sfi-rebuild-now" ''
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
in
{
  options.sfi.auto-rebuild = {
    enable = lib.mkEnableOption "pull-based nixos-rebuild from the deployments repo";

    dates = lib.mkOption {
      type = lib.types.str;
      default = "00/6:30"; # 00:30, 06:30, 12:30, 18:30
      description = ''
        systemd calendar expression for the upgrade timer. Every 6h bounds
        drift at a workday, not a day, without eval-per-commit load (see
        header); sfi-rebuild-now covers "I know a change is waiting". Real
        production hosts can narrow this to a nightly window later.
      '';
    };

    randomizedDelaySec = lib.mkOption {
      type = lib.types.str;
      default = "10min";
      description = ''
        Timer jitter: staggers the fleet VMs' concurrent evals on the
        shared hypervisor when a tick finds new commits.
      '';
    };

    gitKnownHosts = lib.mkOption {
      type = lib.types.str;
      # Same pin as flux-bootstrap.nix gitKnownHosts.
      default = "[gitea.zen.lofi]:30022 ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAACAQClPt06+mIzU3p5SGeGELvfWqoxgLAtvnG4yAbLz/jvReNLyj44aoKC3ciIKQc+yH1UZIaghznzZ+k8ChdoVX6vnV1qQWa9270/tGcpFT9UcYsgIdfkGfWyy9Kh8SGw56qldK+wcX3tWzbZU1ZyzS5d0RJjDu62MqkomVJ4nbnMuZIdw6v3qiOm7q1VdorqtVgn/0QbcLmT8kHiGopqR/BUoCG4+b7utmgrxTuHoYvpfLNIJcYKu8DLEPbLAjSlt38Z+qK25usWxuS4ZHIE7+q4yGZfSYfyG768594OE7OKlcUrlH5nArvI9eCi0U2L+/sWSAKkSE5P8UEmlXiaVl6+XLQVI036w/t6VJO4+Z4gM2FaukH5+XMhZcngdoMY5h46rUC1zV6fepd/MpyaEh5JO9Kntfkbx7Q+mvqvJBMh5+2hLQx5Umm6dmv+y9oXll/PATzxVFxe5+D3O03u0R0cAqp010ddSqHPRS3FNhuyLfiW53L0t9kGKWJQJoMKLSZuCEBt+F+iTe3bVeaIbSu4RBClFA/cHC1+CAWb3OsScdkP7vNObB5dDjMqfd/BaAw8CEJp0wwbR/gXjK4RHQ0WzSFi7++09zbaEZ4cYJwsNuePrhTrFpOMWVM1RdO70eCihMH7aqgTsa95LxD8dCdifiECQuz877gCZ3uEk8ekbQ==";
      description = "SSH known_hosts line(s) for the git host.";
    };
  };

  imports = [ ./textfile-metrics.nix ]; # nixos-{attempted,applied}.prom

  config = lib.mkIf cfg.enable {
    # Also declared by flux-bootstrap.nix on flux hosts; identical empty
    # definitions merge cleanly, and non-flux hosts get it from here.
    sops.secrets.flux-git-deploy-key = { };

    system.autoUpgrade = {
      enable = true;
      flake = flakeRef;
      dates = cfg.dates;
      randomizedDelaySec = cfg.randomizedDelaySec;
      allowReboot = false;
    };

    systemd.services.nixos-upgrade = {
      # Upstream's script interpolates the flake ref UNQUOTED
      # (`--flake ''${cfg.flake}` joined by toString, tasks/auto-upgrade.nix),
      # so the `&` in ?ref=main&dir=nixos backgrounds a half command, turns
      # `dir=nixos#<host>` into a bare env assignment, and executes
      # `--upgrade` as a command — the unit fails on every run (caught live
      # by sfi-rebuild-now + NixosUpgradeFailed on staging's first repave).
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
        # Only on ExecStart success — see recordApplied.
        ExecStartPost = "${recordApplied}";
      };
    };

    environment.systemPackages = [ rebuildNow ];

    # The escape hatch for production-profile hosts, where deployer has no
    # wheel: run the same pull loop, now. Matched on the stable
    # current-system path so the rule survives generation switches. On a
    # host without a deployer account the rule simply never matches.
    security.sudo.extraRules = [{
      users = [ "deployer" ];
      commands = [{
        command = "/run/current-system/sw/bin/sfi-rebuild-now";
        options = [ "NOPASSWD" ];
      }];
    }];
  };
}
