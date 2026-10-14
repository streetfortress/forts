# The estate's verbs that are not the fort CLI. Recipes run from the root
# of the checkout wherever you invoke `just` (it searches upward).
#
# The fort CLI itself needs no wrapper here. The operator installs the
# package once (`nix profile install ./nixos#fort`) and then calls
# `fort <verb>` — scaffold, mint, provision, check, add, seed, dismantle,
# destroy (docs/README.md in sfi/forts). This file holds only the work
# that no verb does.

# A fort renders its own read-only ops kubeconfig at
# /var/lib/sfi/kubeconfig-ro (modules/ops/ops-kubeconfig.nix): the ops-ro
# ServiceAccount token, with the mesh address as the server URL, so the
# same bytes work on the host and over the VPN. This file is the observe
# rung of the ops ladder. Cluster-admin is break-glass on the host.
#
# The local copy is keyed by host, and so is every name inside it.
# kubectl merges each file on KUBECONFIG by key name, and the first file
# wins, so one fort's token must never answer for another.
#
#   just fetch-kubeconfig <fort>
#   KUBECONFIG=~/.kube/<fort>.config kubectl get nodes
#
# Pull one fort's read-only ops kubeconfig to ~/.kube/<host>.config.
fetch-kubeconfig host:
    #!/usr/bin/env bash
    set -euo pipefail
    # The address, the account and the host key all come from the
    # rendered client config (docs/ssh-config.md in sfi/forts). fort mint
    # and fort dismantle keep both files current, and `fort check --all`
    # fails a stale ssh_config. So this recipe needs no address and no
    # user name of its own.
    [ -f mesh/ssh_config ] || { echo "mesh/ssh_config is not there — run: fort ssh-config --write" >&2; exit 1; }
    mkdir -p ~/.kube
    scp -q -F mesh/ssh_config -o UserKnownHostsFile=mesh/known_hosts \
      "{{ host }}:/var/lib/sfi/kubeconfig-ro" ~/.kube/{{ host }}.config
    chmod 600 ~/.kube/{{ host }}.config
    echo "Wrote ~/.kube/{{ host }}.config (read-only: view + flux CRDs + logs, no secrets) — KUBECONFIG=~/.kube/{{ host }}.config kubectl get nodes"

# An edit to a sops rule changes nothing on disk; this recipe applies it.
# Run it after a recipient change — a new admin key, a rotation, a fort
# that fort mint added. Two configs hold the rules: nixos/.sops.yaml
# covers nixos/secrets/*.yaml, and the root .sops.yaml covers everything
# else (provisioning/secrets.yaml, the fleet Secrets under
# flux/infrastructure/). Each file is found by its `sops:` metadata
# block, not by its name.
#
# Use a key that decrypts every file TODAY. Never use the key you add.
#
# Re-encrypt every sops file (both configs) after a recipient change.
updatekeys:
    #!/usr/bin/env bash
    set -euo pipefail
    for f in nixos/secrets/*.yaml; do
        [ -e "$f" ] || continue
        sops --config nixos/.sops.yaml updatekeys -y "$f"
    done
    rest=$(git grep -l '^sops:' -- '*.yaml' ':!nixos/secrets' || true)
    if [ -n "$rest" ]; then
        printf '%s\n' "$rest" | xargs -n1 sops --config .sops.yaml updatekeys -y
    fi
