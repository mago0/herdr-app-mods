package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

type fakeHC map[string]hc.Terminal

func (f fakeHC) Get(addr string) (hc.Terminal, error) {
	t, ok := f[addr]
	if !ok {
		return hc.Terminal{}, hc.ErrNotFound
	}
	return t, nil
}
func (fakeHC) Send(string, string) error { return errors.New("unexpected Send") }
func (fakeHC) Run(hc.RunSpec) (hc.Terminal, error) {
	return hc.Terminal{}, errors.New("unexpected Run")
}

type fakeGH struct {
	prs   map[string]github.PR
	calls int
}

func (f *fakeGH) Fetch(_ context.Context, refs []prurl.Ref) (map[string]github.PR, error) {
	f.calls++
	out := map[string]github.PR{}
	for _, r := range refs {
		if p, ok := f.prs[r.URL()]; ok {
			out[r.URL()] = p
		}
	}
	return out, nil
}
func (*fakeGH) Viewer(context.Context) (string, error) { return "mago0", nil }

// claudeTerm is a Claude terminal whose session comes from the launch-recipe label.
func claudeTerm(addr, session string) hc.Terminal {
	return hc.Terminal{
		Address: addr, CreatedMs: 1000, State: "live",
		Labels: map[string]string{"herdr-pr.session": session},
		Foreground: &struct {
			Name string   `json:"name"`
			Pid  int      `json:"pid"`
			Argv []string `json:"argv"`
			Cwd  string   `json:"cwd"`
		}{Name: "claude", Pid: 70},
	}
}

func deps(t *testing.T, terminal string, c fakeHC, gh *fakeGH) app.Deps {
	t.Helper()
	return app.Deps{
		Env: func(k string) string { return map[string]string{"HC_TERMINAL_ID": terminal}[k] },
		HC:  c,
		GH:  gh,
		// The state directory does not exist yet, as on first use.
		StatePath:      filepath.Join(t.TempDir(), "herdr-pr", "registry.json"),
		ClaudeSessions: t.TempDir(),
		Now:            func() time.Time { return time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC) },
	}
}

func run(d app.Deps, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := Run(args, d, &out, &errb)
	return code, out.String(), errb.String()
}

func reviewedPR() *fakeGH {
	return &fakeGH{prs: map[string]github.PR{prURL: {
		URL: prURL, Title: "Add role", State: "OPEN", Mergeable: "MERGEABLE",
		HeadSHA: "a1b2c3d4", CI: "SUCCESS", ReviewDecision: "CHANGES_REQUESTED",
		Items: []github.Item{{ID: "R1", Kind: "review", Author: github.Actor{Login: "alice"}, State: "CHANGES_REQUESTED", Body: "fix"}},
	}}}
}

func TestLinkPrimesNoWake(t *testing.T) {
	gh := reviewedPR()
	d := deps(t, "t7", fakeHC{"t7": claudeTerm("t7", "S7")}, gh)
	code, out, errs := run(d, "link", "https://github.com/FloCasts/IAM/pull/412/files", "--watch")
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if !strings.HasPrefix(out, "[herdr-pr] ") {
		t.Errorf("stdout = %q", out)
	}
	if gh.calls != 1 {
		t.Errorf("fetch calls = %d, want 1", gh.calls)
	}
	r, err := registry.Load(d.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	e := r.Find(prURL)
	if e == nil {
		t.Fatalf("entry missing: %+v", r.Entries)
	}
	if !e.Primed || !e.Watched || !slices.Contains(e.Last.SeenIDs, "R1") || e.Last.State != "OPEN" {
		t.Errorf("primed/watched/last = %v/%v/%+v", e.Primed, e.Watched, e.Last)
	}
	if e.Display.Title != "Add role" || e.Display.Review != "CHANGES_REQUESTED" || e.Display.CI != "SUCCESS" {
		t.Errorf("display = %+v", e.Display)
	}
	if e.Owner.Address != "t7" || e.Owner.SessionID != "S7" || e.Repo != "flocasts/iam" || e.Number != 412 {
		t.Errorf("owner/repo/number = %+v %q %d", e.Owner, e.Repo, e.Number)
	}
}

func TestLinkOwnedByOther(t *testing.T) {
	c := fakeHC{"t7": claudeTerm("t7", "S7"), "t8": claudeTerm("t8", "S8")}
	d := deps(t, "t7", c, reviewedPR())
	if code, _, errs := run(d, "link", prURL); code != 0 {
		t.Fatalf("first link exit %d: %s", code, errs)
	}
	d.Env = func(k string) string { return map[string]string{"HC_TERMINAL_ID": "t8"}[k] }
	code, _, errs := run(d, "link", prURL)
	if code != 1 || !strings.Contains(errs, "already linked by") {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
	if code, _, errs := run(d, "link", prURL, "--force"); code != 0 {
		t.Fatalf("forced link exit %d: %s", code, errs)
	}
	r, _ := registry.Load(d.StatePath)
	if got := r.Find(prURL).Owner.SessionID; got != "S8" {
		t.Errorf("owner after --force = %q", got)
	}
}

func TestWatchUnwatchUnlink(t *testing.T) {
	d := deps(t, "t7", fakeHC{"t7": claudeTerm("t7", "S7")}, reviewedPR())
	if code, _, errs := run(d, "link", prURL); code != 0 {
		t.Fatalf("link exit %d: %s", code, errs)
	}
	watched := func() bool {
		r, _ := registry.Load(d.StatePath)
		return r.Find(prURL).Watched
	}
	if code, _, _ := run(d, "watch", prURL+"/files"); code != 0 || !watched() {
		t.Fatalf("watch exit %d, watched %v", code, watched())
	}
	if code, _, _ := run(d, "unwatch", prURL); code != 0 || watched() {
		t.Fatalf("unwatch exit %d, watched %v", code, watched())
	}
	if code, _, _ := run(d, "unlink", prURL); code != 0 {
		t.Fatalf("unlink exit %d", code)
	}
	r, _ := registry.Load(d.StatePath)
	if r.Find(prURL) != nil {
		t.Fatal("entry still present after unlink")
	}
	if code, _, errs := run(d, "watch", prURL); code != 1 || !strings.Contains(errs, "not linked") {
		t.Fatalf("watch unlinked: exit %d, stderr %q", code, errs)
	}
}

func TestWatchClearsReviveFailed(t *testing.T) {
	d := deps(t, "", nil, nil)
	registry.Update(d.StatePath, func(r *registry.Registry) error {
		r.Entries = []registry.Entry{{URL: prURL, Repo: "flocasts/iam", Number: 412, ReviveFailed: "cwd missing: /w", ReviveFailedTold: true, RevivedAtMs: 5}}
		return nil
	})
	if code, _, errs := run(d, "watch", prURL); code != 0 {
		t.Fatalf("watch exit %d: %s", code, errs)
	}
	r, _ := registry.Load(d.StatePath)
	if e := r.Find(prURL); e.ReviveFailed != "" || e.ReviveFailedTold || e.RevivedAtMs != 0 || !e.Watched {
		t.Errorf("entry = %+v", e)
	}
}

func TestLsWithChildren(t *testing.T) {
	d := deps(t, "", nil, nil)
	pr := func(n int, o registry.Owner) registry.Entry {
		ref := prurl.Ref{Owner: "flocasts", Name: "iam", Number: n}
		return registry.Entry{URL: ref.URL(), Repo: ref.Repo(), Number: n, Owner: o, Display: registry.Display{Title: "pr", State: "OPEN"}}
	}
	err := registry.Update(d.StatePath, func(r *registry.Registry) error {
		r.Entries = []registry.Entry{
			pr(1, registry.Owner{Address: "t3", SessionID: "S3", Kind: "pi"}),
			pr(2, registry.Owner{Address: "t7", SessionID: "S7", Kind: "claude", Cwd: "/w7", Parent: "t3"}),
			pr(3, registry.Owner{SessionID: "C1", Kind: "pi", Cwd: "/w3", Parent: "t3"}),
			pr(4, registry.Owner{Address: "t9", SessionID: "S9", Kind: "claude", Parent: "t7"}),
			pr(5, registry.Owner{Address: "t7", SessionID: "S7", Kind: "claude", Cwd: "/w7", Parent: "t3"}),
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(d, "ls", "--agent", "t3", "--with-children", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	var got struct {
		Agent string
		Own   []map[string]any
		// Decoded loosely so the test checks the wire keys.
		Children []struct {
			Owner map[string]any
			PRs   []map[string]any `json:"prs"`
		}
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got.Agent != "t3" || len(got.Own) != 1 || got.Own[0]["number"] != 1.0 || got.Own[0]["owner_address"] != "t3" {
		t.Errorf("agent/own = %q %v", got.Agent, got.Own)
	}
	if len(got.Children) != 2 {
		t.Fatalf("children = %+v", got.Children)
	}
	c7, c1 := got.Children[0], got.Children[1]
	if c7.Owner["address"] != "t7" || c7.Owner["session"] != "S7" || c7.Owner["kind"] != "claude" || c7.Owner["cwd"] != "/w7" || len(c7.PRs) != 2 {
		t.Errorf("t7 group = %+v", c7)
	}
	if c1.Owner["address"] != "" || c1.Owner["session"] != "C1" || len(c1.PRs) != 1 || c1.PRs[0]["number"] != 3.0 {
		t.Errorf("in-process group = %+v", c1)
	}
	for _, k := range []string{"url", "repo", "number", "watched", "title", "state", "draft", "ci", "review", "owner_address"} {
		if _, ok := got.Own[0][k]; !ok {
			t.Errorf("own entry missing key %q: %v", k, got.Own[0])
		}
	}
	if strings.Contains(out, "/pull/4\"") {
		t.Errorf("grandchild t9 PR listed: %s", out)
	}
}

func TestLsAllWithoutAgent(t *testing.T) {
	d := deps(t, "", nil, nil)
	registry.Update(d.StatePath, func(r *registry.Registry) error {
		r.Entries = []registry.Entry{{URL: prURL, Repo: "flocasts/iam", Number: 412, Owner: registry.Owner{Address: "t1", Parent: "t0"}}}
		return nil
	})
	code, out, _ := run(d, "ls", "--json")
	if code != 0 || !strings.Contains(out, `"own":[{`) || !strings.Contains(out, `"children":[]`) {
		t.Fatalf("exit %d: %s", code, out)
	}
}

type failingGH struct{ fakeGH }

func (*failingGH) Fetch(context.Context, []prurl.Ref) (map[string]github.PR, error) {
	return nil, errors.New("gh down")
}

func TestPollEmptyRegistry(t *testing.T) {
	code, _, errs := run(deps(t, "", nil, &fakeGH{}), "poll")
	if code != 0 || errs != "" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}

func TestPollErrorExits1(t *testing.T) {
	d := deps(t, "", nil, nil)
	d.GH = &failingGH{}
	registry.Update(d.StatePath, func(r *registry.Registry) error {
		r.Entries = []registry.Entry{{URL: prURL, Repo: "flocasts/iam", Number: 412}}
		return nil
	})
	code, _, errs := run(d, "poll")
	if code != 1 || errs != "[herdr-pr] poll: gh down\n" {
		t.Fatalf("exit %d, stderr %q", code, errs)
	}
}
