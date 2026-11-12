// Package deliver words the messages sent to agents and checks whether an owner terminal can take them.
package deliver

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

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

// FinalOwnerMessage ends the watch for the owner: the state line, then any other events of the
// same pass, then the worktree line when the owner is the one told about it.
func FinalOwnerMessage(e registry.Entry, state string, others []events.Event, worktree string) string {
	lines := []string{fmt.Sprintf("[herdr-pr] %s was %s. Watch ended.", ref(e), state)}
	for _, ev := range others {
		lines = append(lines, "- "+ev.Text)
	}
	if worktree != "" {
		lines = append(lines, worktreeLine(worktree))
	}
	return strings.Join(lines, "\n")
}

// FinalParentMessage tells the parent the watch ended; ownerAddr is empty for an in-process child.
func FinalParentMessage(e registry.Entry, state, ownerAddr, worktree string) string {
	child := ownerAddr
	if child == "" {
		child = "background child"
	}
	msg := fmt.Sprintf("[herdr-pr] FYI, no action needed: %s (child %s) was %s. Watch ended.", ref(e), child, state)
	if worktree != "" {
		msg += "\n" + worktreeLine(worktree)
	}
	return msg
}

func worktreeLine(path string) string {
	return fmt.Sprintf("FYI: worktree %s has no open PRs left and can be removed.", path)
}

type Liveness int

const (
	Live Liveness = iota
	Busy
	Gone
)

// A shell in the foreground, with no agent status, means the agent exited back to its prompt.
var shells = map[string]bool{"zsh": true, "bash": true, "sh": true, "fish": true, "dash": true}

// settle is how long a terminal whose agent reports no status (Claude Code) must exist before it takes messages.
const settle = 30 * time.Second

// Check classifies the owner terminal. A reused address (CreatedMs differs) or an exited agent is Gone.
// A live terminal with no status is Busy so a starting agent is not revived twice, except
// a Claude agent that has settled: Claude Code never reports status and queues typed input.
// Any other program with no status (an editor, pager or build run after the agent exited)
// stays Busy so a message is never typed into it. blocked is Busy because hc send refuses a blocked program.
func Check(c hc.Client, addr string, createdMs int64, now time.Time) (Liveness, hc.Terminal, error) {
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
	case exited(t):
		return Gone, t, nil
	case t.Status == nil:
		if isClaude(t) && now.Sub(time.UnixMilli(t.CreatedMs)) >= settle {
			return Live, t, nil
		}
		return Busy, t, nil
	case t.Status.State == "working", t.Status.State == "blocked":
		return Busy, t, nil
	}
	return Live, t, nil
}

// exited reports an agent that quit back to its shell prompt. A status the agent reports outranks
// the foreground: a busy agent's tool call can put a shell, or a wrapper like atuin, there.
func exited(t hc.Terminal) bool {
	reportsStatus := t.Status != nil && t.Status.App != ""
	return !reportsStatus && t.Foreground != nil && shells[t.Foreground.Name]
}

// SessionOpenIn returns the address of a live terminal whose agent still runs o's session, or "".
// A terminal runs it when it carries the session label, or its launch or foreground argv resumes it.
// A top-level agent started without --session or --resume (`pi "task"`) has no such mark in hc,
// so it is not found here.
func SessionOpenIn(c hc.Client, o registry.Owner) (string, error) {
	if o.SessionID == "" {
		return "", nil
	}
	addrs, err := c.List()
	if err != nil {
		return "", err
	}
	for _, addr := range addrs {
		t, err := c.Get(addr)
		if errors.Is(err, hc.ErrNotFound) {
			continue
		}
		if err != nil {
			return "", err
		}
		if t.State != "live" || exited(t) {
			continue
		}
		if t.Labels[sessionLabel] == o.SessionID || resumes(t.Argv, o) || (t.Foreground != nil && resumes(t.Foreground.Argv, o)) {
			return addr, nil
		}
	}
	return "", nil
}

// sessionLabel marks a terminal with the session it runs; Revive sets it.
const sessionLabel = "herdr-pr.session"

// resumes reports argv naming o's session after --session (pi) or --resume (claude).
func resumes(argv []string, o registry.Owner) bool {
	isSession := func(v string) bool { return v == o.SessionID || (o.SessionFile != "" && v == o.SessionFile) }
	for i, a := range argv {
		if (a == "--session" || a == "--resume") && i+1 < len(argv) && isSession(argv[i+1]) {
			return true
		}
		if v, ok := strings.CutPrefix(a, "--session="); ok && isSession(v) {
			return true
		}
		if v, ok := strings.CutPrefix(a, "--resume="); ok && isSession(v) {
			return true
		}
	}
	return false
}

// isClaude reports a Claude Code foreground: the claude binary, or node running Claude's script.
func isClaude(t hc.Terminal) bool {
	f := t.Foreground
	if f == nil {
		return false
	}
	if f.Name == "claude" {
		return true
	}
	return f.Name == "node" && slices.ContainsFunc(f.Argv, func(a string) bool { return strings.Contains(a, "claude") })
}

// Revive resumes the owner's session in a new background terminal in the same cwd.
// `hc run` can return no created_ms, so it is read back; without it the next pass would take
// the new terminal for a reused address and count the revive as failed.
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
		Labels: map[string]string{"herdr.space": o.Space, "herdr.tab": o.Tab, sessionLabel: o.SessionID},
	}
	if parentLive {
		spec.Parent = o.Parent
	} else {
		spec.NoParent = true
	}
	t, err := c.Run(spec)
	if err != nil || t.CreatedMs != 0 || t.Address == "" {
		return t, err
	}
	// The terminal runs already, so a failed read is not a failed revive; SessionOpenIn
	// still finds it by its label, so it is not revived a second time.
	if got, err := c.Get(t.Address); err == nil {
		t.CreatedMs = got.CreatedMs
	}
	return t, nil
}
