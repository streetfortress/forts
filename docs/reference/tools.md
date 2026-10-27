# The tool set

The fort CLI shells out for real work: `sops` encrypts every secret,
`kustomize build` backs `fort check`, `qemu-img` and `mkisofs` shape
images, `git` moves the estate. `internal/tools/tools.json` declares
every binary the CLI execs, as one set with two halves, and two readers
share the file so they can never disagree about what the CLI needs.

The two readers:

- **flake.nix** wraps `packages.fort` (and `fort-pulumi`) with the
  `packaged` half on PATH, from the flake's pinned nixpkgs, and puts the
  same list in `devShells.default`. Install the package and the tools
  arrive with it, once, pinned.
- **internal/tools** checks for a tool at run time, before the verb does
  any work, and names a missing one with every way to get it. It never
  installs anything.

## The packaged half

```
git, helm, jq, kustomize, mkisofs, mkpasswd, qemu-img, sops, xsltproc
```

These are pinned on the wrapper's PATH for a reason stronger than
convenience: their output is shared state. `fort check` gates merges in
CI, `scaffold` and `mint` render files that land in git, and `sops`
writes the encryption format every fort must read back. Each of these
tools changes behavior across versions, so "whatever the operator has
installed" would mean check results and rendered diffs that vary by
machine. Pinning the set keeps the gate and the render deterministic:
CI and every operator run the same tools the flake locked.

A tool nixpkgs does not support on the host platform is filtered out of
the wrapper (`availableOn` in flake.nix) rather than breaking the
package there. The run-time check catches it instead, and the operator
supplies that one tool.

## The ambient half

```
nix, pulumi
```

These the CLI refuses to carry, and `tools.json` records why of each:

- **nix** is the client of the host's own daemon, so it must be the
  host's. `fort image` builds with it, `fort check --eval` evaluates with it.
- **pulumi** loads its bundled Go language host from the directory of
  its own binary, so it must stay the distribution the operator
  installed. The CLI also sets `PULUMI_IGNORE_AMBIENT_PLUGINS`, which
  makes a pulumi on a wrapper's PATH ignore every plugin beside it
  anyway.

## Without nix

Nothing above requires nix on the machine that runs the CLI. A `go run
./cmd/fort`, or a `fort` binary on a machine whose tools come from
another package manager, works the same way: every tool resolves from
PATH, and a missing one stops the verb before it starts, with the
install routes in the error —

```
kustomize is not on PATH. Install it with your package manager, or take
the whole tool set: nix profile install <estate>/nixos#fort (or nix
develop in a checkout of sfi/forts). nixpkgs attribute: kustomize
```

The trade is the one the packaged half exists to avoid: on this route,
tool versions are yours to keep in step with CI.

## Adding a tool

Declare it in `tools.json` before you exec it. `tools.Command` and
`tools.Runner` are the only way the CLI starts a declared tool, and they
refuse a name the set does not carry. Two guards keep the set honest:

- `TestDeclaredSetCoversEveryExec` reads the source for exec calls that
  went around `tools.Command`.
- Removing a tool is one deleted line, and the same test proves nothing
  still execs it.

Choose the half deliberately. `packaged` is the default: name the
binary and the nixpkgs attribute path that carries it. `ambient` is for
a tool that bundling would break; record the reason in its `why`, since
the field is the only place that knowledge lives.
