package owner

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/hc"
)

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

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func term(addr string, created int64, fgName string, fgPid int, parent string, labels map[string]string) hc.Terminal {
	t := hc.Terminal{Address: addr, CreatedMs: created, State: "live", Labels: labels}
	if parent != "" {
		t.Parent = &parent
	}
	t.Foreground = &struct {
		Name string   `json:"name"`
		Pid  int      `json:"pid"`
		Argv []string `json:"argv"`
		Cwd  string   `json:"cwd"`
	}{Name: fgName, Pid: fgPid}
	return t
}

// procTree fakes the ancestry of this test process: self -> 50 (sh) -> 40 (pi) -> 1.
func procTree(pid int) (int, string, error) {
	switch pid {
	case os.Getpid():
		return 50, "herdr-pr", nil
	case 50:
		return 40, "sh", nil
	case 40:
		return 1, "pi", nil
	}
	return 0, "", errors.New("no such process")
}

var labels = map[string]string{"herdr.space": "sp1", "herdr.tab": "tb1"}

func TestResolvePiTopLevel(t *testing.T) {
	c := fakeHC{
		"t1": term("t1", 111, "pi", 40, "t0", labels),
		"t0": term("t0", 100, "pi", 9, "", nil),
	}
	env := envOf(map[string]string{"HC_TERMINAL_ID": "t1", "PI_SESSION_ID": "s1", "PI_SESSION_FILE": "/f.jsonl"})
	o, err := Resolve(env, c, t.TempDir(), procTree)
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if o.Kind != "pi" || o.SessionID != "s1" || o.SessionFile != "/f.jsonl" {
		t.Errorf("kind/session/file = %q/%q/%q", o.Kind, o.SessionID, o.SessionFile)
	}
	if o.Address != "t1" || o.CreatedMs != 111 || o.Parent != "t0" || o.ParentCreatedMs != 100 {
		t.Errorf("address/created/parent/parentCreated = %q/%d/%q/%d", o.Address, o.CreatedMs, o.Parent, o.ParentCreatedMs)
	}
	if o.Cwd != wd || o.Space != "sp1" || o.Tab != "tb1" {
		t.Errorf("cwd/space/tab = %q/%q/%q", o.Cwd, o.Space, o.Tab)
	}
}

func TestResolvePiInProcessChild(t *testing.T) {
	env := envOf(map[string]string{"HC_TERMINAL_ID": "t1", "PI_SESSION_ID": "child", "PI_SESSION_FILE": "/c.jsonl"})
	for name, c := range map[string]fakeHC{
		// The terminal's foreground pi is not the pi that started herdr-pr.
		"other pi process": {"t1": term("t1", 111, "pi", 30, "t0", labels)},
		// The terminal was labelled with a different top-level session.
		"label differs": {"t1": term("t1", 111, "pi", 40, "t0", map[string]string{"herdr-pr.session": "top"})},
	} {
		t.Run(name, func(t *testing.T) {
			o, err := Resolve(env, c, t.TempDir(), procTree)
			if err != nil {
				t.Fatal(err)
			}
			if o.Address != "" || o.CreatedMs != 0 || o.Parent != "t1" || o.ParentCreatedMs != 111 {
				t.Errorf("address/created/parent/parentCreated = %q/%d/%q/%d", o.Address, o.CreatedMs, o.Parent, o.ParentCreatedMs)
			}
			if o.Kind != "pi" || o.SessionID != "child" || o.SessionFile != "/c.jsonl" {
				t.Errorf("kind/session/file = %q/%q/%q", o.Kind, o.SessionID, o.SessionFile)
			}
		})
	}
}

func TestResolveClaudeLabel(t *testing.T) {
	for _, fg := range []string{"claude", "node"} {
		c := fakeHC{"t7": term("t7", 777, fg, 70, "t3", map[string]string{"herdr-pr.session": "uuid-1"})}
		o, err := Resolve(envOf(map[string]string{"HC_TERMINAL_ID": "t7"}), c, t.TempDir(), procTree)
		if err != nil {
			t.Fatal(err)
		}
		if o.Kind != "claude" || o.SessionID != "uuid-1" || o.Address != "t7" || o.CreatedMs != 777 {
			t.Errorf("fg %s: kind/session/address/created = %q/%q/%q/%d", fg, o.Kind, o.SessionID, o.Address, o.CreatedMs)
		}
		// A parent that no longer exists keeps its address with no created time.
		if o.Parent != "t3" || o.ParentCreatedMs != 0 {
			t.Errorf("fg %s: parent/parentCreated = %q/%d", fg, o.Parent, o.ParentCreatedMs)
		}
	}
}

func TestResolveClaudeSessionsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "70.json"), []byte(`{"pid":70,"sessionId":"uuid-2","cwd":"/w"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := fakeHC{"t7": term("t7", 777, "claude", 70, "", nil)}
	o, err := Resolve(envOf(map[string]string{"HC_TERMINAL_ID": "t7"}), c, dir, procTree)
	if err != nil {
		t.Fatal(err)
	}
	if o.Kind != "claude" || o.SessionID != "uuid-2" || o.Address != "t7" || o.Parent != "" {
		t.Errorf("kind/session/address/parent = %q/%q/%q/%q", o.Kind, o.SessionID, o.Address, o.Parent)
	}
}

func TestResolveRemoteRefused(t *testing.T) {
	_, err := Resolve(envOf(map[string]string{"HC_TERMINAL_ID": "box/t1", "PI_SESSION_ID": "s1"}), fakeHC{}, t.TempDir(), procTree)
	if err == nil || !strings.Contains(err.Error(), "remote owners are not supported") {
		t.Fatalf("err = %v", err)
	}
	_, err = Resolve(envOf(nil), fakeHC{}, t.TempDir(), procTree)
	if err == nil || !strings.Contains(err.Error(), "not inside a Herdr terminal") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadProcSelf(t *testing.T) {
	ppid, name, err := readProc(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if ppid != os.Getppid() || name == "" {
		t.Errorf("ppid/name = %d/%q, want %d/non-empty", ppid, name, os.Getppid())
	}
}

func TestResolveNoSession(t *testing.T) {
	c := fakeHC{"t7": term("t7", 777, "claude", 70, "", nil)}
	_, err := Resolve(envOf(map[string]string{"HC_TERMINAL_ID": "t7"}), c, t.TempDir(), procTree)
	if err == nil || !strings.Contains(err.Error(), "no session id") {
		t.Fatalf("err = %v", err)
	}
}
