# Flux ops verbs (declared via ops-commands.nix). Reconcile/restart only
# nudge flux's machinery — retrigger a reconcile, restart a controller —
# never edit what any resource declares; desired state stays in git.
# Suspend/resume toggle spec.suspend on a Kustomization/HelmRelease: a
# deliberate, sudo-logged pause of the reconcile loop that makes runbook
# windows safe (sfi-k8s-scale refuses to scale an unsuspended workload).
# Suspension is NOT auto-restored — sfi-flux-resume closes the window,
# and the Fleet Overview flux table shows what's suspended meanwhile.
# The patch names --field-manager=flux-client-side-apply, and that flag is
# what makes the sentence above true (#554): under the default Override
# policy kustomize-controller reverts a field that a kubectl field manager
# wrote, at the next apply of the owning Kustomization (10m on a fort),
# but it keeps a field that flux-client-side-apply owns while git does not
# declare that field. Both verbs then read spec.suspend back and fail when
# the value is not the requested one.
#
# All use the admin kubeconfig internally: that narrow, sudo-logged use
# is exactly what the framework exists to permit. The flux CRDs arrive
# with flux itself, so on a not-yet-bootstrapped cluster the verbs report
# an empty/absent state rather than failing mysteriously.
{ config, pkgs, ... }:

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
      --type=merge --field-manager=flux-client-side-apply \
      -p '{"spec":{"suspend":${value}}}'

    # A patch that kubectl accepted is not yet a window: read the object
    # back and say what it holds. An empty read is the <none> the menu
    # prints for an object that declares no spec.suspend.
    observed=$(kubectl -n "$matched_ns" get "$matched_type/$requested_name" \
      -o jsonpath='{.spec.suspend}' 2>/dev/null || true)
    if [ "$observed" != "${value}" ]; then
      echo "error: $matched_type/$requested_name -n $matched_ns reads back spec.suspend=''${observed:-<none>}, not ${value} — the patch did not hold" >&2
      exit 1
    fi
    echo "${done}: $matched_type/$requested_name -n $matched_ns (spec.suspend=$observed)"
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
    # The READ verb (#185). The drill fort's flux deadlock was debugged
    # blind: rung 1 (the read-only kubeconfig) is rendered from a token
    # the fleet applies, so it did not exist yet, and rung 2 knew only
    # act-verbs. This verb reads what every flux object says about itself
    # — Ready, its reason, its message, its revision — and it needs no
    # kubeconfig but root's, so it answers on a cluster flux has never
    # converged on. 'clone timeout' is a condition message: one command,
    # one screen, second one.
    #
    # It WIDENS NOTHING: the built-in `view` role the RO kubeconfig binds
    # already reads every object listed here. The verb moves those reads
    # onto the host for the day-0 window, and every use is sudo-logged
    # like any other verb.
    flux-status = {
      description = "READ ONLY: what flux says about itself — FluxInstance, controllers, sources, Kustomizations, HelmReleases with Ready/reason/message/revision, then recent warning events; no arguments";
      runtimeInputs = [ pkgs.kubectl pkgs.coreutils pkgs.gnused pkgs.systemd ];
      script = ''
        export KUBECONFIG=/etc/rancher/k3s/k3s.yaml

        if [ "$#" -ne 0 ]; then
          echo "usage: sudo sfi-flux-status   (no arguments; reads only, changes nothing)" >&2
          exit 64
        fi

        # The CRDs arrive with flux itself. Before they do, the only story
        # is flux-bootstrap's own — tell that one instead of printing five
        # empty tables.
        if ! kubectl get crd kustomizations.kustomize.toolkit.fluxcd.io >/dev/null 2>&1; then
          echo "flux is not installed on this cluster yet: no Kustomization CRD."
          echo "the day-0 unit that installs it (sudo sfi-systemd-restart flux-bootstrap retries it):"
          systemctl status --no-pager --lines=20 flux-bootstrap || true
          exit 0
        fi

        # One row per object: ns|name|suspend|ready|reason|revision|message.
        # jsonpath, not custom-columns: a condition message carries spaces
        # and columns cannot be split back apart. The record separator is
        # our own marker, not a newline, because a helm error message IS
        # several lines — `tr` folds those into spaces first, then the
        # marker becomes the row break. A field that held a newline would
        # otherwise become a row with no name, and the tail of the very
        # message the operator needs would be dropped.
        rows() {
          kind=$1
          revpath=$2
          shift 2
          { kubectl get "$kind" "$@" -o jsonpath='{range .items[*]}'\
'{.metadata.namespace}{"|"}{.metadata.name}{"|"}{.spec.suspend}{"|"}'\
'{.status.conditions[?(@.type=="Ready")].status}{"|"}'\
'{.status.conditions[?(@.type=="Ready")].reason}{"|"}'"$revpath"'{"|"}'\
'{.status.conditions[?(@.type=="Ready")].message}{"@@ROW@@"}{end}' 2>/dev/null \
            || true; } | tr '\n' ' ' | sed 's/@@ROW@@/\n/g'
        }

        report() {
          title=$1
          kind=$2
          revpath=$3
          shift 3
          echo "$title:"
          printed=0
          while IFS='|' read -r ns name susp ready reason rev msg; do
            if [ -z "$name" ]; then
              continue
            fi
            printed=1
            mark=""
            if [ "$susp" = true ]; then
              mark="  SUSPENDED"
            fi
            printf '  %-34s -n %-16s Ready=%s%s\n' "$name" "$ns" "''${ready:-<none>}" "$mark"
            if [ -n "$rev" ]; then
              printf '      revision: %s\n' "$rev"
            fi
            if [ -n "$reason" ]; then
              printf '      reason:   %s\n' "$reason"
            fi
            if [ -n "$msg" ]; then
              printf '      message:  %s\n' "$msg"
            fi
          done < <(rows "$kind" "$revpath" "$@")
          if [ "$printed" -eq 0 ]; then
            echo "  (none)"
          fi
        }

        echo "flux on ${config.networking.hostName} (read-only; nothing below was changed)"
        echo

        if kubectl get crd fluxinstances.fluxcd.controlplane.io >/dev/null 2>&1; then
          report "FluxInstance" fluxinstances '{.status.lastAppliedRevision}' -A
          echo
        fi

        echo "controllers in flux-system:"
        ctrl=$(kubectl -n flux-system get deployments --no-headers \
          -o custom-columns='NAME:.metadata.name,READY:.status.readyReplicas,WANT:.spec.replicas' 2>/dev/null || true)
        if [ -n "$ctrl" ]; then
          printf '%s\n' "$ctrl" | while read -r name ready want; do
            printf '  %-28s ready=%s/%s\n' "$name" "$ready" "$want"
          done
        else
          echo "  (none — the CRDs are here but no controller is; see flux-operator)"
        fi
        echo

        report "sources (GitRepository)" gitrepositories '{.status.artifact.revision}' -A
        echo
        report "sources (OCIRepository)" ocirepositories '{.status.artifact.revision}' -A
        echo
        report "sources (HelmRepository)" helmrepositories '{.status.artifact.revision}' -A
        echo
        report "Kustomizations" kustomizations '{.status.lastAppliedRevision}' -A
        echo
        report "HelmReleases" helmreleases '{.status.lastAttemptedRevision}' -A
        echo

        # Warnings are where a pull, a clone or a chart says why in words
        # the conditions summarise. Events expire (1 h by default), so an
        # empty list is not a clean bill of health.
        echo "recent warning events (newest last; events expire after an hour):"
        warn=$(kubectl get events -A --field-selector type=Warning \
          --sort-by=.lastTimestamp --no-headers 2>/dev/null | tail -n 15 || true)
        if [ -n "$warn" ]; then
          printf '%s\n' "$warn" | while IFS= read -r line; do
            printf '  %s\n' "$line"
          done
        else
          echo "  (none)"
        fi
        echo
        echo "nudge from here: sudo sfi-flux-reconcile <name> [ns]; sudo sfi-flux-restart; sudo sfi-k8s-logs <pod> [ns]"
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
