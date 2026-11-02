# PR watch for Herdr.app design

Date: 2026-11-02. Status: draft, pending approval. Builder check done; answers folded in.

## Intent

Matt starts agents from `~/Projects/flosports` and one effort often opens pull requests (PRs) in several repos, through a parent agent and its children. In Herdr.app (the desktop app, `herdr-app` + herdr-core `hc`) he needs to:

1. See which PRs an effort opened, rolled up from children to their parent.
2. Watch a PR for updates, as T3 Code does (right-click menu: Copy link, Open, Watch for changes, Unlink), so the agent that owns the PR wakes and acts.

Today Herdr has no PR concept, and `hosts.md` has no Herdr PR watch, so the global rule "watch every PR you open" cannot be met in Herdr.

## Goals

- Each PR has exactly one owner: the agent terminal that linked it.
- A view that follows the focused agent: a child sees its own PRs; a parent sees its own PRs plus its direct children's.
- Watching a PR wakes its owner only for changes that need action from it.
- An owner that has exited is brought back from its saved session, in its original folder, under the same parent.
- The owner's direct parent gets an FYI line for each event on a child's PR. Nobody above the parent is told.
- Watches keep running when the Herdr window is closed.

## Non-goals

- Webhooks or near-real-time delivery. A poll delay of about 2 minutes is fine.
- Notifications to Matt. The agents are the audience. (Herdr's own attention states still show a woken agent.)
- Grandparent rollup, in the view or in notifications.
- PRs not linked to any agent (no "all my PRs" view).

## Anti-goals

- Parent and child both acting on the same event (two replies to one review comment, two pushes to one branch). The parent message is informational only and says so.
- Waking an agent when nothing actionable changed. Each wake can cost a full session start.
- Model calls in the poll loop. Polling is deterministic; a model runs only when an agent is woken.

## Constraints and facts

- Herdr records a parent for terminals started with `hc run` from an agent (`hc get <t> --json` → `parent`); `hc run --parent <t>` sets one explicitly. Children started inside pi (`bg_delegate`, pi-subagents) are invisible to Herdr; they inherit the parent terminal's `HC_TERMINAL_ID` but have their own `PI_SESSION_ID`. Per `hosts.md`, Herdr background children use `hc run --bg`.
- By default an `hc run` child ends when its parent's program ends (`--outlive-parent` overrides).
- Herdr never re-runs an exited program; a lost kept terminal gets a shell. Revival is our job.
- Session resume facts, checked 2026-11-02:
  - pi exports `PI_SESSION_ID` and `PI_SESSION_FILE` to its tools' environment; resume with `pi --session <file>`.
  - Claude Code accepts `--session-id <uuid>` at launch and `claude --resume <id>` keeps that id (unless `--fork-session`). Interactive sessions also write `~/.claude/sessions/<pid>.json` with `sessionId` and `cwd`; print-mode (`-p`) runs do not, so the launch recipe passes `--session-id`.
- Inside a Herdr terminal, `HC_TERMINAL_ID` names the terminal (`t1`). `hc get <t> --json` gives `foreground.pid`, `foreground.argv`, `cwd`, `labels`, `parent`, `state`.
- Herdr.app mods (gpui-kit JavaScript) run only while the window is open; they can `exec`, read files under home, and `run` app commands. See `~/.config/herdr-app/AGENTS.md`.
- Matt's global rules: recurring checks are a deterministic script on a timer that wakes a session only when output changes; a PR watch ends on merge or close, not approval.

## Approaches

| Option | Summary | Tradeoff |
|---|---|---|
| A. All in a Herdr mod | Mod stores links and polls `gh` on its own timer | One piece, but watches stop when the window closes; breaks the timer rule |
| **B. CLI + timer + view-only mod (chosen)** | `herdr-pr` CLI owns a JSON registry; a systemd user timer polls and wakes; a mod displays and calls the CLI | Three pieces, but survives window close, fits the timer rule, gives `hosts.md` a named Herdr PR watch |
| C. GitHub webhooks | Push events to a local receiver | Near-instant, but needs a public endpoint per repo or org; too much setup to save ~2 minutes |

Matt picked B.

## Design

### Registry

One JSON file, `~/.local/state/herdr-pr/registry.json`, written atomically (write temp, rename) under a lock file. Each PR entry:

- `url`, `repo` (`owner/name`), `number`
- `owner`: terminal address at link time, agent kind (`pi` | `claude`), session id, session file (pi), `cwd`, the `herdr.space`/`herdr.tab` labels, and `parent` (terminal address or null)
- `watched` (bool), `linked_at`
- `last`: last seen state used for change detection (see Events)
- `revived_as`: current terminal address when the owner was brought back

A PR has one owner. Linking a PR that another agent already owns is an error unless `--force`.

### CLI: `herdr-pr`

- `herdr-pr link <url> [--watch]`: resolve the owner and record the entry. Prime `last` from current PR state so linking does not wake anyone. Owner resolution:
  - Session id: `PI_SESSION_ID`/`PI_SESSION_FILE` when set (pi, including in-process pi children); else the `herdr-pr.session` label on the terminal (set by the launch recipe); else Claude's `~/.claude/sessions/<foreground pid>.json`. No session id: refuse.
  - Terminal: `HC_TERMINAL_ID`. If the session id differs from the terminal's own top-level session (an in-process pi child), the owner is that child session and its parent is the terminal. *(Builder's-call, settled by author.)*
  - Parent: the terminal's Herdr `parent`, or the terminal itself for an in-process child.
  - Store the terminal's `created_ms` so a reused address is never treated as the old owner. *(Author's call.)*
  - Local machine only in v1: refuse a remote owner (address with a `machine/` prefix). *(Author's call.)*
- `herdr-pr watch <url>` / `unwatch <url>` / `unlink <url>`.
- `herdr-pr ls [--agent <t>] [--with-children] [--json]`: the view's data source.
- `herdr-pr poll`: one poll pass (what the timer runs).

Global agent rule (in `global/AGENTS.md` and `hosts.md` Herdr section): after opening a PR in Herdr, run `herdr-pr link <url> --watch`. This replaces "the watch is not armed" for Herdr.

### Poller

A systemd user timer runs `herdr-pr poll` every 2 minutes. Each pass:

1. Load watched entries. One `gh api graphql` query fetches every watched PR: state, merged, mergeable, head SHA, CI rollup, reviews, review threads, comments.
2. Diff against `last`. Classify new events (below). Save the new `last` only after delivery succeeds, so a failed wake retries next pass.
3. For each PR with actionable events: ensure the owner is live (revive if not), then `hc send` one message listing the events. Then send the parent its FYI line if the parent is live.
4. On merge or close: send owner and parent a final note, set `watched=false`. The watch is over.

### Events

Actionable (wake the owner):

- Review with state `CHANGES_REQUESTED`
- New review comment or PR conversation comment from another human. Not from Matt's GitHub login (agents post as Matt too, and Matt's own comments must not wake; decided by Matt), and not from bot accounts (`[bot]` suffix or GitHub `Bot` type).
- CI rollup turned failing on the current head SHA
- PR became unmergeable (conflicts)
- Merged or closed (final note; ends the watch)

Not actionable (recorded in `last`, no wake): approvals, CI passing, label or title edits, pushes by the owner, comments by Matt's login or by bots.

### Owner message

Typed into the owner's terminal with `hc send`, once the owner is not `working` (wait for idle, blocked or done; author's call). Example:

```
[herdr-pr] flocasts/iam#412 needs action:
- review from alice: changes requested
- CI failing on a1b2c3d (terraform-plan)
Run `gh pr view https://github.com/flocasts/iam/pull/412 --comments` for details.
```

### Parent FYI

```
[herdr-pr] FYI, no action needed: child t7 is handling flocasts/iam#412 (changes requested, CI failing).
```

Sent only to the owner's direct parent, only if the parent is live. A parent that has exited is not revived for an FYI.

### Revival

If the owner terminal is not live (`hc get` state is not `live`, or its foreground is no longer the agent):

- Start `hc run --bg --keep --parent <parent> --outlive-parent --cwd <cwd> --label herdr.space=<space> --label herdr.tab=<tab> --label herdr-pr.session=<id> --name <name> -- <resume argv>`. Resume argv: `pi --session <file>` or `claude --resume <id>` (interactive, so it stays up for later events). If the parent is gone, use `--no-parent` and keep the registry's parent for rollup.
- The registry's `parent` is the source of truth for rollup and FYIs; `hc get` parent is only used at link time.
- Record the new address in `revived_as`; deliver the message once the agent reports a status (`hc wait --output` or status poll, builder's choice).
- If the cwd no longer exists, do not revive. Send the parent (if live) a line saying the owner cannot be revived and why; keep the watch.

### Worktree rule change

A child's worktree stays until every PR it owns is merged or closed. v1 never deletes anything: when the last PR owned from a worktree ends, the poller tells the parent (or the owner, if it has no parent) "worktree <path> can be removed" (decided by Matt). Update the delegation rule in `global/AGENTS.md`: "Whoever creates a worktree removes it, after the PRs it owns are merged or closed."

### Launch recipe change

`hosts.md` Herdr background child becomes: generate a UUID, then `hc run --bg --cwd <checkout> --name <name> --label herdr-pr.session=<uuid> -- claude --session-id <uuid> -p '<task>'`. Pi children need nothing extra (pi exports its session env).

### Herdr mod: `pr-watch`

Lives in `~/.config/herdr-app/mods/pr-watch/`, docked right by default.

- Follows `focused().agent` (or the focused terminal). Shows that agent's PRs, then its direct children's, grouped under each child.
- Each row: `repo#number`, title, state (draft/open/merged/closed), CI result, review state, an eye icon when watched.
- Right-click menu: Copy link, Open, Watch for changes / Stop watching, Unlink. Each action calls the CLI through `exec`.
- Reads `herdr-pr ls --json` on a modest timer (about 10 seconds), plus `watchFolder` on the registry directory.
- Backstop: scans the focused agent's terminal text (`hc read`) for `github.com/<owner>/<repo>/pull/<n>` links that aren't registered and shows them dimmed with a "Link" action.

## Where code goes

`mago0/herdr-app-mods` (new, private; Herdr.app add-ons, apart from `mago0/herdr-plugins`, which targets the old Herdr fork), `pr-watch/` directory: CLI, poller, systemd units, the mod source, and a Nix package. The mod is installed (copied or linked) into `~/.config/herdr-app/mods/pr-watch/`.

## Left to the builder

- Implementation language for the CLI (match the repo: `fleet` is Go).
- Exact GraphQL query and batching.
- How to detect that a revived agent is ready for input.
- Whether the mod lists in-process pi children as their own group (they have no terminal until revived).
- Lock-file mechanism and registry schema version field.
- Mod styling within the Herdr theme.
