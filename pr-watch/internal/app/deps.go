// Package app holds the dependencies shared by the CLI and the poller.
package app

import (
	"time"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/github"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/hc"
)

// Deps are the outside world as herdr-pr sees it; tests replace each one.
type Deps struct {
	Env            func(string) string
	HC             hc.Client
	GH             github.Fetcher
	StatePath      string // registry file
	ClaudeSessions string // directory of Claude Code's <pid>.json session files
	Now            func() time.Time
}
