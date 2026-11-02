# PR watch for Herdr.app Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Agents in Herdr.app link the PRs they open; a timer wakes the owning agent (reviving it if exited) on actionable PR changes, tells its direct parent FYI, and a Herdr mod shows each agent's PRs plus its children's.

**Architecture:** A Go CLI `herdr-pr` owns a JSON registry under `~/.local/state/herdr-pr/`. `herdr-pr poll`, run by a systemd user timer every 2 minutes, makes one `gh api graphql` call, diffs against stored state, and delivers with `hc send` / revives with `hc run`. A gpui-kit mod reads `herdr-pr ls --json` and calls the CLI for its menu actions.

**Tech Stack:** Go 1.24+ stdlib only (no new deps), `gh` CLI, herdr-core `hc`, systemd user units, Herdr.app gpui-kit JavaScript mod, Nix `buildGoModule`.

**Spec:** `docs/2026-11-02-pr-watch-design.md`

## Global Constraints

- Code lives in `pr-watch/` in `mago0/herdr-app-mods` (private), module `github.com/mago0/herdr-app-mods/pr-watch`, binary `herdr-pr`. Layout follows `mago0/herdr-plugins` `tree/` (local copy `~/Projects/personal/herdr-plugins/tree`): `cmd/`, `internal/`, `test/smoke.sh`, `package.nix`, `README.md`. No `herdr-plugin.toml` (that is the old Herdr's plugin manifest); version lives in `pr-watch/VERSION`.
- Registry path `~/.local/state/herdr-pr/registry.json` (override `HERDR_PR_STATE`), lock file `registry.lock` beside it; atomic write (temp + rename) under `flock`.
- Poll interval: 2 minutes. One GraphQL request per pass for all linked open PRs.
- No model calls anywhere. No deletion of anything (worktrees are reported, never removed).
- Local machine only: refuse owners whose address contains `/`.
- Messages start with `[herdr-pr]`. Parent message says "FYI, no action needed".
- Comments by the `gh` viewer login (Matt, `mago0`) and by bots (`__typename == "Bot"` or login ending `[bot]`) never wake.
- Comments in Go/JS: purpose and durable reasoning only, about two lines.

## Review Focus

1. Link while a poll is running: both write the registry. Expect no lost entry (lock) - test in Task 1.
2. `gh` fails, is rate limited, or one PR is missing from the response: expect no wake, no change to that PR's `last`, nonzero exit only on whole-request failure - test in Task 6.
3. Owner is `working` or revival just started: expect no send this pass and `last` unchanged, so the next pass retries - test in Task 6.
4. Same PR in different URL forms (`/files` suffix, `http://`, mixed-case owner, trailing slash): expect one entry - test in Task 1.
5. Terminal address reused by a new terminal after the owner exited (`created_ms` differs): expect revival, never a send to the stranger - test in Task 5.

---

### Task 1: Module scaffold, PR URL parsing, registry

**Files:**
- Create: `pr-watch/go.mod`, `pr-watch/VERSION` (`0.1.0`), `pr-watch/.gitignore` (`/herdr-pr`, `mod/*.d.ts`, `mod/jsconfig.json`)
- Create: `pr-watch/internal/prurl/prurl.go`, `prurl_test.go`
- Create: `pr-watch/internal/registry/registry.go`, `registry_test.go`

**Interfaces:**
- Produces:
  - `prurl.Parse(raw string) (Ref, error)`; `type Ref struct{ Owner, Name string; Number int }`; `(Ref) Repo() string` = `owner/name` lowercased; `(Ref) URL() string` = `https://github.com/<owner>/<name>/pull/<n>` (canonical key).
  - `type Owner struct{ Address string; CreatedMs int64; Kind string /* "pi"|"claude" */; SessionID, SessionFile, Cwd, Space, Tab, Parent string; ParentCreatedMs int64 }`
  - `type Display struct{ Title, State string /* OPEN|MERGED|CLOSED */; Draft bool; CI string /* SUCCESS|FAILURE|PENDING|"" */; Review string /* reviewDecision */ }`
  - `type Last struct{ State string; SeenIDs []string; CIFailingSHA string; Conflicting bool; WorktreeNoted bool }`
  - `type Entry struct{ URL, Repo string; Number int; Owner Owner; Watched bool; LinkedAt time.Time; Display Display; Last Last; RevivedAs string; RevivedCreatedMs int64; Primed bool }`
  - `type Registry struct{ Version int; Entries []Entry }` (`Version` = 1)
  - `registry.Path() string`; `registry.Update(path string, fn func(*Registry) error) error` (flock + load + fn + atomic save; missing file = empty); `registry.Load(path string) (*Registry, error)` (read-only, no lock).
  - `(*Registry) Find(url string) *Entry`; `(*Registry) Add(e Entry, force bool) error` returns `registry.ErrOwned` when URL exists with a different `Owner.SessionID` and `!force`; same session re-link is a no-op that keeps `Last`.

- [ ] **Step 1: Write failing tests**
  - `TestParseVariants`: `https://github.com/FloCasts/IAM/pull/412`, `http://github.com/flocasts/iam/pull/412/files`, `github.com/flocasts/iam/pull/412/` all give `URL() == "https://github.com/flocasts/iam/pull/412"`; `https://github.com/flocasts/iam/issues/412` and `not a url` return an error.
  - `TestAddOwnership`: Add for session A, Add same URL session B without force → `errors.Is(err, ErrOwned)`; with force → owner is B; Add same URL session A again keeps the existing `Last`.
  - `TestUpdateConcurrent`: 20 goroutines each `Update` adding a distinct URL to a temp path → `Load` has 20 entries.
  - `TestUpdateAtomicOnError`: `fn` returns an error → file content unchanged.
- [ ] **Step 2: Run** `cd pr-watch && go test ./internal/...` - Expected: FAIL (undefined).
- [ ] **Step 3: Implement** `prurl` and `registry` per Interfaces. Lock with `syscall.Flock(fd, LOCK_EX)` on `registry.lock`; save via `os.CreateTemp` in the same dir + `os.Rename`; `json.MarshalIndent`.
- [ ] **Step 4: Run** `go test ./internal/...` - Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "pr-watch: PR URL parsing and locked registry"`

### Task 2: herdr-core client

**Files:**
- Create: `pr-watch/internal/hc/hc.go`, `hc_test.go`

**Interfaces:**
- Produces:
  - `type Terminal struct{ Address string; CreatedMs int64 \`json:"created_ms"\`; Cwd, State string; Parent *string; Labels map[string]string; Status *struct{ App, State string }; Foreground *struct{ Name string; Pid int; Argv []string; Cwd string } }` (fields match `hc get <t> --json`).
  - `type RunSpec struct{ Name, Cwd, Parent string; NoParent bool; Labels map[string]string; Argv []string }`
  - `type Client interface{ Get(addr string) (Terminal, error); Send(addr, text string) error; Run(RunSpec) (Terminal, error) }`
  - `hc.ErrNotFound` from `Get` when the terminal does not exist; `hc.New(bin string) Client` (exec implementation; `bin` default `"hc"`).
  - `Run` always passes `--bg --keep --outlive-parent --json`; `--parent <p>` or `--no-parent`; one `--label k=v` per label; `--name`, `--cwd`; then `--` and argv. Parse the returned JSON into `Terminal`.

- [ ] **Step 1: Write failing tests** with a fake `hc` script on a temp PATH that logs its argv to a file and prints fixture JSON:
  - `TestGetParses`: fixture copied from a real `hc get t1 --json` → `CreatedMs`, `Foreground.Name == "pi"`, `Status.State == "working"`.
  - `TestGetNotFound`: fake exits 1 with `no terminal` → `errors.Is(err, ErrNotFound)`.
  - `TestRunArgv`: `Run(RunSpec{Name:"pr-iam-412", Cwd:"/w", Parent:"t1", Labels:{"herdr.space":"s"}, Argv:["pi","--session","/f.jsonl"]})` logs exactly `run --bg --keep --outlive-parent --json --name pr-iam-412 --cwd /w --parent t1 --label herdr.space=s -- pi --session /f.jsonl`.
- [ ] **Step 2: Run** `go test ./internal/hc/` - Expected: FAIL.
- [ ] **Step 3: Implement** with `os/exec`; labels sorted by key for stable argv. Before writing `TestGetNotFound`'s fixture, run `hc get t999 --json; echo $?` and copy the real stderr/exit code.
- [ ] **Step 4: Run** - Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "pr-watch: hc client"`

### Task 3: GitHub fetch

**Files:**
- Create: `pr-watch/internal/github/github.go`, `query.graphql` (embedded), `github_test.go`, `testdata/response.json`

**Interfaces:**
- Produces:
  - `type Actor struct{ Login string; Bot bool }`
  - `type Item struct{ ID string; Kind string /* "review"|"comment"|"thread_comment" */; Author Actor; State string /* review state */; Body string }`
  - `type PR struct{ URL, Title, State string; Draft bool; Mergeable string /* MERGEABLE|CONFLICTING|UNKNOWN */; HeadSHA, CI, ReviewDecision string; FailingChecks []string; Items []Item }`
  - `type Fetcher interface{ Fetch(ctx context.Context, refs []prurl.Ref) (map[string]PR, error); Viewer(ctx context.Context) (string, error) }` (map keyed by `Ref.URL()`; a PR missing upstream is absent from the map, not an error).
  - `github.New() Fetcher` runs `gh api graphql -f query=<built>`; one aliased `repository(owner,name){ pr<i>: pullRequest(number){…} }` block per ref; CI from `commits(last:1){nodes{commit{oid statusCheckRollup{state contexts(first:50){nodes{... on CheckRun{name conclusion} ... on StatusContext{context state}}}}}}}`; items from `reviews(last:50)`, `comments(last:50)`, `reviewThreads(last:50){nodes{comments(last:20)}}`; `author{login __typename}`.
  - `github.Parse(body []byte, refs []prurl.Ref) (map[string]PR, error)` exported for tests.

- [ ] **Step 1: Write failing tests**
  - Capture `testdata/response.json` once from one merged and one open PR in a public repo (for example `cli/cli`), never a `flocasts` repo, even though this repo is private, so it can be made public later. `TestParse`: open PR has `CI` set, items include a review and a comment with `Author.Bot` correct; merged PR `State == "MERGED"`.
  - `TestParseMissingPR`: response with `pr1: null` → map lacks that URL, no error.
  - `TestBuildQueryAliases`: two refs in the same repo and one in another produce aliases `r0 { pr0 pr1 } r1 { pr0 }` and a mapping back to URLs.
- [ ] **Step 2: Run** `go test ./internal/github/` - Expected: FAIL.
- [ ] **Step 3: Implement.** `FailingChecks` = check names with conclusion `FAILURE|TIMED_OUT|CANCELLED` or status `FAILURE|ERROR`.
- [ ] **Step 4: Run** - Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "pr-watch: batched GitHub fetch"`

### Task 4: Owner resolution and `link` / `unlink` / `watch` / `unwatch` / `ls`

**Files:**
- Create: `pr-watch/internal/owner/resolve.go`, `resolve_test.go`
- Create: `pr-watch/internal/cli/cli.go`, `cli_test.go`
- Create: `pr-watch/cmd/herdr-pr/main.go`

**Interfaces:**
- Consumes: `registry.*`, `prurl.Parse`, `hc.Client`, `github.Fetcher` (Task 3).
- Produces:
  - `owner.Resolve(env func(string) string, c hc.Client, claudeSessionsDir string) (registry.Owner, error)`. Order (spec "Owner resolution"):
    1. Terminal = `env("HC_TERMINAL_ID")`; empty → error `not inside a Herdr terminal`; contains `/` → error `remote owners are not supported`.
    2. `t := c.Get(terminal)`.
    3. Session: `PI_SESSION_ID` (+`PI_SESSION_FILE`, Kind `pi`) → else `t.Labels["herdr-pr.session"]` (Kind from `t.Foreground.Name`) → else `<claudeSessionsDir>/<t.Foreground.Pid>.json` `sessionId` (Kind `claude`) → else error `no session id`.
    4. In-process pi child: Kind `pi` and `t.Labels["herdr-pr.session"]` set and different from `PI_SESSION_ID`, or the terminal's own pi session (read `/proc/<fgpid>/environ` `PI_SESSION_ID`) differs from env → `Address=""`, `Parent=terminal`, `ParentCreatedMs=t.CreatedMs`. Otherwise `Address=terminal`, `CreatedMs=t.CreatedMs`, `Parent=*t.Parent` (and its `CreatedMs` via `Get`).
    5. `Cwd` = `os.Getwd()`; `Space`/`Tab` from `t.Labels["herdr.space"|"herdr.tab"]`.
  - `cli.Run(args []string, deps Deps, stdout, stderr io.Writer) int` with `type Deps struct{ Env func(string) string; HC hc.Client; GH github.Fetcher; StatePath, ClaudeSessions string; Now func() time.Time }` (`github.Fetcher` from Task 3; `link` calls it once to prime).
  - Subcommands: `link <url> [--watch] [--force]` (prime `Display` and `Last` from one fetch so linking never wakes; set `Primed=true`), `unlink <url>`, `watch <url>`, `unwatch <url>`, `ls [--agent <addr>] [--with-children] [--json]`, `poll` (Task 6).
  - `ls --json` shape: `{"agent":"t3","own":[EntryView],"children":[{"owner":{"address":"t7","session":"…","kind":"claude","cwd":"…"},"prs":[EntryView]}]}`; `EntryView` = `{url, repo, number, watched, title, state, draft, ci, review, owner_address}`. "Own" = entries whose `Owner.Address == agent` or (`RevivedAs == agent`); "children" = entries whose `Owner.Parent == agent`, grouped by `Owner.SessionID`. Without `--agent`, all entries under `"own"`.

- [ ] **Step 1: Write failing tests** (fake `hc.Client`, temp claude sessions dir, fake fetcher):
  - `TestResolvePiTopLevel`, `TestResolvePiInProcessChild` (`Address==""`, `Parent=="t1"`), `TestResolveClaudeLabel`, `TestResolveClaudeSessionsFile`, `TestResolveRemoteRefused`, `TestResolveNoSession`.
  - `TestLinkPrimesNoWake`: link with fetcher returning a PR that already has a `CHANGES_REQUESTED` review → entry `Primed`, its review ID in `Last.SeenIDs`.
  - `TestLinkOwnedByOther`: exit code 1, stderr contains `already linked by`.
  - `TestLsWithChildren`: entries for t3, t7 (parent t3), in-process child (parent t3, no address), grandchild t9 (parent t7) → `own` has t3's, `children` has t7's and the in-process child's groups, t9 absent.
- [ ] **Step 2: Run** `go test ./internal/owner/ ./internal/cli/` - Expected: FAIL.
- [ ] **Step 3: Implement** `Resolve`, `cli.Run` with stdlib `flag` per subcommand, and `main.go` wiring real deps (`hc.New("hc")`, `github.New()`, `registry.Path()`, `~/.claude/sessions`, `time.Now`).
- [ ] **Step 4: Run** `go test ./...` - Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "pr-watch: owner resolution and link/ls commands"`

### Task 5: Event classification, messages, liveness and revival

**Files:**
- Create: `pr-watch/internal/events/events.go`, `events_test.go`
- Create: `pr-watch/internal/deliver/deliver.go`, `deliver_test.go`

**Interfaces:**
- Consumes: `registry.Entry/Last/Display`, `github.PR`, `hc.Client/Terminal/RunSpec`.
- Produces:
  - `type Event struct{ Kind, Text string }`; kinds `changes_requested`, `comment`, `ci_failing`, `conflict`, `merged`, `closed`.
  - `events.Classify(prev registry.Last, cur github.PR, viewer string) []Event`: new items (ID not in `prev.SeenIDs`) → review `CHANGES_REQUESTED` → `changes_requested` ("review from <login>: changes requested"); any item with non-empty body from a non-bot author != viewer → `comment` ("<kind> from <login>"); `cur.CI=="FAILURE"` and `cur.HeadSHA != prev.CIFailingSHA` → `ci_failing` ("CI failing on <sha7> (<checks>)"); `Mergeable=="CONFLICTING"` and `!prev.Conflicting` → `conflict`; state changed to MERGED/CLOSED → `merged`/`closed`.
  - `events.Next(cur github.PR) registry.Last` (all current IDs seen, `CIFailingSHA` = head SHA when failing else "", `Conflicting`, `State`).
  - `events.DisplayOf(cur github.PR) registry.Display`.
  - `deliver.OwnerMessage(e registry.Entry, evs []events.Event) string`, `deliver.ParentFYI(e registry.Entry, ownerAddr string, evs []events.Event) string`, `deliver.FinalNote(e registry.Entry, state string) string`, `deliver.WorktreeNote(cwd string) string`. Exact formats (spec "Owner message", "Parent FYI"):
    ```
    [herdr-pr] flocasts/iam#412 needs action:
    - review from alice: changes requested
    - CI failing on a1b2c3d (terraform-plan)
    Run `gh pr view https://github.com/flocasts/iam/pull/412 --comments` for details.
    ```
    ```
    [herdr-pr] FYI, no action needed: child t7 is handling flocasts/iam#412 (changes requested, CI failing).
    ```
    Final: `[herdr-pr] flocasts/iam#412 was merged. Watch ended.` Worktree: `[herdr-pr] FYI: worktree <cwd> has no open PRs left and can be removed.`
  - `type Liveness int` (`Live`, `Busy`, `Gone`); `deliver.Check(c hc.Client, addr string, createdMs int64) (Liveness, hc.Terminal, error)`: `ErrNotFound`, state != `live`, `CreatedMs` mismatch, or no `Status` → `Gone`; `Status.State == "working"` → `Busy`; else `Live`.
  - `deliver.Revive(c hc.Client, e registry.Entry, parentLive bool) (hc.Terminal, error)`: refuses (error `cwd missing`) if `e.Owner.Cwd` does not exist; argv `pi --session <SessionFile>` or `claude --resume <SessionID>`; name `pr-<repo name>-<number>` truncated to 32; labels `herdr.space`, `herdr.tab`, `herdr-pr.session`; `Parent` when `parentLive` else `NoParent`.

- [ ] **Step 1: Write failing tests**
  - `TestClassifyViewerAndBotIgnored`: new comment by `mago0`, new comment by `github-actions` (Bot), new comment by `renovate[bot]` → no events.
  - `TestClassifyChangesRequestedAndComment`, `TestClassifyCIOncePerSHA` (same failing SHA twice → one event), `TestClassifyApprovalAndCIGreenSilent`, `TestClassifyMerged`.
  - `TestOwnerMessageExact`, `TestParentFYIExact` (strings above, byte-for-byte).
  - `TestCheckCreatedMsMismatchIsGone` (Review Focus 5), `TestCheckWorkingIsBusy`, `TestCheckNotFoundIsGone`.
  - `TestReviveArgvPiAndClaude`, `TestReviveNoParentWhenParentGone`, `TestReviveCwdMissing`.
- [ ] **Step 2: Run** `go test ./internal/events/ ./internal/deliver/` - Expected: FAIL.
- [ ] **Step 3: Implement** per Interfaces.
- [ ] **Step 4: Run** - Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "pr-watch: event classification, messages and revival"`

### Task 6: `poll`

**Files:**
- Create: `pr-watch/internal/poll/poll.go`, `poll_test.go`
- Modify: `pr-watch/internal/cli/cli.go` (wire `poll`)

**Interfaces:**
- Consumes: everything above.
- Produces: `poll.Run(ctx context.Context, d cli.Deps) error`.

Algorithm (one pass; the registry lock is held only for load and for save, never across `gh` or `hc` calls):

```
snapshot := registry.Load
refs := entries with Display.State in {"", OPEN}          // display for unwatched too
prs, err := GH.Fetch(refs); if err: return err            // nothing saved
viewer := env HERDR_PR_VIEWER if set (test override, documented in README) else GH.Viewer()
for each entry e with PR p present in prs:
    update e.Display = DisplayOf(p)                          // always
    if !e.Watched: e.Last = Next(p); continue
    evs := Classify(e.Last, p, viewer); if len(evs)==0: e.Last = Next(p); continue
    addr, created := current owner terminal: RevivedAs/RevivedCreatedMs if set, else Owner.Address/CreatedMs
    switch Check(addr, created):
      Busy: continue                                         // Last unchanged -> retry
      Gone: t, err := Revive(e, parentLive); if err: send parent "cannot revive: <err>" once, continue
            e.RevivedAs, e.RevivedCreatedMs = t.Address, t.CreatedMs; continue   // deliver next pass
      Live: Send(addr, OwnerMessage); if parent live: Send(parent, ParentFYI)
            e.Last = Next(p)
            if merged/closed: Send FinalNote to owner (and parent), e.Watched = false
                              if no other open entry shares Owner.Cwd and !Last.WorktreeNoted: WorktreeNote to parent (or owner), Last.WorktreeNoted = true
registry.Update: merge changed fields back by URL (entries added/removed meanwhile are kept/dropped as on disk)
```

Merged/closed PRs found with `Watched == false` get `Display` updated and nothing sent.

- [ ] **Step 1: Write failing tests** (fake `hc.Client` recording sends, fake fetcher, temp registry):
  - `TestPollWakesOwnerAndFYIParent`: one changes-requested review → owner gets the exact `OwnerMessage`, parent gets `ParentFYI`, grandparent nothing; `Last.SeenIDs` updated.
  - `TestPollNoChangeNoSend`.
  - `TestPollBusyRetries` (Review Focus 3): owner `working` → no send, `Last` unchanged; second pass with owner idle → send.
  - `TestPollRevivesThenDeliversNextPass`: owner `ErrNotFound` → `Run` called with `--parent`, no send; next pass revived terminal idle → send.
  - `TestPollFetchErrorSavesNothing` (Review Focus 2): fetcher error → `Run` returns error, registry bytes unchanged. `TestPollMissingPRUntouched`: PR absent from map → entry unchanged, no send.
  - `TestPollMergedEndsWatchAndNotesWorktree`: merged → final note to owner and parent, `Watched=false`, worktree note once; second pass sends nothing.
  - `TestPollConcurrentLinkKept`: an entry added to the file between load and save survives.
- [ ] **Step 2: Run** `go test ./internal/poll/` - Expected: FAIL.
- [ ] **Step 3: Implement** `poll.Run` per algorithm; wire `herdr-pr poll` (exit 1 and message on stderr on error).
- [ ] **Step 4: Run** `go test ./...` - Expected: PASS.
- [ ] **Step 5: Commit** `git commit -m "pr-watch: poll pass with wake, FYI and revival"`

### Task 7: systemd units, Nix package, smoke test, README

**Files:**
- Create: `pr-watch/systemd/herdr-pr-poll.service`, `pr-watch/systemd/herdr-pr-poll.timer`
- Create: `pr-watch/package.nix` (copy `~/Projects/personal/herdr-plugins/tree/package.nix` shape, version from `VERSION`; `subPackages = [ "cmd/herdr-pr" ]`; install `systemd/` and `mod/` under `$out/share/herdr-app-mods/pr-watch`)
- Create: root `flake.nix` (same shape as `herdr-plugins/flake.nix`, packages `pr-watch` and `default = pr-watch`, no apps or module), root `README.md` (one-table index of mods, like `herdr-plugins/README.md`)
- Create: `pr-watch/test/smoke.sh`, `pr-watch/README.md`

Units:
- Service: `Type=oneshot`, `ExecStart=%h/.local/bin/herdr-pr poll`, `Environment=PATH=%h/.local/bin:%h/.local/share/mise/shims:/usr/bin` (needs `hc` and `gh`).
- Timer: `OnBootSec=2min`, `OnUnitActiveSec=2min`, `Persistent=true`, `WantedBy=timers.target`.

- [ ] **Step 1: Write** `test/smoke.sh`: builds `herdr-pr` to a temp dir, puts fake `hc` and `gh` on PATH (fixture JSON), sets `HERDR_PR_STATE` to a temp dir, runs `link` (as a fake terminal t3 with `PI_SESSION_ID`), mutates the fake `gh` response to add a changes-requested review, runs `poll`, and asserts the fake `hc` log contains `send t3` with `needs action`.
- [ ] **Step 2: Run** `pr-watch/test/smoke.sh` - Expected: FAIL until wired, then `smoke: ok`.
- [ ] **Step 3: Write** units, `package.nix`, flake entry, READMEs (install: `go build -o ~/.local/bin/herdr-pr ./cmd/herdr-pr`, copy units to `~/.config/systemd/user/`, `systemctl --user enable --now herdr-pr-poll.timer`).
- [ ] **Step 4: Verify** `nix build .#pr-watch` succeeds (set `vendorHash = null`, since there are no deps); `systemd-analyze --user verify pr-watch/systemd/*.service pr-watch/systemd/*.timer` prints nothing.
- [ ] **Step 5: Commit** `git commit -m "pr-watch: systemd units, nix package, smoke test"`

### Task 8: Herdr.app mod `pr-watch`

**Files:**
- Create: `pr-watch/mod/main.js`, `pr-watch/mod/mod.json` (`{ "place": "right", "size": 320 }`), `pr-watch/mod/eye.svg`
- Install: `ln -s ~/Projects/personal/herdr-app-mods/pr-watch/mod ~/.config/herdr-app/mods/pr-watch`

**Interfaces:**
- Consumes: `herdr-pr ls --agent <addr> --with-children --json` (Task 4 shape); `herdr-pr watch|unwatch|unlink|link <url>`.
- Read first: `~/.config/herdr-app/AGENTS.md` sections "Mods", "The `herdr` module"; the shipped `mods/agents/main.js` for list rows and `mods/menu` for the right-click popup pattern.

Behavior:
- Agent = `focused()?.agent` address, else the focused terminal's address; none → empty state "Focus an agent to see its PRs".
- Refresh: on focus change (render-time compare) and `cx.timer.every(10000)`, plus `watchFolder(~/.local/state/herdr-pr)` loop.
- Sections: "This agent" then one group per child (header = child address or "background child" + kind; dim if not live). Row: `repo#number`, title (truncate), state chip (draft/open/merged/closed), CI dot (`theme().done` success, `theme().blocked` failure, `theme().working` pending), review text, eye icon when watched.
- Click row → `exec("xdg-open", [url])`. Right-click → popup menu: Copy link (`exec("wl-copy", [url])`), Open, Watch for changes / Stop watching, Unlink. After an action, refresh.
- Backstop: every refresh, `hc(["read", agent, "--lines", "400"])`, regex `https://github\.com/[\w.-]+/[\w.-]+/pull/\d+`; URLs not in the list shown dimmed under "Seen in output" with a "Link" action → `exec("herdr-pr", ["link", url, "--watch"], { env: { HC_TERMINAL_ID: agent } })` (if `exec` has no `env` option, run `env HC_TERMINAL_ID=<agent> herdr-pr link …`). The link then resolves the owner from that terminal (labels / Claude sessions file); a pi agent without the label is refused with the CLI's error shown as a toast.

- [ ] **Step 1: Write** `mod/main.js` and `mod.json`.
- [ ] **Step 2: Run** `herdr-app check ~/.config/herdr-app/mods/pr-watch` - Expected: exit 0.
- [ ] **Step 3: Verify live:** `herdr-app status` shows no mod error; focus t1 with a linked test PR and confirm the row renders; right-click Watch toggles the eye (re-run `herdr-pr ls --json` to confirm `watched`).
- [ ] **Step 4: Commit** `git commit -m "pr-watch: Herdr.app panel"`

### Task 9: Rule updates (repo `mago0/agent-skills`) and end-to-end check

**Files:**
- Modify: `~/Projects/personal/agent-skills/global/AGENTS.md` (Delegating work: worktree rule; PRs opened for me: Herdr watch)
- Modify: `~/Projects/personal/agent-skills/global/hosts.md` (Herdr.app section)

Text:
- AGENTS.md delegation bullet ends: "Whoever creates a worktree removes it, after the PRs it owns are merged or closed."
- hosts.md Herdr.app:
  - Background child: `id=$(uuidgen); hc run --bg --cwd <checkout> --name <name> --label herdr-pr.session=$id -- claude --session-id $id -p '<task>'`, then `hc wait <name>` and `hc read <name>`.
  - PR watch: `herdr-pr link <url> --watch` right after opening the PR. `herdr-pr unwatch` is automatic on merge or close.

- [ ] **Step 1: Edit** both files in a branch `herdr-pr-watch` of `agent-skills`.
- [ ] **Step 2: End-to-end:** in Herdr, from a pi agent, `herdr-pr link <a real open PR of Matt's> --watch`; confirm `herdr-pr ls --json` shows it and the panel row; post a comment on it from a non-`mago0` account, or run `HERDR_PR_VIEWER=nobody herdr-pr poll` once and confirm the agent receives the `[herdr-pr]` message within one timer interval (`journalctl --user -u herdr-pr-poll -n 20`).
- [ ] **Step 3: Commit** in `agent-skills`: `git commit -m "rules: Herdr PR watch and worktree lifetime"`; in `herdr-app-mods` commit any README/test fixes.
