// Command herdr-pr records which Herdr agent owns which GitHub PR and wakes it when the PR needs action.
package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/app"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/cli"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/github"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/hc"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/registry"
)

func main() {
	home, _ := os.UserHomeDir()
	d := app.Deps{
		Env:            os.Getenv,
		HC:             hc.New("hc"),
		GH:             github.New(),
		StatePath:      registry.Path(),
		ClaudeSessions: filepath.Join(home, ".claude", "sessions"),
		Now:            time.Now,
	}
	os.Exit(cli.Run(os.Args[1:], d, os.Stdout, os.Stderr))
}
