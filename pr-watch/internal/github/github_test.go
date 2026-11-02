package github

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/prurl"
)

func ref(t *testing.T, raw string) prurl.Ref {
	t.Helper()
	r, err := prurl.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBuildQueryAliases(t *testing.T) {
	refs := []prurl.Ref{
		ref(t, "https://github.com/cli/cli/pull/1"),
		ref(t, "https://github.com/other/repo/pull/7"),
		ref(t, "https://github.com/cli/cli/pull/2"),
	}
	q, idx := buildQuery(refs)
	for _, want := range []string{
		`r0: repository(owner: "cli", name: "cli")`,
		`pr0: pullRequest(number: 1)`,
		`pr1: pullRequest(number: 2)`,
		`r1: repository(owner: "other", name: "repo")`,
		`pr0: pullRequest(number: 7)`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %q:\n%s", want, q)
		}
	}
	if !strings.Contains(q, "viewer { login }") {
		t.Errorf("query lacks the viewer:\n%s", q)
	}
	wantIdx := map[string]string{
		"r0.pr0": "https://github.com/cli/cli/pull/1",
		"r0.pr1": "https://github.com/cli/cli/pull/2",
		"r1.pr0": "https://github.com/other/repo/pull/7",
	}
	if !reflect.DeepEqual(idx, wantIdx) {
		t.Errorf("index = %v, want %v", idx, wantIdx)
	}
}

func TestParse(t *testing.T) {
	body, err := os.ReadFile("testdata/response.json")
	if err != nil {
		t.Fatal(err)
	}
	merged := ref(t, "https://github.com/cli/cli/pull/14801")
	open := ref(t, "https://github.com/cli/cli/pull/14783")
	b, err := Parse(body, []prurl.Ref{merged, open})
	if err != nil {
		t.Fatal(err)
	}
	got := b.PRs
	m, ok := got[merged.URL()]
	if !ok || m.State != "MERGED" {
		t.Fatalf("merged PR = %+v (present %v)", m, ok)
	}
	o, ok := got[open.URL()]
	if !ok || o.State != "OPEN" {
		t.Fatalf("open PR = %+v (present %v)", o, ok)
	}
	if o.CI == "" || o.HeadSHA == "" || o.Title == "" || o.URL != open.URL() {
		t.Errorf("open PR missing fields: %+v", o)
	}
	kinds := map[string]bool{}
	for _, it := range o.Items {
		kinds[it.Kind] = true
		if it.ID == "" || it.Author.Login == "" {
			t.Errorf("item missing id/author: %+v", it)
		}
	}
	if !kinds["review"] || !kinds["comment"] {
		t.Errorf("want review and comment items, got kinds %v", kinds)
	}
	var bots int
	for _, it := range append(append([]Item{}, o.Items...), m.Items...) {
		if it.Author.Bot {
			bots++
			if it.Author.Login == "" {
				t.Errorf("bot without login: %+v", it)
			}
		}
	}
	if bots == 0 {
		t.Error("fixture should contain at least one bot-authored item")
	}
}

func TestParseMissingPR(t *testing.T) {
	body := []byte(`{"data":{"r0":{"pr0":null,"pr1":{"url":"https://github.com/cli/cli/pull/2","title":"t","state":"OPEN","isDraft":false,"mergeable":"UNKNOWN","reviewDecision":null,"commits":{"nodes":[]},"reviews":{"nodes":[]},"comments":{"nodes":[]},"reviewThreads":{"nodes":[]}}}}}`)
	refs := []prurl.Ref{ref(t, "https://github.com/cli/cli/pull/1"), ref(t, "https://github.com/cli/cli/pull/2")}
	b, err := Parse(body, refs)
	if err != nil {
		t.Fatal(err)
	}
	got := b.PRs
	if _, ok := got[refs[0].URL()]; ok {
		t.Error("missing PR should be absent")
	}
	if _, ok := got[refs[1].URL()]; !ok {
		t.Error("present PR should be in map")
	}
}

func TestParseChecksAndItems(t *testing.T) {
	body := []byte(`{"data":{"r0":{"pr0":{
"url":"https://github.com/o/n/pull/1","title":"t","state":"OPEN","isDraft":true,"mergeable":"CONFLICTING","reviewDecision":"CHANGES_REQUESTED",
"commits":{"nodes":[{"commit":{"oid":"abc","statusCheckRollup":{"state":"FAILURE","contexts":{"nodes":[
 {"name":"build","conclusion":"FAILURE"},
 {"name":"lint","conclusion":"SUCCESS"},
 {"name":"slow","conclusion":"TIMED_OUT"},
 {"name":"cancelled","conclusion":"CANCELLED"},
 {"context":"ci/legacy","state":"ERROR"},
 {"context":"ci/ok","state":"SUCCESS"}]}}}}]},
"reviews":{"nodes":[{"id":"R1","state":"APPROVED","body":"lgtm","author":{"login":"alice","__typename":"User"}}]},
"comments":{"nodes":[{"id":"C1","body":"ping","author":{"login":"dependabot","__typename":"Bot"}},{"id":"C2","body":"ghost","author":null}]},
"reviewThreads":{"nodes":[{"comments":{"nodes":[{"id":"T1","body":"nit","author":{"login":"bob","__typename":"User"}}]}}]}
}}}}`)
	r := ref(t, "https://github.com/o/n/pull/1")
	b, err := Parse(body, []prurl.Ref{r})
	if err != nil {
		t.Fatal(err)
	}
	pr := b.PRs[r.URL()]
	if !pr.Draft || pr.Mergeable != "CONFLICTING" || pr.ReviewDecision != "CHANGES_REQUESTED" || pr.HeadSHA != "abc" || pr.CI != "FAILURE" {
		t.Errorf("scalar fields wrong: %+v", pr)
	}
	if want := []string{"build", "slow", "cancelled", "ci/legacy"}; !reflect.DeepEqual(pr.FailingChecks, want) {
		t.Errorf("FailingChecks = %v, want %v", pr.FailingChecks, want)
	}
	want := []Item{
		{ID: "R1", Kind: "review", Author: Actor{Login: "alice"}, State: "APPROVED", Body: "lgtm"},
		{ID: "C1", Kind: "comment", Author: Actor{Login: "dependabot", Bot: true}, Body: "ping"},
		{ID: "C2", Kind: "comment", Body: "ghost"},
		{ID: "T1", Kind: "thread_comment", Author: Actor{Login: "bob"}, Body: "nit"},
	}
	if !reflect.DeepEqual(pr.Items, want) {
		t.Errorf("Items = %+v, want %+v", pr.Items, want)
	}
}

func TestParseViewer(t *testing.T) {
	body := []byte(`{"data":{"viewer":{"login":"mago0"},"r0":{"pr0":null}}}`)
	b, err := Parse(body, []prurl.Ref{ref(t, "https://github.com/cli/cli/pull/1")})
	if err != nil {
		t.Fatal(err)
	}
	if b.Viewer != "mago0" || len(b.PRs) != 0 {
		t.Errorf("batch = %+v", b)
	}
}
