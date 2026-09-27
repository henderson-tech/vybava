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
// A shell command can do several things at once (`cat a.go; rg foo`), so
// Read, Search and Edit are independent; Class is the headline kind for
// display (edit > read > search > other).
type Call struct {
	Tool    string
	Class   Class
	Read    bool
	Search  bool
	Edit    bool
	Visible int      // pieces of output in the result (shell pipelines); 1 for a plain tool
	Lines   int      // lines the result put into context
	Empty   bool     // a search that found nothing
	Spans   []Span   // file ranges a read returned
	Edited  []string // files an edit or write changed
	Changed int      // lines added or removed
	Stale   bool     // a file changed behind the harness's back
	Blocked string   // the claude-guards rule that denied the call
	Found   []string // files a search printed, for the hit rate
	Query   string   // a search's segment: compared, never printed

	dir       string   // where a search ran, to resolve what it printed
	wrote     bool     // a redirect or tee wrote a file whose lines are unmeasured
	open      bool     // a placeholder whose result has not arrived
	patched   bool     // an apply_patch ran: its heredoc body is the change
	patchDirs []string // where each apply_patch ran, in order
	outputs   uint8    // kinds of output in the result: 1<<0 read, 1<<1 search, 1<<2 other
}

// classify sets the headline Class from what the call did.
func (c *Call) classify() {
	switch {
	case c.Edit:
		c.Class = ClassEdit
	case c.Read:
		c.Class = ClassRead
	case c.Search:
		c.Class = ClassSearch
	default:
		c.Class = ClassOther
	}
}

// settle fills what a call's output says — only when a single piece of
// output is in the result, so it belongs to that piece: a search's emptiness
// and hits, a single-file whole read's length.
func settle(c Call, out string, failed bool) Call {
	if c.Visible != 1 {
		return c
	}
	if c.Search {
		if c.Empty = emptyOutput(out); !c.Empty {
			c.Found = foundPaths(out, c.dir)
		}
	}
	if c.Read && !failed && len(c.Spans) == 1 && c.Spans[0].Whole && c.Spans[0].N == 0 {
		c.Spans[0].N, c.Spans[0].Total = c.Lines, c.Lines
	}
	return c
}

// Session is one transcript's calls, in order.
type Session struct {
	Agent string // "claude" | "codex"
	ID    string
	Repo  string
	Start time.Time
	Calls []Call
	// Oversize counts records past the scan limit (16 MiB), skipped unread:
	// their calls are missing from the counts.
	Oversize int
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
	return s == "" || s == "(Bash completed with no output)" ||
		strings.HasPrefix(s, "No files found") || strings.HasPrefix(s, "No matches found")
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

// patchChanged counts an apply_patch body's changed lines and the files it
// names. Text carrying `*** Begin Patch` is read only between its markers,
// so a heredoc'd patch inside a longer command counts nothing else.
func patchChanged(patch, dir string) (files []string, changed int) {
	marked := strings.Contains(patch, "*** Begin Patch")
	in := !marked
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case marked && strings.HasPrefix(line, "*** Begin Patch"):
			in = true
		case marked && strings.HasPrefix(line, "*** End Patch"):
			in = false
		case !in:
		case strings.HasPrefix(line, "*** Update File: "), strings.HasPrefix(line, "*** Add File: "), strings.HasPrefix(line, "*** Delete File: "):
			files = append(files, resolve(dir, strings.TrimSpace(line[strings.IndexByte(line, ':')+1:])))
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"), strings.HasPrefix(line, "-"):
			changed++
		}
	}
	return files, changed
}
