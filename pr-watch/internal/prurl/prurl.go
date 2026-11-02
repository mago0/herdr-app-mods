// Package prurl parses GitHub pull request URLs into a canonical form.
package prurl

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Ref names one pull request.
type Ref struct {
	Owner, Name string
	Number      int
}

var pullRe = regexp.MustCompile(`^(?:https?://)?github\.com/([^/\s]+)/([^/\s]+)/pull/(\d+)(?:/.*)?$`)

// Parse accepts a PR URL with or without scheme, trailing slash or sub-path such as /files.
func Parse(raw string) (Ref, error) {
	m := pullRe.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return Ref{}, fmt.Errorf("not a GitHub pull request URL: %q", raw)
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n < 1 {
		return Ref{}, fmt.Errorf("bad pull request number in %q", raw)
	}
	return Ref{Owner: strings.ToLower(m[1]), Name: strings.ToLower(m[2]), Number: n}, nil
}

// Repo is owner/name, lowercased.
func (r Ref) Repo() string { return r.Owner + "/" + r.Name }

// URL is the canonical registry key.
func (r Ref) URL() string {
	return fmt.Sprintf("https://github.com/%s/%s/pull/%d", r.Owner, r.Name, r.Number)
}
