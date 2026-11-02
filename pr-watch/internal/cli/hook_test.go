package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/app"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/registry"
)

func runHook(d app.Deps, stdin string) (int, string, string) {
	d.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	code := Run([]string{"hook"}, d, &out, &errb)
	return code, out.String(), errb.String()
}

func genericPayload(command, output string) string {
	b, _ := json.Marshal(map[string]string{"command": command, "output": output})
	return string(b)
}

func hookLog(t *testing.T, d app.Deps) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(d.StatePath), "hook.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

func entries(t *testing.T, d app.Deps) []registry.Entry {
	t.Helper()
	r, err := registry.Load(d.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	return r.Entries
}

func TestHookLinksAndWatchesCreatedPR(t *testing.T) {
	d := deps(t, "t7", fakeHC{"t7": claudeTerm("t7", "S7")}, reviewedPR())
	code, out, errs := runHook(d, genericPayload("gh pr create --fill", prURL+"\n"))
	if code != 0 || out != "" || errs != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	es := entries(t, d)
	if len(es) != 1 || es[0].URL != prURL || !es[0].Watched || es[0].Owner.SessionID != "S7" {
		t.Fatalf("entries = %+v", es)
	}
}

func TestHookClaudePayloadReadsStderr(t *testing.T) {
	d := deps(t, "t7", fakeHC{"t7": claudeTerm("t7", "S7")}, reviewedPR())
	payload := `{"tool_name":"Bash","tool_input":{"command":"gh pr create --fill"},` +
		`"tool_response":{"stdout":"","stderr":"` + prURL + `\n","interrupted":false}}`
	if code, out, errs := runHook(d, payload); code != 0 || out != "" || errs != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if es := entries(t, d); len(es) != 1 || !es[0].Watched {
		t.Fatalf("entries = %+v", es)
	}
}

func TestHookNoTerminalIsNoop(t *testing.T) {
	gh := reviewedPR()
	d := deps(t, "", fakeHC{}, gh)
	if code, _, _ := runHook(d, genericPayload("gh pr create --fill", prURL)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if gh.calls != 0 || len(entries(t, d)) != 0 {
		t.Fatalf("fetch calls %d, entries %+v", gh.calls, entries(t, d))
	}
}

func TestHookNonCreateCommandIsNoop(t *testing.T) {
	gh := reviewedPR()
	d := deps(t, "t7", fakeHC{"t7": claudeTerm("t7", "S7")}, gh)
	if code, _, _ := runHook(d, genericPayload("gh pr view 412", prURL)); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if gh.calls != 0 {
		t.Fatalf("fetch calls = %d, want 0", gh.calls)
	}
}

func TestHookOwnedByOtherIsLeftAlone(t *testing.T) {
	c := fakeHC{"t7": claudeTerm("t7", "S7"), "t8": claudeTerm("t8", "S8")}
	d := deps(t, "t7", c, reviewedPR())
	if code, _, errs := run(d, "link", prURL); code != 0 {
		t.Fatalf("link exit %d: %s", code, errs)
	}
	d.Env = func(k string) string { return map[string]string{"HC_TERMINAL_ID": "t8"}[k] }
	code, out, errs := runHook(d, genericPayload("gh pr create --fill", prURL))
	if code != 0 || out != "" || errs != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if es := entries(t, d); len(es) != 1 || es[0].Owner.SessionID != "S7" {
		t.Fatalf("entries = %+v", es)
	}
	if !strings.Contains(hookLog(t, d), "already linked by") {
		t.Errorf("hook.log = %q", hookLog(t, d))
	}
}

func TestHookRelinkSameOwnerIsNoop(t *testing.T) {
	d := deps(t, "t7", fakeHC{"t7": claudeTerm("t7", "S7")}, reviewedPR())
	for range 2 {
		if code, _, errs := runHook(d, genericPayload("gh pr create --fill", prURL)); code != 0 || errs != "" {
			t.Fatalf("exit %d, stderr %q", code, errs)
		}
	}
	if es := entries(t, d); len(es) != 1 {
		t.Fatalf("entries = %+v", es)
	}
	if l := hookLog(t, d); l != "" {
		t.Errorf("hook.log = %q", l)
	}
}

func TestHookGitHubErrorStillExitsZero(t *testing.T) {
	d := deps(t, "t7", fakeHC{"t7": claudeTerm("t7", "S7")}, reviewedPR())
	d.GH = &failingGH{}
	code, out, errs := runHook(d, genericPayload("gh pr create --fill", prURL))
	if code != 0 || out != "" || errs != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
	if !strings.Contains(hookLog(t, d), "gh down") {
		t.Errorf("hook.log = %q", hookLog(t, d))
	}
}

func TestHookBadJSONStillExitsZero(t *testing.T) {
	d := deps(t, "t7", fakeHC{"t7": claudeTerm("t7", "S7")}, reviewedPR())
	if code, out, errs := runHook(d, "not json"); code != 0 || out != "" || errs != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, out, errs)
	}
}
