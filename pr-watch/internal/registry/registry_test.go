package registry

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func entry(url, session string) Entry {
	return Entry{URL: url, Owner: Owner{SessionID: session}}
}

func TestAddOwnership(t *testing.T) {
	r := &Registry{Version: 1}
	u := "https://github.com/a/b/pull/1"
	if err := r.Add(entry(u, "A"), false); err != nil {
		t.Fatal(err)
	}
	r.Find(u).Last.State = "OPEN"
	if err := r.Add(entry(u, "B"), false); !errors.Is(err, ErrOwned) {
		t.Fatalf("want ErrOwned, got %v", err)
	}
	if got := r.Find(u).Owner.SessionID; got != "A" {
		t.Fatalf("owner changed without force: %q", got)
	}
	if err := r.Add(entry(u, "B"), true); err != nil {
		t.Fatal(err)
	}
	if got := r.Find(u).Owner.SessionID; got != "B" {
		t.Fatalf("owner = %q, want B", got)
	}
	r.Find(u).Last.State = "OPEN"
	if err := r.Add(entry(u, "B"), false); err != nil {
		t.Fatal(err)
	}
	if got := r.Find(u).Last.State; got != "OPEN" {
		t.Fatalf("re-link dropped Last: %q", got)
	}
	if len(r.Entries) != 1 {
		t.Fatalf("entries = %d", len(r.Entries))
	}
}

func TestUpdateConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "registry.json")
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := Update(p, func(r *Registry) error {
				return r.Add(entry(fmt.Sprintf("https://github.com/a/b/pull/%d", i), "S"), false)
			})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	r, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Entries) != 20 {
		t.Fatalf("entries = %d, want 20", len(r.Entries))
	}
}

func TestPathUsesStateDir(t *testing.T) {
	t.Setenv("HERDR_PR_STATE", "/x")
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if p != "/x/registry.json" {
		t.Fatalf("Path() = %q, want /x/registry.json", p)
	}

	t.Setenv("HERDR_PR_STATE", "")
	t.Setenv("HOME", "/home/u")
	if p, err := Path(); err != nil || p != "/home/u/.local/state/herdr-pr/registry.json" {
		t.Fatalf("default Path() = %q, %v", p, err)
	}
}

func TestUpdateCreatesStateDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state", "herdr-pr")
	p := filepath.Join(dir, "registry.json")
	if err := Update(p, func(r *Registry) error {
		return r.Add(entry("https://github.com/a/b/pull/1", "S"), false)
	}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 700", perm)
	}
	for _, name := range []string{"registry.json", "registry.lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestUpdateAtomicOnError(t *testing.T) {
	p := filepath.Join(t.TempDir(), "registry.json")
	if err := Update(p, func(r *Registry) error {
		return r.Add(entry("https://github.com/a/b/pull/1", "S"), false)
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)
	boom := errors.New("boom")
	err := Update(p, func(r *Registry) error {
		r.Entries = nil
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("file changed on error")
	}
}
