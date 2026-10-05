# Workload verbs (declared via ops-commands.nix): restart + scale. The
# runbook enablers for the envFrom gap: changing a Secret's contents never
# reaches a running container (env is injected at start), and an
# existingSecret lives outside helm's rendered manifests, so nothing rolls
# the pods — the 2026-09-12 Config B switch sat on the old database for
# hours this way.
#
#   sudo sfi-k8s-restart deployment symbology-worker
#   sudo sfi-k8s-restart pod postgres-1            # delete; controller recreates
#   sudo sfi-k8s-scale deployment symbology-worker 0
#
# One deliberate nuance to the framework's "never desired state" rule:
# sfi-k8s-scale DOES edit live spec.replicas — but the edit is ephemeral
# by design. Git still owns replicas, and the next helm upgrade / flux
# reconcile restores the declared count; the verb exists to quiesce a
# workload mid-runbook (the Config B copy needed exactly this), not to
# manage capacity. Restart-by-pod is the same shape: deleting a pod nudges
# its controller (CNPG instance pods, one-off stuck replicas) and the
# controller re-realizes desired state.
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
      description = "Restart a workload: sfi-k8s-restart <deployment|statefulset|daemonset|pod> <name> [namespace]; pods are deleted for their controller to recreate; no args lists candidates";
      runtimeInputs = [ pkgs.kubectl ];
      script = ''
        export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

        usage() {
          echo "usage: sudo sfi-k8s-restart <deployment|statefulset|daemonset|pod> <name> [namespace]"
        }

        list_kind() {
          # namespace<TAB>name rows straight from the API server.
          kubectl get "$1" -A --no-headers \
            -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name' 2>/dev/null
        }

        if [ "$#" -eq 0 ]; then
          usage
          for kind in deployment statefulset daemonset pod; do
            echo "$kind:"
            list_kind "$kind" | while read -r ns name; do
              printf '  %-40s -n %s\n' "$name" "$ns"
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
        for k in deployment statefulset daemonset pod; do
          if [ "$requested_kind" = "$k" ]; then
            kind=$k
          fi
        done
        if [ -z "$kind" ]; then
          echo "error: kind must be deployment, statefulset, daemonset or pod" >&2
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
        if [ "$kind" = "pod" ]; then
          # No rollout machinery for bare pods: delete and let the owning
          # controller recreate. CNPG instance pods come back under the
          # same name — wait for that; controllers that rename (ReplicaSet
          # pods) make the wait a no-op, which is fine: the delete is the
          # nudge, `kubectl get pods -n $ns` shows the successor.
          kubectl -n "$ns" delete "pod/$requested_name" --wait=true
          if kubectl -n "$ns" wait --for=condition=Ready \
              "pod/$requested_name" --timeout=180s 2>/dev/null; then
            echo "pod recreated and Ready"
          else
            echo "deleted; successor (if renamed) not awaited — check: kubectl get pods -n $ns"
          fi
        else
          kubectl -n "$ns" rollout restart "$kind/$requested_name"
          kubectl -n "$ns" rollout status "$kind/$requested_name" --timeout=180s
        fi
      '';
    };

    k8s-scale = {
      description = "Ephemerally scale a workload: sfi-k8s-scale <deployment|statefulset> <name> <replicas> [namespace] — git still owns replicas, reconciliation restores them; no args lists candidates";
      runtimeInputs = [ pkgs.kubectl ];
      script = ''
        export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

        usage() {
          echo "usage: sudo sfi-k8s-scale <deployment|statefulset> <name> <replicas> [namespace]"
          echo "note: the edit is ephemeral — the next helm/flux reconcile restores the declared count"
        }

        list_kind() {
          kubectl get "$1" -A --no-headers \
            -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,REPLICAS:.spec.replicas' 2>/dev/null
        }

        if [ "$#" -eq 0 ]; then
          usage
          for kind in deployment statefulset; do
            echo "$kind:"
            list_kind "$kind" | while read -r ns name replicas; do
              printf '  %-40s -n %-20s replicas=%s\n' "$name" "$ns" "$replicas"
            done
          done
          exit 0
        fi

        if [ "$#" -lt 3 ] || [ "$#" -gt 4 ]; then
          usage >&2
          exit 64
        fi

        requested_kind=$1
        requested_name=$2
        requested_replicas=$3
        requested_ns=''${4-}

        kind=""
        for k in deployment statefulset; do
          if [ "$requested_kind" = "$k" ]; then
            kind=$k
          fi
        done
        if [ -z "$kind" ]; then
          echo "error: kind must be deployment or statefulset (the scalable kinds)" >&2
          exit 1
        fi

        # Replicas: small non-negative integer, validated before kubectl
        # sees it. Two digits is already generous on single-node forts.
        case "$requested_replicas" in
          [0-9]|[0-9][0-9]) ;;
          *)
            echo "error: replicas must be an integer 0-99, got '$requested_replicas'" >&2
            exit 1
            ;;
        esac

        matches=()
        while read -r ns name _; do
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
          printf '  sudo sfi-k8s-scale %s %s %s %s\n' \
            "$kind" "$requested_name" "$requested_replicas" "''${matches[@]}" >&2
          exit 1
        fi

        ns=''${matches[0]}

        # Flux-awareness: an ephemeral scale under active reconciliation
        # can be reverted mid-runbook (a helm upgrade re-renders replicas
        # right through the quiesce window). helm-controller stamps every
        # rendered object with its owning HelmRelease — require that
        # release suspended before touching replicas; workloads without
        # flux ownership labels proceed (nothing will fight the edit).
        hr_name=$(kubectl -n "$ns" get "$kind/$requested_name" \
          -o jsonpath="{.metadata.labels['helm\.toolkit\.fluxcd\.io/name']}")
        hr_ns=$(kubectl -n "$ns" get "$kind/$requested_name" \
          -o jsonpath="{.metadata.labels['helm\.toolkit\.fluxcd\.io/namespace']}")
        if [ -n "$hr_name" ] && [ -n "$hr_ns" ]; then
          suspended=$(kubectl -n "$hr_ns" get helmrelease "$hr_name" \
            -o jsonpath='{.spec.suspend}' 2>/dev/null || true)
          if [ "$suspended" != "true" ]; then
            echo "error: $kind/$requested_name is owned by HelmRelease $hr_ns/$hr_name, which is not suspended —" >&2
            echo "an active reconcile can revert the scale mid-runbook. Open the window first:" >&2
            echo "  sudo sfi-flux-suspend $hr_name $hr_ns" >&2
            echo "and close it afterwards (also restores declared replicas):" >&2
            echo "  sudo sfi-flux-resume $hr_name $hr_ns" >&2
            exit 1
          fi
          echo "owning HelmRelease $hr_ns/$hr_name is suspended — proceeding"
        else
          echo "note: no flux ownership labels on $kind/$requested_name — proceeding"
        fi

        kubectl -n "$ns" scale "$kind/$requested_name" --replicas="$requested_replicas"
        if [ "$requested_replicas" -gt 0 ]; then
          kubectl -n "$ns" rollout status "$kind/$requested_name" --timeout=180s
        else
          kubectl -n "$ns" get "$kind/$requested_name"
          echo "scaled to 0 (ephemeral — reconciliation restores the declared count)"
        fi
      '';
    };
  };
}
