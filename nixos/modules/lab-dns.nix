# gitea.zen.lofi is one static fact — the mesh address of zenbook — and the
# host owns it. This module renders that fact twice from a single option:
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
# Manifests are target state (Steven, 2026-09-13): no per-cluster
# address, no LAN-vs-mesh variant. A host whose mesh path to zenbook is
# missing is fixed in the mesh (#183), not here. The ConfigMap carries
# flux's prune-disabled annotation so the fleet can stop managing its
# own copy without kustomize-controller garbage-collecting this one.
{ config, lib, ... }:

let
  cfg = config.sfi.lab-dns;
in
{
  options.sfi.lab-dns = {
    giteaAddress = lib.mkOption {
      type = lib.types.str;
      default = "10.1.0.6";
      description = "gitea.zen.lofi's mesh address — one constant, not per cluster.";
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
