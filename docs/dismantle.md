# fort dismantle — the repo half of retirement

```
fort dismantle <host> [--write]
```

Retiring a fort is two steps with a reviewed merge between them:
`dismantle` removes exactly what [`scaffold`](scaffold.md) and
[`mint`](mint.md) generated and stages it for a PR; [`destroy`](destroy.md)
acts on the provider only once main says the host is gone. Git is the
record and the merge is the gate — the irreversible half never runs on an
unreviewed intent.

## What goes

```
dismantle sfi-x   (cloud: linode)
✗ flux/apps/sfi-x
✗ flux/clusters/sfi-x
✗ nixos/hosts/sfi-x
✗ nixos/secrets/sfi-x.yaml
✗ forts/sfi-x.toml
✗ flux/hub/luxor/sfi-x.yaml
○ nixos/.sops.yaml               - BEGIN/END fort sops rule: sfi-x
○ .sops.yaml                     - BEGIN/END fort sops rule: sfi-x; - 1 `# cluster: sfi-x` recipient line(s) in the shared flux/infrastructure rule
◆ flux/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml   sops updatekeys (lost the sfi-x recipient)
○ flux/hub/luxor/kustomization.yaml   - sfi-x.yaml
○ mesh/peers.json           - .peers.sfi-x (the other peers shed it on their next rebuild)
◆ mesh/ssh_config           re-rendered without sfi-x
◆ mesh/known_hosts          - the sfi-x line
○ provisioning/console-passwords.yaml   - the sfi-x entry (its console passphrase, #524)
! residue — 3 file(s) still mention sfi-x:
    README.md: 2   (doc)
    flux/apps/luxor/monitoring/s3-exporter-targets.yaml: 3   (central monitoring — anything here naming the host is hand-written and usually goes)
    cmd/fort/testdata/forts/sfi-x/…   (CLI test fixture / golden — keep)
· the gitea deploy key `flux-sfi-x` stays registered until fort destroy revokes it.

Dismantle staged. Merge, then run `fort destroy sfi-x` to delete the pulumi stack.
```

Three things deserve the explanation:

- **The console passphrase is withdrawn by name.** It is the one
  credential of a fort that does NOT live in
  `nixos/secrets/<host>.yaml`, because an operator reads it and the host
  must not (`docs/nixos/operations.md`, "The console login"). Removing
  the host's secrets file therefore does not remove it, so dismantle
  unsets the fort's entry in the registry. Without that step a retired
  fort's passphrase outlives the fort.
- **`sops updatekeys` is load-bearing.** Removing a recipient from
  `.sops.yaml` changes nothing by itself: the departed cluster key keeps
  decrypting every shared `flux/infrastructure` file until they are
  re-encrypted. Dismantle does that right after the rule edit, so the
  retired host loses access in the same PR.
- **The residue is a report, not an action.** Everything outside the
  generated surface that still names the host is listed with a triage
  hint — and nothing the retirement already settles is listed at all:
  the console registry and the hub's rustfs users file lose the host's
  entry in this plan, and the stack config goes with the stack in `fort
  destroy`, so all three get a `○` line of their own instead of a
  residue count. What the list holds is work for the reviewer, one item
  per line. Docs and CLI fixtures are usually kept (history, goldens); the
  fort's own file for the hub (`flux/hub/<hub host>/<host>.yaml`, its
  dead man's switch and datasource; [hub.md](hub.md)) is part of the
  generated surface and goes with the plan — the hub's flux prunes the
  resources when the merge lands, whichever estate the hub is in — so
  anything else under central monitoring that names the host is
  hand-written — an s3-exporter target, a dashboard — and usually goes
  too.
The residue is counted on the post-edit text of the files the plan
edits, so the dry run shows what the reviewer will actually face.

A plain fort has no fleet dirs and no fleet sops rule to lose — the plan is the manifest, the host dir, the secrets
file, the `nixos/.sops.yaml` rule and the peer. The `.sops.yaml` edit is
planned only where the host's rule block exists.

## What stays, on purpose

- **The pulumi stack**, its state and its config file
  (`provisioning/pulumi/Pulumi.<host>.yaml`). Touching them here would
  put a provider-side action inside a repo mutation; `destroy` is gated
  on this PR's merge instead, and `fort preview --all` reports the stack
  as `PENDING` until then. `just iac-drift` is red for the host between
  the merge and the destroy, which is the correct signal. The last line
  of the report is that one next step.
- **The gitea deploy key**, revoked by `destroy`.
- **A layer's identities** (a backup store's keys), withdrawn by the
  layer's own dismantle, which runs before this one ([layers](layers.md)).
- **The `.files` wireguard copy** for the ubuntu mesh members (#183) —
  still a hand re-render.

## Then

Commit, PR, merge. Then [`fort destroy <host>`](destroy.md).
