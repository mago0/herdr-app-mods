// Package poll runs one pass of the watch: fetch open PRs, wake owners whose PR needs action, save what was handled.
package poll

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/app"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/deliver"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/events"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/github"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/hc"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/prurl"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/registry"
)

// Run does one pass. The registry lock is not held across gh or hc calls: the pass
// works on a snapshot and merges its changes back by URL at the end.
func Run(ctx context.Context, d app.Deps) error {
	snap, err := registry.Load(d.StatePath)
	if err != nil {
		return err
	}
	p := &pass{
		hc: d.HC, now: d.Now(), entries: snap.Entries, linked: d.LinkedWorktree, stat: d.Stat,
		ended: map[string]bool{}, revived: map[string]bool{}, touched: map[string]bool{},
	}
	if p.linked == nil {
		p.linked = linkedWorktree
	}
	if p.stat == nil {
		p.stat = os.Stat
	}

	// Unwatched open PRs are fetched too so the display stays current. A watched PR
	// stays fetched until its owner has been told it closed, even after Display says so.
	var refs []prurl.Ref
	for _, e := range snap.Entries {
		if !isOpen(e) && !(e.Watched && (e.Last.State == "" || e.Last.State == "OPEN")) {
			continue
		}
		ref, err := prurl.Parse(e.URL)
		if err != nil {
			p.errs = append(p.errs, err)
			continue
		}
		refs = append(refs, ref)
	}
	if len(refs) == 0 {
		return errors.Join(p.errs...)
	}
	b, err := d.GH.Fetch(ctx, refs)
	if err != nil {
		return err
	}
	// HERDR_PR_VIEWER is a test override for the gh login whose own comments never wake.
	if p.viewer = d.Env("HERDR_PR_VIEWER"); p.viewer == "" {
		if p.viewer = b.Viewer; p.viewer == "" {
			return errors.New("github: viewer login missing")
		}
	}

	// Displays first, so the worktree check sees every PR's state from this pass.
	for i := range p.entries {
		if pr, ok := b.PRs[p.entries[i].URL]; ok {
			p.entries[i].Display = events.DisplayOf(pr)
		}
	}
	done := map[string]bool{}
	for i := range p.entries {
		e := &p.entries[i]
		if pr, ok := b.PRs[e.URL]; ok {
			done[e.URL] = true
			p.handle(e, pr)
		}
	}

	err = registry.Update(d.StatePath, func(r *registry.Registry) error {
		for _, e := range p.entries {
			cur := r.Find(e.URL)
			// Skip entries unlinked meanwhile or taken over by another session.
			if !(done[e.URL] || p.touched[e.URL]) || cur == nil || cur.Owner.SessionID != e.Owner.SessionID {
				continue
			}
			// Revive fields are shared by every PR of a session, fetched or not.
			cur.RevivedAs, cur.RevivedCreatedMs, cur.RevivedAtMs = e.RevivedAs, e.RevivedCreatedMs, e.RevivedAtMs
			if !done[e.URL] {
				continue
			}
			cur.Display, cur.Last = e.Display, e.Last
			cur.ReviveFailed, cur.ReviveFailedTold = e.ReviveFailed, e.ReviveFailedTold
			if p.ended[e.URL] {
				cur.Watched = false
			}
		}
		return nil
	})
	return errors.Join(append(p.errs, err)...)
}

// isOpen counts a never-fetched entry (empty state) as open.
func isOpen(e registry.Entry) bool { return e.Display.State == "" || e.Display.State == "OPEN" }

type pass struct {
	hc      hc.Client
	now     time.Time
	viewer  string
	entries []registry.Entry
	linked  func(string) bool
	stat    func(string) (fs.FileInfo, error)
	ended   map[string]bool // URL -> watch ended this pass
	revived map[string]bool // session ID -> revived this pass
	touched map[string]bool // URL -> revive fields changed through another PR of its session
	errs    []error
}

func ref(e *registry.Entry) string { return fmt.Sprintf("%s#%d", e.Repo, e.Number) }

// handle advances e.Last only once the owner has been told, so a busy or revived owner gets the events next pass.
func (p *pass) handle(e *registry.Entry, pr github.PR) {
	evs := events.Classify(e.Last, pr, p.viewer)
	if !e.Watched || len(evs) == 0 {
		e.Last = events.Next(e.Last, pr)
		return
	}
	addr, live, ok := p.owner(e)
	if !ok {
		return
	}
	if pr.State == "MERGED" || pr.State == "CLOSED" {
		p.finish(e, pr, evs, addr, live)
		return
	}
	switch live {
	case deliver.Busy:
		return
	case deliver.Gone:
		p.revive(e)
		return
	}
	if !p.send(addr, deliver.OwnerMessage(*e, evs)) {
		return
	}
	p.delivered(e)
	if p.parent(e) == deliver.Live {
		p.send(e.Owner.Parent, deliver.ParentFYI(*e, addr, evs))
	}
	e.Last = events.Next(e.Last, pr)
}

// owner finds where e's session runs now and whether it can take a message. The session,
// not the PR, is what runs: a terminal revived for another PR of the same session is adopted.
// ok is false after an hc failure; the PR then waits for the next pass.
func (p *pass) owner(e *registry.Entry) (addr string, live deliver.Liveness, ok bool) {
	if p.revived[e.Owner.SessionID] {
		return e.RevivedAs, deliver.Busy, true
	}
	addr, created := e.Owner.Address, e.Owner.CreatedMs
	if e.RevivedAs != "" {
		addr, created = e.RevivedAs, e.RevivedCreatedMs
	}
	// An in-process child with no terminal of its own has nothing to check.
	if addr != "" {
		l, _, err := deliver.Check(p.hc, addr, created, p.now)
		if err != nil {
			p.errs = append(p.errs, fmt.Errorf("%s: check owner %s: %w", ref(e), addr, err))
			return addr, deliver.Gone, false
		}
		if l != deliver.Gone {
			return addr, l, true
		}
	}
	for i := range p.entries {
		o := &p.entries[i]
		if o.URL == e.URL || o.Owner.SessionID != e.Owner.SessionID || o.RevivedAs == "" || o.RevivedAs == addr {
			continue
		}
		l, _, err := deliver.Check(p.hc, o.RevivedAs, o.RevivedCreatedMs, p.now)
		if err != nil {
			p.errs = append(p.errs, fmt.Errorf("%s: check owner %s: %w", ref(e), o.RevivedAs, err))
			return addr, deliver.Gone, false
		}
		if l != deliver.Gone {
			e.RevivedAs, e.RevivedCreatedMs, e.RevivedAtMs = o.RevivedAs, o.RevivedCreatedMs, o.RevivedAtMs
			return e.RevivedAs, l, true
		}
	}
	// An in-process child may still be running inside its parent's pi; its session file shows it.
	if e.Owner.Address == "" && e.RevivedAs == "" && p.recentlyActive(e.Owner.SessionFile) {
		return "", deliver.Busy, true
	}
	return addr, deliver.Gone, true
}

// inProcessQuiet is how long an in-process child's session file must be unchanged before the child counts as exited.
const inProcessQuiet = 10 * time.Minute

func (p *pass) recentlyActive(file string) bool {
	if file == "" {
		return false
	}
	fi, err := p.stat(file)
	return err == nil && p.now.Sub(fi.ModTime()) < inProcessQuiet
}

// delivered ends the startup window of the terminal that took a message, for every PR of its session:
// a revived session that has taken a message has started, so closing it later is not a startup failure.
func (p *pass) delivered(e *registry.Entry) {
	e.RevivedAtMs = 0
	if e.RevivedAs == "" {
		return
	}
	for i := range p.entries {
		o := &p.entries[i]
		if o.URL != e.URL && o.Owner.SessionID == e.Owner.SessionID && o.RevivedAs == e.RevivedAs && o.RevivedAtMs != 0 {
			o.RevivedAtMs = 0
			p.touched[o.URL] = true
		}
	}
}

// finish ends the watch on merge or close with one message per recipient. It never revives:
// a Live owner is told and the watch ends once that send succeeds; otherwise the parent, if
// Live, is told and the watch ends anyway, since a gone owner may never come back. When
// neither is Live but one is only busy, the watch stays on so the note goes out next pass.
func (p *pass) finish(e *registry.Entry, pr github.PR, evs []events.Event, addr string, live deliver.Liveness) {
	state := strings.ToLower(pr.State)
	var others []events.Event
	for _, ev := range evs {
		if ev.Kind != events.Merged && ev.Kind != events.Closed {
			others = append(others, ev)
		}
	}
	parent := p.parent(e)
	parentLive := parent == deliver.Live
	if live != deliver.Live && !parentLive && (live == deliver.Busy || parent == deliver.Busy) {
		return
	}
	wt := p.freeWorktree(e)
	ownerWT, parentWT := "", wt
	if !parentLive {
		ownerWT, parentWT = wt, ""
	}
	noted := e.Last.WorktreeNoted
	if live == deliver.Live {
		if !p.send(addr, deliver.FinalOwnerMessage(*e, state, others, ownerWT)) {
			return
		}
		p.delivered(e)
		noted = noted || ownerWT != ""
	}
	if parentLive && p.send(e.Owner.Parent, deliver.FinalParentMessage(*e, state, addr, parentWT)) {
		noted = noted || parentWT != ""
	}
	e.Last = events.Next(e.Last, pr)
	e.Last.WorktreeNoted = noted
	e.Watched = false
	p.ended[e.URL] = true
}

// freeWorktree is the owner's worktree once this PR was its last open one, else "". Only a linked
// git worktree under _worktrees/ qualifies, so a main checkout or home is never offered for removal.
func (p *pass) freeWorktree(e *registry.Entry) string {
	wt := e.Owner.Worktree
	if wt == "" || e.Last.WorktreeNoted || !strings.Contains(wt, "/_worktrees/") {
		return ""
	}
	for _, o := range p.entries {
		if o.URL != e.URL && o.Owner.Worktree == wt && isOpen(o) {
			return ""
		}
	}
	if !p.linked(wt) {
		return ""
	}
	return wt
}

// linkedWorktree reports whether path is a linked git worktree: its git dir is not the shared one.
func linkedWorktree(path string) bool {
	out, err := exec.Command("git", "-C", path, "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir").Output()
	if err != nil {
		return false
	}
	dirs := strings.Split(strings.TrimSpace(string(out)), "\n")
	return len(dirs) == 2 && dirs[0] != dirs[1]
}

// parent reports the owner's parent terminal; no parent or an hc failure counts as Gone.
func (p *pass) parent(e *registry.Entry) deliver.Liveness {
	if e.Owner.Parent == "" {
		return deliver.Gone
	}
	l, _, err := deliver.Check(p.hc, e.Owner.Parent, e.Owner.ParentCreatedMs, p.now)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: check parent %s: %w", ref(e), e.Owner.Parent, err))
		return deliver.Gone
	}
	return l
}

// startupWindow is how soon a revived terminal may disappear, before its first delivery, for the revive to count as failed.
const startupWindow = 10 * time.Minute

// revive starts the owner again; delivery waits for the next pass, when the new terminal has settled.
// A recorded failure stops further revives until `watch` re-arms, so a session that cannot start is not respawned forever.
// Every PR of the session gets the new terminal, so the session is not started once per PR.
func (p *pass) revive(e *registry.Entry) {
	if e.ReviveFailed != "" {
		if !e.ReviveFailedTold {
			p.tellReviveFailed(e, p.parent(e))
		}
		return
	}
	pl := p.parent(e)
	if e.RevivedAs != "" && e.RevivedAtMs != 0 && p.now.Sub(time.UnixMilli(e.RevivedAtMs)) < startupWindow {
		p.reviveFailed(e, pl, errors.New("revived session exited at startup"))
		return
	}
	// A busy parent still exists, so the revived terminal stays under it.
	t, err := deliver.Revive(p.hc, *e, pl != deliver.Gone)
	if err != nil {
		p.reviveFailed(e, pl, err)
		return
	}
	e.RevivedAs, e.RevivedCreatedMs, e.RevivedAtMs = t.Address, t.CreatedMs, p.now.UnixMilli()
	p.revived[e.Owner.SessionID] = true
	for i := range p.entries {
		o := &p.entries[i]
		if o.URL != e.URL && o.Owner.SessionID == e.Owner.SessionID {
			o.RevivedAs, o.RevivedCreatedMs, o.RevivedAtMs = e.RevivedAs, e.RevivedCreatedMs, e.RevivedAtMs
			p.touched[o.URL] = true
		}
	}
}

func (p *pass) reviveFailed(e *registry.Entry, parent deliver.Liveness, err error) {
	e.ReviveFailed, e.ReviveFailedTold = err.Error(), false
	p.errs = append(p.errs, fmt.Errorf("%s: revive: %w", ref(e), err))
	p.tellReviveFailed(e, parent)
}

// tellReviveFailed tells the parent once, on the first pass it is Live, so a busy parent hears later.
func (p *pass) tellReviveFailed(e *registry.Entry, parent deliver.Liveness) {
	if parent != deliver.Live {
		return
	}
	msg := fmt.Sprintf("[herdr-pr] FYI, no action needed: cannot revive the owner of %s: %s. The watch stays on.", ref(e), e.ReviveFailed)
	if p.send(e.Owner.Parent, msg) {
		e.ReviveFailedTold = true
	}
}

func (p *pass) send(addr, text string) bool {
	if err := p.hc.Send(addr, text); err != nil {
		p.errs = append(p.errs, fmt.Errorf("send to %s: %w", addr, err))
		return false
	}
	return true
}
