package events

import (
	"reflect"
	"testing"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/github"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/registry"
)

func item(id, kind, login string, bot bool, state, body string) github.Item {
	return github.Item{ID: id, Kind: kind, Author: github.Actor{Login: login, Bot: bot}, State: state, Body: body}
}

func kinds(evs []Event) []string {
	var k []string
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return k
}

func TestClassifyViewerAndBotIgnored(t *testing.T) {
	cur := github.PR{State: "OPEN", Items: []github.Item{
		item("1", "comment", "mago0", false, "", "mine"),
		item("2", "comment", "github-actions", true, "", "bot"),
		item("3", "comment", "renovate[bot]", false, "", "bot by name"),
	}}
	if evs := Classify(registry.Last{State: "OPEN"}, cur, "mago0"); len(evs) != 0 {
		t.Fatalf("got %+v", evs)
	}
}

func TestClassifyChangesRequestedAndComment(t *testing.T) {
	cur := github.PR{State: "OPEN", Items: []github.Item{
		item("old", "comment", "bob", false, "", "seen already"),
		item("1", "review", "alice", false, "CHANGES_REQUESTED", "fix it"),
		item("2", "thread_comment", "carol", false, "", "nit"),
	}}
	prev := registry.Last{State: "OPEN", SeenIDs: []string{"old"}}
	evs := Classify(prev, cur, "mago0")
	want := []Event{
		{"changes_requested", "review from alice: changes requested"},
		{"comment", "thread_comment from carol"},
	}
	if !reflect.DeepEqual(evs, want) {
		t.Fatalf("got %+v want %+v", evs, want)
	}
}

func TestClassifyCIOncePerSHA(t *testing.T) {
	cur := github.PR{State: "OPEN", CI: "FAILURE", HeadSHA: "a1b2c3d4e5", FailingChecks: []string{"terraform-plan", "lint"}}
	evs := Classify(registry.Last{State: "OPEN"}, cur, "mago0")
	want := []Event{{"ci_failing", "CI failing on a1b2c3d (terraform-plan, lint)"}}
	if !reflect.DeepEqual(evs, want) {
		t.Fatalf("got %+v", evs)
	}
	if again := Classify(Next(cur), cur, "mago0"); len(again) != 0 {
		t.Fatalf("second pass: %+v", again)
	}
	cur.HeadSHA = "ffffffff00"
	if got := Classify(Next(github.PR{State: "OPEN", CI: "FAILURE", HeadSHA: "a1b2c3d4e5"}), cur, "mago0"); len(got) != 1 {
		t.Fatalf("new sha: %+v", got)
	}
}

func TestClassifyConflictOnce(t *testing.T) {
	cur := github.PR{State: "OPEN", Mergeable: "CONFLICTING"}
	if got := kinds(Classify(registry.Last{State: "OPEN"}, cur, "v")); !reflect.DeepEqual(got, []string{"conflict"}) {
		t.Fatalf("got %v", got)
	}
	if got := Classify(Next(cur), cur, "v"); len(got) != 0 {
		t.Fatalf("repeat: %+v", got)
	}
}

func TestClassifyApprovalAndCIGreenSilent(t *testing.T) {
	cur := github.PR{State: "OPEN", CI: "SUCCESS", HeadSHA: "abc", Items: []github.Item{
		item("1", "review", "alice", false, "APPROVED", ""),
	}}
	if evs := Classify(registry.Last{State: "OPEN"}, cur, "mago0"); len(evs) != 0 {
		t.Fatalf("got %+v", evs)
	}
}

func TestClassifyMerged(t *testing.T) {
	for state, kind := range map[string]string{"MERGED": "merged", "CLOSED": "closed"} {
		evs := Classify(registry.Last{State: "OPEN"}, github.PR{State: state}, "v")
		if got := kinds(evs); !reflect.DeepEqual(got, []string{kind}) {
			t.Errorf("%s: got %v", state, got)
		}
		if again := Classify(registry.Last{State: state}, github.PR{State: state}, "v"); len(again) != 0 {
			t.Errorf("%s repeat: %+v", state, again)
		}
	}
}

func TestNextAndDisplayOf(t *testing.T) {
	pr := github.PR{State: "OPEN", Title: "t", Draft: true, CI: "FAILURE", ReviewDecision: "APPROVED", HeadSHA: "abc", Mergeable: "CONFLICTING",
		Items: []github.Item{{ID: "1"}, {ID: "2"}}}
	want := registry.Last{State: "OPEN", SeenIDs: []string{"1", "2"}, CIFailingSHA: "abc", Conflicting: true}
	if got := Next(pr); !reflect.DeepEqual(got, want) {
		t.Errorf("Next = %+v", got)
	}
	if got := DisplayOf(pr); got != (registry.Display{Title: "t", State: "OPEN", Draft: true, CI: "FAILURE", Review: "APPROVED"}) {
		t.Errorf("DisplayOf = %+v", got)
	}
}
