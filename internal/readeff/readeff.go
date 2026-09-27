// Package readeff measures how coding agents navigate code: what their reads
// and searches put into context, how much of it was re-read, and how that
// compares with what they changed. It reads Claude Code transcripts and Codex
// rollouts through internal/transcripts and classifies shell commands through
// internal/shellseg. Nothing is stored: every report scans a window on demand,
// and no command text or output leaves the scan — only counts and paths.
package readeff

import (
	"strings"
	"time"
)

// Class is what a tool call did, for navigation accounting.
type Class string

const (
	ClassRead   Class = "read"   // printed a file
	ClassSearch Class = "search" // printed where something is
	ClassEdit   Class = "edit"   // changed a file
	ClassOther  Class = "other"
)

// Span is a range of a file a read returned. Start is 1-based; Start 0 means
// the range is unknown (a tail, a filtered read). Whole marks a read of the
// entire file; N is its line count where known, Total the file's length.
type Span struct {
	Path  string
	Start int
	N     int
	Total int
	Whole bool
}

// Call is one tool invocation reduced to what navigation accounting needs.
type Call struct {
	Tool    string
	Class   Class
	Lines   int      // lines the result put into context
	Empty   bool     // a search that found nothing
	Spans   []Span   // file ranges a read returned
	Edited  []string // files an edit or write changed
	Changed int      // lines added or removed
	Stale   bool     // a file changed behind the harness's back
	Blocked string   // the claude-guards rule that denied the call
	Found   []string // files a search printed, for the hit rate
	Query   string   // a search's segment: compared, never printed

	dir string // where a search ran, to resolve what it printed
}

// Session is one transcript's calls, in order.
type Session struct {
	Agent string // "claude" | "codex"
	ID    string
	Repo  string
	Start time.Time
	Calls []Call
}

// countLines counts the lines of a tool result as they land in context.
func countLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// emptyOutput reports a result that carries nothing: no text, the harness's
// no-output marker, or only a nonzero exit status (grep finding nothing).
func emptyOutput(s string) bool {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "Exit code ") && !strings.Contains(s, "\n") {
		return true
	}
	return s == "" || s == "(Bash completed with no output)"
}

// maxFound caps the paths kept from one search's output.
const maxFound = 200

// foundPaths lists the files a search printed: grep-style `path:line:` rows or
// bare path rows (rg --files, find), resolved against the search's directory.
func foundPaths(out, dir string) []string {
	var paths []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		p := line
		if i := strings.IndexByte(line, ':'); i > 0 {
			p = line[:i]
		}
		p = strings.TrimSpace(p)
		if p == "" || strings.ContainsAny(p, " \t") || !strings.ContainsAny(p, "/.") {
			continue
		}
		p = resolve(dir, p)
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
			if len(paths) == maxFound {
				break
			}
		}
	}
	return paths
}

// guardRule extracts the claude-guards rule id from a denial message.
func guardRule(s string) string {
	const marker = "BLOCKED by claude-guards ("
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	rest := s[i+len(marker):]
	if j := strings.IndexByte(rest, ')'); j > 0 {
		return rest[:j]
	}
	return ""
}

// patchChanged counts a unified or apply_patch body's changed lines and the
// files it names.
func patchChanged(patch, dir string) (files []string, changed int) {
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "*** Update File: "), strings.HasPrefix(line, "*** Add File: "), strings.HasPrefix(line, "*** Delete File: "):
			files = append(files, resolve(dir, strings.TrimSpace(line[strings.IndexByte(line, ':')+1:])))
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"), strings.HasPrefix(line, "-"):
			changed++
		}
	}
	return files, changed
}
