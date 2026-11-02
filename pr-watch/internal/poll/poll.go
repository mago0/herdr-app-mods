// Package poll runs one pass of the watch: fetch open PRs, wake owners whose PR needs action, save what was handled.
package poll

import (
	"context"
	"errors"
	"fmt"
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
	p := &pass{hc: d.HC, now: d.Now(), entries: snap.Entries, ended: map[string]bool{}}

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
	prs, err := d.GH.Fetch(ctx, refs)
	if err != nil {
		return err
	}
	// HERDR_PR_VIEWER is a test override for the gh login whose own comments never wake.
	if p.viewer = d.Env("HERDR_PR_VIEWER"); p.viewer == "" {
		if p.viewer, err = d.GH.Viewer(ctx); err != nil {
			return err
		}
	}

	// Displays first, so the worktree check sees every PR's state from this pass.
	for i := range p.entries {
		if pr, ok := prs[p.entries[i].URL]; ok {
			p.entries[i].Display = events.DisplayOf(pr)
		}
	}
	done := map[string]bool{}
	for i := range p.entries {
		e := &p.entries[i]
		if pr, ok := prs[e.URL]; ok {
			done[e.URL] = true
			p.handle(e, pr)
		}
	}

	err = registry.Update(d.StatePath, func(r *registry.Registry) error {
		for _, e := range p.entries {
			cur := r.Find(e.URL)
			// Skip entries unlinked meanwhile or taken over by another session.
			if !done[e.URL] || cur == nil || cur.Owner.SessionID != e.Owner.SessionID {
				continue
			}
			cur.Display, cur.Last = e.Display, e.Last
			cur.RevivedAs, cur.RevivedCreatedMs, cur.RevivedAtMs = e.RevivedAs, e.RevivedCreatedMs, e.RevivedAtMs
			cur.ReviveFailed = e.ReviveFailed
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
	ended   map[string]bool
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
	addr, created := e.Owner.Address, e.Owner.CreatedMs
	if e.RevivedAs != "" {
		addr, created = e.RevivedAs, e.RevivedCreatedMs
	}
	// An in-process child with no terminal of its own can only be reached by reviving it.
	live := deliver.Gone
	if addr != "" {
		l, _, err := deliver.Check(p.hc, addr, created)
		if err != nil {
			p.errs = append(p.errs, fmt.Errorf("%s: check owner %s: %w", ref(e), addr, err))
			return
		}
		live = l
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
	parentLive := p.parent(e) == deliver.Live
	if parentLive {
		p.send(e.Owner.Parent, deliver.ParentFYI(*e, addr, evs))
	}
	e.Last = events.Next(e.Last, pr)
	if pr.State == "MERGED" || pr.State == "CLOSED" {
		p.end(e, strings.ToLower(pr.State), addr, parentLive)
	}
}

// parent reports the owner's parent terminal; no parent or an hc failure counts as Gone.
func (p *pass) parent(e *registry.Entry) deliver.Liveness {
	if e.Owner.Parent == "" {
		return deliver.Gone
	}
	l, _, err := deliver.Check(p.hc, e.Owner.Parent, e.Owner.ParentCreatedMs)
	if err != nil {
		p.errs = append(p.errs, fmt.Errorf("%s: check parent %s: %w", ref(e), e.Owner.Parent, err))
		return deliver.Gone
	}
	return l
}

// startupWindow is how soon a revived terminal may disappear before the revive counts as failed.
const startupWindow = 10 * time.Minute

// revive starts the owner again; delivery waits for the next pass, when the new terminal has settled.
// A recorded failure stops further revives until `watch` re-arms, so a session that cannot start is not respawned forever.
func (p *pass) revive(e *registry.Entry) {
	if e.ReviveFailed != "" {
		return
	}
	pl := p.parent(e)
	if e.RevivedAs != "" && p.now.Sub(time.UnixMilli(e.RevivedAtMs)) < startupWindow {
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
}

// reviveFailed records err and tells a live parent; a busy or gone parent is not told later.
func (p *pass) reviveFailed(e *registry.Entry, parent deliver.Liveness, err error) {
	e.ReviveFailed = err.Error()
	p.errs = append(p.errs, fmt.Errorf("%s: revive: %w", ref(e), err))
	if parent == deliver.Live {
		p.send(e.Owner.Parent, fmt.Sprintf("[herdr-pr] FYI, no action needed: cannot revive the owner of %s: %v. The watch stays on.", ref(e), err))
	}
}

// end closes the watch and, once a worktree has no open PR left, says it can go.
func (p *pass) end(e *registry.Entry, state, addr string, parentLive bool) {
	final := deliver.FinalNote(*e, state)
	p.send(addr, final)
	if parentLive {
		p.send(e.Owner.Parent, final)
	}
	e.Watched = false
	p.ended[e.URL] = true

	cwd := e.Owner.Cwd
	if cwd == "" || e.Last.WorktreeNoted {
		return
	}
	for _, o := range p.entries {
		if o.URL != e.URL && o.Owner.Cwd == cwd && isOpen(o) {
			return
		}
	}
	to := addr
	if parentLive {
		to = e.Owner.Parent
	}
	if p.send(to, deliver.WorktreeNote(cwd)) {
		e.Last.WorktreeNoted = true
	}
}

func (p *pass) send(addr, text string) bool {
	if err := p.hc.Send(addr, text); err != nil {
		p.errs = append(p.errs, fmt.Errorf("send to %s: %w", addr, err))
		return false
	}
	return true
}
