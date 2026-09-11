# Flux ops verbs (declared via ops-commands.nix). Both only nudge flux's
# machinery — retrigger a reconcile, restart a controller — never edit
# what any resource declares; desired state stays in git.
#
# Both use the admin kubeconfig internally: that narrow, sudo-logged use
# is exactly what the framework exists to permit. The flux CRDs arrive
# with flux itself, so on a not-yet-bootstrapped cluster the verbs report
# an empty/absent state rather than failing mysteriously.
{ pkgs, ... }:

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
      description = "Stamp reconcile.fluxcd.io/requestedAt on the named flux-system Kustomization (and same-named source); no arg lists them";
      runtimeInputs = [ pkgs.kubectl pkgs.coreutils ];
      script = ''
        export KUBECONFIG=/etc/rancher/k3s/k3s.yaml
        ns=flux-system

        mapfile -t available < <(
          kubectl get kustomizations,gitrepositories,ocirepositories \
            -n "$ns" -o name 2>/dev/null
        )

        if [ "$#" -eq 0 ]; then
          echo "usage: sudo sfi-flux-reconcile <name>"
          echo "reconcilable objects in $ns:"
          if [ "''${#available[@]}" -eq 0 ]; then
            echo "  (none found — is flux bootstrapped on this host?)"
          else
            printf '  %s\n' "''${available[@]}"
          fi
          exit 0
        fi

        if [ "$#" -gt 1 ]; then
          echo "error: exactly one name expected" >&2
          exit 64
        fi

        requested=$1
        matches=()
        for obj in "''${available[@]}"; do
          if [ "''${obj##*/}" = "$requested" ]; then
            matches+=("$obj")
          fi
        done

        if [ "''${#matches[@]}" -eq 0 ]; then
          echo "error: '$requested' is not a Kustomization/GitRepository/OCIRepository in $ns" >&2
          if [ "''${#available[@]}" -gt 0 ]; then
            printf 'known: %s\n' "''${available[@]##*/}" | sort -u >&2
          fi
          exit 1
        fi

        stamp=$(date -u +%Y-%m-%dT%H:%M:%SZ)
        for obj in "''${matches[@]}"; do
          kubectl annotate -n "$ns" "$obj" \
            "reconcile.fluxcd.io/requestedAt=$stamp" --overwrite
        done
        echo "requestedAt=$stamp stamped; watch: kubectl get -n $ns ''${matches[0]}"
      '';
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
