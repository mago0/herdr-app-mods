package poll

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
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
	terms   map[string]hc.Terminal
	getErr  map[string]error
	sendErr map[string]error
	sends   []sent
	runs    []hc.RunSpec
	runAs   hc.Terminal
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
	if err := f.sendErr[addr]; err != nil {
		return err
	}
	f.sends = append(f.sends, sent{addr, text})
	return nil
}

func (f *fakeHC) Run(s hc.RunSpec) (hc.Terminal, error) {
	f.runs = append(f.runs, s)
	return f.runAs, nil
}

func (f *fakeHC) List() ([]string, error) { return slices.Sorted(maps.Keys(f.terms)), nil }

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

func (f *fakeGH) Fetch(_ context.Context, refs []prurl.Ref) (github.Batch, error) {
	if f.after != nil {
		f.after()
	}
	if f.err != nil {
		return github.Batch{}, f.err
	}
	out := github.Batch{PRs: map[string]github.PR{}, Viewer: "mago0"}
	for _, r := range refs {
		if p, ok := f.prs[r.URL()]; ok {
			out.PRs[r.URL()] = p
		}
	}
	return out, nil
}

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
		// Worktree paths in these tests are made up; treat every one as a linked worktree.
		LinkedWorktree: func(string) bool { return true },
		Stat:           func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist },
	}
	return d, c, gh
}

func watched(t *testing.T) registry.Entry {
	return registry.Entry{
		URL: prURL, Repo: "flocasts/iam", Number: 412, Watched: true,
		Owner: registry.Owner{
			Address: "t2", CreatedMs: 12, Kind: "claude", SessionID: "sid",
			Cwd: t.TempDir(), Worktree: "/repo/_worktrees/iam", Parent: "t1", ParentCreatedMs: 11,
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

// A busy Pi owner mid tool call can show a shell in the foreground; its working status keeps it from being revived.
func TestPollBusyOwnerWithShellForegroundNotRevived(t *testing.T) {
	e := watched(t)
	e.Owner.Kind, e.Owner.SessionFile = "pi", "/s/sid.jsonl"
	d, c, gh := setup(t, e)
	busy := term("t2", 12, "working")
	busy.Status.App = "pi"
	busy.Foreground = &struct {
		Name string   `json:"name"`
		Pid  int      `json:"pid"`
		Argv []string `json:"argv"`
		Cwd  string   `json:"cwd"`
	}{Name: "bash"}
	c.terms["t2"] = busy
	gh.prs[prURL] = openPR(oldComment, review)
	run(t, d)
	if len(c.runs) != 0 || len(c.sends) != 0 {
		t.Errorf("runs %+v sends %v", c.runs, c.sends)
	}
}

// The owner looks gone, but its session is still open in another live terminal: starting it again would fork its history.
func TestPollReviveSkippedWhenSessionOpen(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	delete(c.terms, "t2")
	open := term("t7", 17, "working")
	open.Labels = map[string]string{"herdr-pr.session": "sid"}
	c.terms["t7"] = open
	gh.prs[prURL] = openPR(oldComment, review)
	err := Run(context.Background(), d)
	if err == nil || !strings.Contains(err.Error(), "not reviving: session sid is open in t7") {
		t.Fatalf("err = %v", err)
	}
	if len(c.runs) != 0 || len(c.sends) != 0 {
		t.Errorf("runs %+v sends %v", c.runs, c.sends)
	}
	if e := load(t, d); !e.Watched || e.ReviveFailed != "" || e.RevivedAs != "" || slices.Contains(e.Last.SeenIDs, "r1") {
		t.Errorf("entry = %+v", e)
	}
}

// hc run can return no created_ms; it is read back so the next pass does not take the new terminal for a reused address.
func TestPollReviveReadsCreatedMsThenDelivers(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	delete(c.terms, "t2")
	c.runAs = hc.Terminal{Address: "t9", State: "live"}
	c.terms["t9"] = term("t9", 99, "idle")
	gh.prs[prURL] = openPR(oldComment, review)
	run(t, d)
	if e := load(t, d); e.RevivedAs != "t9" || e.RevivedCreatedMs != 99 {
		t.Fatalf("entry = %+v", e)
	}
	run(t, d)
	if got := c.to("t9"); !slices.Equal(got, []string{ownerMsg}) {
		t.Errorf("revived owner got %q", got)
	}
	if e := load(t, d); e.ReviveFailed != "" || len(c.runs) != 1 {
		t.Errorf("entry = %+v runs %+v", e, c.runs)
	}
}

func TestPollReviveFailureToldOnce(t *testing.T) {
	e := watched(t)
	e.Owner.Cwd = "/nonexistent/x"
	d, c, gh := setup(t, e)
	delete(c.terms, "t2")
	gh.prs[prURL] = openPR(oldComment, review)
	if err := Run(context.Background(), d); err == nil {
		t.Fatal("want revive error")
	}
	// A recorded failure stops further revives, so the second pass has nothing to report.
	run(t, d)
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

const (
	finalParent  = "[herdr-pr] FYI, no action needed: flocasts/iam#412 (child t2) was merged. Watch ended."
	worktreeLine = "FYI: worktree /repo/_worktrees/iam has no open PRs left and can be removed."
)

func TestPollMergedEndsWatchAndNotesWorktree(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	gh.prs[prURL] = merged(oldComment)
	run(t, d)

	if got := c.to("t2"); !slices.Equal(got, []string{finalMerged}) {
		t.Errorf("owner got %q", got)
	}
	if got := c.to("t1"); !slices.Equal(got, []string{finalParent + "\n" + worktreeLine}) {
		t.Errorf("parent got %q", got)
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

func TestPollMergedOneMessageWithOtherEvents(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	gh.prs[prURL] = merged(oldComment, review)
	run(t, d)
	want := finalMerged + "\n- review from alice: changes requested"
	if got := c.to("t2"); !slices.Equal(got, []string{want}) {
		t.Errorf("owner got %q", got)
	}
	if got := c.to("t1"); len(got) != 1 {
		t.Errorf("parent got %q", got)
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
		if strings.Contains(s.Text, "worktree") {
			t.Errorf("worktree note with an open PR left: %q", s.Text)
		}
	}
	if got := c.to("t2"); !slices.Equal(got, []string{"[herdr-pr] flocasts/iam#412 was closed. Watch ended."}) {
		t.Errorf("owner got %q", got)
	}
}

func TestPollWorktreeNoteToOwnerWithoutParent(t *testing.T) {
	e := watched(t)
	e.Owner.Parent, e.Owner.ParentCreatedMs = "", 0
	d, c, gh := setup(t, e)
	gh.prs[prURL] = merged(oldComment)
	run(t, d)
	if got := c.to("t2"); !slices.Equal(got, []string{finalMerged + "\n" + worktreeLine}) {
		t.Errorf("owner got %q", got)
	}
}

// A main checkout, home, or a folder outside _worktrees/ is never offered for removal.
func TestPollNoWorktreeNoteOutsideLinkedWorktree(t *testing.T) {
	for name, tc := range map[string]struct {
		worktree string
		linked   bool
	}{
		"not a linked worktree": {"/repo/_worktrees/iam", false},
		"not under _worktrees":  {"/home/mattw/Projects/flosports", true},
		"home or no git":        {"", true},
	} {
		t.Run(name, func(t *testing.T) {
			e := watched(t)
			e.Owner.Worktree = tc.worktree
			d, c, gh := setup(t, e)
			d.LinkedWorktree = func(string) bool { return tc.linked }
			gh.prs[prURL] = merged(oldComment)
			run(t, d)
			for _, s := range c.sends {
				if strings.Contains(s.Text, "worktree") {
					t.Errorf("worktree note sent: %q", s.Text)
				}
			}
			if got := load(t, d); got.Watched || got.Last.WorktreeNoted {
				t.Errorf("entry = %+v", got)
			}
		})
	}
}

func TestLinkedWorktreeGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	dir := t.TempDir()
	main, wt := filepath.Join(dir, "repo"), filepath.Join(dir, "repo", "_worktrees", "x")
	for _, args := range [][]string{
		{"init", "-q", main},
		{"-C", main, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"-C", main, "worktree", "add", "-q", "--detach", wt},
	} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if linkedWorktree(main) {
		t.Error("main checkout counted as linked worktree")
	}
	if !linkedWorktree(wt) {
		t.Error("linked worktree not detected")
	}
	if linkedWorktree(filepath.Join(dir, "missing")) {
		t.Error("missing folder counted as linked worktree")
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

func merged(items ...github.Item) github.PR {
	p := openPR(items...)
	p.State = "MERGED"
	return p
}

// clock lets a test move time between passes.
func clock(d *app.Deps) *time.Time {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	d.Now = func() time.Time { return now }
	return &now
}

const finalMerged = "[herdr-pr] flocasts/iam#412 was merged. Watch ended."

// A merge never revives or waits: an owner that is not Live is skipped, the parent is told, and the watch ends.
func TestPollBusyAtMergeTellsParentAndEnds(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	c.terms["t2"] = term("t2", 12, "working")
	gh.prs[prURL] = merged(oldComment)
	run(t, d)
	if got := c.to("t2"); len(got) != 0 {
		t.Errorf("busy owner got %q", got)
	}
	if got := c.to("t1"); !slices.Equal(got, []string{finalParent + "\n" + worktreeLine}) {
		t.Errorf("parent got %q", got)
	}
	if load(t, d).Watched {
		t.Error("watch not ended")
	}
}

func TestPollGoneAtMergeNotRevived(t *testing.T) {
	for name, mod := range map[string]func(*registry.Entry){
		"gone":          func(*registry.Entry) {},
		"revive failed": func(e *registry.Entry) { e.ReviveFailed, e.ReviveFailedTold = "cwd missing: /w", true },
	} {
		t.Run(name, func(t *testing.T) {
			e := watched(t)
			mod(&e)
			d, c, gh := setup(t, e)
			delete(c.terms, "t2")
			gh.prs[prURL] = merged(oldComment, review)
			run(t, d)
			if len(c.runs) != 0 {
				t.Errorf("revived for a merge: %+v", c.runs)
			}
			if got := c.to("t1"); !slices.Equal(got, []string{finalParent + "\n" + worktreeLine}) {
				t.Errorf("parent got %q", got)
			}
			if got := load(t, d); got.Watched || !got.Last.WorktreeNoted {
				t.Errorf("entry = %+v", got)
			}
		})
	}
}

func TestPollInProcessChildMergedTellsParent(t *testing.T) {
	e := watched(t)
	e.Owner.Address, e.Owner.CreatedMs, e.Owner.Kind = "", 0, "pi"
	d, c, gh := setup(t, e)
	gh.prs[prURL] = merged(oldComment)
	run(t, d)
	want := "[herdr-pr] FYI, no action needed: flocasts/iam#412 (child background child) was merged. Watch ended.\n" + worktreeLine
	if got := c.to("t1"); !slices.Equal(got, []string{want}) || len(c.runs) != 0 {
		t.Errorf("parent got %q, runs %v", got, c.runs)
	}
}

func TestPollMergeSendFailureKeepsWatch(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	c.sendErr = map[string]error{"t2": errors.New("hc send failed")}
	gh.prs[prURL] = merged(oldComment)
	if err := Run(context.Background(), d); err == nil {
		t.Fatal("want send error")
	}
	if got := c.to("t1"); len(got) != 0 {
		t.Errorf("parent got %q before the owner was told", got)
	}
	if got := load(t, d); !got.Watched || got.Last.State != "OPEN" {
		t.Errorf("entry = %+v", got)
	}

	c.sendErr = nil
	run(t, d)
	if got := c.to("t2"); !slices.Equal(got, []string{finalMerged}) {
		t.Errorf("owner got %q", got)
	}
	if load(t, d).Watched {
		t.Error("watch not ended")
	}
}

func TestPollRevivedDiesAtStartupNotRevivedAgain(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	now := clock(&d)
	delete(c.terms, "t2")
	c.runAs = hc.Terminal{Address: "t9", CreatedMs: 99}
	gh.prs[prURL] = openPR(oldComment, review)
	run(t, d)
	if got := load(t, d); got.RevivedAtMs != now.UnixMilli() {
		t.Fatalf("RevivedAtMs = %d", got.RevivedAtMs)
	}

	// t9 never appears: the revived session exited at startup.
	*now = now.Add(2 * time.Minute)
	if err := Run(context.Background(), d); err == nil {
		t.Fatal("want startup exit reported")
	}
	*now = now.Add(2 * time.Minute)
	run(t, d)

	if len(c.runs) != 1 {
		t.Errorf("hc run %d times", len(c.runs))
	}
	want := "[herdr-pr] FYI, no action needed: cannot revive the owner of flocasts/iam#412: revived session exited at startup. The watch stays on."
	if got := c.to("t1"); !slices.Equal(got, []string{want}) {
		t.Errorf("parent got %q", got)
	}
	got := load(t, d)
	if got.ReviveFailed == "" || !got.Watched || slices.Contains(got.Last.SeenIDs, "r1") {
		t.Errorf("entry = %+v", got)
	}
}

func TestPollRevivedGoneLaterIsRevivedAgain(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	now := clock(&d)
	delete(c.terms, "t2")
	c.runAs = hc.Terminal{Address: "t9", CreatedMs: 99}
	gh.prs[prURL] = openPR(oldComment, review)
	run(t, d)

	*now = now.Add(11 * time.Minute)
	c.runAs = hc.Terminal{Address: "t10", CreatedMs: 100}
	run(t, d)
	if len(c.runs) != 2 || len(c.sends) != 0 {
		t.Errorf("runs %d sends %v", len(c.runs), c.sends)
	}
	if got := load(t, d); got.RevivedAs != "t10" || got.ReviveFailed != "" {
		t.Errorf("entry = %+v", got)
	}
}

func TestPollReviveFailureToldWhenParentFreesUp(t *testing.T) {
	e := watched(t)
	e.Owner.Cwd = "/nonexistent/x"
	d, c, gh := setup(t, e)
	delete(c.terms, "t2")
	c.terms["t1"] = term("t1", 11, "working")
	gh.prs[prURL] = openPR(oldComment, review)
	if err := Run(context.Background(), d); err == nil {
		t.Fatal("want revive error")
	}
	if got := c.to("t1"); len(got) != 0 {
		t.Fatalf("busy parent got %q", got)
	}

	c.terms["t1"] = term("t1", 11, "idle")
	run(t, d)
	run(t, d)
	want := "[herdr-pr] FYI, no action needed: cannot revive the owner of flocasts/iam#412: cwd missing: /nonexistent/x. The watch stays on."
	if got := c.to("t1"); !slices.Equal(got, []string{want}) {
		t.Errorf("parent got %q", got)
	}
	if len(c.runs) != 0 {
		t.Errorf("hc run %d times", len(c.runs))
	}
	if got := load(t, d); !got.ReviveFailedTold {
		t.Errorf("entry = %+v", got)
	}
}

func TestPollRevivedClosedAfterDeliveryIsRevivedAgain(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	now := clock(&d)
	delete(c.terms, "t2")
	c.runAs = hc.Terminal{Address: "t9", CreatedMs: 99}
	gh.prs[prURL] = openPR(oldComment, review)
	run(t, d)

	*now = now.Add(2 * time.Minute)
	c.terms["t9"] = term("t9", 99, "idle")
	run(t, d)
	if got := c.to("t9"); len(got) != 1 {
		t.Fatalf("revived owner got %q", got)
	}

	// The user closes the revived session, then a new comment arrives, all inside the startup window.
	*now = now.Add(2 * time.Minute)
	delete(c.terms, "t9")
	c.runAs = hc.Terminal{Address: "t10", CreatedMs: 100}
	gh.prs[prURL] = openPR(oldComment, review, github.Item{ID: "c2", Kind: "comment", Author: github.Actor{Login: "carol"}, Body: "nit"})
	run(t, d)
	if len(c.runs) != 2 {
		t.Errorf("hc run %d times", len(c.runs))
	}
	if got := load(t, d); got.RevivedAs != "t10" || got.ReviveFailed != "" {
		t.Errorf("entry = %+v", got)
	}
}

// claudeTerm is an interactive Claude terminal: no status, created at created ms.
func claudeTerm(addr string, created int64) hc.Terminal {
	return hc.Terminal{Address: addr, CreatedMs: created, State: "live", Foreground: &struct {
		Name string   `json:"name"`
		Pid  int      `json:"pid"`
		Argv []string `json:"argv"`
		Cwd  string   `json:"cwd"`
	}{Name: "claude"}}
}

func TestPollClaudeOwnerWithoutStatusWoken(t *testing.T) {
	d, c, gh := setup(t, watched(t))
	c.terms["t2"] = claudeTerm("t2", 12) // created in 1970: long settled
	gh.prs[prURL] = openPR(oldComment, review)
	run(t, d)
	if got := c.to("t2"); !slices.Equal(got, []string{ownerMsg}) {
		t.Errorf("claude owner got %q", got)
	}
}

func second(t *testing.T) registry.Entry {
	e := watched(t)
	e.URL, e.Repo, e.Number = "https://github.com/flocasts/infra/pull/99", "flocasts/infra", 99
	return e
}

func prAt(url string, items ...github.Item) github.PR {
	p := openPR(items...)
	p.URL = url
	return p
}

// One session that owns two PRs is started once, and both PRs are delivered to that terminal.
func TestPollOneSessionRevivedOnce(t *testing.T) {
	other := second(t)
	d, c, gh := setup(t, watched(t), other)
	now := clock(&d)
	delete(c.terms, "t2")
	c.runAs = hc.Terminal{Address: "t9", CreatedMs: 99}
	gh.prs[prURL] = openPR(oldComment, review)
	gh.prs[other.URL] = prAt(other.URL, oldComment, review)
	run(t, d)
	if len(c.runs) != 1 || len(c.sends) != 0 {
		t.Fatalf("runs %+v sends %v", c.runs, c.sends)
	}
	r, _ := registry.Load(d.StatePath)
	for _, e := range r.Entries {
		if e.RevivedAs != "t9" || e.RevivedCreatedMs != 99 || e.RevivedAtMs != now.UnixMilli() {
			t.Errorf("%s: revive fields %q/%d/%d", e.URL, e.RevivedAs, e.RevivedCreatedMs, e.RevivedAtMs)
		}
	}

	*now = now.Add(2 * time.Minute)
	c.terms["t9"] = term("t9", 99, "idle")
	run(t, d)
	if len(c.runs) != 1 {
		t.Errorf("hc run %d times", len(c.runs))
	}
	got := c.to("t9")
	if len(got) != 2 || !strings.Contains(got[0], "flocasts/iam#412") || !strings.Contains(got[1], "flocasts/infra#99") {
		t.Errorf("revived owner got %q", got)
	}
	r, _ = registry.Load(d.StatePath)
	for _, e := range r.Entries {
		if e.RevivedAtMs != 0 {
			t.Errorf("%s: startup window still open", e.URL)
		}
	}
}

// A PR with no events while its session was revived for another PR still finds that terminal later.
func TestPollAdoptsTerminalRevivedForAnotherPR(t *testing.T) {
	e := watched(t)
	e.RevivedAs, e.RevivedCreatedMs = "t9", 99
	other := second(t)
	d, c, gh := setup(t, e, other)
	delete(c.terms, "t2")
	c.terms["t9"] = term("t9", 99, "idle")
	gh.prs[prURL] = openPR(oldComment)
	gh.prs[other.URL] = prAt(other.URL, oldComment, review)
	run(t, d)
	if len(c.runs) != 0 {
		t.Errorf("revived again: %+v", c.runs)
	}
	if got := c.to("t9"); len(got) != 1 || !strings.Contains(got[0], "flocasts/infra#99") {
		t.Errorf("t9 got %q", got)
	}
	r, _ := registry.Load(d.StatePath)
	if got := r.Find(other.URL); got.RevivedAs != "t9" {
		t.Errorf("entry = %+v", got)
	}
}

type modTime time.Time

func (m modTime) Name() string       { return "s.jsonl" }
func (m modTime) Size() int64        { return 0 }
func (m modTime) Mode() fs.FileMode  { return 0 }
func (m modTime) ModTime() time.Time { return time.Time(m) }
func (m modTime) IsDir() bool        { return false }
func (m modTime) Sys() any           { return nil }

// An in-process child whose session file changed recently may still run inside its parent.
func TestPollInProcessChildRecentlyActiveNotRevived(t *testing.T) {
	e := watched(t)
	e.Owner.Address, e.Owner.CreatedMs, e.Owner.Kind, e.Owner.SessionFile = "", 0, "pi", "/s/child.jsonl"
	d, c, gh := setup(t, e)
	now := clock(&d)
	c.runAs = hc.Terminal{Address: "t9", CreatedMs: 99}
	written := now.Add(-2 * time.Minute)
	d.Stat = func(path string) (fs.FileInfo, error) {
		if path != "/s/child.jsonl" {
			return nil, fs.ErrNotExist
		}
		return modTime(written), nil
	}
	gh.prs[prURL] = openPR(oldComment, review)
	run(t, d)
	if len(c.runs) != 0 || len(c.sends) != 0 {
		t.Fatalf("runs %+v sends %v", c.runs, c.sends)
	}

	*now = now.Add(9 * time.Minute) // 11 minutes since the last write
	run(t, d)
	if len(c.runs) != 1 || load(t, d).RevivedAs != "t9" {
		t.Errorf("runs %+v", c.runs)
	}
}
