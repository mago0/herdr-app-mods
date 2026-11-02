// Package owner works out which agent session is running herdr-pr, and where it sits in Herdr.
package owner

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/hc"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/registry"
)

// ProcFunc returns the parent pid and program name of pid.
type ProcFunc func(pid int) (ppid int, name string, err error)

// sessionLabel is set by the launch recipe on terminals whose session id cannot be read otherwise.
const sessionLabel = "herdr-pr.session"

// Resolve builds the owner for the calling process. proc reads the process tree; nil means /proc.
func Resolve(env func(string) string, c hc.Client, claudeSessionsDir string, proc ProcFunc) (registry.Owner, error) {
	if proc == nil {
		proc = readProc
	}
	terminal := env("HC_TERMINAL_ID")
	if terminal == "" {
		return registry.Owner{}, errors.New("not inside a Herdr terminal")
	}
	if strings.Contains(terminal, "/") {
		return registry.Owner{}, fmt.Errorf("remote owners are not supported: %s", terminal)
	}
	t, err := c.Get(terminal)
	if err != nil {
		return registry.Owner{}, err
	}

	o := registry.Owner{Space: t.Labels["herdr.space"], Tab: t.Labels["herdr.tab"]}
	label := t.Labels[sessionLabel]
	switch {
	case env("PI_SESSION_ID") != "":
		o.Kind, o.SessionID, o.SessionFile = "pi", env("PI_SESSION_ID"), env("PI_SESSION_FILE")
	case label != "":
		o.Kind, o.SessionID = "claude", label
		if t.Foreground != nil && t.Foreground.Name == "pi" {
			o.Kind = "pi"
		}
	case t.Foreground != nil:
		o.Kind = "claude"
		o.SessionID = claudeSession(claudeSessionsDir, t.Foreground.Pid)
	}
	if o.SessionID == "" {
		return registry.Owner{}, fmt.Errorf("no session id for terminal %s", terminal)
	}

	if o.Kind == "pi" && env("PI_SESSION_ID") != "" && inProcessChild(t, label, o.SessionID, proc) {
		// Herdr cannot see this session, so it is owned through the terminal it runs in.
		o.Parent, o.ParentCreatedMs = terminal, t.CreatedMs
	} else {
		o.Address, o.CreatedMs = terminal, t.CreatedMs
		if t.Parent != nil && *t.Parent != "" {
			o.Parent = *t.Parent
			p, err := c.Get(o.Parent)
			switch {
			case err == nil:
				o.ParentCreatedMs = p.CreatedMs
			case !errors.Is(err, hc.ErrNotFound):
				return registry.Owner{}, err
			}
		}
	}

	// The agent process's folder, not herdr-pr's: the mod links from the app's folder and agents cd around.
	o.Cwd = t.Cwd
	if t.Foreground != nil && t.Foreground.Cwd != "" {
		o.Cwd = t.Foreground.Cwd
	}
	wd, err := os.Getwd()
	if err != nil {
		return registry.Owner{}, err
	}
	o.Worktree = worktree(wd)
	return o, nil
}

// worktree is the git worktree root holding dir. Home is never one, so a link from
// the mod (which runs in the app's folder) or a dotfiles repo is not offered for removal.
func worktree(dir string) string {
	home, _ := os.UserHomeDir()
	if dir == home {
		return ""
	}
	if root := gitToplevel(dir); root != home {
		return root
	}
	return ""
}

// gitToplevel is `git rev-parse --show-toplevel` in dir, or "" outside git. Tests replace it.
var gitToplevel = func(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// inProcessChild reports whether the pi session calling herdr-pr is not the terminal's own pi.
// pi does not export its session id to its own environment, so the pid is compared instead.
func inProcessChild(t hc.Terminal, label, session string, proc ProcFunc) bool {
	if label != "" && label != session {
		return true
	}
	if t.Foreground == nil {
		return false
	}
	pid, ok := nearestPi(proc)
	return ok && pid != t.Foreground.Pid
}

// nearestPi walks up from herdr-pr's parent to the first process named pi.
func nearestPi(proc ProcFunc) (int, bool) {
	pid, _, err := proc(os.Getpid())
	// The depth cap guards against a cycle from a pid reused mid-walk.
	for depth := 0; err == nil && pid > 1 && depth < 64; depth++ {
		var ppid int
		var name string
		if ppid, name, err = proc(pid); err == nil && name == "pi" {
			return pid, true
		}
		pid = ppid
	}
	return 0, false
}

// readProc reads /proc. The name is "pi" when either comm or argv[0]'s base name is pi,
// since comm is truncated and some launchers rename argv[0].
func readProc(pid int) (int, string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, "", err
	}
	// Format is "pid (comm) state ppid ..."; comm may itself contain spaces or parens.
	s := string(b)
	open, end := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
	if open < 0 || end < open {
		return 0, "", fmt.Errorf("bad /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[end+1:])
	if len(fields) < 2 {
		return 0, "", fmt.Errorf("bad /proc/%d/stat", pid)
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, "", err
	}
	name := s[open+1 : end]
	if cmd, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
		argv0, _, _ := strings.Cut(string(cmd), "\x00")
		if filepath.Base(argv0) == "pi" {
			name = "pi"
		}
	}
	return ppid, name, nil
}

// claudeSession reads the session id Claude Code writes for an interactive process.
func claudeSession(dir string, pid int) string {
	b, err := os.ReadFile(filepath.Join(dir, strconv.Itoa(pid)+".json"))
	if err != nil {
		return ""
	}
	var f struct {
		SessionID string `json:"sessionId"`
	}
	json.Unmarshal(b, &f)
	return f.SessionID
}
