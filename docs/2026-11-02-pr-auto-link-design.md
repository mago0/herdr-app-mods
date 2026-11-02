# PR auto-link design

Short design. Extends [pr-watch design](2026-11-02-pr-watch-design.md).

## Intent

The pr-watch panel made typing lag in agent panes. Every 10 seconds, and on each registry change, it ran `herdr-pr ls` and `hc read <agent> --lines 400` on herdr-app's UI thread to find PR URLs in terminal output ("Seen in output"). Moving to a layout without the panel removed the lag.

Linking today depends on an agent rule (`herdr-pr link <url> --watch` after opening a PR); the output scan is the backstop when an agent forgets. Replace both with what T3 Code does: link a PR when the command that created it finishes, using that command's own input and output, never the screen.

## Goals

- A PR an agent opens from a shell (`gh pr create`) is linked to that agent and watched, with no agent action.
- The panel does no periodic work. It redraws when the registry changes or focus moves.
- Works for pi and Claude Code panes in Herdr.

## Non-goals

- PRs opened through other means (GitHub web UI, MCP GitHub tools, `gh api`). Agents can still run `herdr-pr link` by hand.
- Agents outside Herdr. Without `HC_TERMINAL_ID` the hook does nothing.

## Anti-goals

- Linking a PR the command did not create (`gh pr view 12`, `gh pr list`, a URL echoed by `printf`). A wrong owner gets woken for someone else's PR.
- Any hook failure blocking or slowing the agent's tool call.

## Approaches

| Option | Tradeoff |
|---|---|
| **A. Hook in each agent (chosen)** | Pi extension on `tool_result`, Claude Code `PostToolUse` hook. Exact command and output, immediate. Two small adapters to keep. |
| B. Read session JSONL files in `herdr-pr poll` | No agent code, but up to 2 minutes late and tied to two session file formats. |
| C. Keep scanning terminal output | The cause of the lag. |

Matt picked A, with link and watch automatic.

## Design

### `herdr-pr hook`

New subcommand. Reads one JSON object on stdin: `{"command": "...", "output": "..."}`. Also accepts Claude Code's `PostToolUse` payload (`tool_name`, `tool_input.command`, `tool_response.stdout`/`stderr`), so the Claude hook calls it directly.

1. If `HC_TERMINAL_ID` is unset, exit 0.
2. Run `createdBy(command, output)`, a Go port of T3 Code's `changeRequestsCreatedBy` ([source](https://github.com/pingdotgg/t3code/blob/8648aea3db09b209751dcf2501c62b5f929710b4/apps/server/src/orchestration-v2/PullRequestSyncReactor.ts)), GitHub only:
   - Only if the script runs `gh pr create`.
   - Split on `&&`, `||`, `;`, `|`, newline; ignore quoted text and heredoc bodies when finding commands.
   - Take URLs on lines of their own in the output.
   - Skip a URL whose PR number appears in the other commands.
   - Take none if more URLs than create commands.
3. For each URL, run the existing `link --watch` path. Already linked by this owner: no-op. Owned by another agent: leave it, no `--force`.
4. Always exit 0. Errors go to `$HERDR_PR_STATE/hook.log`, never to the agent.

Port T3's test cases for `changeRequestsCreatedBy` as Go table tests.

### Pi extension

`pr-watch/pi/herdr-pr-link.ts`, installed to `~/.pi/agent/extensions/`.

- On `tool_result` for the `bash` tool, if the command text contains `pr create`, spawn `herdr-pr hook` detached with `{command, output}` on stdin. Return the result unchanged.
- Child env: the pi process env (carries `HC_TERMINAL_ID`) plus `PI_SESSION_ID` and `PI_SESSION_FILE` for the current session, since owner resolution needs them and pi only sets them for tool processes.
- Must work when tools arrive through `claude-bridge` over MCP; verify the tool name the handler sees in that path.

### Claude Code hook

`PostToolUse` entry, matcher `Bash`, `async: true`, command `herdr-pr hook`. Added to `~/.claude/settings.json`; README shows the snippet.

### Panel (`pr-watch/mod/main.js`)

- Remove the 10s `cx.timer.every`. Retry a failed `watchFolder` on focus change instead.
- Remove `hc read` and the "Seen in output" section, and its link-from-output action.
- Refresh on registry folder change, focus change, and menu actions only.

### Docs

README: replace "Agent rule" with "Automatic linking" (hook install steps), keep manual `link` as fallback.

### Rollout

Last step, after the hooks are installed and a real `gh pr create` is linked automatically: rewrite the Herdr PR watch line in `agent-skills/global/hosts.md` in its own commit (decided by Matt):

> **PR watch:** opening a PR with `gh pr create` links and watches it automatically. Confirm with `herdr-pr ls --agent $HC_TERMINAL_ID`; if it is missing (PR opened another way), run `herdr-pr link <url> --watch`. The watch ends on its own at merge or close; `herdr-pr unwatch <url>` stops it early.

Changing the rule before the hook works would leave PRs unlinked.

## Left to the builder

- Exact Claude `tool_response` field names; confirm from a real payload.
- Whether `registry.Add` already treats a same-owner relink as a no-op.
- Pi extension API details for reading session id and file.
