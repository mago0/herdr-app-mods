package poll

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/app"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/github"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/hc"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/prurl"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/registry"
)

const prURL = "https://github.com/flocasts/iam/pull/412"

type sent struct{ To, Text string }

// fakeHC holds terminals by address and records sends and runs.
type fakeHC struct {
	terms  map[string]hc.Terminal
	getErr map[string]error
	sends  []sent
	runs   []hc.RunSpec
	runAs  hc.Terminal
}

func (f *fakeHC) Get(addr string) (hc.Terminal, error) {
	if err := f.getErr[addr]; err != nil {
		return hc.Terminal{}, err
	}
	t, ok := f.terms[addr]
	if !ok {
		return hc.Terminal{}, hc.ErrNotFound
	}
	return t, nil
}

func (f *fakeHC) Send(addr, text string) error {
	f.sends = append(f.sends, sent{addr, text})
	return nil
}

func (f *fakeHC) Run(s hc.RunSpec) (hc.Terminal, error) {
	f.runs = append(f.runs, s)
	return f.runAs, nil
}

func (f *fakeHC) to(addr string) []string {
	var out []string
	for _, s := range f.sends {
		if s.To == addr {
			out = append(out, s.Text)
		}
	}
	return out
}

type fakeGH struct {
	prs   map[string]github.PR
	err   error
	after func() // runs inside Fetch, between load and save
}

func (f *fakeGH) Fetch(_ context.Context, refs []prurl.Ref) (map[string]github.PR, error) {
	if f.after != nil {
		f.after()
	}
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]github.PR{}
	for _, r := range refs {
		if p, ok := f.prs[r.URL()]; ok {
			out[r.URL()] = p
		}
	}
	return out, nil
}

func (*fakeGH) Viewer(context.Context) (string, error) { return "mago0", nil }

type status = struct {
	App   string `json:"app"`
	State string `json:"state"`
}

func term(addr string, created int64, state string) hc.Terminal {
	return hc.Terminal{Address: addr, CreatedMs: created, State: "live", Status: &status{App: "claude", State: state}}
}

func setup(t *testing.T, entries ...registry.Entry) (app.Deps, *fakeHC, *fakeGH) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.json")
	err := registry.Update(path, func(r *registry.Registry) error {
		r.Entries = entries
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeHC{terms: map[string]hc.Terminal{
		"t0": term("t0", 10, "idle"), // grandparent
		"t1": term("t1", 11, "idle"), // parent
		"t2": term("t2", 12, "idle"), // owner
	}}
	gh := &fakeGH{prs: map[string]github.PR{}}
	d := app.Deps{
		Env:       func(string) string { return "" },
		HC:        c,
		GH:        gh,
		StatePath: path,
		Now:       func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) },
	}
	return d, c, gh
}

func watched(t *testing.T) registry.Entry {
	return registry.Entry{
		URL: prURL, Repo: "flocasts/iam", Number: 412, Watched: true,
		Owner: registry.Owner{
			Address: "t2", CreatedMs: 12, Kind: "claude", SessionID: "sid",
			Cwd: t.TempDir(), Parent: "t1", ParentCreatedMs: 11,
		},
		Display: registry.Display{State: "OPEN"},
		Last:    registry.Last{State: "OPEN", SeenIDs: []string{"c1"}},
	}
}

func openPR(items ...github.Item) github.PR {
	return github.PR{URL: prURL, Title: "IAM change", State: "OPEN", Mergeable: "MERGEABLE", CI: "SUCCESS", Items: items}
}

var (
	oldComment = github.Item{ID: "c1", Kind: "comment", Author: github.Actor{Login: "bob"}, Body: "old"}
	review     = github.Item{ID: "r1", Kind: "review", Author: github.Actor{Login: "alice"}, State: "CHANGES_REQUESTED"}
)

func load(t *testing.T, d app.Deps) registry.Entry {
	t.Helper()
	r, err := registry.Load(d.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	e := r.Find(prURL)
	if e == nil {
		t.Fatal("entry missing")
	}
	return *e
}

func run(t *testing.T, d app.Deps) {
	t.Helper()
	if err := Run(context.Background(), d); err != nil {
		t.Fatal(err)
	}
}

const ownerMsg = "[herdr-pr] flocasts/iam#412 needs action:\n" +
	"- review from alice: changes requested\n" +
	"Run `gh pr view https://github.com/flocasts/iam/pull/412 --comments` for details."

func TestPollWakesOwnerAndFYIParent(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	gh.prs[prURL] = openPR(oldComment, review)
	run(t, d)

	if got := c.to("t2"); !slices.Equal(got, []string{ownerMsg}) {
		t.Errorf("owner got %q", got)
	}
	fyi := "[herdr-pr] FYI, no action needed: child t2 is handling flocasts/iam#412 (changes requested)."
	if got := c.to("t1"); !slices.Equal(got, []string{fyi}) {
		t.Errorf("parent got %q", got)
	}
	if got := c.to("t0"); len(got) != 0 {
		t.Errorf("grandparent got %q", got)
	}
	e := load(t, d)
	if !slices.Equal(e.Last.SeenIDs, []string{"c1", "r1"}) {
		t.Errorf("SeenIDs = %v", e.Last.SeenIDs)
	}
	if e.Display.Title != "IAM change" || e.Display.CI != "SUCCESS" {
		t.Errorf("Display = %+v", e.Display)
	}
}

func TestPollNoChangeNoSend(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	gh.prs[prURL] = openPR(oldComment)
	run(t, d)
	if len(c.sends) != 0 || len(c.runs) != 0 {
		t.Errorf("sends %v runs %v", c.sends, c.runs)
	}
}

func TestPollViewerOverride(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	d.Env = func(k string) string { return map[string]string{"HERDR_PR_VIEWER": "bob"}[k] }
	gh.prs[prURL] = openPR(oldComment, github.Item{ID: "c2", Kind: "comment", Author: github.Actor{Login: "bob"}, Body: "mine"})
	run(t, d)
	if len(c.sends) != 0 {
		t.Errorf("viewer's own comment woke: %v", c.sends)
	}
}

func TestPollBusyRetries(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	c.terms["t2"] = term("t2", 12, "working")
	gh.prs[prURL] = openPR(oldComment, review)
	before := load(t, d).Last
	run(t, d)
	if len(c.sends) != 0 {
		t.Fatalf("busy owner got %v", c.sends)
	}
	if got := load(t, d).Last; !slices.Equal(got.SeenIDs, before.SeenIDs) {
		t.Fatalf("Last changed while busy: %+v", got)
	}

	c.terms["t2"] = term("t2", 12, "idle")
	run(t, d)
	if got := c.to("t2"); !slices.Equal(got, []string{ownerMsg}) {
		t.Errorf("owner got %q", got)
	}
}

func TestPollParentNotLiveSkipsFYI(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	c.terms["t1"] = term("t1", 11, "working")
	gh.prs[prURL] = openPR(oldComment, review)
	run(t, d)
	if got := c.to("t1"); len(got) != 0 {
		t.Errorf("busy parent got %q", got)
	}
	if got := c.to("t2"); len(got) != 1 {
		t.Errorf("owner got %q", got)
	}
	// The FYI is not retried.
	c.terms["t1"] = term("t1", 11, "idle")
	run(t, d)
	if got := c.to("t1"); len(got) != 0 {
		t.Errorf("parent got %q on retry", got)
	}
}

func TestPollRevivesThenDeliversNextPass(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	delete(c.terms, "t2")
	c.runAs = hc.Terminal{Address: "t9", CreatedMs: 99, State: "live"}
	gh.prs[prURL] = openPR(oldComment, review)
	run(t, d)

	if len(c.runs) != 1 || c.runs[0].Parent != "t1" || c.runs[0].NoParent {
		t.Fatalf("runs = %+v", c.runs)
	}
	if len(c.sends) != 0 {
		t.Fatalf("sent on revive pass: %v", c.sends)
	}
	e := load(t, d)
	if e.RevivedAs != "t9" || e.RevivedCreatedMs != 99 || slices.Contains(e.Last.SeenIDs, "r1") {
		t.Fatalf("entry = %+v", e)
	}

	c.terms["t9"] = term("t9", 99, "idle")
	run(t, d)
	if got := c.to("t9"); !slices.Equal(got, []string{ownerMsg}) {
		t.Errorf("revived owner got %q", got)
	}
	if got := c.to("t1"); len(got) != 1 {
		t.Errorf("parent got %q", got)
	}
	if len(c.runs) != 1 {
		t.Errorf("revived twice: %+v", c.runs)
	}
}

func TestPollInProcessChildRevivedWithoutCheck(t *testing.T) {
	e := watched(t)
	e.Owner.Address, e.Owner.CreatedMs, e.Owner.Kind = "", 0, "pi"
	d, c, gh := setup(t, e)
	c.getErr = map[string]error{"": errors.New("Get called with empty address")}
	c.runAs = hc.Terminal{Address: "t9", CreatedMs: 99}
	gh.prs[prURL] = openPR(oldComment, review)
	run(t, d)
	if len(c.runs) != 1 || load(t, d).RevivedAs != "t9" {
		t.Errorf("runs = %+v", c.runs)
	}
}

func TestPollReviveFailureToldOnce(t *testing.T) {
	e := watched(t)
	e.Owner.Cwd = "/nonexistent/x"
	d, c, gh := setup(t, e)
	delete(c.terms, "t2")
	gh.prs[prURL] = openPR(oldComment, review)
	for range 2 {
		if err := Run(context.Background(), d); err == nil {
			t.Fatal("want revive error")
		}
	}
	want := "[herdr-pr] FYI, no action needed: cannot revive the owner of flocasts/iam#412: cwd missing: /nonexistent/x. The watch stays on."
	if got := c.to("t1"); !slices.Equal(got, []string{want}) {
		t.Errorf("parent got %q", got)
	}
	if got := load(t, d); got.ReviveFailed == "" || !got.Watched {
		t.Errorf("entry = %+v", got)
	}
}

func TestPollOwnerCheckErrorSkipsPR(t *testing.T) {
	other := watched(t)
	other.URL, other.Number = "https://github.com/flocasts/iam/pull/413", 413
	other.Owner.Address, other.Owner.CreatedMs = "t3", 13
	d, c, gh := setup(t, watched(t), other)
	c.terms["t3"] = term("t3", 13, "idle")
	c.getErr = map[string]error{"t2": errors.New("hc broke")}
	gh.prs[prURL] = openPR(oldComment, review)
	p413 := openPR(oldComment, review)
	p413.URL = other.URL
	gh.prs[other.URL] = p413

	if err := Run(context.Background(), d); err == nil {
		t.Fatal("want error")
	}
	if got := c.to("t3"); len(got) != 1 {
		t.Errorf("other owner got %q", got)
	}
	if got := load(t, d).Last; slices.Contains(got.SeenIDs, "r1") {
		t.Errorf("Last advanced after check error: %+v", got)
	}
}

func TestPollFetchErrorSavesNothing(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	gh.err = errors.New("gh down")
	before, err := os.ReadFile(d.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), d); err == nil {
		t.Fatal("want error")
	}
	after, _ := os.ReadFile(d.StatePath)
	if !bytes.Equal(before, after) || len(c.sends) != 0 {
		t.Error("registry changed or sends made")
	}
}

func TestPollMissingPRUntouched(t *testing.T) {
	d, c, _ := setup(t, watched(t))
	before := load(t, d)
	run(t, d)
	after := load(t, d)
	if len(c.sends) != 0 || !slices.Equal(before.Last.SeenIDs, after.Last.SeenIDs) || before.Display != after.Display {
		t.Errorf("before %+v after %+v", before, after)
	}
}

func TestPollUnwatchedUpdatesDisplayOnly(t *testing.T) {
	e := watched(t)
	e.Watched = false
	d, c, gh := setup(t, e)
	p := openPR(oldComment, review)
	p.State = "MERGED"
	gh.prs[prURL] = p
	run(t, d)
	got := load(t, d)
	if len(c.sends) != 0 || got.Display.State != "MERGED" {
		t.Errorf("sends %v display %+v", c.sends, got.Display)
	}
}

func TestPollMergedEndsWatchAndNotesWorktree(t *testing.T) {
	e := watched(t)
	d, c, gh := setup(t, e)
	p := openPR(oldComment)
	p.State = "MERGED"
	gh.prs[prURL] = p
	run(t, d)

	final := "[herdr-pr] flocasts/iam#412 was merged. Watch ended."
	note := "[herdr-pr] FYI: worktree " + e.Owner.Cwd + " has no open PRs left and can be removed."
	owner := c.to("t2")
	if len(owner) != 2 || owner[1] != final {
		t.Errorf("owner got %q", owner)
	}
	parent := c.to("t1")
	if len(parent) != 3 || parent[1] != final || parent[2] != note {
		t.Errorf("parent got %q", parent)
	}
	got := load(t, d)
	if got.Watched || !got.Last.WorktreeNoted || got.Display.State != "MERGED" {
		t.Errorf("entry = %+v", got)
	}

	n := len(c.sends)
	run(t, d)
	if len(c.sends) != n {
		t.Errorf("second pass sent %v", c.sends[n:])
	}
}

func TestPollWorktreeNoteWaitsForLastOpenPR(t *testing.T) {
	e := watched(t)
	other := e
	other.URL, other.Number, other.Watched = "https://github.com/flocasts/iam/pull/413", 413, false
	d, c, gh := setup(t, e, other)
	p := openPR(oldComment)
	p.State = "CLOSED"
	gh.prs[prURL] = p
	p413 := openPR()
	p413.URL = other.URL
	gh.prs[other.URL] = p413
	run(t, d)
	for _, s := range c.sends {
		if bytes.Contains([]byte(s.Text), []byte("worktree")) {
			t.Errorf("worktree note with an open PR left: %q", s.Text)
		}
	}
	if got := c.to("t2"); len(got) != 2 || got[1] != "[herdr-pr] flocasts/iam#412 was closed. Watch ended." {
		t.Errorf("owner got %q", got)
	}
}

func TestPollWorktreeNoteToOwnerWithoutParent(t *testing.T) {
	e := watched(t)
	e.Owner.Parent, e.Owner.ParentCreatedMs = "", 0
	d, c, gh := setup(t, e)
	p := openPR(oldComment)
	p.State = "MERGED"
	gh.prs[prURL] = p
	run(t, d)
	owner := c.to("t2")
	if len(owner) != 3 || !bytes.Contains([]byte(owner[2]), []byte("worktree")) {
		t.Errorf("owner got %q", owner)
	}
}

func TestPollConcurrentLinkKept(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	gh.prs[prURL] = openPR(oldComment, review)
	added := "https://github.com/flocasts/iam/pull/500"
	gh.after = func() {
		err := registry.Update(d.StatePath, func(r *registry.Registry) error {
			r.Entries = append(r.Entries, registry.Entry{URL: added, Repo: "flocasts/iam", Number: 500})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	run(t, d)
	r, err := registry.Load(d.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if r.Find(added) == nil {
		t.Error("concurrently linked entry lost")
	}
	if e := r.Find(prURL); e == nil || !slices.Contains(e.Last.SeenIDs, "r1") {
		t.Errorf("processed entry not saved: %+v", e)
	}
	if len(c.to("t2")) != 1 {
		t.Errorf("owner got %v", c.sends)
	}
}

func TestPollConcurrentUnlinkStaysRemoved(t *testing.T) {
	d, _, gh := setup(t, watched(t))
	gh.prs[prURL] = openPR(oldComment, review)
	gh.after = func() {
		err := registry.Update(d.StatePath, func(r *registry.Registry) error {
			r.Entries = nil
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	run(t, d)
	r, _ := registry.Load(d.StatePath)
	if len(r.Entries) != 0 {
		t.Errorf("unlinked entry came back: %+v", r.Entries)
	}
}
