// Package events decides which PR changes need an agent's attention.
package events

import (
	"fmt"
	"slices"
	"strings"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/github"
	"github.com/mago0/herdr-app-mods/pr-watch/internal/registry"
)

// Event kinds.
const (
	ChangesRequested = "changes_requested"
	Comment          = "comment"
	CIFailing        = "ci_failing"
	Conflict         = "conflict"
	Merged           = "merged"
	Closed           = "closed"
)

type Event struct{ Kind, Text string }

func isBot(a github.Actor) bool {
	return a.Bot || strings.HasSuffix(a.Login, "[bot]")
}

func sha7(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

// Classify compares the last seen state with the current PR. Only new items,
// a new failing head SHA, a new conflict or a state change produce events.
func Classify(prev registry.Last, cur github.PR, viewer string) []Event {
	var evs []Event
	for _, it := range cur.Items {
		if slices.Contains(prev.SeenIDs, it.ID) {
			continue
		}
		switch {
		case it.Kind == "review" && it.State == "CHANGES_REQUESTED":
			evs = append(evs, Event{ChangesRequested, fmt.Sprintf("review from %s: changes requested", it.Author.Login)})
		case strings.TrimSpace(it.Body) != "" && !isBot(it.Author) && it.Author.Login != viewer:
			evs = append(evs, Event{Comment, fmt.Sprintf("%s from %s", it.Kind, it.Author.Login)})
		}
	}
	if cur.CI == "FAILURE" && cur.HeadSHA != prev.CIFailingSHA {
		evs = append(evs, Event{CIFailing, fmt.Sprintf("CI failing on %s (%s)", sha7(cur.HeadSHA), strings.Join(cur.FailingChecks, ", "))})
	}
	if cur.Mergeable == "CONFLICTING" && !prev.Conflicting {
		evs = append(evs, Event{Conflict, "merge conflict with the base branch"})
	}
	if cur.State != prev.State {
		switch cur.State {
		case "MERGED":
			evs = append(evs, Event{Merged, "PR was merged"})
		case "CLOSED":
			evs = append(evs, Event{Closed, "PR was closed"})
		}
	}
	return evs
}

// Next is the state to store after handling cur: everything on the PR counts as seen.
func Next(prev registry.Last, cur github.PR) registry.Last {
	l := registry.Last{State: cur.State, Conflicting: cur.Mergeable == "CONFLICTING"}
	// UNKNOWN is GitHub recomputing mergeability; keep the flag so the same conflict does not re-wake.
	if cur.Mergeable == "UNKNOWN" {
		l.Conflicting = prev.Conflicting
	}
	for _, it := range cur.Items {
		l.SeenIDs = append(l.SeenIDs, it.ID)
	}
	if cur.CI == "FAILURE" {
		l.CIFailingSHA = cur.HeadSHA
	}
	return l
}

func DisplayOf(cur github.PR) registry.Display {
	return registry.Display{Title: cur.Title, State: cur.State, Draft: cur.Draft, CI: cur.CI, Review: cur.ReviewDecision}
}
