# Dev fort profile (#413): the hardened host with agent accounts instead
# of a `deployer`. hardened.nix + lab-host.nix, unprivileged accounts that
# BUILD (unlike deployer, which only observes), Home Manager with the dev
# toolchains and the dotfiles, nix-ld for the prebuilt binaries a dev box
# meets (uv-managed pythons, npm native modules). No k3s, no flux, no
# backups: a dev fort is transient, and nothing on it is precious
# (forts#12). Rendered by `fort scaffold` for a manifest with a [dev]
# table; mutually exclusive with production.nix.
#
# What makes it a boundary rather than a laptop: the only credentials on
# the box are the fort's own (the bundles #414 mints per fort and per
# account), SSH answers on the mesh only (hardened.nix), no account has
# wheel or an ops verb — the pull loop is the only way config reaches this
# host — and the box is replaced, not repaired. That is what makes an
# unattended Claude with permissions bypassed sane here (#418) and
# nowhere else.
#
# ONE ACCOUNT PER ROLE (#532). A session used to post on gitea as
# sfi-agent whatever it was doing, so a thread holding two sessions could
# not say which one wrote which comment. Now the verb dials an account
# (`ssh sfi-scout scout …`), the account holds one gitea identity, and the
# kernel keeps them apart: each account's secrets are owner plus mode
# 0400, so the analyst cannot read the agent's token even with
# permissions bypassed. The alternative — a credential selected inside the
# session, or a fort per role — is weighed on #532.
#
# The dispatch loop (#418, docs/forts/dev.md): `ssh sfi-dev pickup
# <owner/repo> <n>` preps a worktree and hands the task to Claude inside a
# herdr workspace; herdr is the supervisor — its server runs as a user
# service in each account's lingering manager (home/agent.nix), the
# package comes from the second nixpkgs input (below), and macmini watches
# it over the mesh with `herdr --remote`.
#
# The fort also carries its own telemetry (#437): an OpenTelemetry
# collector that takes Claude Code's usage metrics and events on
# localhost, publishes the metrics for luxor to scrape over the mesh
# (like node-exporter), and pushes the system journal + the events to
# luxor's Loki. A dev fort has no k3s and so no fluent-bit; this is its
# only log path, and the one that lets NixosUpgradeFailed fire for it.
#
# Toolchains come from the store, not from installers at runtime: nothing
# the SFI checkouts pin today needs a channel manager (go 1.24–1.26, node
# ≥22, python 3.13 via uv). Add rustup only when a repo grows a
# rust-toolchain.toml; don't build an FHS env unless something specific
# breaks after nix-ld.
{ config, lib, pkgs, inputs, ... }:

let
  # gitea's host key, from the one pin auto-rebuild.nix carries
  # ("[gitea.zen.lofi]:30022 ssh-rsa AAAA…" → type + material), so an
  # account's first clone over ssh never meets a trust prompt.
  giteaParts = lib.splitString " " config.sfi.auto-rebuild.gitKnownHosts;
  giteaKey = "${lib.elemAt giteaParts 1} ${lib.elemAt giteaParts 2}";
  # The fort's name, stamped on everything its collector ships (below):
  # `cluster` because that is the label every fleet rule and dashboard
  # keys on — a dev fort is a cluster of none, and the alternative was a
  # second label meaning the same thing.
  host = config.networking.hostName;

  # THE ACCOUNTS (#532). One per role: the Unix account, the one dispatch
  # verb it installs, the gitea user it acts as, and the address on that
  # user. The same word runs through every layer — the account `scout`,
  # the gitea user `sfi-scout`, the ssh alias `sfi-scout`, the verb
  # `scout` — so the author of a comment says which session wrote it.
  #
  # This list is written TWICE, here and in each dev fort's manifest
  # ([dev].accounts, which `fort mint` reads for the bundles). Nix cannot
  # read the manifest: provisioning/forts is outside the flake's root
  # (nixos/), and a flake sees nothing above it. So the two are kept equal
  # by a test instead — TestDevAccountsMatchDevFortModule in
  # forts/cmd/fort compares this list, field by field, with every [dev]
  # manifest. Keep the fields on one line each; the test reads them.
  #
  # An account's email must be an address ITS gitea user holds: gitea
  # keeps every address unique across users, and marks a GPG uid active
  # only for an address that user owns. git signs with the uid, so a
  # mismatch shows the commits as unverified.
  accounts = [
    { user = "agent"; verb = "pickup"; giteaUser = "sfi-agent"; email = "support@streetfortress.com"; }
    { user = "scout"; verb = "scout"; giteaUser = "sfi-scout"; email = "scout@streetfortress.com"; }
    { user = "analyst"; verb = "review"; giteaUser = "sfi-analyst"; email = "analyst@streetfortress.com"; }
  ];
  users = map (a: a.user) accounts;
  byUser = f: lib.listToAttrs (map (a: lib.nameValuePair a.user (f a)) accounts);

  # The devices that dispatch. Mesh-only reach (hardened.nix); the key set
  # matches deployer's for now — trim to the dispatching devices when #415
  # settles it. Every account takes the same set: who may dial is the
  # fort's boundary, and WHICH account they dial is what selects the
  # identity.
  dispatchKeys = [
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBFSTKL7m9ViSstGGhgg1TBnrWEGkNptCCysU17Oxgfl"
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIG0sCtYCOvdb/J7yluSSX9yixiG3pvhZo+OtVQWefjVj"
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBuuLb9A36fWGYZexMb2soTxpHFB0HVQTHqR8vihvFMD"
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINqO0N5rBif1+bFwnluWEmrkaoTAtqTrP/vONG6/fQKl"
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBhrjrcm0Mb4gSMGM/GiWXvkZj3a2ej7/MOcw0Qujx+M"
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFSfukFhjws+MpZu+qMqgVCoIitc43jGeqEnGMcF4ydQ"
    "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEdgJK6LiKUZ+fQQZX28B1v82hCl1z4RxomJLn5pHiPH"
  ];
in
{
  imports = [
    ./hardened.nix
    ./lab-host.nix
    ./dotfiles.nix # ~<account>/.fortfiles, the checkout the HM symlinks point into
    ./auto-rebuild.nix # the gc window below is its option
    ./shell.nix # the prompt, keys and aliases at a herdr pane (#557)
  ];

  # The one profile with Home Manager users: the accounts', and only
  # theirs. The flake wires Home Manager in and declares nobody; this is
  # the declaration. Each home is home/agent.nix applied to its account,
  # so a home differs from another only in the identity it reads and the
  # verb it installs.
  home-manager.users = byUser (a: import ../home/agent.nix a);
  sfi.dotfiles.users = users;
  # Every dev fort: a person reads the prompt at a pane. The module also
  # turns programs.zsh on and sets users.defaultUserShell, so zsh is the
  # login shell of every account that does not pin one — all three here
  # (#576).
  sfi.shell.enable = true;

  # The accounts. Same shape for each: the role is the identity it holds,
  # never a different grant on the host.
  users.users = byUser (a: {
    isNormalUser = true;
    # No colon — the GECOS field is colon-separated.
    description = "SFI coding agent — the ${a.verb} verb, as ${a.giteaUser} on gitea";
    # A user manager that outlives the login: the herdr server (#418,
    # home/agent.nix) lives in it, so a task keeps running after the
    # dispatching ssh session ends. One server per account.
    linger = true;
    # systemd-journal: read the host journal — the OBSERVE rung for a host
    # with no other operator account (same grant as deployer's,
    # production.nix). agents: the fort's one Claude credential (below).
    extraGroups = [ "systemd-journal" "agents" ];
    openssh.authorizedKeys.keys = dispatchKeys;
  });

  # The group that reads the fort's Claude credential, and nothing else.
  users.groups.agents = { };

  # The accounts build: `nix develop`, `nix build` in the checkouts.
  # hardened.nix keeps root; this appends.
  nix.settings.allowed-users = users;

  # herdr from the second nixpkgs input (flake.nix): the fleet pin predates
  # the package. An overlay, so home/agent.nix says `pkgs.herdr` like
  # anything else and moving it to the fleet pin is a delete here.
  # The agent's fast-moving tools from the fresh nixpkgs input, not the
  # fleet pin: herdr (#418) and claude-code — the fleet pin's Claude Code
  # (2.1.137, 2026-05) trails the laptops by months, and a store package
  # cannot self-update. Dev forts only; every other host keeps the pin.
  nixpkgs.overlays = [
    (final: prev:
      let
        fresh = import inputs.nixpkgs-herdr {
          inherit (prev.stdenv.hostPlatform) system;
          # A second nixpkgs is a second config: the fleet's allowUnfree
          # (common.nix) does not reach legacyPackages, and the eval
          # refused claude-code as unfree. Let through by name, only what
          # this overlay takes — the carve-out is the dev fort's, not a
          # second fleet-wide allowUnfree.
          config.allowUnfreePredicate = pkg: builtins.elem (lib.getName pkg) [ "claude-code" ];
        };
      in { inherit (fresh) herdr claude-code; })
  ];

  # A build box churns generations and fills a store faster than a fort
  # that only receives merges: half the fleet window (#423).
  sfi.auto-rebuild.gc.olderThan = "14d";

  # Disk metrics, dev-fort shaped (#453). The default path list
  # (disk-usage-metrics.nix) is cluster-shaped — /var/lib/rancher/k3s,
  # the PVC dir, /root/.cache/nix — and sizes none of what a dev fort
  # actually accumulates: the accounts' homes. One set of paths per
  # account (#532): three homes now hold clones, and which one grows is
  # the question the metric answers. Each pickup (#418) leaves a
  # worktree and a transcript behind, and nothing removes either (gotea
  # sessions --prune closes the workspace and keeps the worktree on
  # purpose: the branch is pushed, the PR is the record, and a fort is
  # replaced rather than tidied). So the 80 G disk grows monotonically
  # over the fort's life, and this is what says how fast — the input to
  # deciding whether anything should ever remove a worktree, or whether
  # the answer stays "retire the fort".
  #
  # Both worktree roots resolve to one series PER REPO, never per issue:
  # per-issue depth would add a series for every dispatch ever made, the
  # unbounded shape the module's own comment warns against. The depths
  # differ because the layouts do — ~/worktrees/<repo>/issue-<n> puts the
  # repo at depth 1, ~/src/<owner>/<repo> at depth 2 (the issue said 1
  # there, which would read "sfi is 12 G" and name no repo; the bound is
  # the same handful either way). ~/.claude is a total — its children are
  # one dir per project path, i.e. per worktree again.
  sfi.disk-usage-metrics = {
    paths = (lib.concatMap
      (a: [
        { path = "/home/${a.user}/worktrees"; depth = 1; } # <repo>/issue-<n>
        { path = "/home/${a.user}/src"; depth = 2; } # <owner>/<repo>, the checkouts worktrees hang off
        "/home/${a.user}/.claude" # transcripts; die with the fort, sized while it lives
      ])
      accounts) ++ [
      "/nix/store"
      { path = "/var/log"; depth = 1; }
    ];
    pvcDir = null; # no local-path-provisioner on a fort with no k3s
  };

  # Prebuilt dynamic binaries find a loader and the common libraries;
  # the set stays modest on purpose — grow it when a specific binary
  # names a missing one.
  programs.nix-ld = {
    enable = true;
    libraries = with pkgs; [
      stdenv.cc.cc.lib
      zlib
      openssl
      libffi
      libxml2
      icu
      curl
      expat
    ];
  };

  # The accounts' git transport dials gitea over the mesh on 30022; pin
  # the host key system-wide so the first clone is hands-off.
  programs.ssh.knownHosts."gitea.zen.lofi" = {
    hostNames = [ "gitea.zen.lofi" "[gitea.zen.lofi]:30022" ];
    publicKey = giteaKey;
  };

  # The identity bundles (#414, #532): minted per fort AND PER ACCOUNT by
  # `fort mint`, revoked by `fort destroy`. Each key is owned by the one
  # account that reads it (git's ssh transport, the gpg import, tea's
  # login — home/agent.nix), mode 0400: this is the boundary between the
  # roles, and it is the kernel's, not a prompt's.
  #
  # agent-claude-oauth-token is the one fleet-wide value among them —
  # billing is the subscription, not the API — so it is root-owned and
  # read by the `agents` group instead. Its name predates the accounts and
  # is not the `agent` account's: renaming it would re-key every fort's
  # secrets file for nothing.
  sops.secrets = lib.mkMerge (
    [{ agent-claude-oauth-token = { group = "agents"; mode = "0440"; }; }]
    ++ map
      (a: {
        "${a.user}-ssh-key" = { owner = a.user; mode = "0400"; };
        "${a.user}-gpg-key" = { owner = a.user; mode = "0400"; };
        "${a.user}-gitea-token" = { owner = a.user; mode = "0400"; };
      })
      accounts
  );

  # ---- telemetry (#437) --------------------------------------------------
  # One collector per fort, the [dev] profile's telemetry contract: Claude
  # Code points at localhost (home/agent.nix sets the env) and never learns
  # a luxor address; the collector is what stamps cluster/host, so a task's
  # env only has to add what the fort cannot know (repo, issue — the
  # herdr workspace pickup creates, #418). Three pipelines:
  #   metrics  otlp → deltatocumulative → stamp → :9464 (prometheus
  #            exporter; luxor's ScrapeConfig from clusters/<host>.yaml
  #            pulls it beside node-exporter). Claude Code is asked for
  #            cumulative temporality (the env); deltatocumulative is the
  #            belt for a session that ignores the preference.
  #   logs     Claude Code's events (prompt/response/tool_result/api_request,
  #            text redacted by default) → loki-mesh, job=claude-code
  #   logs     the system journal → loki-mesh, job=systemd — the same
  #            labels fluent-bit ships from a cluster fort, so the fleet's
  #            journal rules and dashboards see a dev fort unchanged. The
  #            line is the journal record as JSON (fluent-bit's
  #            line_format json), so `| json | _SYSTEMD_UNIT=...` is the
  #            same query on both.
  # Which resource attributes Loki indexes as labels is the hub's call:
  # otlp_config in fleet/apps/luxor/monitoring/loki.yaml (job, cluster,
  # host, repo, issue); the rest ride as structured metadata.
  services.opentelemetry-collector = {
    enable = true;
    # contrib: journald receiver, deltatocumulative, prometheus exporter.
    package = pkgs.opentelemetry-collector-contrib;
    settings = {
      receivers = {
        otlp.protocols.grpc.endpoint = "127.0.0.1:4317";
        # The system journal, from now: no cursor store, so a collector
        # restart skips what happened while it was down (Loki is the
        # dashboard, journalctl on the host is the record).
        journald.start_at = "end";
      };
      processors = {
        batch = { };
        deltatocumulative = { };
        "transform/metrics".metric_statements = [{
          statements = [
            ''set(datapoint.attributes["cluster"], "${host}")''
            ''set(datapoint.attributes["host"], "${host}")''
          ];
        }];
        "transform/claude".log_statements = [{
          statements = [
            ''set(resource.attributes["job"], "claude-code")''
            ''set(resource.attributes["cluster"], "${host}")''
            ''set(resource.attributes["host"], "${host}")''
          ];
        }];
        "transform/journal".log_statements = [{
          statements = [
            ''set(resource.attributes["job"], "systemd")''
            ''set(resource.attributes["cluster"], "${host}")''
            ''set(resource.attributes["host"], "${host}")''
            # The unit as structured metadata: filterable without a label
            # per unit.
            ''set(log.attributes["unit"], log.body["_SYSTEMD_UNIT"]) where log.body["_SYSTEMD_UNIT"] != nil''
          ];
        }];
      };
      exporters = {
        # All interfaces, like node-exporter; hardened.nix's wg0-only
        # firewall (+ the port below) is what makes it mesh-only.
        prometheus = {
          endpoint = "0.0.0.0:9464";
          # Claude Code already copies OTEL_RESOURCE_ATTRIBUTES onto every
          # datapoint (OTEL_METRICS_INCLUDE_RESOURCE_ATTRIBUTES), so no
          # resource→label conversion here — it would also drag
          # service.* onto every series.
          resource_to_telemetry_conversion.enabled = false;
          # A task's series outlive its process by this much; the scrape is
          # 30 s (kube-prometheus-stack's default), so nothing is missed.
          metric_expiration = "10m";
        };
        # Loki's native OTLP ingest (Loki ≥ 3.0) on the mesh NodePort
        # (fleet/apps/luxor/monitoring/mesh-services.yaml). The mesh IP,
        # not luxor.lofi: the name is a dual A record (fluent-bit's lesson).
        otlphttp.endpoint = "http://10.1.0.5:31100/otlp";
      };
      service.pipelines = {
        metrics = {
          receivers = [ "otlp" ];
          processors = [ "deltatocumulative" "transform/metrics" "batch" ];
          exporters = [ "prometheus" ];
        };
        "logs/claude" = {
          receivers = [ "otlp" ];
          processors = [ "transform/claude" "batch" ];
          exporters = [ "otlphttp" ];
        };
        "logs/journal" = {
          receivers = [ "journald" ];
          processors = [ "transform/journal" "batch" ];
          exporters = [ "otlphttp" ];
        };
      };
    };
  };
  # The journald receiver shells out to journalctl (the module grants the
  # systemd-journal group but not the binary).
  systemd.services.opentelemetry-collector.path = [ pkgs.systemd ];
  # Mesh-only, beside 22 and 9100 from hardened.nix.
  networking.firewall.interfaces.wg0.allowedTCPPorts = [ 9464 ];

  environment.systemPackages = [ pkgs.git ];
}
