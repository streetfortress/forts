# Contributing

Thanks for your interest in forts. Issues and pull requests are welcome.

**Forts is primarily developed on a private forgejo instance.** We will transfer 
your issue and/or pull request and bring it across the finish line on `main`. 
Credit will be recorded in the repo's git history and be made public.
(commit author's email, or user mention in the commit message)

## Before you start

- For a bug, open an issue with the verb you ran, the output you got, and the
  output you expected. `fort check` output and the manifest (with secrets and
  addresses redacted) help a lot.
- For a feature or a behavior change, open an issue first and wait for a
  maintainer to agree on the direction. This saves you from building something
  we cannot merge.

## Pull requests

- Keep a PR to one change. Small PRs merge fast; mixed ones stall.
- `go build ./...` and `go test ./...` must pass. CI also evaluates the test
  estates and runs `fort check --all` against them.
- Match the style of the surrounding code and output. CLI reports are terse:
  the tree is the output, and a verb earns at most one `next:` line.
- Write tests for behavior you add or change.

## AI-assisted contributions

We use agentic development heavily ourselves — see
[SFI's AI disclosure](https://github.com/streetfortress/ai-disclosure). The
same rules apply inbound:

- AI-assisted contributions are welcome. Say so in the PR description.
- You own what you submit. You are the human who accepted the change's intent
  and direction before we see it, and review will treat you as its author.
- Undisclosed machine-generated PRs that waste reviewer time are closed
  without ceremony. The problem is never the tool; it is submitting work
  nobody stood behind.

## Documentation

The README and the getting-started guide are written by hand, on purpose. For
those, open an issue describing what is wrong or missing rather than
rewriting them in a PR. Reference documentation under `docs/` takes direct
PRs.

## License

Contributions are accepted under the [MIT License](LICENSE), the same terms
as the project.
