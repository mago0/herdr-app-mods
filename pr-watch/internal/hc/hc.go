// Package hc wraps the herdr-core `hc` CLI.
package hc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
)

// ErrNotFound means the terminal does not exist.
var ErrNotFound = errors.New("hc: terminal not found")

// Terminal mirrors the fields of `hc get <t> --json` that herdr-pr uses.
type Terminal struct {
	Address   string            `json:"address"`
	CreatedMs int64             `json:"created_ms"`
	Cwd       string            `json:"cwd"`
	Argv      []string          `json:"argv"`
	State     string            `json:"state"`
	Parent    *string           `json:"parent"`
	Labels    map[string]string `json:"labels"`
	Status    *struct {
		App   string `json:"app"`
		State string `json:"state"`
	} `json:"status"`
	Foreground *struct {
		Name string   `json:"name"`
		Pid  int      `json:"pid"`
		Argv []string `json:"argv"`
		Cwd  string   `json:"cwd"`
	} `json:"foreground"`
}

// RunSpec describes a background terminal to start.
type RunSpec struct {
	Name, Cwd, Parent string
	NoParent          bool
	Labels            map[string]string
	Argv              []string
}

// Client is the subset of hc that herdr-pr needs.
type Client interface {
	Get(addr string) (Terminal, error)
	Send(addr, text string) error
	Run(RunSpec) (Terminal, error)
	List() ([]string, error)
}

type execClient struct{ bin string }

// New returns a Client that shells out to bin (default "hc").
func New(bin string) Client {
	if bin == "" {
		bin = "hc"
	}
	return execClient{bin: bin}
}

// hc exits 3 with a not_found error object when the terminal is missing.
const notFoundExit = 3

func (c execClient) exec(args ...string) ([]byte, error) {
	cmd := exec.Command(c.bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == notFoundExit {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("hc %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()+stdout.String()))
	}
	return stdout.Bytes(), nil
}

func (c execClient) Get(addr string) (Terminal, error) {
	out, err := c.exec("get", addr, "--json")
	if err != nil {
		return Terminal{}, err
	}
	return parse(out)
}

// List returns the addresses of live terminals. `hc ls` gives only a summary; Get has the details.
func (c execClient) List() ([]string, error) {
	out, err := c.exec("ls", "--json")
	if err != nil {
		return nil, err
	}
	var l struct {
		Terminals []struct {
			Address string `json:"address"`
		} `json:"terminals"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, fmt.Errorf("hc: parse ls output: %w", err)
	}
	addrs := make([]string, 0, len(l.Terminals))
	for _, t := range l.Terminals {
		addrs = append(addrs, t.Address)
	}
	return addrs, nil
}

func (c execClient) Send(addr, text string) error {
	_, err := c.exec("send", addr, text)
	return err
}

func (c execClient) Run(s RunSpec) (Terminal, error) {
	args := []string{"run", "--bg", "--keep", "--outlive-parent", "--json"}
	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}
	if s.Cwd != "" {
		args = append(args, "--cwd", s.Cwd)
	}
	if s.NoParent {
		args = append(args, "--no-parent")
	} else if s.Parent != "" {
		args = append(args, "--parent", s.Parent)
	}
	keys := make([]string, 0, len(s.Labels))
	for k := range s.Labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--label", k+"="+s.Labels[k])
	}
	args = append(args, "--")
	args = append(args, s.Argv...)
	out, err := c.exec(args...)
	if err != nil {
		return Terminal{}, err
	}
	return parse(out)
}

func parse(out []byte) (Terminal, error) {
	var t Terminal
	if err := json.Unmarshal(out, &t); err != nil {
		return Terminal{}, fmt.Errorf("hc: parse output: %w", err)
	}
	return t, nil
}
