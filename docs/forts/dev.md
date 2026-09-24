# The dev fort's dispatch loop — `pickup`, herdr, and what survives

One command on macmini turns a gitea issue into a PR from a dev fort, and
comes back with a pointer to the running session (#418; the laptop half
is `gotea dispatch`, sfi/gotea#37). This page is the fort half: what
`pickup` does, what herdr is for, how a human continues a task, and what
is and is not durable on a host that is replaced rather than repaired.
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

## `pickup <owner/repo> <n> [--model <name>]`

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
   `herdr agent prompt … "/pickup owner/repo#n"`. The standing instruction
   is the `pickup` skill in `.files` (`shell/config/claude-skills/pickup`),
   linked into `~/.claude/skills` like `kb`; there is no prompt file.
   `--model` is `gotea dispatch --model` arriving: the name is passed to
   `claude` as given and the set of valid names is the CLI's, not ours —
   an alias (`opus`), a model id, or a provider's. Without it `claude`
   picks its own default, which is what every dispatch before this took.
   The flag only decides what a *new* agent starts on: an identity whose
   workspace is already open is reported at step 3 and nothing is
   restarted, so the running agent keeps the model it started on.
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
  (cost, tokens, time in tools, commits, PRs, lines), the activity
  counters, and the events — API calls and latency by model, tool
  calls and failures by tool, the event log. `$cluster` and `$repo`
  select; a row with no repo/issue is the fort's own interactive usage.
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

Two things about the counters that the dashboard works around, worth
knowing when reading them raw: `session.count` is born at 1 (the CLI
increments it before the first export), so `increase()` never sees a
session start — sessions are counted from the SessionStart hook's
completion event in Loki; and `active_time{type="cli"}` is time inside
tracked operations (tool calls), not the session's wall-clock.

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
