# The dev fort's dispatch loop — `pickup`, `scout`, herdr, and what survives

One command on macmini turns a gitea issue into a PR from a dev fort, and
comes back with a pointer to the running session (#418; the laptop half
is `gotea dispatch`, sfi/gotea#37). This page is the fort half: what
`pickup` and `scout` do, what herdr is for, how a human continues a task,
and what is and is not durable on a host that is replaced rather than
repaired.
The host, the identity and the repo policy are
[`docs/infra/dev-forts.md`](../infra/dev-forts.md).

```
gotea dispatch sfi/deployments#123                  macmini: the vault task, the fort, then
  └─ ssh sfi-dev pickup sfi/deployments 123         the fort: worktree + workspace + Claude
       └─ herdr workspace "sfi/deployments#123"     the supervisor: claude in its root pane,
            └─ claude --permission-mode bypassPermissions   prompted "/pickup sfi/deployments#123"
                 └─ … tea pr create … → PR                  the skill's loop; the PR is the report
                      └─ Stop hook: pickup-transcript       the transcript, attached to the PR
```

## `pickup <owner/repo> <n> [--model <name>] [--note <text>]`

`nixos/home/agent.nix` — a `writeShellApplication` on the agent's PATH,
so `ssh sfi-dev pickup …` finds it through the login shell. Deterministic
prep first, the hand-off to Claude last, so the worktree and the task's
environment exist before Claude's first metric:

1. **The checkout** — `~/src/<owner>/<repo>`, cloned over gitea's ssh
   with the fort's key (host key pinned by `dev-fort.nix`), or fetched
   with `--prune` when it exists.
2. **The worktree** — `~/worktrees/<repo>/issue-<n>` on branch
   `issue-<n>`, the global convention. A branch that already exists —
   locally, or on origin from an earlier fort — is checked out, not
   recreated; a new one starts from `origin/HEAD`.
3. **The workspace** — `herdr workspace create --cwd <worktree> --label
   owner/repo#n --env …`. The label is the identity and the join key:
   `gotea sessions` joins herdr's workspace list to the vault by it, and
   a re-dispatch of an identity whose workspace is open reports it
   (`existing=1`) rather than opening a second. The env on the workspace
   is what every pane in it inherits:
   `OTEL_RESOURCE_ATTRIBUTES=repo=<owner/repo>,issue=<n>` (#437 — the
   collector stamps cluster and host; this is the half the fort cannot
   know) and `PICKUP_TASK=owner/repo#n` (what the Stop hook and the skill
   read).
4. **Claude** — `herdr agent start <repo>-<n> --kind claude --pane <root
   pane> -- --permission-mode bypassPermissions [--model <name>]`, then
   `herdr agent prompt … "/pickup owner/repo#n [<note>]"`. The standing instruction
   is the `pickup` skill in `.files` (`shell/config/claude-skills/pickup`),
   linked into `~/.claude/skills` like `kb`; there is no prompt file.
   `--model` is `gotea dispatch --model` arriving: the name is passed to
   `claude` as given and the set of valid names is the CLI's, not ours —
   an alias (`opus`), a model id, or a provider's. Without it `claude`
   picks its own default, which is what every dispatch before this took.
   The flag only decides what a *new* agent starts on: an identity whose
   workspace is already open is reported at step 3 and nothing is
   restarted, so the running agent keeps the model it started on.
   `--note` (`-n`) is `gotea dispatch --note` arriving (sfi/gotea#49): the
   ad-hoc context the issue does not carry — "prefer the pure half", "#46
   just landed" — appended to the prompt **on the same line**, because
   `herdr agent prompt` submits the text and a newline in it would be the
   submit, leaving the rest to arrive as a second turn after the skill had
   already run without it. gotea refuses a multi-line note before it
   dials; a note passed here by hand is flattened to one line instead. It
   reaches only a fresh agent, for the same reason `--model` does.
   Bypassed permissions are the whole point of the fort: the only
   credentials on the box are the per-fort SSH key, GPG key and token
   (#414), so the blast radius of a bypassed prompt is the revocation
   unit. Reckless on devbox, correct here.
5. **The report** — key=value lines on stdout, which is what `gotea
   dispatch` parses; everything else pickup says goes to stderr and
   streams through the ssh session as progress:

   ```
   workspace=w3          herdr's id for the workspace
   session=<uuid>        the Claude session id, from herdr's claude integration
   existing=1            only when the workspace was already open
   ```

   A non-zero exit is "pickup failed" and the vault is not touched. The
   session id comes from herdr's claude integration — a SessionStart hook
   `agent-identity` installs that reports the id to the server, which
   `herdr agent get` shows as `agent_session`; pickup waits up to 30 s
   for it.

## `scout <owner/repo> --note <question> [--model <name>]`

The same prep and the same report for the session that has **no issue
behind it** (`gotea scout`, sfi/gotea#51): the one sent to look at
something nobody has written down yet, whose output is issues rather than a
PR. Sharing is real — the checkout and the Claude hand-off are one pair of
shell fragments in `agent.nix`, used by both verbs — so only what follows
from having no identity differs:

| | `pickup` | `scout` |
| --- | --- | --- |
| label | `owner/repo#n`, the join key | `scout:<owner/repo>@<YYYYmmdd-HHMMSS>` (UTC), reported back as `label=` because only the fort knows the stamp |
| worktree | `issue-<n>`, reused when it exists | `scout-<stamp>` off `origin/HEAD`, always new — the stamp is the scout's whole identity, so nothing is ever "already open" |
| `PICKUP_TASK` | the identity | **unset**, so the Stop hook posts nothing: there is no task to hang a transcript on, and the issues the scout files are the record |
| `OTEL_RESOURCE_ATTRIBUTES` | `repo=…,issue=<n>` | `repo=…,issue=scout-<stamp>`, so the session still gets its own row in the cost dashboard (#437) rather than reading as the fort's own interactive usage |
| prompt | `/pickup owner/repo#n` | `/scout owner/repo <question>` — the question is required, since it is the whole brief |
| swept by | `gotea sessions --prune`, when gitea closes the issue | the same sweep, when the agent is quiet and the stamp is over a day old: nothing will ever close to say a scout is done |

A fix smaller than the issue describing it is allowed. The `scout` skill in
`.files` says how: file the issue first, rename the branch to `issue-<n>`,
then open the PR — which puts the work back on the path
`pickup-transcript` already knows, since it looks for exactly that branch.

**What an unattended interactive Claude needs answered first.** The
print-mode design of the first draft had no dialogs; an interactive
session in a herdr pane has three, and each would leave the agent
`blocked` on a fresh box. `agent-identity` (at boot) merges the fort's
settings into `~/.claude/settings.json` (the Stop hook below, the
auto-updater off — the store's binary cannot update itself) and marks
onboarding done and bypass-permissions accepted in `~/.claude.json`;
`pickup` (at dispatch) marks the worktree trusted there, since that
dialog is per path. Claude Code rewrites both files itself, so neither is
a store symlink: they are merged, never overwritten.

## herdr, the supervisor

herdr is what replaced the first draft's `systemd-run --user` unit: a
workspace per task instead of a transient unit, an agent state
(`working` / `blocked` / `done`) instead of a journal to tail, and the
same TUI on the fort as on the laptop.

- **The package** is `pkgs.herdr` through an overlay in `dev-fort.nix`
  from a second flake input, `nixpkgs-herdr` (nixos-unstable): the fleet
  pin (2026-05) predates the package, and a fleet-wide bump is not this
  issue's. The pin is a plain nixos-unstable revision, so the fort
  fetches herdr from cache.nixos.org rather than building rust + zig on
  4 vCPUs. Delete the input at the next fleet bump.
- **The server** is a Home Manager user service, `herdr.service`, in the
  agent's lingering manager: always on, `WantedBy default.target`, after
  `agent-identity` (the integration installed, the Claude token in the
  manager's environment). herdr would start a server itself on first
  use; the unit is the declared state, and what a fresh boot has before
  anyone dials in. Panes are the login shell, zsh, which is where the
  OTEL variables and the Claude token come from — the manager's own PATH
  is bare and stays so.
- **The config** is the laptop's: `~/.config/herdr/config.toml` is
  linked from `.files` (`shell/config/herdr.toml`) in `home/base.nix`,
  like the rest of the dotfiles.
- **Everything in a workspace is the server's child.** Stopping or
  restarting `herdr.service` stops every task in flight — the one way a
  dispatched task dies short of the fort itself. Home Manager does not
  restart user services on a rebuild (`startServices = "suggest"`), so
  the auto-rebuild pull loop leaves a running server alone.

**From macmini.** `herdr --remote sfi-dev` attaches to the fort's server
over ssh — the alias dials the mesh address directly, no ProxyJump, which
is what `--remote` wants — and shows the workspace per task. `herdr
--machine sfi-dev-831c637 agent list` runs API commands against it
without a TUI; that needs a saved profile labelled with the fort's host
name, `herdr machine add agent@10.1.0.61 --label sfi-dev-831c637`, which
is also what `gotea sessions` reads. The client and the server should be
the same minor (0.9 on both today; macmini via brew, the fort via the
input above).

## Continuing a task

- **Watch or take over:** `herdr --remote sfi-dev`, open the workspace
  labelled with the identity, and the pane is Claude's — answer a
  `blocked` dialog, or type the next prompt.
- **Resume in a new pane:** split a pane in that workspace and
  `claude --resume <session id>` — the workspace env travels with the
  pane, so the resumed session is still attributed to the task (#437).
  The id is on the vault task (`agentSession: "<fort> <workspace>
  <session>"`, what `gotea next` prints on the in-flight row) and in the
  transcript comment on the PR.
- **A plain `ssh -t sfi-dev` then `claude --resume …`** also works, but
  is attributed to the fort only, not the task: the env lives on the
  workspace.
- **Dispatching the same issue again** reports the open workspace
  (`existing=1`) and starts nothing; close the workspace first if a
  fresh start is wanted. Each new dispatch is a new Claude process:
  Claude Code reads `OTEL_RESOURCE_ATTRIBUTES` once at start, so a
  session is never re-stamped to another issue.

## What it did, what it cost: the dashboard and the alerts

The fort's collector (#437, `nixos/modules/dev-fort.nix`) is what makes
the loop observable from luxor: Claude Code's usage counters scraped
over the mesh, one series set per task (`cluster` = the fort, `repo` +
`issue` from the workspace env, no `session.id`), and the session's
event stream in Loki (`{job="claude-code"}`, the same labels, every
event attribute as structured metadata, prompt and response text
`<REDACTED>`). On top of it:

- **The dashboard** — *Claude Code (dev forts)*, `sfi-claude-code`
  (`fleet/apps/luxor/monitoring/dashboards/claude-code.py`): totals over
  the range, cost and tokens by task and by model, a per-task table
  (cost, tokens, time in tools, commits, lines), the activity
  counters, and the events — API calls and latency by model, tool
  calls and failures by tool, the event log. `$cluster` and `$repo`
  select; a row with no repo/issue is the fort's own interactive usage.
- **Its denominator** — *Gitea activity*, `sfi-gitea-activity`
  (`fleet/apps/luxor/monitoring/dashboards/gitea-activity.py`), #463:
  issues and PRs opened, closed and merged over the same window, per
  repo, read straight out of gitea's database (a Grafana Postgres
  datasource over the mesh, on Grafana's own read-only `grafana` role on
  that fort's sites-db — beside databasus', never sharing it). Spend
  means one thing beside a
  week that closed eleven issues and another beside a week that closed
  none, and a human opening a PR counts there too — which is why it is
  its own dashboard and not a row on this one.
- **`ClaudeCodeDailyCostHigh`** (`rules-central.yaml`, the metrics side):
  one fort's 24 h spend above 50 USD-equivalent. The number is the
  CLI's API-equivalent — billing is the subscription — so it is a pace
  alarm: a looping task, or more dispatches than the fort should carry.
- **`ClaudeCodeTurnStuck`** (`loki-rules.yaml`, the log side): a turn
  that is open and silent — its `user_prompt` has no Stop hook
  completion behind it and no API call or tool event for 30 minutes. A
  finished task is idle at its prompt with the turn *closed*, so it does
  not fire; a hung tool does. This is a log rule because no counter can
  see it: the CLI's active-time counter is added when an operation
  ends, so a tool that never returns moves nothing.

What the counters can and cannot see, worth knowing when reading them
raw — the dashboard works around each, and the generator's header
carries the long form (#462):

- **A counter's series is born carrying its first increment.** Nothing
  is exported until the counter ticks once, so `increase()`, which takes
  the first sample as its baseline, never sees that tick. At the fort's
  10 s export interval that is nothing for cost, tokens and active time;
  for `commit.count`, `lines_of_code.count` and
  `code_edit_tool.decision` — a handful of ticks per task — it is most
  or all of the value, and a task that commits once reads 0 forever. The
  dashboard totals those three with `max_over_time()` instead: a task's
  series is monotonic and dies with the task (`metric_expiration` is
  10 m), so its max over a range that covers the task *is* the total.
  Read raw, remember the other side of that: a range starting mid-task
  counts the task's whole life, and a second pickup of the same issue
  reuses the same label set, so the later series' max hides the
  earlier's. `session.count` is the older instance of the same trap —
  born at 1 before the first export — so sessions started are counted
  from the SessionStart hook's completion event in Loki instead.
- **`pull_request.count` cannot see a PR opened here**, which is why the
  dashboard no longer has a PR panel. Claude Code bumps it on `gh pr
  create` / `glab mr create`, a POST to a `…/pulls` URL, or an MCP tool
  named `create_pull_request`; `tea pr create` — the PR verb the
  `pickup` and `scout` skills use — matches none of them, and gitea's
  `/pulls/<n>` URLs miss its URL recogniser too. It also fires on
  unrelated POSTs. PR counts come back from gitea itself (#463).
- **`lines_of_code.count` only sees the edit tools.** It is computed
  from an Edit / Write / NotebookEdit diff; a `sed -i`, a heredoc or a
  `cat >>` through Bash moves it by nothing — and `bypassPermissions`,
  which is how `pickup` starts Claude, steers the agent towards exactly
  those. (`commit.count` is tool-agnostic: it is parsed out of `git
  commit`'s `[branch abc1234]` stdout.) So the panels say "edit tools"
  and the number is a floor; the diff a task actually shipped is its
  PR's, which gitea has (#463).
- **`active_time{type="cli"}`** is time inside tracked operations (tool
  calls), not the session's wall-clock.

## What is durable, and what is not

A dev fort is `[backups] profile = "none"` by requirement. What the loop
leaves behind, and where:

| thing | where it survives | how |
|---|---|---|
| the work | the branch on gitea | the skill pushes as it goes |
| the report | the PR, its description kept current | the skill |
| the transcript | a comment on the PR (or the issue, when the task stopped before a PR) with `claude-session-<id>.jsonl.gz` attached | `pickup-transcript`, the Stop hook: one comment per session, found by its marker and updated in place, the attachment replaced — after the dispatched turn and after every human continuation. tea has no attachment verb, so it is the REST API with the fort's token; `.gz` is in gitea's allowed types, `.jsonl` is not |
| the session pointer | the vault task (`agentSession`) | `gotea dispatch` |
| the metrics and events | luxor — Prometheus series on `:9464` labelled `repo`, `issue`; Loki events with the same labels | the fort's collector (#437) |
| the worktree, `~/.claude/projects`, the herdr workspace | the fort — nothing else | die with it |

The transcript comment is the record for the same reason the
`Claude-Session` trailer on the commits is not enough here: the trailer
names a file that lives under `~/.claude/projects` on a host that will
be dismantled. A session outside a dispatched task (no `PICKUP_TASK`)
posts nothing.

The same hook says what the dispatch has made, on the pane herdr draws in
the sidebar: a `pickup` metadata token, `!<n>` when the task's branch has
an open PR and `no PR` when it has none. herdr's own state icon reads the
screen, so it reports that a turn finished and never whether the work
exists — a session that stopped at a blocker looks exactly like one that
opened a PR. The token is display-only metadata under the `pickup` source,
so it never contends with that detection, and the laptop's `config.toml`
(`.files`, `shell/config/herdr.toml`) gives `$pickup` its colour. A scout
reports no token: it has no `PICKUP_TASK`, so the hook returns first.

## The first raise: what to verify

The checklist for the first real dispatch (`gotea dispatch` on one
task), each a claim above:

- `gotea dispatch` returns a workspace and a session id; `herdr --remote
  sfi-dev` shows the workspace by its identity label with Claude
  `working` — not `blocked` on a dialog. If it is blocked, the dialog
  names which of the three answers above is wrong on this Claude Code.
- A pane split later in the workspace prints `PICKUP_TASK` and
  `OTEL_RESOURCE_ATTRIBUTES`; the series on `:9464` carry `repo` and
  `issue`; `claude --resume` from that pane keeps them. (Verified on
  sfi-dev-831c637, 2026-09-24, from inside a dispatched session: the
  pane's shell and the Claude process both carry the env — the Bash
  tool's own subprocesses do not, Claude Code strips `OTEL_*` from
  them — and `:9464` shows `repo="sfi/deployments", issue="437"` on
  every counter, `vcs_*` beside them, no `session_id`.)
- On luxor: `up{cluster="<fort>", component="otel"}` is 1 on the
  central Prometheus; `{job="claude-code", cluster="<fort>"}` streams in
  Loki with `event_name` as structured metadata and `prompt` /
  `response` reading `<REDACTED>`; the Claude Code dashboard's per-task
  table has the task's row.
- The PR appears with a current description, and after the agent stops,
  the transcript comment with its attachment.
- `herdr machine add … --label <host>` saved on macmini; `gotea
  sessions` lists the workspace.
