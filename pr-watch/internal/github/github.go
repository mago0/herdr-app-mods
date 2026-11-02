// Package github reads pull request state from GitHub through the gh CLI.
package github

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/prurl"
)

// Actor is the author of an item. A deleted account has an empty Login.
type Actor struct {
	Login string
	Bot   bool
}

// Item is one review, PR comment or review-thread comment.
type Item struct {
	ID     string
	Kind   string // "review" | "comment" | "thread_comment"
	Author Actor
	State  string // review state; empty for comments
	Body   string
}

// PR is the slice of pull request state that the event classifier diffs.
type PR struct {
	URL, Title, State string
	Draft             bool
	Mergeable         string // MERGEABLE | CONFLICTING | UNKNOWN
	HeadSHA, CI       string
	ReviewDecision    string
	FailingChecks     []string
	Items             []Item
}

// Fetcher reads PR state. Fetch keys its result by prurl.Ref.URL(); a PR
// missing upstream is absent from the map, not an error.
type Fetcher interface {
	Fetch(ctx context.Context, refs []prurl.Ref) (map[string]PR, error)
	Viewer(ctx context.Context) (string, error)
}

//go:embed query.graphql
var fragment string

type ghFetcher struct{}

// New returns a Fetcher that shells out to `gh api graphql`.
func New() Fetcher { return ghFetcher{} }

func gh(ctx context.Context, query string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		// gh exits non-zero on partial GraphQL errors but still prints the body.
		return stdout.Bytes(), fmt.Errorf("gh api graphql: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

func (ghFetcher) Fetch(ctx context.Context, refs []prurl.Ref) (map[string]PR, error) {
	if len(refs) == 0 {
		return map[string]PR{}, nil
	}
	q, _ := buildQuery(refs)
	out, err := gh(ctx, q)
	if err != nil {
		// A deleted PR or repo is a NOT_FOUND error; Parse accepts those.
		if prs, perr := Parse(out, refs); perr == nil {
			return prs, nil
		}
		return nil, err
	}
	return Parse(out, refs)
}

func (ghFetcher) Viewer(ctx context.Context) (string, error) {
	out, err := gh(ctx, "{ viewer { login } }")
	if err != nil {
		return "", err
	}
	var resp struct {
		Data struct {
			Viewer struct{ Login string }
		}
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("github: parse viewer: %w", err)
	}
	if resp.Data.Viewer.Login == "" {
		return "", fmt.Errorf("github: viewer login missing")
	}
	return resp.Data.Viewer.Login, nil
}

// buildQuery emits one aliased repository block per repo (r0, r1, ...) with one
// pullRequest alias per PR (pr0, pr1, ...). The index maps "r0.pr1" to a PR URL.
func buildQuery(refs []prurl.Ref) (string, map[string]string) {
	var order []string
	byRepo := map[string][]prurl.Ref{}
	for _, r := range refs {
		if _, ok := byRepo[r.Repo()]; !ok {
			order = append(order, r.Repo())
		}
		byRepo[r.Repo()] = append(byRepo[r.Repo()], r)
	}
	idx := map[string]string{}
	var b strings.Builder
	b.WriteString("query {\n")
	for i, repo := range order {
		group := byRepo[repo]
		fmt.Fprintf(&b, "  r%d: repository(owner: %q, name: %q) {\n", i, group[0].Owner, group[0].Name)
		for j, r := range group {
			fmt.Fprintf(&b, "    pr%d: pullRequest(number: %d) { ...PRFields }\n", j, r.Number)
			idx[fmt.Sprintf("r%d.pr%d", i, j)] = r.URL()
		}
		b.WriteString("  }\n")
	}
	b.WriteString("}\n")
	b.WriteString(fragment)
	return b.String(), idx
}

type rawActor struct {
	Login    string `json:"login"`
	Typename string `json:"__typename"`
}

func (a *rawActor) actor() Actor {
	if a == nil {
		return Actor{}
	}
	return Actor{Login: a.Login, Bot: a.Typename == "Bot"}
}

type rawItem struct {
	ID     string    `json:"id"`
	State  string    `json:"state"`
	Body   string    `json:"body"`
	Author *rawActor `json:"author"`
}

type rawItems struct {
	Nodes []rawItem `json:"nodes"`
}

type rawPR struct {
	URL            string `json:"url"`
	Title          string `json:"title"`
	State          string `json:"state"`
	IsDraft        bool   `json:"isDraft"`
	Mergeable      string `json:"mergeable"`
	ReviewDecision string `json:"reviewDecision"`
	Commits        struct {
		Nodes []struct {
			Commit struct {
				OID               string `json:"oid"`
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						Nodes []struct {
							Name       string `json:"name"`
							Conclusion string `json:"conclusion"`
							Context    string `json:"context"`
							State      string `json:"state"`
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Reviews       rawItems `json:"reviews"`
	Comments      rawItems `json:"comments"`
	ReviewThreads struct {
		Nodes []struct {
			Comments rawItems `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
}

// Parse decodes a response to buildQuery(refs). PRs that GitHub reports as
// missing (null alias or NOT_FOUND error) are left out of the result.
func Parse(body []byte, refs []prurl.Ref) (map[string]PR, error) {
	var resp struct {
		Data   map[string]map[string]*rawPR `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("github: parse response: %w", err)
	}
	for _, e := range resp.Errors {
		if e.Type != "NOT_FOUND" {
			return nil, fmt.Errorf("github: graphql error: %s", e.Message)
		}
	}
	_, idx := buildQuery(refs)
	out := map[string]PR{}
	for key, url := range idx {
		r, p, _ := strings.Cut(key, ".")
		raw := resp.Data[r][p]
		if raw == nil {
			continue
		}
		pr := convert(raw)
		pr.URL = url
		out[url] = pr
	}
	return out, nil
}

func convert(raw *rawPR) PR {
	pr := PR{
		URL: raw.URL, Title: raw.Title, State: raw.State, Draft: raw.IsDraft,
		Mergeable: raw.Mergeable, ReviewDecision: raw.ReviewDecision,
	}
	if len(raw.Commits.Nodes) > 0 {
		c := raw.Commits.Nodes[0].Commit
		pr.HeadSHA = c.OID
		if roll := c.StatusCheckRollup; roll != nil {
			pr.CI = roll.State
			for _, n := range roll.Contexts.Nodes {
				switch {
				case n.Name != "" && (n.Conclusion == "FAILURE" || n.Conclusion == "TIMED_OUT" || n.Conclusion == "CANCELLED"):
					pr.FailingChecks = append(pr.FailingChecks, n.Name)
				case n.Context != "" && (n.State == "FAILURE" || n.State == "ERROR"):
					pr.FailingChecks = append(pr.FailingChecks, n.Context)
				}
			}
		}
	}
	add := func(kind string, items []rawItem) {
		for _, it := range items {
			pr.Items = append(pr.Items, Item{ID: it.ID, Kind: kind, Author: it.Author.actor(), State: it.State, Body: it.Body})
		}
	}
	add("review", raw.Reviews.Nodes)
	add("comment", raw.Comments.Nodes)
	for _, t := range raw.ReviewThreads.Nodes {
		add("thread_comment", t.Comments.Nodes)
	}
	return pr
}
