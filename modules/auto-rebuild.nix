# Pull-based OS updates (#93): a root-side timer watches main and runs
# `nixos-rebuild switch --flake` when the host's config changes — flux for
# the k8s layer, this loop for the nix layer. No interactive root, no new
# SSH surface; deployer stays read-only on every fort (the pinned
# sudo carve-out comes from the ops framework, ops/ops-commands.nix) and
# the no-SSH end-state stays viable.
#
# Motivating incident: a hub's running WireGuard peer set silently drifted
# from mesh/peers.json (a fort added, the hub never rebuilt),
# which broke the mesh path to a new cluster while every LAN path kept
# working. Config drift on hosts nobody rebuilds is invisible until
# something depends on the stale part.
#
# Mechanics:
#   - system.autoUpgrade with a git+ssh flake ref. Flakes copy the whole
#     repo tree (dir= only locates flake.nix), so the modules' relative
#     reads like ../../mesh/peers.json work from the fetched source —
#     verified by eval'ing a peers-derived attr from the git URL (#93's
#     "wrinkle" checkbox). The bare nixosConfigurations entry is made
#     switch-capable by runtime/qemu.nix in the flake's vmModules.
#   - Minutely timer (flux's cadence) + an ExecCondition gate that is
#     PATH-granular: `git ls-remote` first (one round trip; unchanged tip
#     = free skip, which is nearly every tick), then, when the tip moved,
#     a shallow bare clone in the state dir answers whether the trees the
#     host closure actually reads — nixos/, mesh/, flux/clusters/
#     — changed since the last attempt. Most commits are fleet-layer
#     manifests: those are recorded as attempted+applied at the new tip
#     (the host honestly runs main@tip's nixos tree) and skipped without
#     an eval — "applied" is the rev the host has caught up to, not the
#     rev it was built from; that is "effective" (below), which only a
#     real switch advances. Only a closure-relevant commit buys the flake eval (~2.5s /
#     0.8 GB on a lab machine; tens of seconds on the VMs) and a switch — the
#     cost that used to justify an hours-long cadence. Merging a config
#     change is the rollout; nixos-pull (the HANDS-ON rung,
#     ops/ops-commands.nix) remains the "now" lever and the retry for a
#     failed attempt.
#   - Gating on last-attempted (not last-succeeded) rev means a failing
#     commit gets exactly one try — no eval-crash loop across ticks — and
#     the NixosUpgradeFailed alert fires once. The next push to main (or
#     `nixos-pull`) retries. If the forge is unreachable the gate lets
#     the run proceed and fail loudly rather than skipping silently
#     forever.
#   - The way back: `sudo nixos-rollback <generation>` (ops/nixos-
#     rollback.nix) switches to a prior generation and PINS the loop
#     there — the gate skips while the host is on that generation, a
#     textfile metric shows the hold, and this verb lifts it.
#   - Escape hatch: `sudo nixos-pull` as deployer clears the
#     attempted-rev marker and starts nixos-upgrade.service immediately —
#     the moral equivalent of `flux reconcile --with-source`. Declared via
#     the ops framework (ops/ops-commands.nix), which generates the same
#     pinned NOPASSWD sudoers rule this module used to carry directly; the
#     host still only ever pulls vetted main, so deployer gains no write
#     path. The verb is not named `nixos-rebuild`: that is NixOS's own
#     tool on every host's PATH, and the system path resolves a name
#     collision silently (forts#151).
#   - Auth is the estate's clone route (sfi.estate.authRoute, estate.nix;
#     forts#167). On the ssh route it reuses the host's
#     flux-git-deploy-key (read-only deploy key, already in every flux
#     host's sops file) via GIT_SSH_COMMAND, and the pinned forge host
#     key matches flux-bootstrap.nix. On the anonymous route — an
#     https:// repoUrl, a public repo — there is no deploy key, no
#     GIT_SSH_COMMAND and no host key: git and nix's fetcher clone with
#     no credential, and TLS trust is the system roots' (plus the
#     estate's own CA where it carries one, estate.nix).
#   - allowReboot off: kernel/major bumps go through the re-provision
#     path. This module is the hybrid's "patch" leg — routine changes land
#     within a minute or two of merging; majors are deliberate.
#   - A failed switch fails nixos-upgrade.service and stays on the old
#     generation (atomic) — and so does an unreachable forge (gate exit
#     255; nix would otherwise rebuild the cached clone and report
#     success); the failure is visible in journald (and a log shipper's stream)
#     ({job="systemd"} SYSTEMD_UNIT="nixos-upgrade.service"). The
#     NixosUpgradeFailed rule of a monitoring hub
#     alerts on it. switch-to-configuration exit 4 is NOT a failed
#     switch: it means "switched, some units failed to start" — the
#     profile link names the new generation and the host runs it — so
#     the unit's script maps it to success with a warning, the records
#     below run, and the ledger keeps the generation (#182). The failed
#     units page through their own alerts. Every other non-zero exit
#     stays a failure.
{ config, lib, pkgs, ... }:

let
  cfg = config.sfi.auto-rebuild;
  repoUrl = cfg.repoUrl;
  textfileDir = "/var/lib/node-exporter-textfiles"; # textfile-metrics.nix
  flakeRef = "git+${repoUrl}?ref=main&dir=nixos#${config.networking.hostName}";
  knownHostsFile = pkgs.writeText "auto-rebuild-known-hosts" cfg.gitKnownHosts;

  # repoUrl, read apart (#77). `--from <ip>` (the verb, below) replaces
  # the ADDRESS of the forge and nothing else, so the user, the port, the
  # repo path and the known_hosts alias all have to come from the same
  # option the timer builds flakeRef from. They were literals — the
  # first estate's repo path and its forge's known_hosts alias — which
  # made the recovery hatch of EVERY estate pull the first estate's
  # repo and check a host key that is not its own forge's. The fixture
  # estates (tests/estate, tests/github-estate) name other repos.
  #
  # Two schemes, two routes (forts#167): ssh:// clones with the deploy
  # key and pins the forge's host key; https:// clones a public repo
  # anonymously. The scheme is read here, where the clone is built, and
  # must agree with the estate's declared route (the assertion below):
  # gitKnownHosts is an ssh known_hosts line and GIT_SSH_COMMAND carries
  # the deploy key, both meaningless on an https URL. A repoUrl neither
  # pattern can read is a config error, and the eval — `fort check`, in
  # CI — is where it must appear. The one remaining verb of a stranded
  # fort is the wrong place to learn it.
  anonymous = config.sfi.estate.authRoute == "none";
  repo =
    let
      ssh = builtins.match "ssh://(([^@/]*)@)?([^@:/]+)(:([0-9]+))?(/.+)" repoUrl;
      https = builtins.match "https://([^@:/]+)(:([0-9]+))?(/.+)" repoUrl;
    in if ssh != null then rec {
      scheme = "ssh";
      user = let u = builtins.elemAt ssh 1; in if u == null || u == "" then "git" else u;
      host = builtins.elemAt ssh 2;
      port = builtins.elemAt ssh 4;
      path = builtins.elemAt ssh 5;
      # known_hosts spells a non-default port as [host]:port, and ssh
      # matches a HostKeyAlias against that text verbatim — brackets and
      # port included (checked against gitea 2026-09-18). The alias has
      # to name a line of gitKnownHosts or the fetch fails closed.
      hostKeyAlias = if port == null then host else "[${host}]:${port}";
      # The address is the one hole: the verb passes a shell $from.
      urlFor = address: "ssh://${user}@${address}${lib.optionalString (port != null) ":${port}"}${path}";
    } else if https != null then {
      scheme = "https";
      host = builtins.elemAt https 0;
      port = builtins.elemAt https 2;
      path = builtins.elemAt https 3;
      # No urlFor and no hostKeyAlias: an https clone by address has no
      # pin to check against — the certificate names the forge, not its
      # address — so `--from` is refused on this route (below).
    } else
      throw "sfi.auto-rebuild.repoUrl: want ssh://[user@]host[:port]/owner/name (the deploy-key route) or https://host[:port]/owner/name (a public repo, cloned anonymously), got '${repoUrl}'";

  # ExecCondition: exit 1 = clean "condition failed" skip; exit 0 = run;
  # exit 255 = the UNIT fails (systemd.service(5)). Inherits the service's
  # GIT_SSH_COMMAND and $STATE_DIRECTORY. Recording the rev here (not
  # after the switch) is what caps failing commits at one attempt each.
  # Echoes land in the unit's journal so gate decisions are
  # visible next to the rebuild output they explain.
  #
  # An unreachable forge is a unit FAILURE, not a pass-through: nix's git
  # fetcher only warns ("could not update local clone …; continuing with
  # the most recent version") and rebuilds the cached clone, so a host
  # that cannot reach the forge would otherwise report a green "Finished NixOS
  # Upgrade" forever while silently frozen — a hub did exactly that for
  # 12h+ on 2026-09-13 (resolver stuck, the forge's name NXDOMAIN). Exit 255
  # trips a hub's NixosUpgradeFailed rule the same as a failed switch.
  #
  # Path granularity: the trees below are everything the host closure
  # reads from the repo (the flake under nixos/, peers.json, and the
  # cluster's flux-instance.yaml). A tip that leaves them untouched is a
  # fleet-only commit: markers advance, no eval.
  closureTrees = [ "nixos" "mesh" "flux/clusters" ];
  gate = pkgs.writeShellScript "auto-rebuild-gate" ''
    # node_nixos_generations (#423): what is on disk for nixos-rollback
    # to choose from, and the thing nix.gc (below) actually bounds. Written
    # first so it is fresh whatever the gate decides next — a pin skip, a
    # failed ls-remote — and minutely, so a collection shows within the
    # tick rather than at the next switch.
    gens=$(${pkgs.coreutils}/bin/ls /nix/var/nix/profiles | ${pkgs.gnugrep}/bin/grep -c '^system-[0-9][0-9]*-link$' || true)
    tmp=$(${pkgs.coreutils}/bin/mktemp ${textfileDir}/.nixos-generations.prom.XXXXXX)
    printf 'node_nixos_generations %s\n' "''${gens:-0}" > "$tmp"
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp"
    ${pkgs.coreutils}/bin/mv "$tmp" ${textfileDir}/nixos-generations.prom

    # A pin (ops/nixos-rollback.nix) holds the host on the generation it
    # names; it is stale — and dropped — the moment the host is on any
    # other generation, so an older generation's nixos-pull (which
    # does not know pins) can never strand the host behind one.
    pin="$STATE_DIRECTORY/pinned-generation"
    if [ -e "$pin" ]; then
      pinned=$(${pkgs.coreutils}/bin/cat "$pin")
      current=$(${pkgs.coreutils}/bin/readlink /nix/var/nix/profiles/system | ${pkgs.gnused}/bin/sed -n 's/^system-\([0-9][0-9]*\)-link$/\1/p')
      if [ "$pinned" = "$current" ]; then
        echo "gate: pinned to generation $pinned (nixos-rollback); skipping — sudo nixos-pull lifts the pin"
        exit 1
      fi
      echo "gate: pin to generation $pinned is stale (host is on $current); lifting it"
      ${pkgs.coreutils}/bin/rm -f "$pin" ${textfileDir}/nixos-pinned.prom
    fi

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
    # Ledger for nixos-rollback's menu: which main commit each
    # generation was built from. The switch has happened by now, so the
    # profile link names the new generation. Append-only; a generation
    # that nix-env has deleted simply stops being listed.
    gen=$(${pkgs.coreutils}/bin/readlink /nix/var/nix/profiles/system | ${pkgs.gnused}/bin/sed -n 's/^system-\([0-9][0-9]*\)-link$/\1/p')
    if [ -n "$gen" ]; then
      printf '%s %s %s\n' "$gen" "$rev" "$(${pkgs.coreutils}/bin/date +%s)" >> /var/lib/sfi-auto-rebuild/generations
    fi
    tmp=$(${pkgs.coreutils}/bin/mktemp ${textfileDir}/.nixos-effective.prom.XXXXXX)
    {
      printf 'node_nixos_effective_revision_info{revision="%s"} 1\n' "$rev"
      printf 'node_nixos_effective_timestamp_seconds %s\n' "$(${pkgs.coreutils}/bin/date +%s)"
    } > "$tmp"
    ${pkgs.coreutils}/bin/chmod 0644 "$tmp"
    ${pkgs.coreutils}/bin/mv "$tmp" ${textfileDir}/nixos-effective.prom
  '';

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
      description = "Jitter so the fleet's ticks (and their ls-remotes against the forge) don't line up.";
    };

    # Retention (#423). The loop makes a generation per closure-relevant
    # merge and nothing else ever removes one, so the loop owns the
    # bound too. The window is the rollback depth: nixos-rollback can
    # reach any generation younger than this (the running one — and so a
    # pinned one — is never collected, whatever its age). Measured
    # 2026-09-22: stores 3.4–4.3 GiB, growing 0.2–0.4 GiB/week, 8–15
    # generations per host — a month is many incidents' worth and ~2 GiB.
    gc = {
      olderThan = lib.mkOption {
        type = lib.types.str;
        default = "30d";
        description = "nix-collect-garbage --delete-older-than window: generations (and their closures) younger than this survive. This IS nixos-rollback's reach.";
      };
      dates = lib.mkOption {
        type = lib.types.str;
        default = "weekly";
        description = "systemd.time(7) calendar for the collection.";
      };
      bootEntries = lib.mkOption {
        type = lib.types.int;
        default = 30;
        description = "Bootloader menu entries kept (grub / systemd-boot configurationLimit); sized to hold a month of generations at the observed rate.";
      };
    };

    repoUrl = lib.mkOption {
      type = lib.types.str;
      example = "ssh://git@forge.example:22/acme/estate.git";
      description = ''
        The estate's git URL: what the gate reads main's tip from and
        what the rebuild clones. The host file sets it; `fort scaffold`
        renders it from estate.toml. ssh://[user@]host[:port]/owner/name
        clones with the deploy key fort mint registered, and the module
        reads the host, the port and the path back out of it for
        `nixos-pull --from` (#77). https://host[:port]/owner/name is the
        anonymous route (forts#167): a public repo, no credential, no
        host key, and no `--from`. The scheme must agree with the
        estate's route (sfi.estate.authRoute); the eval fails when it
        does not, or when neither pattern reads the URL.
      '';
    };
    gitKnownHosts = lib.mkOption {
      type = lib.types.str;
      default = config.sfi.estate.knownHosts;
      defaultText = "estate.toml's `[source] known_hosts` (sfi.estate.knownHosts)";
      description = ''
        SSH known_hosts line(s) for the git host, on the ssh route. One
        line must name the host and port of repoUrl, spelled as
        known_hosts spells them ([host]:port for a port other than 22):
        `nixos-pull --from <ip>` checks the host key against that line,
        and a forge the file does not name has no recovery path. Unread
        on the anonymous route.
      '';
    };
  };

  imports = [
    ./estate.nix # sfi.estate.authRoute, sfi.estate.knownHosts
    ./textfile-metrics.nix # nixos-{attempted,applied,effective}.prom
    ./ops/ops-commands.nix # nixos-pull is an ops verb
    ./ops/nixos-rollback.nix # nixos-rollback: prior generation + pin
    ./ops/nixos-revision.nix # nixos-revision: what the host runs (#185)
  ];

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = (repo.scheme == "https") == anonymous;
        message = "sfi.auto-rebuild.repoUrl = \"${repoUrl}\" is on the ${repo.scheme} scheme, and estate.toml's [source] route is ${config.sfi.estate.authRoute}: the rebuild loop clones the URL the way the estate's route says (ssh:// with the deploy key, https:// anonymously; forts#167). fort scaffold renders repoUrl from estate.toml's [source] repo — set it to that URL";
      }
    ];

    # The deploy key, on the ssh route: also declared by flux-bootstrap.nix
    # on flux hosts; identical empty definitions merge cleanly, and
    # non-flux hosts get it from here. The anonymous route has none
    # (forts#167), and fort mint minted none into the host's sops file.
    sops.secrets = lib.optionalAttrs (!anonymous) { flux-git-deploy-key = { }; };

    # Retention — see the gc options above. Collection and optimisation
    # are root's own timers, sudo-free and not verbs: nothing about them
    # is a decision an operator makes on a host. The pull loop's own GC
    # roots (the in-flight build) are honoured by nix-collect-garbage, so
    # the two timers can overlap.
    nix.gc = {
      automatic = true;
      dates = cfg.gc.dates;
      randomizedDelaySec = "45min";
      options = "--delete-older-than ${cfg.gc.olderThan}";
    };
    # Adjacent generations share almost their whole closure; hard-linking
    # identical store files is the cheap half of the store's size.
    nix.optimise.automatic = true;
    # Both bootloaders' options exist whether or not that loader is the
    # host's (qcow/linode: grub; baremetal: systemd-boot); the inactive
    # one is inert.
    boot.loader.grub.configurationLimit = lib.mkDefault cfg.gc.bootEntries;
    boot.loader.systemd-boot.configurationLimit = lib.mkDefault cfg.gc.bootEntries;

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
      # first re-provision).
      # The allowReboot=false branch upstream is this single line, so
      # replace it wholesale with a properly escaped ref; --refresh bypasses
      # the flake fetcher's cache so every run sees main's tip (upstream
      # adds it too, but to the flags this override ignores).
      #
      # Exit 4 is a switch that happened (#182). nixos-rebuild runs
      # switch-to-configuration through `systemd-run --wait`
      # (nixos-rebuild-switch-to-configuration.service) and exits with
      # that unit's status, and switch-to-configuration exits 4 when the
      # switch completed but a unit failed to start during activation —
      # the profile link already names the new generation and the host
      # runs it. Treating 4 as a failure skipped ExecStartPost, so the
      # generations ledger and the applied/effective metrics stopped at
      # the last clean activation, nixos-revision answered "no revision
      # recorded" for pull-built generations, nixos-rollback's menu lost
      # their commits, and NixosUpgradeFailed fired on every merge. Map 4
      # to success with a loud warning so the records run; the failed
      # units page through their own alerts. Every other non-zero exit
      # stays a failure.
      script = lib.mkForce ''
        rc=0
        ${config.system.build.nixos-rebuild}/bin/nixos-rebuild switch --refresh --flake ${lib.escapeShellArg flakeRef} || rc=$?
        if [ "$rc" -eq 4 ]; then
          echo "warning: switch-to-configuration exited 4 — the host SWITCHED to the new generation, but some units failed to start during activation (#182)"
          echo "warning: recording the generation as applied and effective; check 'systemctl --failed' — the failed units alert on their own"
        elif [ "$rc" -ne 0 ]; then
          exit "$rc"
        fi
      '';
      # The clone's credential, on the ssh route alone: the anonymous
      # route sets nothing, and git and nix's fetcher clone the public
      # repo over https with the system roots.
      environment = lib.optionalAttrs (!anonymous) {
        GIT_SSH_COMMAND = lib.concatStringsSep " " [
          "${pkgs.openssh}/bin/ssh"
          "-i ${config.sops.secrets.flux-git-deploy-key.path}"
          "-o UserKnownHostsFile=${knownHostsFile}"
          "-o IdentitiesOnly=yes"
        ];
      };
      serviceConfig = {
        StateDirectory = "sfi-auto-rebuild"; # /var/lib/sfi-auto-rebuild
        ExecCondition = "${gate}";
        # Only on ExecStart success — see recordApplied / recordEffective.
        # "Success" includes a switch whose activation left units failed:
        # the script above maps switch-to-configuration's exit 4 to 0, so
        # these records run for every generation the host switched to (#182).
        ExecStartPost = [ "${recordApplied}" "${recordEffective}" ];
      };
    };

    # The HANDS-ON rung's first verb (forts#151): run the same pull loop,
    # now, as deployer, who has no wheel. The framework generates the
    # PATH wrapper, the pinned NOPASSWD sudoers rule, and the `ops`
    # index row. `--wait` makes it synchronous: the operator's prompt
    # blocks through eval+switch and the exit status reflects the unit's
    # result (fire-and-forget `start` reported nothing, which read as
    # success even when the unit failed).
    #
    # `--from <ip>` is the hatch under the hatch: pull and switch from a
    # forge reached BY ADDRESS, bypassing the name. The name is pinned in
    # /etc/hosts (lab-dns.nix, the forge pin) and moves with the forge; a fort pinned to a
    # host that is gone — the interim never came up, or the flip has to
    # be rolled back to the previous host — cannot fetch the config that would
    # re-pin it, and without this verb the only way forward is the
    # provider console. The address is all it replaces: the user, the
    # port, the repo path and the known_hosts alias come from repoUrl
    # (the `repo` parse above, #77), so the verb pulls THIS estate's
    # repo. The source is still authenticated: the deploy key dials the
    # address with HostKeyAlias set to the forge's pinned known_hosts
    # entry, so any address may be named but only the real forge's host
    # key passes. On the anonymous route (forts#167) the verb refuses
    # `--from`: an https clone by address has no host key to check, and
    # the forge's certificate names the forge, not its address, so the
    # TLS check fails and nothing can make it pass short of disabling
    # it — which this verb will not do. A fort on that route that
    # cannot resolve its forge waits for the name, as any https client
    # does. The branch
    # is always main — this verb has no way to name another one (#76).
    # It used to take `--ref <branch>`, and that was a path from the
    # deployer key to root: a branch is an unvetted closure that
    # nixos-rebuild runs as root, every member of sfi-agentic-dev can
    # push one, and the pinned sudoers rule restricts no argument. The
    # way back from a bad main is nixos-rollback
    # (ops/nixos-rollback.nix) — a menu of generations main already
    # built, not an arbitrary build target.
    sfi.ops.commands.nixos-pull = {
      description = "Clear the last-attempted marker and pull/switch to main now via nixos-upgrade.service; --from <ip> pulls main from the forge by address (the name is pinned and moves with the forge)${lib.optionalString anonymous " — refused on this estate's anonymous https route"}. Main only — the way back is nixos-rollback";
      script = ''
        from=""
        while [ $# -gt 0 ]; do
          case "$1" in
            --from) from=$2; shift 2 ;;
            --ref) echo "--ref is gone: this verb builds main only (#76); to leave a bad main use 'sudo nixos-rollback'" >&2; exit 2 ;;
            *) echo "usage: sudo nixos-pull [--from <ip>]" >&2; exit 2 ;;
          esac
        done
        # A rollback pin (nixos-rollback) ends here, on either path:
        # this verb IS "move on from the hold". The gate would drop it
        # anyway once the switch lands (stale pin), but lifting first
        # keeps the metric honest through the run.
        lift_pin() {
          pin=/var/lib/sfi-auto-rebuild/pinned-generation
          if [ -e "$pin" ]; then
            echo "lifted the rollback pin to generation $(${pkgs.coreutils}/bin/cat "$pin")"
            ${pkgs.coreutils}/bin/rm -f "$pin" ${textfileDir}/nixos-pinned.prom
            ${pkgs.systemd}/bin/systemctl start nixos-upgrade.timer
          fi
        }
        if [ -n "$from" ]; then
          case "$from" in
            *[!0-9.]*|"") echo "--from: want an IPv4 address, got '$from'" >&2; exit 2 ;;
          esac
          ${if anonymous then ''
          echo "--from: this estate clones ${repoUrl} anonymously over https (forts#167); an address in the forge's place fails the certificate check, and there is no host key to pin instead. Fix the name — the resolver, or the forge's own DNS — and run 'sudo nixos-pull' without --from" >&2
          exit 2
          '' else ''
          lift_pin
          url="${repo.urlFor "$from"}"
          echo "pulling main from $url (host key checked as ${repo.hostKeyAlias}) and switching..."
          export GIT_SSH_COMMAND="${lib.concatStringsSep " " [
            "${pkgs.openssh}/bin/ssh"
            "-i ${config.sops.secrets.flux-git-deploy-key.path}"
            "-o UserKnownHostsFile=${knownHostsFile}"
            "-o IdentitiesOnly=yes"
            "-o HostKeyAlias=${repo.hostKeyAlias}"
          ]}"
          # Exit 4 = switched, some units failed during activation (#182):
          # the switch happened, so the verb reports it and succeeds — the
          # failed units alert on their own. Other non-zero exits abort.
          rc=0
          ${config.system.build.nixos-rebuild}/bin/nixos-rebuild switch --refresh \
            --flake "git+$url?ref=main&dir=nixos#${config.networking.hostName}" || rc=$?
          if [ "$rc" -eq 4 ]; then
            echo "warning: switched, but some units failed to start during activation — check 'systemctl --failed'"
          elif [ "$rc" -ne 0 ]; then
            exit "$rc"
          fi
          echo "switched from $from (main); the timer converges back to main from wherever ${repo.host} now points"
          exit 0
          ''}
        fi
        lift_pin
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
  };
}
