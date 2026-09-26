# Dev fort profile (#413): the hardened host with an `agent` instead of a
# `deployer`. hardened.nix + lab-host.nix, an unprivileged account that
# BUILDS (unlike deployer, which only observes), Home Manager with the dev
# toolchains and the dotfiles, nix-ld for the prebuilt binaries a dev box
# meets (uv-managed pythons, npm native modules). No k3s, no flux, no
# backups: a dev fort is transient, and nothing on it is precious
# (forts#12). Rendered by `fort scaffold` for a manifest with a [dev]
# table; mutually exclusive with production.nix.
#
# What makes it a boundary rather than a laptop: the only credentials on
# the box are the fort's own (the agent bundle #414 mints per fort), SSH
# answers on the mesh only (hardened.nix), the agent has no wheel and no
# ops verbs — the pull loop is the only way config reaches this host —
# and the box is replaced, not repaired. That is what makes an unattended
# Claude with permissions bypassed sane here (#418) and nowhere else.
#
# The dispatch loop (#418, docs/forts/dev.md): `ssh sfi-dev pickup
# <owner/repo> <n>` preps a worktree and hands the task to Claude inside a
# herdr workspace; herdr is the supervisor — its server runs as a user
# service in agent's lingering manager (home/agent.nix), the package comes
# from the second nixpkgs input (below), and macmini watches it over the
# mesh with `herdr --remote`.
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
  # ("[gitea.zen.lofi]:30022 ssh-rsa AAAA…" → type + material), so the
  # agent's first clone over ssh never meets a trust prompt.
  giteaParts = lib.splitString " " config.sfi.auto-rebuild.gitKnownHosts;
  giteaKey = "${lib.elemAt giteaParts 1} ${lib.elemAt giteaParts 2}";
  # The fort's name, stamped on everything its collector ships (below):
  # `cluster` because that is the label every fleet rule and dashboard
  # keys on — a dev fort is a cluster of none, and the alternative was a
  # second label meaning the same thing.
  host = config.networking.hostName;
in
{
  imports = [
    ./hardened.nix
    ./lab-host.nix
    ./dotfiles.nix # ~agent/.fortfiles, the checkout the HM symlinks point into
    ./auto-rebuild.nix # the gc window below is its option
    ./shell.nix # the prompt, keys and aliases at a herdr pane (#557)
  ];

  # The one profile with a Home Manager user: agent's. The flake wires
  # Home Manager in and declares nobody; this is the declaration.
  home-manager.users.agent = import ../home/agent.nix;
  sfi.dotfiles.user = "agent";
  programs.zsh.enable = true; # the login shell below
  sfi.shell.enable = true; # every dev fort: a person reads the prompt at a pane

  users.users.agent = {
    isNormalUser = true;
    description = "SFI coding agent";
    shell = pkgs.zsh;
    # A user manager that outlives the login: the herdr server (#418,
    # home/agent.nix) lives in it, so a task keeps running after the
    # dispatching ssh session ends.
    linger = true;
    # Read the host journal — the OBSERVE rung for a host with no other
    # operator account (same grant as deployer's, production.nix).
    extraGroups = [ "systemd-journal" ];
    # Mesh-only reach (hardened.nix); the key set matches deployer's for
    # now — trim to the dispatching devices when #415 settles it.
    openssh.authorizedKeys.keys = [
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBFSTKL7m9ViSstGGhgg1TBnrWEGkNptCCysU17Oxgfl"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIG0sCtYCOvdb/J7yluSSX9yixiG3pvhZo+OtVQWefjVj"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBuuLb9A36fWGYZexMb2soTxpHFB0HVQTHqR8vihvFMD"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAINqO0N5rBif1+bFwnluWEmrkaoTAtqTrP/vONG6/fQKl"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBhrjrcm0Mb4gSMGM/GiWXvkZj3a2ej7/MOcw0Qujx+M"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFSfukFhjws+MpZu+qMqgVCoIitc43jGeqEnGMcF4ydQ"
      "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEdgJK6LiKUZ+fQQZX28B1v82hCl1z4RxomJLn5pHiPH"
    ];
  };

  # agent builds: `nix develop`, `nix build` in the checkouts. hardened.nix
  # keeps root; this appends.
  nix.settings.allowed-users = [ "agent" ];

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
  # actually accumulates: the agent's home. Each pickup (#418) leaves a
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
    paths = [
      { path = "/home/agent/worktrees"; depth = 1; } # <repo>/issue-<n>
      { path = "/home/agent/src"; depth = 2; } # <owner>/<repo>, the checkouts worktrees hang off
      "/home/agent/.claude" # transcripts; die with the fort, sized while it lives
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

  # The agent's git transport dials gitea over the mesh on 30022; pin the
  # host key system-wide so the first clone is hands-off.
  programs.ssh.knownHosts."gitea.zen.lofi" = {
    hostNames = [ "gitea.zen.lofi" "[gitea.zen.lofi]:30022" ];
    publicKey = giteaKey;
  };

  # The agent identity bundle (#414): minted per fort by `fort mint`,
  # revoked by `fort destroy`. Owned by agent, read by the tools in
  # home/agent.nix (git's ssh transport, the gpg import, tea's login,
  # Claude Code's session). agent-claude-oauth-token is the one fleet-wide
  # value among them — billing is the subscription, not the API.
  sops.secrets = {
    agent-ssh-key = { owner = "agent"; mode = "0400"; };
    agent-gpg-key = { owner = "agent"; mode = "0400"; };
    agent-gitea-token = { owner = "agent"; mode = "0400"; };
    agent-claude-oauth-token = { owner = "agent"; mode = "0400"; };
  };

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
