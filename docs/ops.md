# Ops — the ladder on a fort

A fort is operated through `deployer` over the mesh, and `deployer` has
no shell path to root. What it has is a ladder of three rungs, and
every verb the core ships is on one rung (forts#151). `ops`, run as
`deployer` on any fort, lists the verbs that host has.

| rung | what it is | the core's verbs |
|---|---|---|
| **observe** | read-only signals: what the host runs, what the journal says, what the cluster holds | the journal (`journalctl`, `deployer` is in `systemd-journal`); the read-only kubeconfig on a cluster fort (`KUBECONFIG` of every login shell, and `just fetch-kubeconfig` from an operator machine); `ops`; `sudo nixos-revision` |
| **hands-on** | bounded actions on the pull loop, each one sudo-logged, each one a choice among states git already produced | `sudo nixos-pull` (pull main now; `--from <ip>` pulls the same main from the forge by address when the forge's name is what broke, forts#76); `sudo nixos-rollback` (a prior generation, pinned until `nixos-pull` or `--lift`) |
| **break-glass** | the admin rung, behind the admin-key challenge that is sudo's authentication on a fort | `sudo -i` and the real `kubectl` on a host that enables `ops/admin.nix`; `sudo admin-unlock` (mints the kubectl credential, which `kubectl` runs for you); `sudo challenge` (the rehearsal); the console password where a host enables `ops/console.nix` |

The hands-on verbs are pinned NOPASSWD lines in sudoers, on the exact
path `/run/current-system/sw/bin/<verb>`, so they survive a generation
switch and never widen reach: `nixos-pull` builds main and nothing else,
and `nixos-rollback` chooses from a menu of generations main already
built. The break-glass rung is the opposite shape: `deployer ALL=(ALL)
ALL` without NOPASSWD, so the first unpinned `sudo` on a terminal runs
the admin-key ceremony (`modules/ops/challenge.nix`) and sudo's own
timestamp is the window. One option, `sfi.ops.challenge.timeout`, sets
the window for the shell and the life of the kubectl credential alike.

## No verb for the cluster

The core ships verbs for the mechanisms it owns, the pull loop and the
admin gate, and no verb for the cluster. The cluster has `kubectl` and
`flux`, and the framework gives two kubeconfigs to use them with: the
read-only one, which is the observe rung from an operator's machine, and
the admin one behind the challenge, which is the break-glass rung. A
maintenance window is the admin kubeconfig and `flux suspend`, flux's own
command; a stuck pod is `kubectl delete pod` behind the same unlock. A
wrapper around either would be the tool with training wheels, and the
wheels would be the framework's to keep.

## A verb of your own

A verb that exists for one workload, one runbook or one unit is the
estate's or a layer's. `sfi.ops.commands.<name>` is the extension point,
and it is the same path the core's own verbs take: the script lands on
PATH as `<name>`, the sudoers line pins that path for `deployer`, and
`ops` lists it. The name is the whole of the verb's path, so it must not
be the name of a tool the host already carries (the system path resolves
a collision silently; this is why the pull verb is `nixos-pull` and not
`nixos-rebuild`).

The worked example below is the shape the core's old `systemd-restart`
verb had: a restart over an allowlist, where the operator's argument is
compared against the list and the list's own spelling is what reaches
`systemctl`. Operator input selects from a menu; it is never
interpolated into a command.

```nix
# nixos/hosts/<host>/default.nix, or a module of the estate
{ pkgs, lib, ... }:
let
  # The units this host lets deployer restart. A module that owns a
  # unit can contribute to a list like this one; the host decides.
  restartable = [ "wg-quick-wg0.service" "coredns.service" "flux-bootstrap.service" ];
in
{
  sfi.ops.commands.systemd-restart = {
    description = "Restart an allowlisted systemd unit: systemd-restart <unit>; no args lists the allowlist";
    runtimeInputs = [ pkgs.systemd ];
    script = ''
      allowed=(${lib.concatMapStringsSep " " lib.escapeShellArg restartable})
      if [ "$#" -eq 0 ]; then
        printf '%s\n' "''${allowed[@]}"
        exit 0
      fi
      unit=""
      for u in "''${allowed[@]}"; do
        [ "$1" = "$u" ] && unit=$u
      done
      if [ -z "$unit" ]; then
        echo "error: '$1' is not in this host's restart allowlist" >&2
        exit 1
      fi
      systemctl restart "$unit"
      systemctl status --no-pager --lines=8 "$unit"
    '';
  };
}
```

`sudo systemd-restart coredns.service` then works for `deployer`, is
sudo-logged with its argument, and shows up in `ops`. A verb that
widens reach rather than acting on machinery git already governs sets
`challenge = true`, and a host puts any verb behind the challenge by
naming it in `sfi.ops.challenge.verbs`.

The verbs the core used to carry for the cluster — the flux status,
suspend, resume, reconcile and restart verbs, the pod logs, the
workload restart and scale — are the same pattern at length. Their last
revision in this repo is the parent of the commit that removed them
(`git log -- modules/ops/flux.nix modules/ops/logs.nix
modules/ops/workloads.nix modules/ops/systemd.nix`), for a layer that
wants them as a worked example.

## Where the pieces live

| file | what it is |
|---|---|
| `modules/ops/ops-commands.nix` | `sfi.ops.commands`, the sudoers lines and the `ops` index |
| `modules/ops/challenge.nix` | the admin-key challenge as sudo's PAM stack; `challenge`, the rehearsal |
| `modules/ops/admin.nix` | the break-glass rung: `deployer ALL` behind the challenge, `admin-unlock`, deployer's `kubectl` |
| `modules/ops/deployer.nix` | the operator account every fort carries |
| `modules/ops/console.nix` | the console password, a getty-only door to the same rungs after the mesh breaks |
| `modules/ops/ops-kubeconfig.nix` | the read-only kubeconfig of a cluster fort |
| `modules/ops/nixos-revision.nix`, `nixos-rollback.nix`, `modules/auto-rebuild.nix` | the three verbs on the pull loop |
