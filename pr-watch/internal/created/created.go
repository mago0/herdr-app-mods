// Package created finds the GitHub pull requests a finished shell command opened.
//
// Ported from T3 Code's changeRequestsCreatedBy (MIT, Copyright (c) 2026 T3 Tools Inc.):
// https://github.com/pingdotgg/t3code/blob/8648aea3db09b209751dcf2501c62b5f929710b4/apps/server/src/orchestration-v2/PullRequestSyncReactor.ts
// Go's RE2 has no lookaround or backreferences, so quotes, heredocs, comments and word
// boundaries that T3 matches with them are scanned by hand here.
package created

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/mago0/herdr-app-mods/pr-watch/internal/prurl"
)

var (
	createCommand = regexp.MustCompile(`\bgh\s+pr\s+create\b`)
	// A create command counts only where a command starts, after any env assignments or wrappers.
	createRun = regexp.MustCompile("(?:^|[;&|({`\\n]|\\b(?:do|then|else)\\s)\\s*(?:(?:\\w+=\\S*|env|time|nohup|timeout\\s+\\S+)\\s+)*gh\\s+pr\\s+create")
	// Codex-style `sh -lc '<script>'`, quoted in pieces.
	shellWrapper   = regexp.MustCompile(`^\s*(?:\S*/)?(?:ba|z|da|k)?sh\s+-l?c\s+(["'][\s\S]*)$`)
	shellWordPiece = regexp.MustCompile(`"((?:\\[\s\S]|[^"\\])*)"|'([^']*)'|\\([\s\S])|(\S)`)
	doubleEscape   = regexp.MustCompile("\\\\([\"\\\\$`])")
	separator      = regexp.MustCompile(`&&|\|\||[;|\n]`)
	// Commands that print other PRs' URLs, so output beside them proves nothing.
	listsPRs = regexp.MustCompile(`\bgh\s+(?:pr\s+(?:list|status)|search\s+prs|api)\b`)
	urlLine  = regexp.MustCompile(`(?m)^[ \t]*(https?://\S+)[ \t]*$`)
	heredoc  = regexp.MustCompile(`<<-?[ \t]*(['"]?)(\w+)(['"]?)[^\n]*\n`)
)

// By returns the PRs that command input printed as created in output, in output order.
// One the command also names, as `gh pr view 12` does, existed before it ran. More URLs
// than create commands means something else printed some, so none are taken.
func By(input, output string) []prurl.Ref {
	if !createCommand.MatchString(input) {
		return nil
	}
	script := shellScript(input)
	code := shellCode(script)
	creates := createCount(code)
	if creates == 0 {
		return nil
	}
	var other []string
	for _, part := range separator.Split(code, -1) {
		if createCount(part) == 0 {
			other = append(other, part)
		}
	}
	if listsPRs.MatchString(strings.Join(other, "\n")) {
		return nil
	}
	var otherSegs []string
	for _, seg := range shellSegments(script) {
		if createCount(shellCode(seg)) == 0 {
			otherSegs = append(otherSegs, seg)
		}
	}
	otherScript := strings.Join(otherSegs, "\n")

	var out []prurl.Ref
	seen := map[string]bool{}
	for _, m := range urlLine.FindAllStringSubmatch(output, -1) {
		ref, err := prurl.Parse(m[1])
		if err != nil || !strings.HasPrefix(strings.ToLower(m[1]), "https://github.com/") {
			continue
		}
		if names(otherScript, ref.Number) || seen[ref.URL()] {
			continue
		}
		seen[ref.URL()] = true
		out = append(out, ref)
	}
	if len(out) > creates {
		return nil
	}
	return out
}

// createCount counts create commands that run; the subcommand must end its shell word,
// since `gh pr create#x` runs another one.
func createCount(code string) int {
	n := 0
	for _, loc := range createRun.FindAllStringIndex(code, -1) {
		if end := loc[1]; end == len(code) || strings.ContainsRune(" \t\n\r\f\v;&|)", rune(code[end])) {
			n++
		}
	}
	return n
}

// shellScript unwraps `sh -lc <script>` into the script it runs.
func shellScript(input string) string {
	m := shellWrapper.FindStringSubmatch(input)
	if m == nil {
		return input
	}
	var b strings.Builder
	for _, p := range shellWordPiece.FindAllStringSubmatchIndex(m[1], -1) {
		piece := func(g int) (string, bool) {
			if p[2*g] < 0 {
				return "", false
			}
			return m[1][p[2*g]:p[2*g+1]], true
		}
		if s, ok := piece(1); ok {
			b.WriteString(doubleEscape.ReplaceAllString(s, "$1"))
		} else if s, ok := piece(2); ok {
			b.WriteString(s)
		} else if s, ok := piece(3); ok {
			b.WriteString(s)
		} else if s, ok := piece(4); ok {
			b.WriteString(s)
		}
	}
	return b.String()
}

// shellSegments splits a script at command separators outside quotes.
func shellSegments(script string) []string {
	var segs []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(script); i++ {
		c := script[i]
		if quote != 0 {
			cur.WriteByte(c)
			if c == '\\' && quote == '"' && i+1 < len(script) {
				i++
				cur.WriteByte(script[i])
			} else if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
		}
		if c == '\\' {
			cur.WriteByte(c)
			if i+1 < len(script) {
				i++
				cur.WriteByte(script[i])
			}
			continue
		}
		pair := ""
		if i+1 < len(script) {
			pair = script[i : i+2]
		}
		if pair == "&&" || pair == "||" || c == ';' || c == '|' || c == '\n' {
			segs = append(segs, cur.String())
			cur.Reset()
			if pair == "&&" || pair == "||" {
				i++
			}
			continue
		}
		cur.WriteByte(c)
	}
	return append(segs, cur.String())
}

// shellCode keeps the commands a script runs and drops the text it carries:
// heredoc bodies become `<<`, quoted strings and comments become `”`.
func shellCode(script string) string {
	return stripQuotesAndComments(stripHeredocs(script))
}

func stripHeredocs(s string) string {
	var b strings.Builder
	for {
		loc := heredoc.FindStringSubmatchIndex(s)
		// An opening quote needs the same closing quote, as T3's backreference requires;
		// with none, anything after the word is just the rest of the line.
		for loc != nil && loc[3] > loc[2] && s[loc[2]:loc[3]] != s[loc[6]:loc[7]] {
			next := heredoc.FindStringSubmatchIndex(s[loc[0]+2:])
			if next == nil {
				loc = nil
				break
			}
			for i := range next {
				if next[i] >= 0 {
					next[i] += loc[0] + 2
				}
			}
			loc = next
		}
		if loc == nil {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:loc[0]])
		b.WriteString("<<")
		word := s[loc[4]:loc[5]]
		rest := s[loc[1]:]
		end := len(rest)
		for off := 0; off <= len(rest); {
			nl := strings.IndexByte(rest[off:], '\n')
			line := rest[off:]
			if nl >= 0 {
				line = rest[off : off+nl]
			}
			if strings.Trim(line, " \t") == word {
				end = off + len(line)
				break
			}
			if nl < 0 {
				break
			}
			off += nl + 1
		}
		s = rest[end:]
	}
}

func stripQuotesAndComments(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			if j := strings.IndexByte(s[i+1:], '\''); j >= 0 {
				b.WriteString("''")
				i += j + 1
				continue
			}
		case c == '"':
			if j := closingDouble(s, i+1); j >= 0 {
				b.WriteString("''")
				i = j
				continue
			}
		case c == '#' && (i == 0 || strings.ContainsRune(" \t\n\r\f\v;&|(", rune(s[i-1]))):
			j := strings.IndexByte(s[i:], '\n')
			b.WriteString("''")
			if j < 0 {
				return b.String()
			}
			i += j - 1
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// closingDouble returns the index of the quote closing a double-quoted string that starts at from, or -1.
func closingDouble(s string, from int) int {
	for i := from; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return i
		}
	}
	return -1
}

// names reports whether script mentions PR number n as its own token.
func names(script string, n int) bool {
	num := strconv.Itoa(n)
	for off := 0; ; {
		i := strings.Index(script[off:], num)
		if i < 0 {
			return false
		}
		start, end := off+i, off+i+len(num)
		before := start == 0 || strings.ContainsRune(" \t\n\r\f\v#/=(\"'", rune(script[start-1]))
		after := end == len(script) || strings.ContainsRune(" \t\n\r\f\v);&|/\"'", rune(script[end]))
		if before && after {
			return true
		}
		off = start + 1
	}
}
