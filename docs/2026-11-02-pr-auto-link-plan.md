# PR auto-link Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Link and watch a PR automatically when an agent's `gh pr create` finishes, and make the pr-watch panel do no periodic work.

**Architecture:** A new `herdr-pr hook` subcommand takes one finished shell command (input and output) on stdin, finds the PRs it created with a Go port of T3 Code's `changeRequestsCreatedBy`, and runs the existing link path with watch on. A pi extension and a Claude Code `PostToolUse` hook feed it. The panel mod drops its 10s timer and terminal-output scan.

**Tech Stack:** Go (stdlib only, RE2 `regexp`), pi extension TypeScript, Herdr.app mod JavaScript (gpui-kit).

**Spec:** `docs/2026-11-02-pr-auto-link-design.md`

Repo: `~/Projects/personal/herdr-app-mods`, branch `pr-auto-link`, worktree `_worktrees/pr-auto-link`. Go module root: `pr-watch/`. Run Go commands from `pr-watch/`.

## Global Constraints

- `herdr-pr hook` always exits 0 and writes nothing to stdout or stderr. Errors append to `$HERDR_PR_STATE/hook.log` (state dir of `registry.Path()`).
- No `HC_TERMINAL_ID` in the environment: do nothing.
- GitHub only. A PR owned by another agent is left alone (no `--force`).
- Go stdlib only. RE2 has no lookbehind, lookahead or backreferences: hand-scan where T3 uses them.
- T3 Code is MIT (Copyright (c) 2026 T3 Tools Inc.). The ported file carries a header comment naming the source URL and license.
- Comments explain purpose, not history. No em or en dashes.

## Review Focus

1. A long-running or huge-output bash call (MBs of output): the pi extension must not copy it when the command has no `pr create`. Prefilter on the command text before touching output. Test: Task 3 Step 2.
2. `herdr-pr hook` when `gh` is slow or offline: the agent must not wait. pi spawns detached; Claude hook is `async: true`. Test: Task 2 `TestHookGitHubErrorStillExitsZero`.
3. Same PR seen twice (agent re-runs `gh pr create` and gets "already exists" with the URL): no error, no duplicate entry. Test: Task 2 `TestHookRelinkSameOwnerIsNoop`.
4. Claude payload where the URL is in `stderr`, not `stdout`: still linked. Test: Task 2 `TestHookClaudePayloadReadsStderr`.
5. Panel with an empty or missing state folder at startup: `watchFolder` throws; the panel must retry without a timer. Test: Task 4 Step 3 manual check.

---

### Task 1: `created` package, port of `changeRequestsCreatedBy`

**Files:**
- Create: `pr-watch/internal/created/created.go`
- Create: `pr-watch/internal/created/created_test.go`
- Create: `pr-watch/internal/created/testdata/t3_cases.json`

**Interfaces:**
- Consumes: `prurl.Parse(raw string) (prurl.Ref, error)`
- Produces: `created.By(input, output string) []prurl.Ref` (order of first appearance in output, deduped by `Ref.URL()`)

- [ ] **Step 1: Write `testdata/t3_cases.json`** from the `it.effect.each` table in [PullRequestSyncReactor.test.ts lines 1627-1757](https://github.com/pingdotgg/t3code/blob/8648aea3db09b209751dcf2501c62b5f929710b4/apps/server/src/orchestration-v2/PullRequestSyncReactor.test.ts#L1627-L1757). Array of `{name, input, output, linked: [int]}`, all 19 cases verbatim. Change `linked` to `[]` for "a GitLab merge request" (GitHub only). Add two cases:
  - `"gh pr create already exists"`: input `gh pr create --fill`, output `a pull request for branch "x" into branch "main" already exists:\nhttps://github.com/owner/repository/pull/124`, linked `[124]`.
  - `"a bash -c wrapper"`: input `bash -c 'gh pr create --fill'`, output `https://github.com/owner/repository/pull/9`, linked `[9]`.

- [ ] **Step 2: Write `TestByT3Cases`**: load the JSON, for each case `t.Run(name)`, assert the numbers of `By(input, output)` equal `linked` (empty and nil both mean none).

- [ ] **Step 3: Run** `go test ./internal/created/`. Expected: FAIL, `By` undefined.

- [ ] **Step 4: Implement `By`** following T3's `changeRequestsCreatedBy` and helpers (`shellScript`, `shellSegments`, `shellCode`, `createCommandCount`, `LISTS_CHANGE_REQUESTS`, `URL_LINE`) in the same source file, lines 41-200. GitHub's create command is `\bgh\s+pr\s+create\b`. Where T3's regex needs RE2-missing features, scan by hand:
  - `shellCode`: one pass that drops heredoc bodies (match the delimiter word, then skip to a line equal to it, or to end), single/double-quoted strings, and `#` comments that start at line start or after whitespace or `;&|(`.
  - create command must start a command (`COMMAND_START`) and end its shell word (next char is end, whitespace or `;&|)`).

- [ ] **Step 5: Run** `go test ./internal/created/`. Expected: PASS, 21 subtests.

- [ ] **Step 6: Commit** `feat(pr-watch): port T3 created-PR detection`.

### Task 2: `herdr-pr hook` subcommand

**Files:**
- Modify: `pr-watch/internal/app/deps.go` (add `Stdin io.Reader`)
- Modify: `pr-watch/cmd/herdr-pr/main.go` (set `Stdin: os.Stdin`)
- Modify: `pr-watch/internal/cli/cli.go` (usage line, `case "hook"`, extract link core)
- Create: `pr-watch/internal/cli/hook.go`, `pr-watch/internal/cli/hook_test.go`

**Interfaces:**
- Consumes: `created.By`, `owner.Resolve`, `registry.Update`, `registry.Add`
- Produces: `func (c *cmd) linkRef(ref prurl.Ref, watch, force bool) (string, error)` returning the `[herdr-pr] linked ...` line; `link` calls it. `func (c *cmd) hook() error`. Usage line: `  hook                              link PRs a finished shell command created (stdin: JSON)`.

Payload: decode stdin into one struct accepting both shapes. Generic: `{"command", "output"}`. Claude `PostToolUse`: `{"tool_name":"Bash","tool_input":{"command"},"tool_response":{"stdout","stderr"}}`; output is stdout + "\n" + stderr. Confirm Claude field names from a real payload (Task 5 Step 1) before relying on them; the test fixtures use the names above.

- [ ] **Step 1: Write tests** in `hook_test.go`, reusing `fakeHC`, `fakeGH`, `claudeTerm` from `cli_test.go`:
  - `TestHookLinksAndWatchesCreatedPR`: generic payload, `gh pr create --fill` / `https://github.com/flocasts/iam/pull/412`, owner env set. Registry has one entry, `Watched == true`. stdout and stderr empty. exit 0.
  - `TestHookClaudePayloadReadsStderr`: Claude shape, URL only in `stderr`. Entry linked.
  - `TestHookNoTerminalIsNoop`: `HC_TERMINAL_ID` empty. `fakeGH.calls == 0`, registry unchanged.
  - `TestHookNonCreateCommandIsNoop`: `gh pr view 412`. `fakeGH.calls == 0`.
  - `TestHookOwnedByOtherIsLeftAlone`: entry pre-owned by another session. Owner unchanged, exit 0, line in `hook.log`.
  - `TestHookRelinkSameOwnerIsNoop`: run hook twice. One entry.
  - `TestHookGitHubErrorStillExitsZero`: `fakeGH` returns error. exit 0, stderr empty, `hook.log` contains the error.
  - `TestHookBadJSONStillExitsZero`: stdin `not json`. exit 0.

- [ ] **Step 2: Run** `go test ./internal/cli/ -run Hook`. Expected: FAIL.

- [ ] **Step 3: Implement.** `Run` returns 0 for `hook` regardless of error, after appending `time | error` to `filepath.Join(filepath.Dir(d.StatePath), "hook.log")`. `link` keeps its current output and errors via `linkRef`.

- [ ] **Step 4: Run** `go test ./...`. Expected: PASS, existing tests included.

- [ ] **Step 5: Commit** `feat(pr-watch): herdr-pr hook links PRs a command created`.

### Task 3: pi extension

**Files:**
- Create: `pr-watch/pi/herdr-pr-link.ts`
- Modify: `pr-watch/README.md` (Install section)

**Interfaces:**
- Consumes: `herdr-pr hook` generic payload `{"command","output"}` on stdin.
- Produces: nothing other tasks use.

- [ ] **Step 1: Implement** `export default function (pi: ExtensionAPI)` with `pi.on("tool_result", ...)`, typed from `@earendil-works/pi-coding-agent` (pattern: `~/.pi/agent/extensions/protected-paths.ts`). Check the real field names in that package's types for tool name, input and result content.
  - Return `undefined` unless the tool is `bash`, `process.env.HC_TERMINAL_ID` is set, and the command matches `/\bpr\s+create\b/`.
  - Output: join the text blocks of the result content.
  - `spawn("herdr-pr", ["hook"], { detached: true, stdio: ["pipe", "ignore", "ignore"], env: { ...process.env, PI_SESSION_ID, PI_SESSION_FILE } })`, write JSON, end stdin, `unref()`. Session id and file from `ctx.sessionManager` (`getSessionFile()` exists; find the id getter in the types). Swallow spawn errors (`child.on("error", () => {})`).
  - Never modify the result.

- [ ] **Step 2: Verify with a stub.** Put a stub `herdr-pr` first on PATH that writes stdin and `env | grep -E 'PI_SESSION|HC_TERMINAL'` to `/tmp/herdr-pr-stub.log`. Start pi in a Herdr terminal with `PATH=<stubdir>:$PATH pi -e pr-watch/pi/herdr-pr-link.ts`, using the default provider (claude-bridge). Ask it to run `echo https://github.com/o/r/pull/5 # gh pr create` and then `seq 1 200000`. Expected: the log has one payload (the first call), with the command, the URL in output, and `PI_SESSION_ID`, `PI_SESSION_FILE`, `HC_TERMINAL_ID` set. The `seq` call writes nothing. If the first call does not reach the handler under claude-bridge, stop and report.

- [ ] **Step 3: README**: install line `ln -s "$PWD/pi/herdr-pr-link.ts" ~/.pi/agent/extensions/herdr-pr-link.ts`.

- [ ] **Step 4: Commit** `feat(pr-watch): pi extension feeds herdr-pr hook`.

### Task 4: panel without polling

**Files:**
- Modify: `pr-watch/mod/main.js`

- [ ] **Step 1: Edit.**
  - Remove `cx.timer.every(10000, ...)` in `init`.
  - `render`: on focus change, call `this.watch(cx)` (retries a failed watch) and `this.refresh(cx)`.
  - `refresh`: run only `herdr-pr ls`; remove the `hc` import, `hc(["read", ...])`, `PR_URL`, `this.seen`.
  - Remove the "Seen in output" section, `seenRow`, and `link()`.

- [ ] **Step 2: Install and measure.** Copy to `~/.config/herdr-app/mods/pr-watch/main.js` (herdr-app reloads within a second), `herdr-app do layout.switch '{"name":"main-prs"}'`, focus a pi pane. Sample herdr-app main-thread CPU for 20 s (`/proc/<pid>/task/<pid>/stat` fields 14+15). Expected: within 1 point of the no-panel baseline of about 5%. Ask Matt to type in a pi pane; expected: no lag.

- [ ] **Step 3: Missing-folder check.** With `HERDR_PR_STATE` unchanged, rename `~/.local/state/herdr-pr` aside, reload the mod, restore the folder, change focus. Expected: panel shows the registry again. Put the folder back exactly.

- [ ] **Step 4: Commit** `perf(pr-watch): panel refreshes on registry change only`.

### Task 5: Claude hook, install, docs

**Files:**
- Modify: `pr-watch/README.md` (replace "Agent rule" with "Automatic linking"; `hook` row in Commands)
- Modify (local, not in repo): `~/.claude/settings.json`

- [ ] **Step 1: Capture a real `PostToolUse` payload.** Temporarily add a `PostToolUse` `Bash` hook `cat > /tmp/claude-posttool.json` in a scratch project's `.claude/settings.local.json`, run `claude -p 'run: echo hi'` there, read the file. Confirm `tool_input.command`, `tool_response.stdout`, `tool_response.stderr`. If names differ, fix Task 2's struct and fixtures first. Remove the scratch hook.

- [ ] **Step 2: Build and install** `go build -o ~/.local/bin/herdr-pr ./cmd/herdr-pr`. Symlink the pi extension (Task 3 Step 3).

- [ ] **Step 3: Show Matt the `~/.claude/settings.json` diff** (a `PostToolUse` entry: matcher `Bash`, `{"type":"command","command":"herdr-pr hook","async":true}`), then apply after approval. Back up the file first.

- [ ] **Step 4: Replay check.** `echo '<captured payload with command "gh pr create --fill" and stdout set to an open PR URL Matt owns>' | HC_TERMINAL_ID=<a live claude terminal> herdr-pr hook; herdr-pr ls --json`. Expected: entry linked and watched. Then `herdr-pr unlink <url>`.

- [ ] **Step 5: README** "Automatic linking": what triggers it, the two install steps, manual `link` as fallback for PRs opened another way.

- [ ] **Step 6: Commit** `docs(pr-watch): automatic linking`.

### Task 6: real PR, then the agent rule

- [ ] **Step 1: Open this branch's PR from a pi agent in Herdr** with `gh pr create`. Expected: `herdr-pr ls --agent $HC_TERMINAL_ID` shows it, watched, without a manual `link`.

- [ ] **Step 2: Rewrite `global/hosts.md:20`** in `~/Projects/personal/agent-skills`, in a worktree off `origin/main` (local `main` has an unpushed commit), with the exact text from the design's Rollout section. Commit on its own: `docs(hosts): Herdr PR watch links automatically`. Push and PR per Matt's call.
