# pr-watch

`herdr-pr` records which coding agent owns which GitHub pull request (PR) and wakes that agent when the PR needs action. It makes no model calls and deletes nothing. It runs on the local machine only.

## Install

```sh
cd pr-watch
go build -o ~/.local/bin/herdr-pr ./cmd/herdr-pr
mkdir -p ~/.config/systemd/user
cp systemd/herdr-pr-poll.service systemd/herdr-pr-poll.timer ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now herdr-pr-poll.timer
ln -s "$PWD/pi/herdr-pr-link.ts" ~/.pi/agent/extensions/herdr-pr-link.ts
```

Or `nix build .#pr-watch` from the repo root; units are in `result/share/herdr-app-mods/pr-watch/systemd`.

### Herdr panel

Copy the mod into Herdr; it reloads within a second. Run this again after each change to `mod/`. Herdr does not follow a symlinked mod folder: assets go missing and edits do not reload.

```sh
rsync -a --delete --exclude '*.d.ts' --exclude jsconfig.json mod/ ~/.config/herdr-app/mods/pr-watch/
```

The mod has no place of its own (`"place": "none"`). Show it as a tab of the right side panel, beside Files and Changes:

- In a cloned layout (`herdr-app do layout.clone '{"name":"main","as":"main-prs"}'`), add a child to the `panel-body` stack: `{ "id": "prs", "type": "block", "content": { "mod": "pr-watch" } }`.
- In a cloned `tabs` mod used by that layout, add a button beside the others: `.child(panelButton("prs", "PRs", "mods/pr-watch/eye.svg"))`, and add `"prs"` to its `enabled` list so the panel choice is remembered per space.

The panel lists the PRs of the agent in the focused pane and of its direct children.

The service needs `hc` and `gh` on its PATH. The unit sets `PATH=%h/.local/bin:%h/.local/share/mise/shims:/usr/bin`; edit it if they live elsewhere. The timer polls every 2 minutes.

## Automatic linking

When an agent's shell command runs `gh pr create`, the PR it created is linked to that agent and watched. `herdr-pr hook` reads the finished command and its output and decides what was created: a URL the command also names (`gh pr view 12`), or more URLs than create commands, is not taken. The check is a port of T3 Code's, with its test cases.

Two adapters feed it:

- pi: the extension in `pi/herdr-pr-link.ts` (see Install). It runs only for `bash` calls whose command contains `pr create`.
- Claude Code: add a `PostToolUse` hook to `~/.claude/settings.json`:

  ```json
  "PostToolUse": [
    { "matcher": "Bash", "hooks": [{ "type": "command", "command": "setsid -f herdr-pr hook", "async": true }] }
  ]
  ```

  `setsid -f` matters for `claude -p`: it kills async hooks that are still running when it exits, which drops the link when `gh pr create` is its last step.

The hook never prints or fails; problems go to `hook.log` beside the registry. A PR another agent already owns is left alone.

A PR opened another way (GitHub web, `gh api`, an MCP tool) is not seen. Link it by hand:

```sh
herdr-pr link <url> --watch
```

Check with `herdr-pr ls --agent $HC_TERMINAL_ID`.

## Commands

| Command | Effect |
|---|---|
| `link <url> [--watch] [--force]` | Record the caller as owner. `--force` takes the PR from another owner. |
| `unlink <url>` | Forget the PR. |
| `watch <url>` / `unwatch <url>` | Start or stop waking the owner. |
| `ls [--agent <t>] [--with-children] [--json]` | List PRs, for one terminal and its direct children, or all. |
| `poll` | One poll pass. The timer runs this. |
| `hook` | Link and watch PRs a finished shell command created. Reads JSON on stdin: `{"command","output"}` or Claude Code's `PostToolUse` input. |

## Environment

- `HERDR_PR_STATE`: the state directory. The registry is `$HERDR_PR_STATE/registry.json`, with `registry.lock` beside it. Default `~/.local/state/herdr-pr`.
- `HERDR_PR_VIEWER`: test override for the `gh` login whose own comments never wake. Default is the login `gh` reports.

## What wakes the owner

A message starting `[herdr-pr]` is sent to the owner's terminal for:

- a review that requests changes
- a new comment or review-thread comment from a human other than the `gh` viewer
- CI failing (or erroring) on a new head commit
- a new merge conflict

Not woken: comments by the `gh` viewer login or by bots, approvals and dismissed reviews (even with a body; inline comments on an approval still wake), passing CI, draft changes, anything already seen at link time.

A busy owner is skipped and told on a later pass. Busy means status `working` or `blocked`, or no status yet for pi. Claude Code reports no status, so a Claude owner (foreground `claude`, or `node` running Claude) counts as ready once its terminal is 30 seconds old; a message sent while it works waits in its input. Any other program with no status, such as an editor or pager started after the agent exited, counts as busy, so nothing is typed into it. An owner counts as exited when a shell is in the foreground and its agent reports no status; a reported status outranks the foreground, since a busy agent's tool call can put a shell there. The owner's parent gets an FYI when it is ready.

## Merge or close

The watch ends on the pass that sees the PR merged or closed, with one message per recipient:

- A ready owner gets `[herdr-pr] <repo>#<n> was merged. Watch ended.` (or `closed`), followed by any other events of that pass. The watch ends once this send succeeds.
- A ready parent gets `[herdr-pr] FYI, no action needed: <repo>#<n> (child <terminal>) was merged. Watch ended.`
- An owner that is busy or gone is not revived: the parent is told if it is ready, and the watch ends.
- If neither is ready but one is only busy, the watch stays on and the note goes out on a later pass. Only when both are gone does the watch end with nobody told.

## Revival

If the owner terminal is gone, `poll` starts the session again in a new background terminal in the agent's folder (`claude --resume <session>` or `pi --session <file>`), and delivers on the next pass. One session that owns several PRs is started once; all its PRs then go to that terminal. A pi child running inside its parent's pi has no terminal of its own; it is revived only after its session file has been unchanged for 10 minutes. A session still open in a live terminal (labelled `herdr-pr.session=<session>`, or with `--session` or `--resume` for it in its command line or foreground process) is never started a second time: `poll` logs it and the watch stays on. If the revived session exits at startup, or cannot start, revival stops, the parent is told once, and the watch stays on. After fixing the cause, re-arm with:

```sh
herdr-pr watch <url>
```

## Nothing is deleted

`herdr-pr` never removes a PR, worktree, terminal or branch. `link` records the git worktree it runs in. When the last open PR from that worktree ends, the final message to the parent (or to the owner, if there is no ready parent) adds `FYI: worktree <path> has no open PRs left and can be removed.` Only a linked git worktree under a `_worktrees/` folder qualifies; a main checkout or home never does. Removing it is up to the agent.

## Test

```sh
pr-watch/test/smoke.sh   # fake hc and gh; prints "smoke: ok"
go -C pr-watch test ./...
```
