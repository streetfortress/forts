# sfi-k8s-logs: container logs on the host, for the window before rung 1
# exists (#185).
#
#   sudo sfi-k8s-logs                                  # menu: pods + phase
#   sudo sfi-k8s-logs db-1 forts
#   sudo sfi-k8s-logs web-worker-abc web --previous
#   sudo sfi-k8s-logs source-controller-x flux-system --tail 500
#
# Why a verb for a read: the read-only kubeconfig already carries
# pods/log through the built-in `view` role, so this verb WIDENS NOTHING.
# What it adds is a place to stand. Rung 1 is off-host (`just
# fetch-kubeconfig`) and it renders from a ServiceAccount token; when the
# cluster has not converged, or the mesh is down, or the token is not
# there yet, the operator has a console and a sudo menu and no way to
# read a pod. The drill fort was debugged through the kubelet read-only
# port for exactly this reason: /pods answers, logs do not.
#
# Every invocation is sudo-logged with its argument list, like every
# other verb. Logs can carry values a Secret holds, which is why this is
# a verb and not a group membership: the journal says who read what.
#
# Framework security rule (flux.nix precedent): the pod name, the
# namespace and the container name are only ever string-COMPARED against
# the API server's own listing, and the value handed to kubectl is the
# server's own output for the match. --tail takes an integer, validated
# before kubectl sees it.
{ pkgs, ... }:

{
  imports = [ ./ops-commands.nix ];

  sfi.ops.commands.k8s-logs = {
    description = "READ ONLY: container logs of one pod — sfi-k8s-logs <pod> [namespace] [--previous] [--container <name>] [--tail <n>]; all containers and 200 lines by default; no args lists pods";
    runtimeInputs = [ pkgs.kubectl pkgs.coreutils ];
    script = ''
      export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

      usage() {
        echo "usage: sudo sfi-k8s-logs <pod> [namespace] [--previous] [--container <name>] [--tail <n>]"
        echo "       reads only; 200 lines of every container, with timestamps"
      }

      list_pods() {
        kubectl get pods -A --no-headers \
          -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase' 2>/dev/null
      }

      if [ "$#" -eq 0 ]; then
        usage
        echo "pods on this cluster:"
        rows=$(list_pods)
        if [ -z "$rows" ]; then
          echo "  (none — is the API server up? sudo sfi-flux-status says what flux thinks)"
        else
          printf '%s\n' "$rows" | while read -r ns name phase; do
            printf '  %-46s -n %-16s %s\n' "$name" "$ns" "$phase"
          done
        fi
        exit 0
      fi

      requested_pod=""
      requested_ns=""
      requested_container=""
      tail_lines=200
      previous=0
      while [ "$#" -gt 0 ]; do
        case "$1" in
          --previous)
            previous=1
            shift
            ;;
          --container)
            if [ "$#" -lt 2 ]; then
              echo "error: --container needs a name" >&2
              exit 64
            fi
            requested_container=$2
            shift 2
            ;;
          --tail)
            if [ "$#" -lt 2 ]; then
              echo "error: --tail needs a line count" >&2
              exit 64
            fi
            tail_lines=$2
            shift 2
            ;;
          -*)
            usage >&2
            exit 64
            ;;
          *)
            if [ -z "$requested_pod" ]; then
              requested_pod=$1
            elif [ -z "$requested_ns" ]; then
              requested_ns=$1
            else
              usage >&2
              exit 64
            fi
            shift
            ;;
        esac
      done

      if [ -z "$requested_pod" ]; then
        usage >&2
        exit 64
      fi

      # An integer, bounded, before kubectl sees it.
      case "$tail_lines" in
        ""|*[!0-9]*)
          echo "error: --tail wants a line count, got '$tail_lines'" >&2
          exit 1
          ;;
      esac
      if [ "$tail_lines" -lt 1 ] || [ "$tail_lines" -gt 100000 ]; then
        echo "error: --tail must be between 1 and 100000" >&2
        exit 1
      fi

      # The pod (and the namespace, when given) select from the server's
      # listing; the listing's own values are what kubectl receives.
      matches=()
      while read -r ns name _; do
        [ "$name" = "$requested_pod" ] || continue
        if [ -n "$requested_ns" ] && [ "$ns" != "$requested_ns" ]; then
          continue
        fi
        matches+=("$ns")
      done < <(list_pods)

      if [ "''${#matches[@]}" -eq 0 ]; then
        scope=""
        if [ -n "$requested_ns" ]; then
          scope=" in namespace $requested_ns"
        fi
        echo "error: no pod named '$requested_pod'$scope — run without args for the menu" >&2
        exit 1
      fi
      if [ "''${#matches[@]}" -gt 1 ]; then
        echo "error: '$requested_pod' matches a pod in several namespaces; pass one:" >&2
        for m in "''${matches[@]}"; do
          printf '  sudo sfi-k8s-logs %s %s\n' "$requested_pod" "$m" >&2
        done
        exit 1
      fi
      ns=''${matches[0]}

      args=(--tail="$tail_lines" --timestamps)
      if [ "$previous" -eq 1 ]; then
        args+=(--previous)
      fi

      if [ -n "$requested_container" ]; then
        # Containers select from the pod's own spec, init containers
        # included: an init container that never finished is the pod that
        # never starts, and its log is the answer.
        container=""
        while read -r c; do
          if [ "$c" = "$requested_container" ]; then
            container=$c
          fi
        done < <(kubectl -n "$ns" get "pod/$requested_pod" \
          -o jsonpath='{range .spec.initContainers[*]}{.name}{"\n"}{end}{range .spec.containers[*]}{.name}{"\n"}{end}' 2>/dev/null)
        if [ -z "$container" ]; then
          echo "error: pod $requested_pod -n $ns has no container '$requested_container'; it has:" >&2
          kubectl -n "$ns" get "pod/$requested_pod" \
            -o jsonpath='{range .spec.initContainers[*]}{"  "}{.name}{" (init)\n"}{end}{range .spec.containers[*]}{"  "}{.name}{"\n"}{end}' >&2
          exit 1
        fi
        args+=(--container="$container")
        echo "== logs of $requested_pod -n $ns, container $container (last $tail_lines lines)"
      else
        args+=(--all-containers --prefix)
        echo "== logs of $requested_pod -n $ns, every container (last $tail_lines lines)"
      fi

      # A pod that has restarted holds two logs and the interesting one is
      # usually the dead container's: say so rather than let the operator
      # read a healthy 3 lines and conclude nothing happened.
      restarts=$(kubectl -n "$ns" get "pod/$requested_pod" \
        -o jsonpath='{.status.containerStatuses[*].restartCount}' 2>/dev/null || true)
      if [ "$previous" -eq 0 ] && [ -n "$restarts" ] && [ "$restarts" != "0" ]; then
        echo "note: restart counts are '$restarts' — the previous container's log is 'sudo sfi-k8s-logs $requested_pod $ns --previous'"
      fi

      kubectl -n "$ns" logs "pod/$requested_pod" "''${args[@]}"
    '';
  };
}
