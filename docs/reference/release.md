# Releases — what a tag promises an estate

A release of sfi/forts is **the flake at a tag**: `v0.0.1` is one commit,
and that commit is the modules, the `fort` CLI, the init templates and
the lock they were tested with, together. There is no version anywhere
else — no file to bump, no per-component number (the rule is lockstep
artifact versioning: one repo, one product, one tag). An estate reaches
a release through its lock: `nixos/flake.nix` names the tag,

```nix
forts.url = "git+https://forge.example/sfi/forts?ref=refs/tags/v0.0.1";
nixpkgs.follows = "forts/nixpkgs";
sops-nix.follows = "forts/sops-nix";
```

and `nixos/flake.lock` records the commit it resolved to. Everything
below is about what an estate may rely on when that line moves from one
tag to the next, and what it has to do by hand.

The tags to date, `v0.0.1`–`v0.0.5` (from 2026-10-14), are pre-releases
on gitea so that a bump has a name. There is no `v0.1`: the semver plan
is abandoned (#220). A release line is named after the NixOS stable
release it pins — the model Clan (clan.lol) uses — and the first line is
**26.11**, cut when NixOS 26.11 ships (late November 2026). Until then
`main` stays on `nixos-unstable` and any tag it needs stays in the
`v0.0.x` pre-release series.

## The contract

A release promises the interface an estate reaches through its lock.
Every change to one of these surfaces is named in the release note:

- **The modules:** the names under `nixosModules.*` and their options.
  `nixosModules.production` dissolving into `hardened` + `ops-deployer`
  (#154) is the shape of a contract change.
- **The CLI:** the verbs, their flags, and the rules `fort check`
  enforces. A new rule is a contract change — it is what turns a gap
  into a finding (below).
- **The schemas:** `estate.toml`, `forts/<name>.toml`, and what a
  layer's `forts.contract.yaml` may say.
- **The channel:** the nixpkgs branch the framework pins (below).

Outside the promise: the rendered files. An estate owns them from the
commit `fort estate init` or `fort scaffold` wrote them (next section).

A tag is `v<line>.<patch>`: `v26.11.0` is the first release of the
26.11 line, `v26.11.1` the first patch on it. A **patch** changes no
surface above, and an estate may take it blind. A **line switch**
(`v26.11.x` → `v27.05.0`) is the only place a breaking change may land —
and it always moves the channel; the release note says what changed,
and the bump PR's gate says where (below).

## Two kinds of tag

**A patch tag: the lock moved, the contract did not.** Renovate on
sfi/forts bumps `flake.lock` — nixpkgs, sops-nix — CI passes (the module
outputs evaluate, an init estate evaluates against the checkout, the
host tests run), and a tag follows. Nothing in the modules, the CLI or the templates changed, and
the release note is the input diff. An estate's bump PR for a patch tag
carries no hand edit; its gate passing is the whole review, and an
estate may let Renovate merge it.

**A line switch: the channel moved, and a contract surface may have.**
A person cuts it, on NixOS's cadence (May and November). The release
note lists the hand edits, by surface, and what `fort check` will say
about each one before the edit is made — the two halves of "a
structural fix" below.

One maintainer supports **one line at a time**, plus the overlap window
NixOS itself keeps (about one month after a release, until the old
channel's end of life). In that window a critical fix for the old line
may get one more patch tag, cut from a short-lived branch off its last
tag; outside it there are no parallel lines, no backports, and the fix
is the bump.

Tags are immutable: a broken release gets the next number, never a
re-tag. Small and often: a tag's suspect surface is everything in it.

## The channel: nixpkgs follows forts

An estate's hosts build against the framework's nixpkgs — the template
sets `nixpkgs.follows = "forts/nixpkgs"`, so one nixpkgs serves the
modules and the hosts, and a fort builds against what the framework was
tested on. The consequence is the one to design for: **the OS lifecycle
of a fort is the forts pin.** An estate cannot take a package update
without a forts bump, and every forts bump carries one. The framework is
the distro channel for a fort, and that is a promise the framework has
to keep — the patch-tag cadence above is how. A security fix in nixpkgs
reaches an estate exactly as fast as the next patch tag.

The branch is the line's NixOS stable release: every tag of the 26.11
line pins `nixos-26.11`, and the line switch is the commit that moves
`flake.nix` to the next branch. (Before the first line it is still
`nixos-unstable`.) The channel is a contract surface: only a line
switch moves it, and the release note names the move.

The first line is 26.11, not 26.05, on purpose. In October 2026 the
unstable pin already carried k3s 1.36 and nixos-26.05 carries 1.35 — a
switch would have downgraded k3s on live forts, and Kubernetes does not
support a minor-version downgrade. 26.05 also reaches end of life
2026-12-31. The first stable line is the first one the unstable pin can
roll into without a downgrade.

The escape hatch, for an estate that must move nixpkgs on its own clock:
drop the `follows` line and pin `nixpkgs.url` directly. The modules
evaluate against whatever nixpkgs the host has, so this works, with one
caveat the estate takes on: the framework tests against its own pin
only, and a module may use an option the estate's nixpkgs does not have
yet. `fort check` does not look at the lock's shape; the release note
is where a nixpkgs floor would be named.

## Templates are bootstrap only

`fort estate init` and `fort scaffold` render a file once. From that commit the
estate owns it: no verb rewrites it, `fort check` compares the tree
against the manifests and the contracts and never against the
templates, and a template change reaches the next estate, not an
existing one. `docs/README.md` says "you refine in place" for a reason.

So a template is a starting point and the version pins in one — the
chart versions under `flux/infrastructure/` — are stale the day the
estate's own Renovate first runs, which is the intended handoff (#126,
#112). There are no managed markers and no `fort migrate` (#90): a
file the framework rewrote would be a file the estate could not refine.

## A structural fix takes one of two paths

A release that needs something from a file the estate owns — a new key
in `estate.toml`, a renamed import in a host file, a manager in
`renovate.json` — reaches the estate one of two ways, and the release
note says which:

1. **The release note says "edit X by hand."** For a change `fort
   check` cannot see: a rename a host file must follow, a module that
   left for a layer.
2. **`fort check` gets a rule that names the gap.** The rule checks a
   property of the tree — "`estate.toml` names no `operator_keys`",
   "no `.pem` under `nixos/certs/` that `estate.nix` reads" — never a
   diff against a template, so it is satisfiable and clears when the
   estate fixes it, and the fix is in the message. The rule's level
   follows the consequence, not the origin: an **error** when the fort
   would not evaluate or would silently lose a property (the CA rename
   of #150, where a stale file name emptied the trust store and nothing
   said so); a **warning** for a recommendation the estate may decline.

The evidence pass for this is the first bump (sfi/staging#14, `v0.0.1`,
23 commits): five hand edits, three of them surfaced by `fort check
--all` with the fix in the message, one that failed only at nix eval
with the attribute's name, and one — the CA file — that failed silently.
That last one is the rule this page asks for and `fort check` does not
have yet.

## The bump: how an estate adopts a release

The estate's gate runs the CLI at the pin — `nix run ./nixos#fort --
check --all`, in the workflow `fort estate init` rendered — so a bump PR runs
the **new** release's `fort check` over the estate before anyone reads
it. The rules that release added fire first there. That is the whole
mechanism: the migration a release needs from an estate is what its
check says in the bump PR, plus whatever the release note lists under
path 1.

```
# in the estate
sed -i 's|refs/tags/v26.11.0|refs/tags/v26.11.1|' nixos/flake.nix
nix flake lock ./nixos          # the lock follows the ref: forts, and nixpkgs + sops-nix through it
git checkout -b forts-v26.11.1 && git commit -am 'Pin sfi/forts v26.11.1' && git push
# open the PR; read the gate; make the edits it names and the release note lists
```

Renovate can open that PR (#126): with a tag in `forts.url` the lock does
not move on `nix flake update`, so the bump is an edit to `flake.nix` —
a regex manager over the ref with the git-tags datasource — followed by
a lock refresh. The same PR carries the nixpkgs move, by construction.

`fort --version` says where an estate stands. The binary knows the
commit it was built from (the flake stamps it; a flake's `self` has no
tag to stamp), and the estate's lock knows the tag, so inside an estate
the verb joins the two:

```
$ fort --version
fort eef09bc
pin  v0.0.1 (eef09bc)
```

and says when the binary is not the pin's build — a checkout, an
`--override-input`, a profile installed from another estate's lock. On
an estate whose `forts.url` names a branch it says so: a branch is not a
release, and such an estate has no release note to read.

## What this leaves out, on purpose

- **A fort version in the manifest** (#90, #135 point 5). The bump PR
  already shows the distance — Renovate's PR names the tags, the gate
  names the gaps — so a recorded version would be a second place for a
  fact the lock holds.
- **Managed markers and `fort migrate`** (#90). See "bootstrap only".
- **A compatibility matrix.** Lockstep makes the tag the statement:
  the modules, the CLI and the templates at one tag were tested together,
  and that is all a tag claims.
