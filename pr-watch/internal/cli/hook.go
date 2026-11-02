package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/created"
)

// hookPayload accepts the generic {command, output} shape and Claude Code's PostToolUse input.
type hookPayload struct {
	Command   string `json:"command"`
	Output    string `json:"output"`
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
	ToolResponse struct {
		Stdout string `json:"stdout"`
		Stderr string `json:"stderr"`
	} `json:"tool_response"`
}

func (p hookPayload) commandAndOutput() (string, string) {
	if p.Command != "" {
		return p.Command, p.Output
	}
	return p.ToolInput.Command, p.ToolResponse.Stdout + "\n" + p.ToolResponse.Stderr
}

// hook links and watches each PR a finished shell command created, owned by the calling agent.
func (c *cmd) hook() error {
	if c.d.Env("HC_TERMINAL_ID") == "" {
		return nil
	}
	if c.d.Stdin == nil {
		return errors.New("no stdin")
	}
	raw, err := io.ReadAll(c.d.Stdin)
	if err != nil {
		return err
	}
	var p hookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("payload: %w", err)
	}
	var errs []error
	for _, ref := range created.By(p.commandAndOutput()) {
		if _, err := c.linkRef(ref, true, false); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// logHook appends err to hook.log beside the registry; the hook has no other place to report.
func (c *cmd) logHook(err error) {
	path := filepath.Join(filepath.Dir(c.d.StatePath), "hook.log")
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	f, ferr := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if ferr != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %v\n", c.d.Now().Format(time.RFC3339), err)
}
