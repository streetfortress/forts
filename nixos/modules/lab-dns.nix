# gitea.zen.lofi is one fact — the mesh address of the host serving gitea
# — and the host owns it. This module renders that fact twice from a
# single option:
#
#   1. /etc/hosts on the host (networking.hosts): the auto-rebuild flake
#      fetch and flux-bootstrap's clone work with the lab DNS down;
#   2. the cluster's coredns-custom ConfigMap, as a k3s add-on manifest
#      (services.k3s.manifests → /var/lib/rancher/k3s/server/manifests):
#      applied by k3s itself at start, i.e. before flux exists — the
#      day-0 chicken-and-egg flux-bootstrap.nix used to solve with a sed
#      over the fleet's copy. Pods resolve gitea via the pin and every
#      other .lofi name over the mesh dnsmasq listener (10.1.0.1).
#
# No per-cluster address, no LAN-vs-mesh variant. A host whose mesh path
# to gitea is missing is fixed in the mesh (#183), not here. The
# ConfigMap carries flux's prune-disabled annotation so the fleet can
# stop managing its own copy without kustomize-controller
# garbage-collecting this one.
#
# WHY A PIN AND NOT DNS (until #297): the lab dnsmasq answers every
# .lofi name with BOTH its A records, LAN and mesh — `address=` records
# are not localised (`localise-queries` only applies to hosts-file
# records) — and a cloud fort that draws the LAN address waits out a
# SYN timeout before falling back, or never falls back (#119). One
# answer, the mesh one, is what git-over-ssh and containerd need. The
# cost is that this address MOVES WITH GITEA and is rolled by nix:
# when gitea changes host (#338/#350), the new default merges and is
# rolled to every fort (`sudo sfi-nixos-rebuild`, or the ~6h loop)
# BEFORE the old host goes down — a fort still pinned to a host that
# is gone cannot fetch the config that would unpin it.
#
#   2026-09-13 → 10.1.0.6   zenbook
#   hop one    → 10.1.0.16  sfi-fort-d54d5b3, the interim
#   hop two    → 10.1.0.4   sfi-fort-zenbook
{ config, lib, ... }:

let
  cfg = config.sfi.lab-dns;
in
{
  options.sfi.lab-dns = {
    giteaAddress = lib.mkOption {
      type = lib.types.str;
      # Hop two (#350): sfi-fort-zenbook. Merged and rolled while the
      # interim still serves — see the header for why the order matters.
      default = "10.1.0.4";
      description = "gitea.zen.lofi's mesh address — one constant, not per cluster; moves with gitea.";
    };
  };

  config = {
    networking.hosts.${cfg.giteaAddress} = [ "gitea.zen.lofi" ];

    services.k3s.manifests.coredns-custom.content = {
      apiVersion = "v1";
      kind = "ConfigMap";
      metadata = {
        name = "coredns-custom";
        namespace = "kube-system";
        annotations = {
          # Rendered by the host (nixos/modules/lab-dns.nix), not the fleet.
          "kustomize.toolkit.fluxcd.io/prune" = "disabled";
        };
      };
      data = {
        "zen-lofi.server" = ''
          zen.lofi:53 {
            errors
            cache 30
            hosts {
              ${cfg.giteaAddress} gitea.zen.lofi
              fallthrough
            }
            forward . 10.1.0.1
          }
        '';
        "lofi.server" = ''
          lofi:53 {
            errors
            cache 30
            forward . 10.1.0.1
          }
        '';
      };
    };
  };
}
