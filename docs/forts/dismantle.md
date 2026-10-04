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
✗ fleet/apps/sfi-x
✗ fleet/clusters/sfi-x
✗ nixos/hosts/sfi-x
✗ nixos/secrets/sfi-x.yaml
✗ provisioning/forts/sfi-x.toml
✗ fleet/apps/luxor/monitoring/clusters/sfi-x.yaml
○ nixos/.sops.yaml               - BEGIN/END fort sops rule: sfi-x
○ .sops.yaml                     - BEGIN/END fort sops rule: sfi-x; - 1 `# cluster: sfi-x` recipient line(s) in the shared fleet/infrastructure rule
◆ fleet/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml   sops updatekeys (lost the sfi-x recipient)
○ fleet/apps/luxor/monitoring/kustomization.yaml   - clusters/sfi-x.yaml
○ wireguard/peers.json           - .peers.sfi-x (the other peers shed it on their next rebuild)
◆ wireguard/ssh_config           re-rendered without sfi-x
◆ wireguard/known_hosts          - the sfi-x line
○ provisioning/console-passwords.yaml   - the sfi-x entry (its console passphrase, #524)
! residue — 3 file(s) still mention sfi-x; triage in the same PR (incident comments may be worth keeping as provenance — the reviewer's call):
    README.md: 2 mention(s)   (doc)
    fleet/apps/luxor/monitoring/s3-exporter-targets.yaml: 3 mention(s)   (central monitoring — clusters/<host>.yaml is dismantle's; anything else naming the host is hand-written and usually goes)
    forts/cmd/fort/testdata/forts/sfi-x/…   (CLI test fixture / golden — keep)
· pulumi stack `sfi-x` still exists — untouched by design; `fort destroy sfi-x` after this merges. `just iac-drift` is red for it in between, which is the correct signal.
· the gitea deploy key `flux-sfi-x` stays registered until fort destroy revokes it.

staged. Next: triage the residue, commit, PR — then fort destroy after the merge.
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
  decrypting every shared `fleet/infrastructure` file until they are
  re-encrypted. Dismantle does that right after the rule edit, so the
  retired host loses access in the same PR.
- **The residue is a report, not an action.** Everything outside the
  generated surface that still names the host is listed with a triage
  hint: docs and CLI fixtures are usually kept (history, goldens); the
  cluster's own file on the hub (`clusters/<host>.yaml`, its dead man's
  switch and datasource) is part of the generated surface and goes with
  the plan, so anything else under central monitoring that names the
  host is hand-written — an s3-exporter target, a dashboard — and
  usually goes too.
  Cross-fort `sops:` refs *into* this fort are called out separately —
  those contracts lose their source and `fort check` will fail them, so
  re-point them in the same PR.
- **The fleet's B2 pruner is called out too** (#400). `sfi-b2-pruning-key`
  is role-scoped: it belongs to whichever host enables `sfi.pv-prune`'s
  b2 tier, and that host's `nixos/secrets/<host>.yaml` is the one that
  names `restic-prune-s3-env-b2` — a plaintext key name, so the plan can
  tell. Dismantle reports it as a `!` line (the key stays live until
  `fort destroy` revokes it, and the fleet then has no pruner until
  another host enables the tier and mints again); it acts on nothing,
  because it never reaches outside the tree.

The residue is counted on the post-edit text of the files the plan
edits, so the dry run shows what the reviewer will actually face.

A dev fort (#413) has no fleet dirs, no file on the hub and no fleet
sops rule to lose — the plan is the manifest, the host dir, the secrets
file, the `nixos/.sops.yaml` rule and the peer. The `.sops.yaml` edit is
planned only where the host's rule block exists.

## What stays, on purpose

- **The pulumi stack** and its state. Touching it here would put a
  provider-side action inside a repo mutation; `destroy` is gated on this
  PR's merge instead, and `fort preview --all` reports the stack as
  `PENDING` until then.
- **The gitea deploy key**, revoked by `destroy`.
- **The B2 keys**, the fort's own and — when this host is the pruner —
  `sfi-b2-pruning-key`, revoked by `destroy`.
- **The `.files` wireguard copy** for the ubuntu mesh members (#183) —
  still a hand re-render.

## Then

Commit, PR, merge. Then [`fort destroy <host>`](destroy.md).
