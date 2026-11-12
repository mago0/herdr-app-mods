package hc

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const getFixture = `{"address":"t1","argv":["/usr/bin/zsh"],"cols":221,"created_ms":1793642726749,"cwd":"/home/mattw","die_with_parent":false,"exit_code":null,"finished_ms":null,"foreground":{"argv":["/nix/store/x-pi-1.4.0/libexec/pi/pi"],"cwd":"/home/mattw/Projects/flosports","name":"pi","pid":174616},"identity":"ghostty","keep":true,"labels":{"herdr.space":"spde9e","herdr.tab":"tb5baf","where":"flosports / 1"},"name":null,"notify":true,"parent":null,"pid":173940,"reason":null,"reported":true,"rm":false,"rows":56,"screen":"exact","signal":null,"state":"live","status":{"app":"pi","state":"working"},"status_ms":1793643594911,"template":null,"title":"π - flosports","turn":6}`

const notFoundFixture = `{"error":{"hint":"see ` + "`hc ls --all`" + `","kind":"not_found","message":"no terminal \"t999\""}}`

// fakeHC installs a fake hc on a temp dir that logs its argv (one line) and
// prints stdout, then exits with code. It returns the client and the argv log path.
func fakeHC(t *testing.T, stdout string, code int) (Client, string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "argv.log")
	outPath := filepath.Join(dir, "stdout.json")
	if err := os.WriteFile(outPath, []byte(stdout), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho \"$*\" >> '" + logPath + "'\ncat '" + outPath + "'\nexit " + string(rune('0'+code)) + "\n"
	bin := filepath.Join(dir, "hc")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return New(bin), logPath
}

func readLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func TestGetParses(t *testing.T) {
	c, logPath := fakeHC(t, getFixture, 0)
	got, err := c.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Address != "t1" || got.CreatedMs != 1793642726749 {
		t.Errorf("address/created_ms = %q/%d", got.Address, got.CreatedMs)
	}
	if got.Foreground == nil || got.Foreground.Name != "pi" || got.Foreground.Pid != 174616 {
		t.Errorf("foreground = %+v", got.Foreground)
	}
	if got.Status == nil || got.Status.State != "working" || got.Status.App != "pi" {
		t.Errorf("status = %+v", got.Status)
	}
	if !slices.Equal(got.Argv, []string{"/usr/bin/zsh"}) {
		t.Errorf("argv = %v", got.Argv)
	}
	if got.State != "live" || got.Parent != nil || got.Labels["herdr.space"] != "spde9e" {
		t.Errorf("state/parent/labels = %q %v %v", got.State, got.Parent, got.Labels)
	}
	if want := "get t1 --json"; readLog(t, logPath) != want {
		t.Errorf("argv = %q, want %q", readLog(t, logPath), want)
	}
}

const lsFixture = `{"hidden_finished":22,"terminals":[{"address":"t1z","agent":"pi","cwd":"/home/mattw/Projects/flosports","state":"live","status":"done","turn":682,"where":"SRE Review Monitor / 1"},{"address":"t11y","agent":"pi","cwd":"/w","state":"live","status":"working","turn":32,"where":"w / 1"}]}`

func TestListParses(t *testing.T) {
	c, logPath := fakeHC(t, lsFixture, 0)
	got, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"t1z", "t11y"}; !slices.Equal(got, want) {
		t.Errorf("addresses = %v, want %v", got, want)
	}
	if want := "ls --json"; readLog(t, logPath) != want {
		t.Errorf("argv = %q, want %q", readLog(t, logPath), want)
	}
}

func TestGetNotFound(t *testing.T) {
	c, _ := fakeHC(t, notFoundFixture, 3)
	_, err := c.Get("t999")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetOtherFailureIsNotNotFound(t *testing.T) {
	c, _ := fakeHC(t, "boom", 1)
	_, err := c.Get("t1")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want non-NotFound error", err)
	}
}

func TestSendArgv(t *testing.T) {
	c, logPath := fakeHC(t, "", 0)
	if err := c.Send("t1", "[herdr-pr] hello"); err != nil {
		t.Fatal(err)
	}
	if want := "send t1 [herdr-pr] hello"; readLog(t, logPath) != want {
		t.Errorf("argv = %q, want %q", readLog(t, logPath), want)
	}
}

func TestRunArgv(t *testing.T) {
	c, logPath := fakeHC(t, getFixture, 0)
	got, err := c.Run(RunSpec{
		Name:   "pr-iam-412",
		Cwd:    "/w",
		Parent: "t1",
		Labels: map[string]string{"herdr.space": "s"},
		Argv:   []string{"pi", "--session", "/f.jsonl"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "run --bg --keep --outlive-parent --json --name pr-iam-412 --cwd /w --parent t1 --label herdr.space=s -- pi --session /f.jsonl"
	if readLog(t, logPath) != want {
		t.Errorf("argv = %q, want %q", readLog(t, logPath), want)
	}
	if got.Address != "t1" {
		t.Errorf("parsed address = %q", got.Address)
	}
}

func TestRunNoParentSortedLabels(t *testing.T) {
	c, logPath := fakeHC(t, getFixture, 0)
	_, err := c.Run(RunSpec{
		NoParent: true,
		Labels:   map[string]string{"b": "2", "a": "1"},
		Argv:     []string{"pi"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "run --bg --keep --outlive-parent --json --no-parent --label a=1 --label b=2 -- pi"
	if readLog(t, logPath) != want {
		t.Errorf("argv = %q, want %q", readLog(t, logPath), want)
	}
}
