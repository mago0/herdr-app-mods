#!/usr/bin/env bash
# Smoke test: link, then poll, with fake hc and gh. Needs go; touches no real Herdr or GitHub.
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d "${TMPDIR:-/tmp}/herdr-pr-smoke.XXXXXX")"
trap 'rm -rf "$work"' EXIT

mkdir -p "$work/bin" "$work/fake" "$work/state"
(cd "$here" && go build -o "$work/bin/herdr-pr" ./cmd/herdr-pr)

# Fake hc: `get` prints a fixture, `send` appends to a log.
cat >"$work/bin/hc" <<'FAKE'
#!/usr/bin/env bash
case "$1" in
  get) sed "s/@PI_PID@/$(cat "$FAKE/pi.pid")/" "$FAKE/$2.json" ;;
  send) printf 'send %s %s\n' "$2" "$3" >>"$FAKE/hc.log" ;;
  *) echo "fake hc: unexpected: $*" >&2; exit 1 ;;
esac
FAKE

# Fake gh: the viewer query and the batched PR query, answered from fixtures.
cat >"$work/bin/gh" <<'FAKE'
#!/usr/bin/env bash
case "$4" in
  *viewer*) echo '{"data":{"viewer":{"login":"me"}}}' ;;
  *) cat "$FAKE/pr.json" ;;
esac
FAKE

# Fake pi: the nearest pi ancestor of herdr-pr, so the result does not depend on who runs
# the test. Its comm is "pi" (script file name); it must stay the parent, hence no exec.
cat >"$work/bin/pi" <<'FAKE'
#!/bin/sh
echo $$ >"$FAKE/pi.pid"
"$@"
FAKE
chmod +x "$work/bin/hc" "$work/bin/gh" "$work/bin/pi"

cat >"$work/fake/t3.json" <<'JSON'
{"address":"t3","created_ms":1000,"cwd":"/tmp","state":"live","labels":{"herdr.space":"s1","herdr.tab":"tab1"},
 "status":{"app":"pi","state":"idle"},"foreground":{"name":"pi","pid":@PI_PID@,"argv":["pi"],"cwd":"/tmp"}}
JSON

pr() { # pr <reviews-json>
  cat >"$work/fake/pr.json" <<JSON
{"data":{"r0":{"pr0":{"url":"https://github.com/acme/widgets/pull/7","title":"Add widget","state":"OPEN",
"isDraft":false,"mergeable":"MERGEABLE","reviewDecision":"","commits":{"nodes":[{"commit":{"oid":"abc1234567","statusCheckRollup":null}}]},
"reviews":{"nodes":$1},"comments":{"nodes":[]},"reviewThreads":{"nodes":[]}}}}}
JSON
}

export FAKE="$work/fake" HERDR_PR_STATE="$work/state" PATH="$work/bin:$PATH"
export HC_TERMINAL_ID=t3 PI_SESSION_ID=sess-1 PI_SESSION_FILE="$work/sess-1.jsonl"
url=https://github.com/acme/widgets/pull/7

pr '[]'
pi herdr-pr link "$url" --watch

pr '[{"id":"R1","state":"CHANGES_REQUESTED","body":"fix it","author":{"login":"alice","__typename":"User"}}]'
herdr-pr poll

grep -q '^send t3 .*needs action' "$FAKE/hc.log" || { echo "smoke: FAIL: owner t3 was not woken"; cat "$FAKE/hc.log" 2>/dev/null; exit 1; }
echo "smoke: ok"
