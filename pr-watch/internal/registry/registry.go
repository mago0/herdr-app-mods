// Package registry stores which agent owns which PR, in a JSON file shared by several processes.
package registry

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ErrOwned means another session already owns the PR.
var ErrOwned = errors.New("PR is owned by another session")

type Owner struct {
	Address         string
	CreatedMs       int64
	Kind            string // "pi" or "claude"
	SessionID       string
	SessionFile     string
	Cwd             string // where the agent runs; revival resumes here
	Worktree        string // git worktree root herdr-pr was linked from; empty from home or outside git
	Space           string
	Tab             string
	Parent          string
	ParentCreatedMs int64
}

type Display struct {
	Title  string
	State  string // OPEN, MERGED or CLOSED
	Draft  bool
	CI     string // SUCCESS, FAILURE, PENDING or ""
	Review string // reviewDecision
}

type Last struct {
	State         string
	SeenIDs       []string
	CIFailingSHA  string
	Conflicting   bool
	WorktreeNoted bool
}

type Entry struct {
	URL              string
	Repo             string
	Number           int
	Owner            Owner
	Watched          bool
	LinkedAt         time.Time
	Display          Display
	Last             Last
	RevivedAs        string
	RevivedCreatedMs int64
	RevivedAtMs      int64  // when the last revive started; a revived terminal gone soon after died at startup
	ReviveFailed     string // why reviving failed; no revive is tried while set, `watch` re-arms
	ReviveFailedTold bool   // the parent has been told about ReviveFailed
	Primed           bool
}

type Registry struct {
	Version int
	Entries []Entry
}

// Path is registry.json in the state directory: HERDR_PR_STATE if set, else
// ~/.local/state/herdr-pr. The Herdr mod watches that directory.
func Path() (string, error) {
	dir := os.Getenv("HERDR_PR_STATE")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("registry: no state directory: %w", err)
		}
		dir = filepath.Join(home, ".local", "state", "herdr-pr")
	}
	return filepath.Join(dir, "registry.json"), nil
}

// Load reads the registry without locking. A missing file is an empty registry.
func Load(path string) (*Registry, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Registry{Version: 1}, nil
	}
	if err != nil {
		return nil, err
	}
	r := &Registry{}
	if err := json.Unmarshal(b, r); err != nil {
		return nil, err
	}
	if r.Version == 0 {
		r.Version = 1
	}
	// An older binary must not rewrite a format it does not know.
	if r.Version > 1 {
		return nil, fmt.Errorf("registry: %s has version %d; this herdr-pr reads version 1", path, r.Version)
	}
	return r, nil
}

// Update runs fn on the registry under an exclusive lock and saves the result atomically.
// When fn fails the file stays as it was.
func Update(path string, fn func(*Registry) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "registry.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	r, err := Load(path)
	if err != nil {
		return err
	}
	if err := fn(r); err != nil {
		return err
	}
	return save(path, r)
}

func save(path string, r *Registry) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "registry-*.tmp")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Find returns the entry for url, or nil.
func (r *Registry) Find(url string) *Entry {
	for i := range r.Entries {
		if r.Entries[i].URL == url {
			return &r.Entries[i]
		}
	}
	return nil
}

// Add links e. The same session re-linking keeps the stored entry; another session needs force.
func (r *Registry) Add(e Entry, force bool) error {
	cur := r.Find(e.URL)
	if cur == nil {
		r.Entries = append(r.Entries, e)
		return nil
	}
	if cur.Owner.SessionID == e.Owner.SessionID {
		return nil
	}
	if !force {
		return ErrOwned
	}
	*cur = e
	return nil
}
