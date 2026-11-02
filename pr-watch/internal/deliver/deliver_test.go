package deliver

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/events"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/hc"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/registry"
)

var entry = registry.Entry{URL: "https://github.com/flocasts/iam/pull/412", Repo: "flocasts/iam", Number: 412}

func TestOwnerMessageExact(t *testing.T) {
	evs := []events.Event{
		{Kind: "changes_requested", Text: "review from alice: changes requested"},
		{Kind: "ci_failing", Text: "CI failing on a1b2c3d (terraform-plan)"},
	}
	want := "[herdr-pr] flocasts/iam#412 needs action:\n" +
		"- review from alice: changes requested\n" +
		"- CI failing on a1b2c3d (terraform-plan)\n" +
		"Run `gh pr view https://github.com/flocasts/iam/pull/412 --comments` for details."
	if got := OwnerMessage(entry, evs); got != want {
		t.Errorf("got:\n%s", got)
	}
}

func TestParentFYIExact(t *testing.T) {
	evs := []events.Event{{Kind: "changes_requested"}, {Kind: "ci_failing"}}
	want := "[herdr-pr] FYI, no action needed: child t7 is handling flocasts/iam#412 (changes requested, CI failing)."
	if got := ParentFYI(entry, "t7", evs); got != want {
		t.Errorf("got %q", got)
	}
}

func TestFinalAndWorktreeNotes(t *testing.T) {
	if got, want := FinalNote(entry, "merged"), "[herdr-pr] flocasts/iam#412 was merged. Watch ended."; got != want {
		t.Errorf("got %q", got)
	}
	if got, want := WorktreeNote("/w/x"), "[herdr-pr] FYI: worktree /w/x has no open PRs left and can be removed."; got != want {
		t.Errorf("got %q", got)
	}
}

type fakeHC struct {
	term    hc.Terminal
	getErr  error
	ran     hc.RunSpec
	runTerm hc.Terminal
}

func (f *fakeHC) Get(string) (hc.Terminal, error) { return f.term, f.getErr }
func (f *fakeHC) Send(string, string) error       { return nil }
func (f *fakeHC) Run(s hc.RunSpec) (hc.Terminal, error) {
	f.ran = s
	return f.runTerm, nil
}

type status = struct {
	App   string `json:"app"`
	State string `json:"state"`
}

func live(state string) hc.Terminal {
	return hc.Terminal{State: "live", CreatedMs: 5, Status: &status{State: state}}
}

func TestCheckCreatedMsMismatchIsGone(t *testing.T) {
	got, _, err := Check(&fakeHC{term: live("idle")}, "t1", 6)
	if err != nil || got != Gone {
		t.Errorf("got %v %v", got, err)
	}
}

func TestCheckWorkingIsBusy(t *testing.T) {
	got, _, err := Check(&fakeHC{term: live("working")}, "t1", 5)
	if err != nil || got != Busy {
		t.Errorf("got %v %v", got, err)
	}
}

func TestCheckIdleIsLive(t *testing.T) {
	got, term, err := Check(&fakeHC{term: live("idle")}, "t1", 5)
	if err != nil || got != Live || term.CreatedMs != 5 {
		t.Errorf("got %v %v", got, err)
	}
}

func TestCheckNotFoundIsGone(t *testing.T) {
	got, _, err := Check(&fakeHC{getErr: hc.ErrNotFound}, "t1", 5)
	if err != nil || got != Gone {
		t.Errorf("got %v %v", got, err)
	}
}

func TestCheckOtherErrorReturned(t *testing.T) {
	if _, _, err := Check(&fakeHC{getErr: os.ErrPermission}, "t1", 5); err == nil {
		t.Error("want error")
	}
}

func TestCheckNotLiveIsGone(t *testing.T) {
	term := live("idle")
	term.State = "exited"
	if got, _, err := Check(&fakeHC{term: term}, "t1", 5); err != nil || got != Gone {
		t.Errorf("got %v %v", got, err)
	}
}

func TestCheckShellForegroundIsGone(t *testing.T) {
	for _, sh := range []string{"zsh", "bash", "sh", "fish", "dash"} {
		term := live("idle")
		term.Foreground = &struct {
			Name string   `json:"name"`
			Pid  int      `json:"pid"`
			Argv []string `json:"argv"`
			Cwd  string   `json:"cwd"`
		}{Name: sh}
		if got, _, err := Check(&fakeHC{term: term}, "t1", 5); err != nil || got != Gone {
			t.Errorf("%s: got %v %v", sh, got, err)
		}
	}
}

func TestCheckNoStatusIsBusy(t *testing.T) {
	term := hc.Terminal{State: "live", CreatedMs: 5}
	if got, _, err := Check(&fakeHC{term: term}, "t1", 5); err != nil || got != Busy {
		t.Errorf("got %v %v", got, err)
	}
}

func reviveEntry(t *testing.T, kind string) registry.Entry {
	e := entry
	e.Owner = registry.Owner{Kind: kind, SessionID: "sid", SessionFile: "/s/f.jsonl", Cwd: t.TempDir(), Space: "sp", Tab: "tb", Parent: "t0"}
	return e
}

func TestReviveArgvPiAndClaude(t *testing.T) {
	for kind, want := range map[string][]string{
		"pi":     {"pi", "--session", "/s/f.jsonl"},
		"claude": {"claude", "--resume", "sid"},
	} {
		f := &fakeHC{runTerm: hc.Terminal{Address: "t9"}}
		e := reviveEntry(t, kind)
		got, err := Revive(f, e, true)
		if err != nil || got.Address != "t9" {
			t.Fatalf("%s: %v %v", kind, got, err)
		}
		if !reflect.DeepEqual(f.ran.Argv, want) {
			t.Errorf("%s argv = %v", kind, f.ran.Argv)
		}
		if f.ran.Name != "pr-iam-412" || f.ran.Cwd != e.Owner.Cwd || f.ran.Parent != "t0" || f.ran.NoParent {
			t.Errorf("%s spec = %+v", kind, f.ran)
		}
		wl := map[string]string{"herdr.space": "sp", "herdr.tab": "tb", "herdr-pr.session": "sid"}
		if !reflect.DeepEqual(f.ran.Labels, wl) {
			t.Errorf("labels = %v", f.ran.Labels)
		}
	}
}

func TestReviveNameTruncated(t *testing.T) {
	f := &fakeHC{}
	e := reviveEntry(t, "pi")
	e.Repo = "o/" + strings.Repeat("x", 50)
	if _, err := Revive(f, e, true); err != nil || len(f.ran.Name) != 32 {
		t.Errorf("name %q err %v", f.ran.Name, err)
	}
}

func TestReviveNoParentWhenParentGone(t *testing.T) {
	f := &fakeHC{}
	if _, err := Revive(f, reviveEntry(t, "pi"), false); err != nil {
		t.Fatal(err)
	}
	if !f.ran.NoParent || f.ran.Parent != "" {
		t.Errorf("spec = %+v", f.ran)
	}
}

func TestReviveCwdMissing(t *testing.T) {
	e := reviveEntry(t, "pi")
	e.Owner.Cwd = "/nonexistent/x"
	f := &fakeHC{}
	if _, err := Revive(f, e, true); err == nil || !strings.Contains(err.Error(), "cwd missing") {
		t.Errorf("err = %v", err)
	}
	if f.ran.Argv != nil {
		t.Error("Run was called")
	}
}
