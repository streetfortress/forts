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
○ provisioning/inventory.yaml    - sfi-x entry (with its attached comment)
○ nixos/.sops.yaml               - BEGIN/END fort sops rule: sfi-x
○ .sops.yaml                     - BEGIN/END fort sops rule: sfi-x; - 1 `# cluster: sfi-x` recipient line(s) in the shared fleet/infrastructure rule
◆ fleet/infrastructure/configs/monitoring/alertmanager-edge-config.sops.yaml   sops updatekeys (lost the sfi-x recipient)
○ wireguard/peers.json           - .peers.sfi-x (the other peers shed it on their next rebuild)
! residue — 3 file(s) still mention sfi-x; triage in the same PR (incident comments may be worth keeping as provenance — the reviewer's call):
    README.md: 2 mention(s)   (doc)
    fleet/apps/luxor/monitoring/rules/sfi-x.yaml   (central monitoring — per-cluster rule/datasource/dashboard)
    forts/cmd/fort/testdata/forts/sfi-x/…   (CLI test fixture / golden — keep)
· pulumi stack `sfi-x` still exists — untouched by design; `fort destroy sfi-x` after this merges. `just iac-drift` is red for it in between, which is the correct signal.
· the gitea deploy key `flux-sfi-x` stays registered until fort destroy revokes it.

staged. Next: triage the residue, commit, PR — then fort destroy after the merge.
```

Two things deserve the explanation:

- **`sops updatekeys` is load-bearing.** Removing a recipient from
  `.sops.yaml` changes nothing by itself: the departed cluster key keeps
  decrypting every shared `fleet/infrastructure` file until they are
  re-encrypted. Dismantle does that right after the rule edit, so the
  retired host loses access in the same PR.
- **The residue is a report, not an action.** Everything outside the
  generated surface that still names the host is listed with a triage
  hint: docs and CLI fixtures are usually kept (history, goldens), the
  central monitoring's per-cluster rule or dashboard usually goes.
  Cross-fort `sops:` refs *into* this fort are called out separately —
  those contracts lose their source and `fort check` will fail them, so
  re-point them in the same PR.

The residue is counted on the post-edit text of the files the plan
edits, so the dry run shows what the reviewer will actually face.

## What stays, on purpose

- **The pulumi stack** and its state. Touching it here would put a
  provider-side action inside a repo mutation; `destroy` is gated on this
  PR's merge instead, and `fort preview --all` reports the stack as
  `PENDING` until then.
- **The gitea deploy key**, revoked by `destroy`.
- **The `.files` wireguard copy** for the ubuntu mesh members (#183) —
  still a hand re-render.

## Then

Commit, PR, merge. Then [`fort destroy <host>`](destroy.md).
