# Flux ops verbs (declared via ops-commands.nix). Reconcile/restart only
# nudge flux's machinery — retrigger a reconcile, restart a controller —
# never edit what any resource declares; desired state stays in git.
# Suspend/resume toggle spec.suspend on a Kustomization/HelmRelease: a
# deliberate, sudo-logged pause of the reconcile loop that makes runbook
# windows safe (sfi-k8s-scale refuses to scale an unsuspended workload).
# Suspension is NOT auto-restored — sfi-flux-resume closes the window,
# and the Fleet Overview flux table shows what's suspended meanwhile.
#
# All use the admin kubeconfig internally: that narrow, sudo-logged use
# is exactly what the framework exists to permit. The flux CRDs arrive
# with flux itself, so on a not-yet-bootstrapped cluster the verbs report
# an empty/absent state rather than failing mysteriously.
{ pkgs, ... }:

let
  # suspend + resume share one script (they can't drift): only the value
  # patched into spec.suspend and the labels differ.
  mkSuspendScript = { verb, value, done }: ''
    export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

    usage() {
      echo "usage: sudo sfi-flux-${verb} <name> [namespace]"
    }

    # type is our own literal, tagged per row — never derived from input.
    list_all() {
      for t in kustomizations helmreleases; do
        kubectl get "$t" -A --no-headers \
          -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name,SUSPENDED:.spec.suspend' 2>/dev/null \
          | while read -r ns name susp; do
              printf '%s %s %s %s\n' "$t" "$ns" "$name" "$susp"
            done
      done
    }

    if [ "$#" -eq 0 ]; then
      usage
      echo "reconcilers (suspended=true rows are paused):"
      list_all | while read -r t ns name susp; do
        printf '  %-14s %-32s -n %-16s suspended=%s\n' "$t" "$name" "$ns" "$susp"
      done
      exit 0
    fi

    if [ "$#" -gt 2 ]; then
      usage >&2
      exit 64
    fi

    requested_name=$1
    requested_ns=''${2-}

    matched_type=""
    matched_ns=""
    matched_count=0
    candidates=""
    while read -r t ns name _; do
      [ "$name" = "$requested_name" ] || continue
      if [ -n "$requested_ns" ] && [ "$ns" != "$requested_ns" ]; then
        continue
      fi
      matched_type=$t
      matched_ns=$ns
      matched_count=$((matched_count + 1))
      candidates="$candidates  sudo sfi-flux-${verb} $name $ns   # $t"$'\n'
    done < <(list_all)

    if [ "$matched_count" -eq 0 ]; then
      echo "error: no Kustomization/HelmRelease named '$requested_name' — run without args for the menu" >&2
      exit 1
    fi
    if [ "$matched_count" -gt 1 ]; then
      echo "error: '$requested_name' is ambiguous; pick one:" >&2
      printf '%s' "$candidates" >&2
      echo "(same name+namespace as both kinds needs the admin kubeconfig)" >&2
      exit 1
    fi

    kubectl -n "$matched_ns" patch "$matched_type/$requested_name" \
      --type=merge -p '{"spec":{"suspend":${value}}}'
    echo "${done}: $matched_type/$requested_name -n $matched_ns"
  '';
in
{
  imports = [ ./ops-commands.nix ];

  sfi.ops.commands = {
    # sudo + user-supplied args is injection territory, so the argument is
    # never trusted: it is only ever string-COMPARED against the live list
    # of flux source/kustomization names from the API server, and the
    # value passed to kubectl is the API server's own `-o name` output for
    # the match — the operator's input selects from the menu, it is never
    # interpolated into a command.
    flux-reconcile = {
      description = "Reconcile a flux object now: sfi-flux-reconcile <name> [namespace] — Kustomizations/sources in flux-system, HelmReleases anywhere (a stalled HelmRelease also gets its failure counters reset); no args lists them";
      runtimeInputs = [ pkgs.kubectl pkgs.coreutils ];
      script = ''
        export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
        usage() {
          echo "usage: sudo sfi-flux-reconcile <name> [namespace]"
        }
        # Rows are "<kind> <namespace> <name>", kind our own literal per query
        # — never derived from input. Kustomizations and sources live in
        # flux-system; HelmReleases live next to their workloads.
        list_all() {
          for t in kustomizations gitrepositories ocirepositories helmrepositories; do
            kubectl get "$t" -n flux-system --no-headers -o custom-columns='NAME:.metadata.name' 2>/dev/null \
              | while read -r name; do printf '%s %s %s\n' "$t" flux-system "$name"; done
          done
          kubectl get helmreleases -A --no-headers -o custom-columns='NS:.metadata.namespace,NAME:.metadata.name' 2>/dev/null \
            | while read -r ns name; do printf '%s %s %s\n' helmreleases "$ns" "$name"; done
        }
        if [ "$#" -eq 0 ]; then
          usage
          echo "reconcilable objects:"
          rows=$(list_all)
          if [ -z "$rows" ]; then
            echo "  (none found — is flux bootstrapped on this host?)"
          else
            printf '%s\n' "$rows" | while read -r t ns name; do
              printf '  %-18s %-32s -n %s\n' "$t" "$name" "$ns"
            done
          fi
          exit 0
        fi
        if [ "$#" -gt 2 ]; then
          usage >&2
          exit 64
        fi
        requested_name=$1
        requested_ns=''${2-}
        matches=()
        candidates=""
        while read -r t ns name; do
          [ "$name" = "$requested_name" ] || continue
          if [ -n "$requested_ns" ] && [ "$ns" != "$requested_ns" ]; then
            continue
          fi
          matches+=("$t $ns $name")
          candidates="$candidates  sudo sfi-flux-reconcile $name $ns   # $t"$'\n'
        done < <(list_all)
        if [ "''${#matches[@]}" -eq 0 ]; then
          echo "error: no flux object named '$requested_name' — run without args for the menu" >&2
          exit 1
        fi
        # A name shared by a Kustomization and its same-named source (the
        # apps/flux-system pairs) is meant to match both: they live in one
        # namespace. A HelmRelease name that also exists in another
        # namespace is the ambiguity to refuse.
        namespaces=()
        for m in "''${matches[@]}"; do
          read -r _ ns _ <<<"$m"
          namespaces+=("$ns")
        done
        if [ "$(printf '%s\n' "''${namespaces[@]}" | sort -u | wc -l)" -gt 1 ]; then
          echo "error: '$requested_name' exists in more than one namespace; pick one:" >&2
          printf '%s' "$candidates" >&2
          exit 1
        fi
        stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
        for m in "''${matches[@]}"; do
          read -r t ns name <<<"$m"
          if [ "$t" = helmreleases ]; then
            # `flux reconcile hr --reset`: resetAt clears the install/upgrade
            # failure counters, without which a HelmRelease that exhausted
            # its retries ignores requestedAt entirely (RetriesExceeded stays
            # stalled — umami on staging, 28h, deployment long since healthy).
            kubectl annotate -n "$ns" "helmrelease/$name" \
              "reconcile.fluxcd.io/requestedAt=$stamp" \
              "reconcile.fluxcd.io/resetAt=$stamp" --overwrite
          else
            kubectl annotate -n "$ns" "$t/$name" \
              "reconcile.fluxcd.io/requestedAt=$stamp" --overwrite
          fi
          echo "requestedAt=$stamp stamped on $t/$name -n $ns"
        done
        read -r t ns name <<<"''${matches[0]}"
        echo "watch: kubectl get -n $ns $t/$name"
      '';
    };
    flux-suspend = {
      description = "Pause reconciliation of a Kustomization/HelmRelease (spec.suspend=true): sfi-flux-suspend <name> [namespace]; no args lists reconcilers; pair with sfi-flux-resume";
      runtimeInputs = [ pkgs.kubectl ];
      script = mkSuspendScript {
        verb = "suspend";
        value = "true";
        done = "suspended (reconcile paused — close the window with sfi-flux-resume)";
      };
    };

    flux-resume = {
      description = "Resume reconciliation of a suspended Kustomization/HelmRelease: sfi-flux-resume <name> [namespace]";
      runtimeInputs = [ pkgs.kubectl ];
      script = mkSuspendScript {
        verb = "resume";
        value = "false";
        done = "resumed";
      };
    };

    flux-restart = {
      description = "Rollout-restart the flux controllers in flux-system and wait briefly for them to come back";
      runtimeInputs = [ pkgs.kubectl ];
      script = ''
        export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
        ns=flux-system

        restarted=()
        for d in source-controller kustomize-controller helm-controller notification-controller; do
          if kubectl -n "$ns" get deployment "$d" >/dev/null 2>&1; then
            kubectl -n "$ns" rollout restart "deployment/$d"
            restarted+=("$d")
          else
            echo "skipping $d (no such deployment in $ns)"
          fi
        done

        if [ "''${#restarted[@]}" -eq 0 ]; then
          echo "error: no flux controller deployments found in $ns — is flux bootstrapped?" >&2
          exit 1
        fi

        rc=0
        for d in "''${restarted[@]}"; do
          kubectl -n "$ns" rollout status "deployment/$d" --timeout=120s || rc=1
        done
        exit "$rc"
      '';
    };
  };
}
