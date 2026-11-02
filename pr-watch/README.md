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
```

Or `nix build .#pr-watch` from the repo root; units are in `result/share/herdr-app-mods/pr-watch/systemd`.

The service needs `hc` and `gh` on its PATH. The unit sets `PATH=%h/.local/bin:%h/.local/share/mise/shims:/usr/bin`; edit it if they live elsewhere. The timer polls every 2 minutes.

## Agent rule

Run this right after opening a PR:

```sh
herdr-pr link <url> --watch
```

It records the calling agent as the owner (it must run inside a Herdr terminal) and starts the watch. The PR's current state is the baseline, so only later changes wake the owner. A PR that is already merged or closed is linked without a watch.

## Commands

| Command | Effect |
|---|---|
| `link <url> [--watch] [--force]` | Record the caller as owner. `--force` takes the PR from another owner. |
| `unlink <url>` | Forget the PR. |
| `watch <url>` / `unwatch <url>` | Start or stop waking the owner. |
| `ls [--agent <t>] [--with-children] [--json]` | List PRs, for one terminal and its direct children, or all. |
| `poll` | One poll pass. The timer runs this. |

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

A busy owner is skipped and told on a later pass. Busy means status `working` or `blocked`, or no status yet for pi. Claude Code reports no status, so a Claude owner counts as ready once its terminal is 30 seconds old; a message sent while it works waits in its input. The owner's parent gets an FYI when it is ready.

## Merge or close

The watch ends on the pass that sees the PR merged or closed, with one message per recipient:

- A ready owner gets `[herdr-pr] <repo>#<n> was merged. Watch ended.` (or `closed`), followed by any other events of that pass. The watch ends once this send succeeds.
- A ready parent gets `[herdr-pr] FYI, no action needed: <repo>#<n> (child <terminal>) was merged. Watch ended.`
- An owner that is busy or gone is not revived or waited for: the parent is told if it is ready, and the watch ends anyway.

## Revival

If the owner terminal is gone, `poll` starts the session again in a new background terminal in the agent's folder (`claude --resume <session>` or `pi --session <file>`), and delivers on the next pass. One session that owns several PRs is started once; all its PRs then go to that terminal. A pi child running inside its parent's pi has no terminal of its own; it is revived only after its session file has been unchanged for 10 minutes. If the revived session exits at startup, or cannot start, revival stops, the parent is told once, and the watch stays on. After fixing the cause, re-arm with:

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
