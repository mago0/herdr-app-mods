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

It records the calling agent as the owner (it must run inside a Herdr terminal) and starts the watch. The PR's current state is the baseline, so only later changes wake the owner.

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
- CI failing on a new head commit
- a new merge conflict
- the PR merging or closing (the watch then ends)

Not woken: comments by the `gh` viewer login or by bots, approvals, passing CI, draft changes, anything already seen at link time. A busy owner (status `working`, or none yet) is skipped and told on a later pass. The owner's parent gets an FYI when it is live.

## Revival

If the owner terminal is gone, `poll` starts the session again in a new background terminal in the same directory (`claude --resume <session>` or `pi --session <file>`), and delivers on the next pass. If the revived session exits at startup, or cannot start, revival stops, the parent is told once, and the watch stays on. After fixing the cause, re-arm with:

```sh
herdr-pr watch <url>
```

## Nothing is deleted

`herdr-pr` never removes a PR, worktree, terminal or branch. When the last open PR of a worktree ends, the owner is told that the worktree can go; removing it is up to the agent.

## Test

```sh
pr-watch/test/smoke.sh   # fake hc and gh; prints "smoke: ok"
go -C pr-watch test ./...
```
