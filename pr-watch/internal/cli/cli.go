// Package cli implements the herdr-pr subcommands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/app"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/events"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/owner"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/prurl"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/registry"
)

const usage = `usage: herdr-pr <command>
  link <url> [--watch] [--force]   record the calling agent as the PR's owner
  unlink <url>                      forget the PR
  watch <url> | unwatch <url>       start or stop waking the owner on updates
  ls [--agent <t>] [--with-children] [--json]
  poll                              one poll pass (run by the timer)
`

// errUsage marks bad arguments; Run exits 2 for it.
var errUsage = errors.New("usage")

// Run executes one subcommand and returns the process exit code.
func Run(args []string, d app.Deps, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	c := &cmd{d: d, out: stdout, errw: stderr}
	var err error
	switch args[0] {
	case "link":
		err = c.link(args[1:])
	case "unlink":
		err = c.unlink(args[1:])
	case "watch":
		err = c.setWatched(args[1:], "watch", true)
	case "unwatch":
		err = c.setWatched(args[1:], "unwatch", false)
	case "ls":
		err = c.ls(args[1:])
	case "poll":
		fmt.Fprintln(stderr, "[herdr-pr] poll: not implemented")
		return 2
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "[herdr-pr] unknown command %q\n%s", args[0], usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintf(stderr, "[herdr-pr] %s %v\n", args[0], err)
		return 2
	default:
		fmt.Fprintf(stderr, "[herdr-pr] %s: %v\n", args[0], err)
		return 1
	}
}

type cmd struct {
	d         app.Deps
	out, errw io.Writer
}

// parse lets flags follow positional arguments, as in `link <url> --watch`.
func (c *cmd) parse(fs *flag.FlagSet, args []string, npos int) ([]string, error) {
	fs.SetOutput(c.errw)
	var pos []string
	for {
		if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
			return nil, err
		} else if err != nil {
			return nil, fmt.Errorf("%w: %v", errUsage, err)
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) != npos {
		return nil, fmt.Errorf("%w: want %d argument(s), got %d", errUsage, npos, len(pos))
	}
	return pos, nil
}

func (c *cmd) urlArg(name string, args []string) (prurl.Ref, error) {
	pos, err := c.parse(flag.NewFlagSet(name, flag.ContinueOnError), args, 1)
	if err != nil {
		return prurl.Ref{}, err
	}
	return prurl.Parse(pos[0])
}

func (c *cmd) link(args []string) error {
	fs := flag.NewFlagSet("link", flag.ContinueOnError)
	watch := fs.Bool("watch", false, "wake the owner on updates")
	force := fs.Bool("force", false, "take the PR from another owner")
	pos, err := c.parse(fs, args, 1)
	if err != nil {
		return err
	}
	ref, err := prurl.Parse(pos[0])
	if err != nil {
		return err
	}
	o, err := owner.Resolve(c.d.Env, c.d.HC, c.d.ClaudeSessions, nil)
	if err != nil {
		return err
	}
	// Priming from current state means only later changes can wake the owner.
	prs, err := c.d.GH.Fetch(context.Background(), []prurl.Ref{ref})
	if err != nil {
		return err
	}
	pr, ok := prs[ref.URL()]
	if !ok {
		return fmt.Errorf("%s not found on GitHub", ref.URL())
	}
	e := registry.Entry{
		URL: ref.URL(), Repo: ref.Repo(), Number: ref.Number,
		Owner: o, Watched: *watch, LinkedAt: c.d.Now(),
		Display: events.DisplayOf(pr), Last: events.Next(pr), Primed: true,
	}
	err = registry.Update(c.d.StatePath, func(r *registry.Registry) error {
		cur := r.Find(e.URL)
		if err := r.Add(e, *force); errors.Is(err, registry.ErrOwned) {
			return fmt.Errorf("%s#%d is already linked by %s; use --force to take it", ref.Repo(), ref.Number, describe(cur.Owner))
		} else if err != nil {
			return err
		}
		if *watch {
			r.Find(e.URL).Watched = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	note := ""
	if *watch {
		note = ", watching"
	}
	fmt.Fprintf(c.out, "[herdr-pr] linked %s#%d to %s%s\n", ref.Repo(), ref.Number, describe(o), note)
	return nil
}

func (c *cmd) unlink(args []string) error {
	ref, err := c.urlArg("unlink", args)
	if err != nil {
		return err
	}
	err = registry.Update(c.d.StatePath, func(r *registry.Registry) error {
		for i := range r.Entries {
			if r.Entries[i].URL == ref.URL() {
				r.Entries = append(r.Entries[:i], r.Entries[i+1:]...)
				return nil
			}
		}
		return notLinked(ref)
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(c.out, "[herdr-pr] unlinked %s#%d\n", ref.Repo(), ref.Number)
	return nil
}

func (c *cmd) setWatched(args []string, name string, on bool) error {
	ref, err := c.urlArg(name, args)
	if err != nil {
		return err
	}
	err = registry.Update(c.d.StatePath, func(r *registry.Registry) error {
		e := r.Find(ref.URL())
		if e == nil {
			return notLinked(ref)
		}
		e.Watched = on
		return nil
	})
	if err != nil {
		return err
	}
	verb := "watching"
	if !on {
		verb = "stopped watching"
	}
	fmt.Fprintf(c.out, "[herdr-pr] %s %s#%d\n", verb, ref.Repo(), ref.Number)
	return nil
}

func notLinked(ref prurl.Ref) error {
	return fmt.Errorf("%s#%d is not linked", ref.Repo(), ref.Number)
}

// describe names an owner for messages: its terminal, or its parent for an in-process child.
func describe(o registry.Owner) string {
	if o.Address != "" {
		return fmt.Sprintf("%s (%s session %s)", o.Address, o.Kind, o.SessionID)
	}
	return fmt.Sprintf("%s session %s inside %s", o.Kind, o.SessionID, o.Parent)
}

type entryView struct {
	URL          string `json:"url"`
	Repo         string `json:"repo"`
	Number       int    `json:"number"`
	Watched      bool   `json:"watched"`
	Title        string `json:"title"`
	State        string `json:"state"`
	Draft        bool   `json:"draft"`
	CI           string `json:"ci"`
	Review       string `json:"review"`
	OwnerAddress string `json:"owner_address"`
}

type ownerView struct {
	Address string `json:"address"`
	Session string `json:"session"`
	Kind    string `json:"kind"`
	Cwd     string `json:"cwd"`
}

type childGroup struct {
	Owner ownerView   `json:"owner"`
	PRs   []entryView `json:"prs"`
}

type lsView struct {
	Agent    string       `json:"agent"`
	Own      []entryView  `json:"own"`
	Children []childGroup `json:"children"`
}

// address is where the owner runs now: its revived terminal if it was brought back.
func address(e registry.Entry) string {
	if e.RevivedAs != "" {
		return e.RevivedAs
	}
	return e.Owner.Address
}

func viewOf(e registry.Entry) entryView {
	return entryView{
		URL: e.URL, Repo: e.Repo, Number: e.Number, Watched: e.Watched,
		Title: e.Display.Title, State: e.Display.State, Draft: e.Display.Draft,
		CI: e.Display.CI, Review: e.Display.Review, OwnerAddress: address(e),
	}
}

// build selects the agent's own PRs and, if asked, its direct children's grouped by session.
func build(r *registry.Registry, agent string, withChildren bool) lsView {
	v := lsView{Agent: agent, Own: []entryView{}, Children: []childGroup{}}
	group := map[string]int{}
	for _, e := range r.Entries {
		switch {
		case agent == "" || e.Owner.Address == agent || e.RevivedAs == agent:
			v.Own = append(v.Own, viewOf(e))
		case withChildren && e.Owner.Parent == agent:
			i, ok := group[e.Owner.SessionID]
			if !ok {
				i = len(v.Children)
				group[e.Owner.SessionID] = i
				v.Children = append(v.Children, childGroup{Owner: ownerView{
					Address: address(e), Session: e.Owner.SessionID, Kind: e.Owner.Kind, Cwd: e.Owner.Cwd,
				}})
			}
			v.Children[i].PRs = append(v.Children[i].PRs, viewOf(e))
		}
	}
	return v
}

func (c *cmd) ls(args []string) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	agent := fs.String("agent", "", "terminal address whose PRs to list (default: all)")
	withChildren := fs.Bool("with-children", false, "add the PRs of the agent's direct children")
	asJSON := fs.Bool("json", false, "print JSON for the Herdr mod")
	if _, err := c.parse(fs, args, 0); err != nil {
		return err
	}
	r, err := registry.Load(c.d.StatePath)
	if err != nil {
		return err
	}
	v := build(r, *agent, *withChildren)
	if *asJSON {
		return json.NewEncoder(c.out).Encode(v)
	}
	for _, e := range v.Own {
		fmt.Fprintln(c.out, line(e))
	}
	for _, g := range v.Children {
		who := g.Owner.Address
		if who == "" {
			who = g.Owner.Kind + " session " + g.Owner.Session
		}
		fmt.Fprintf(c.out, "child %s:\n", who)
		for _, e := range g.PRs {
			fmt.Fprintln(c.out, "  "+line(e))
		}
	}
	return nil
}

func line(e entryView) string {
	state := strings.ToLower(e.State)
	if e.Draft && e.State == "OPEN" {
		state = "draft"
	}
	f := []string{fmt.Sprintf("%s#%d", e.Repo, e.Number), state}
	if e.CI != "" {
		f = append(f, "ci:"+strings.ToLower(e.CI))
	}
	if e.Review != "" {
		f = append(f, "review:"+strings.ToLower(e.Review))
	}
	if e.Watched {
		f = append(f, "watched")
	}
	if e.Title != "" {
		f = append(f, e.Title)
	}
	return strings.Join(f, "  ")
}
