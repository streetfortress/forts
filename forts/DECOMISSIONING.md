# Decommissioning

Two steps, the reviewed merge between them (#217):

1. **`fort dismantle <host> [--write]`** — the repo half, automated: the
   inverse of `fort scaffold` + `fort mint`. Removes the
   inventory entry, `nixos/hosts/<host>`, `nixos/secrets/<host>.yaml`,
   both sops rule blocks and the shared-infrastructure recipient line(s)
   (then `sops updatekeys` on the files that lost it — the retired key
   keeps decrypting until that runs), the wireguard peer, the fleet
   trees (the flake auto-discovers host dirs, so nothing there); prints the **residue** (every
   remaining mention, for the reviewer to triage in the same PR — #167
   kept incident comments as provenance on purpose) and any cross-fort
   sops refs into the host. Touches no pulumi state. Stage, triage,
   commit, PR, merge.
2. **`fort destroy <host>`** — the half with side effects outside git,
   gated on the merge: refuses unless the host is absent from
   origin/main's inventory (and the working tree's), the tree is clean,
   and the stack exists (a playbook-era lab VM has none: `just delete`).
   Then the runbook below, as one verb with inherited stdio — you read
   and confirm each pulumi step: power the instance off (linode), refresh,
   `state unprotect --all` (only ever right before the destroy), destroy,
   `stack rm`; clears the file backend's residue (history, backups, the
   .bak web3 left behind), revokes the `flux-<host>` deploy key on gitea,
   and stages the state removal for its own PR ("<host>: destroyed" —
   git history is the provenance). The provider comes from the
   checkpoint, not inventory (`pulumi destroy` never runs the program).
   A failure mid-way leaves the unprotected state in the tree and says
   "rerun". Between the two merges `fort preview --all` reports the
   stack as `PENDING … destroy pending`, not as drift.

# Manual Decomissioning with Pulumi

The steps below are what `fort destroy` runs, kept as the record of the web3 teardown they were written from (pulumi unprotect, destroy, and stack rm after the manifest cleanup of [pr #219 Retire web3](https://gitea.zen.lofi/sfi/deployments/pulls/219) — the half `fort dismantle` does). Useful when a step has to be redone by hand.

## Notes from web3 Decomissioning

Preconditions: Main is at the merged #219, web3 is gone from inventory, the stack still holds six protected resources, the Linode's last-known status is offline, and the linode 4.39.0 plugin that state records is installed. 

Setup in a fresh worktree so the state removal lands as its own PR:

git worktree add ~/worktrees/deployments/destroy-web3 -b destroy-web3 origin/main
cd ~/worktrees/deployments/destroy-web3/forts/pulumi
export PULUMI_BACKEND_URL=file://$PWD/state
export PULUMI_IGNORE_AMBIENT_PLUGINS=true
export PULUMI_CONFIG_PASSPHRASE=$(sops --config ../../.sops.yaml -d --extract '["pulumi-passphrase"]' ../../provisioning/secrets.yaml)
export LINODE_TOKEN=$(sops --config ../../.sops.yaml -d --extract '["linode-token"]' ../../provisioning/secrets.yaml)

This is exactly what cloudEnv does for fort commands. Nothing in fort reaches this stack anymore because every subcommand loads the host from inventory, so the stock CLI is the tool from here on.

1. Refresh, so destroy works from truth. pulumi destroy does not run the program. It works purely from the checkpoint, which is why the missing inventory entry does not matter. It doe delete, so state should match reality first:

pulumi refresh -s web3

Expect no changes, or at most the instance's status. If anything was removed by hand in the Linode console, refresh drops it from state here, and destroy would otherwise fail on it.

2. Unprotect. Every resource of a production: true nd destroy refuses on the first one it meets:

pulumi state unprotect --all -s web3

Three things worth knowing about it. It is state-only, no API call, just flipping the flag in the checkpoint file. It prompts before writing, and it is reversible with pulumi state protect. And --all is the right granularity because a stack is one host by
construction. Do not reach for pulumi destroy --excotected resources instead of deleting them.

3. Destroy. The preview lists the deletes in reverse dependency order: the DNS record, the boot config, both disks, the instance,
then the firewall. Read it, type yes:

pulumi destroy -s web3

The instance being offline matters here. Linode refuses disk deletion on a running instance, and the provider does not shut it down for you.

4. Remove the stack. This refuses while any resourccheck that step 3 finished:

pulumi stack rm web3

On the file backend it deletes the stack file, its and Pulumi.web3.yaml. The image-ID provenance in that config file lives on in git history.

5. Commit and PR. git status should show only delet the config file. just iac-drift goes green again,since the stack no longer exists to report on. Push the branch and open the PR.

6. Off-repo cleanup. Two items outside pulumi:

- The gitea deploy key, registered by fort mint (preseed-identity before it) as a read-only key titled flux-web3 on sfi/deployments. Delete it via the repo's deploy-key settings or the keys API.
- The .files wireguard copy still lists web3 with its endpoint. That derived copy feeds the ubuntu mesh members, so remove the entry
  there and re-run the wireguard playbook.
