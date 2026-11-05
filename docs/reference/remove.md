# fort remove — the inverse of add, one app

```
fort remove <fort> <app | oci-ref> [--dry-run]
```

`remove` takes one app out of a fort's apps tree: exactly what
[`add`](add.md) rendered for it, and nothing else. It is
[`dismantle`](dismantle.md) scoped to a chart — the same shape (a plan
of removes and line edits, a residue report, a staged tree for a PR),
on a workload instead of a host. The merge is the gate: the apps
Kustomization that [`scaffold`](scaffold.md) renders carries
`prune: true`, so flux deletes the workload when main no longer
declares it. Nothing runs against the cluster from this verb.

## The app

The second argument is the app's name — the dir under
`flux/apps/<fort>/`, which is what `--name` set at add time — or the
chart's OCI ref. A ref with a version suffix (`oci://…/app:0.3.0`)
matches on the ref alone. Two dirs can pin the same chart (`add
--name`, twice), so a ref both pin is ambiguous and the error names
the dirs: name the app instead. Only a dir `add` rendered qualifies —
one whose `repository.yaml` carries an `OCIRepository` pin; a
hand-written workload dir is refused with a note to remove it by hand,
and `gateway/` is refused because it is the fort's, not an app's.

## What goes

```
flux/apps/lab-fort/   (remove sfi-website: oci://gitea.zen.lofi/sfi/sfi-website 0.3.0)
○ gateway/
  ✗ certificate-sfi-website.yaml
  ✗ listener-sfi-website.yaml
  ○ kustomization.yaml           - certificate-sfi-website.yaml; patches: - listener-sfi-website.yaml
○ kustomization.yaml             - sfi-website
✗ sfi-website/                   the app dir, whole

staged. then: review, commit, PR — flux prunes the app on merge.
```

- **The app dir, whole** — the `OCIRepository`, the `HelmRelease`, the
  route, the PodMonitor, the inventory and the sops-encrypted Secret.
  A dir a layer rendered beneath it (a tenant database's inventory,
  say) goes with it, and the report names it on the same line.
- **One line of the apps kustomization** — the `- <app>` entry.
- **The gateway half**, when the app had a listener: its certificate
  and listener files, the certificate's resource line, and the
  listener's JSON6902 patch entry. Nothing else in `gateway/` changes.

The edits remove lines only — never a rewrite — and they run at plan
time, so a tree that is not the shape `add` wrote (an entry already
gone) fails the dry run with the entry named, not the apply.

## What stays

- **The gateway.** When the last listener leaves, `gateway/` keeps the
  Gateway, the redirect and its kustomization (now without a
  `patches:` block). They are the fort's: the next `add` with an
  `http` block or `--http` appends to them, and renders exactly what
  it would have on a fresh tree. The report says so.
- **The ClusterIssuer, the sops rules.** Both are the estate's and the
  fort's; an app adds nothing to either, so remove edits neither.
- **The Secret's history.** The encrypted file leaves the tree, not
  the git history; the estate's age keys still open it there. Rotate
  what it held where it is still used.

## The residue

Like dismantle, remove deletes what the core wrote and reports what
else still names the app:

```
residue — files outside this plan that still name sfi-website:
  flux/apps/other-fort/tenant/forts.inventory.yaml: 1
  a sops: ref into the app's Secret from another inventory is now dangling —
  `fort check` on that fort reports it; a layer's file naming the app goes by hand.
```

Two things land here. An [inventory](inventory.md) on another fort
with a `sops:` ref into this app's Secret: the ref dangles after the
merge, and [`check`](check.md) on that fort reports it as the gate
already does. And a file a layer rendered for this app *outside* the
app dir: the core cannot know it is the app's, so it names it and
leaves it. The residue is counted on the post-edit text of the files
this plan edits, so a dry run shows what the reviewer will face.

## Why no layer cascade

`dismantle` runs each layer's dismantle before its own plan (#180),
because a layer declares the *fort* in files of its own. An app has no
such declaration today: the layers render under the app dir (what
[`seed`](seed.md) walks) or rewrite one line of its inventory, and both
go with the dir. A layer that keeps state for an app elsewhere shows up
as residue. If the layer interface grows an app-level hook, remove is
where it cascades from.
