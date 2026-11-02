// Package deliver words the messages sent to agents and checks whether an owner terminal can take them.
package deliver

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/events"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/hc"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/registry"
)

func ref(e registry.Entry) string { return fmt.Sprintf("%s#%d", e.Repo, e.Number) }

func OwnerMessage(e registry.Entry, evs []events.Event) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[herdr-pr] %s needs action:\n", ref(e))
	for _, ev := range evs {
		fmt.Fprintf(&b, "- %s\n", ev.Text)
	}
	fmt.Fprintf(&b, "Run `gh pr view %s --comments` for details.", e.URL)
	return b.String()
}

var summary = map[string]string{
	events.ChangesRequested: "changes requested",
	events.Comment:          "new comment",
	events.CIFailing:        "CI failing",
	events.Conflict:         "merge conflict",
	events.Merged:           "merged",
	events.Closed:           "closed",
}

func ParentFYI(e registry.Entry, ownerAddr string, evs []events.Event) string {
	var parts []string
	for _, ev := range evs {
		parts = append(parts, summary[ev.Kind])
	}
	return fmt.Sprintf("[herdr-pr] FYI, no action needed: child %s is handling %s (%s).", ownerAddr, ref(e), strings.Join(parts, ", "))
}

func FinalNote(e registry.Entry, state string) string {
	return fmt.Sprintf("[herdr-pr] %s was %s. Watch ended.", ref(e), state)
}

func WorktreeNote(cwd string) string {
	return fmt.Sprintf("[herdr-pr] FYI: worktree %s has no open PRs left and can be removed.", cwd)
}

type Liveness int

const (
	Live Liveness = iota
	Busy
	Gone
)

// A shell in the foreground means the agent exited back to its prompt.
var shells = map[string]bool{"zsh": true, "bash": true, "sh": true, "fish": true, "dash": true}

// Check classifies the owner terminal. A reused address (CreatedMs differs) is
// Gone. A live terminal with no status yet is Busy so a starting agent is not revived twice.
func Check(c hc.Client, addr string, createdMs int64) (Liveness, hc.Terminal, error) {
	t, err := c.Get(addr)
	if errors.Is(err, hc.ErrNotFound) {
		return Gone, hc.Terminal{}, nil
	}
	if err != nil {
		return Gone, hc.Terminal{}, err
	}
	switch {
	case t.State != "live", t.CreatedMs != createdMs:
		return Gone, t, nil
	case t.Foreground != nil && shells[t.Foreground.Name]:
		return Gone, t, nil
	case t.Status == nil, t.Status.State == "working":
		return Busy, t, nil
	}
	return Live, t, nil
}

// Revive resumes the owner's session in a new background terminal in the same cwd.
func Revive(c hc.Client, e registry.Entry, parentLive bool) (hc.Terminal, error) {
	o := e.Owner
	if _, err := os.Stat(o.Cwd); err != nil {
		return hc.Terminal{}, fmt.Errorf("cwd missing: %s", o.Cwd)
	}
	argv := []string{"claude", "--resume", o.SessionID}
	if o.Kind == "pi" {
		argv = []string{"pi", "--session", o.SessionFile}
	}
	name := fmt.Sprintf("pr-%s-%d", e.Repo[strings.LastIndex(e.Repo, "/")+1:], e.Number)
	if len(name) > 32 {
		name = name[:32]
	}
	spec := hc.RunSpec{
		Name: name, Cwd: o.Cwd, Argv: argv,
		Labels: map[string]string{"herdr.space": o.Space, "herdr.tab": o.Tab, "herdr-pr.session": o.SessionID},
	}
	if parentLive {
		spec.Parent = o.Parent
	} else {
		spec.NoParent = true
	}
	return c.Run(spec)
}
