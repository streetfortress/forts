# Workload restart verb (declared via ops-commands.nix). The runbook
# enabler for the envFrom gap: changing a Secret's contents never reaches
# a running container (env is injected at start), and an existingSecret
# lives outside helm's rendered manifests, so nothing rolls the pods —
# the 2026-09-12 Config B switch sat on the old database for hours this
# way. `sudo sfi-k8s-restart deployment symbology-worker` is the curated
# nudge: a rollout restart re-creates pods against current Secret
# contents without touching desired state.
#
# Framework security rule (flux.nix precedent): sudo + user args is
# injection territory, so arguments are only ever string-COMPARED against
# the API server's own listing, and the values passed to kubectl are the
# server's output for the match — operator input selects from the menu,
# it is never interpolated.
{ pkgs, ... }:

{
  imports = [ ./ops-commands.nix ];

  sfi.ops.commands = {
    k8s-restart = {
      description = "Rollout-restart a workload: sfi-k8s-restart <deployment|statefulset|daemonset> <name> [namespace]; no args lists candidates";
      runtimeInputs = [ pkgs.kubectl ];
      script = ''
        export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

        usage() {
          echo "usage: sudo sfi-k8s-restart <deployment|statefulset|daemonset> <name> [namespace]"
        }

        list_kind() {
          # namespace<TAB>name rows straight from the API server.
          kubectl get "$1" -A --no-headers \
            -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name' 2>/dev/null
        }

        if [ "$#" -eq 0 ]; then
          usage
          for kind in deployment statefulset daemonset; do
            echo "$kind:"
            list_kind "$kind" | while read -r ns name; do
              printf '  %-24s -n %s\n' "$name" "$ns"
            done
          done
          exit 0
        fi

        if [ "$#" -lt 2 ] || [ "$#" -gt 3 ]; then
          usage >&2
          exit 64
        fi

        requested_kind=$1
        requested_name=$2
        requested_ns=''${3-}

        # Kind is compared against the allowlist, never passed through raw.
        kind=""
        for k in deployment statefulset daemonset; do
          if [ "$requested_kind" = "$k" ]; then
            kind=$k
          fi
        done
        if [ -z "$kind" ]; then
          echo "error: kind must be deployment, statefulset or daemonset (rollout-restartable)" >&2
          exit 1
        fi

        # Name (and optional namespace) select from the server's listing.
        matches=()
        while read -r ns name; do
          [ "$name" = "$requested_name" ] || continue
          if [ -n "$requested_ns" ] && [ "$ns" != "$requested_ns" ]; then
            continue
          fi
          matches+=("$ns")
        done < <(list_kind "$kind")

        if [ "''${#matches[@]}" -eq 0 ]; then
          scope=""
          if [ -n "$requested_ns" ]; then
            scope=" in namespace $requested_ns"
          fi
          echo "error: no $kind named '$requested_name'$scope — run without args for the menu" >&2
          exit 1
        fi
        if [ "''${#matches[@]}" -gt 1 ]; then
          echo "error: '$requested_name' matches a $kind in several namespaces; pass one:" >&2
          printf '  sudo sfi-k8s-restart %s %s %s\n' "$kind" "$requested_name" "''${matches[@]}" >&2
          exit 1
        fi

        ns=''${matches[0]}
        kubectl -n "$ns" rollout restart "$kind/$requested_name"
        kubectl -n "$ns" rollout status "$kind/$requested_name" --timeout=180s
      '';
    };
  };
}
