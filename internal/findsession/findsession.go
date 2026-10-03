// Package findsession finds the Claude Code session a piece of conversation
// came from — a pasted ending, a phrase or a session id — and names the
// switcheroo preset (cc, cco, ccoo, …) that reopens it the way it started.
// It scans main-session transcripts on demand and stores nothing.
package findsession

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/secretscan"
)

// Diagnostic codes — the closed vocabulary of find-session.
const (
	DiagEmptyQuery  = "EMPTY_QUERY"
	DiagNoNeedles   = "NO_NEEDLES"
	DiagNoMatch     = "NO_MATCH"
	DiagAmbiguous   = "AMBIGUOUS"
	DiagQuotedOnly  = "QUOTED_ONLY"
	DiagCwdMissing  = "CWD_MISSING"
	DiagCwdUnknown  = "CWD_UNKNOWN"
	DiagManyMatches = "MANY_MATCHES"
	DiagBadFlag     = "BAD_FLAG"
	DiagRootMissing = "ROOT_MISSING"
	DiagPartial     = "PARTIAL_SCAN"
)

// Options scope one search.
type Options struct {
	// Root is the Claude projects directory (~/.claude/projects).
	Root string
	// Home shortens paths in the reopen command to ~/….
	Home string
	// Exclude is a session id left out — the caller's own session, which
	// holds the paste it is searching for.
	Exclude string
	// Since > 0 keeps only transcripts written within it.
	Since time.Duration
	// Limit caps the sessions returned (default 3).
	Limit int
	// Full scans every transcript at once instead of newest tier first.
	Full bool
}

// Result is one search: the needles it used and the sessions ranked best
// first.
type Result struct {
	Mode        string            `json:"mode"`
	Needles     []string          `json:"needles,omitempty"`
	Need        int               `json:"need,omitempty"`
	Scanned     int               `json:"scanned"`
	Matched     int               `json:"matched"`
	Total       int               `json:"total"`
	Elapsed     string            `json:"elapsed"`
	Sessions    []Session         `json:"sessions"`
	Diagnostics []runx.Diagnostic `json:"-"`
}

// Find resolves a query: an id or id prefix is looked up by file name;
// anything else is matched as text.
func Find(query string, opts Options) (Result, error) {
	start := time.Now()
	if opts.Limit <= 0 {
		opts.Limit = 3
	}
	if strings.TrimSpace(query) == "" {
		return Result{}, runx.DiagError{Diag: runx.Diagnostic{Code: DiagEmptyQuery, Severity: "error",
			Detail: "no text to search: pass it as arguments, pipe it on stdin, or copy it to the clipboard",
			Fix:    "pbpaste | find-session"}}
	}
	files, err := listSessions(opts.Root, opts.Since)
	if errors.Is(err, os.ErrNotExist) {
		return Result{}, runx.DiagError{Diag: runx.Diagnostic{Code: DiagRootMissing, Severity: "error",
			Detail: "no Claude projects directory at " + opts.Root,
			Fix:    "find-session --root <claude-config-dir>/projects '<text>'"}}
	}
	if err != nil {
		return Result{}, err
	}
	kept := files[:0]
	for _, f := range files {
		if f.ID != opts.Exclude {
			kept = append(kept, f)
		}
	}
	files = kept

	var res Result
	if id, ok := ParseID(query); ok {
		res.Mode = "id"
		var hits []scanned
		for _, f := range files {
			if strings.HasPrefix(f.ID, id) {
				hits = append(hits, scanned{File: f})
			}
		}
		res.Scanned = len(files)
		if res.Sessions, err = rankHits(hits, nil, opts, map[string]Session{}); err != nil {
			return Result{}, err
		}
	} else {
		res.Mode = "text"
		frags, need := Fragments(query)
		if len(frags) == 0 {
			return Result{}, runx.DiagError{Diag: runx.Diagnostic{Code: DiagNoNeedles, Severity: "error",
				Detail: "the query has no run of 3+ characters to match",
				Fix:    "find-session '<a longer phrase from the conversation>'"}}
		}
		res.Need = need
		for _, f := range frags {
			res.Needles = append(res.Needles, redactSecrets(f)) // a copy: frags stay the search
		}
		needles := make([]needle, len(frags))
		for i, f := range frags {
			needles[i] = newNeedle(f)
		}
		// Newest tier first; stop at the first tier where a session wrote
		// the text. files is sorted newest first, so each tier is a prefix.
		tiers := []time.Duration{0}
		if opts.Since == 0 && !opts.Full {
			tiers = []time.Duration{2 * 24 * time.Hour, 14 * 24 * time.Hour, 0}
		}
		var hits []scanned
		inspected := map[string]Session{}
		for _, tier := range tiers {
			end := len(files)
			if tier > 0 {
				cutoff := start.Add(-tier)
				end = sort.Search(len(files), func(i int) bool { return files[i].ModTime.Before(cutoff) })
			}
			if end <= res.Scanned {
				continue
			}
			more, err := scan(files[res.Scanned:end], needles, need)
			if err != nil {
				return Result{}, err
			}
			hits, res.Scanned = append(hits, more...), end
			res.Matched = len(hits)
			if res.Sessions, err = rankHits(hits, needles, opts, inspected); err != nil {
				return Result{}, err
			}
			if len(res.Sessions) > 0 && res.Sessions[0].Own() {
				break
			}
		}
	}
	res.Total = len(files)
	res.Elapsed = time.Since(start).Round(time.Millisecond).String()
	// The verdict reads the whole ranking; --limit only trims what is shown.
	res.Diagnostics = verdict(res, query)
	if len(res.Sessions) > opts.Limit {
		res.Sessions = res.Sessions[:opts.Limit]
	}
	return res, nil
}

// maxInspect caps the transcripts read for authorship; a query held by more
// sessions than this is too common to rank (MANY_MATCHES).
const maxInspect = 100

// rankHits inspects every scan hit (up to maxInspect, most phrases first)
// before ordering: authorship decides the ranking, so cutting to the limit
// first would let newer sessions that quote the text crowd out its author.
func rankHits(hits []scanned, needles []needle, opts Options, inspected map[string]Session) ([]Session, error) {
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Found != hits[j].Found {
			return hits[i].Found > hits[j].Found
		}
		return hits[i].File.ModTime.After(hits[j].File.ModTime)
	})
	var sessions []Session
	for _, hit := range hits[:min(len(hits), maxInspect)] {
		s, ok := inspected[hit.File.Path]
		if !ok {
			var err error
			s, err = inspect(hit.File, needles)
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, err
			}
			s.Found = hit.Found
			if s.Cwd != "" {
				if _, err := os.Stat(s.Cwd); errors.Is(err, os.ErrNotExist) {
					s.CwdMissing = true
				}
			}
			s.Reopen = reopenCommand(s, opts.Home)
			inspected[hit.File.Path] = s
		}
		sessions = append(sessions, s)
	}
	sort.SliceStable(sessions, func(i, j int) bool { return rank(sessions[i], sessions[j]) })
	return sessions, nil
}

// rank orders sessions: the one that wrote the text before those that only
// quote it, then more needles matched, then the text closer to the
// session's end, then the newer session.
func rank(a, b Session) bool {
	if a.Own() != b.Own() {
		return a.Own()
	}
	if a.Found != b.Found {
		return a.Found > b.Found
	}
	if ta, tb := tail(a), tail(b); ta != tb {
		return ta < tb
	}
	return a.Ended.After(b.Ended)
}

func tail(s Session) int {
	if s.LastHit == 0 {
		return s.Lines
	}
	return s.Lines - s.LastHit
}

func verdict(res Result, query string) []runx.Diagnostic {
	if len(res.Sessions) == 0 {
		fix := "find-session '<a shorter, distinctive phrase>'"
		if res.Mode == "id" {
			fix = "find-session '<text from the conversation>'"
		}
		return []runx.Diagnostic{{Code: DiagNoMatch, Severity: "error",
			Detail: fmt.Sprintf("no session among %d holds the query", res.Scanned), Fix: fix}}
	}
	var diags []runx.Diagnostic
	top := res.Sessions[0]
	if res.Mode == "text" && !top.Own() {
		diags = append(diags, runx.Diagnostic{Code: DiagQuotedOnly, Severity: "warning",
			Detail: "the best match only quotes the text (a paste or tool output) — the session that wrote it may be a subagent or deleted"})
	}
	if res.Matched > maxInspect {
		diags = append(diags, runx.Diagnostic{Code: DiagManyMatches, Severity: "warning",
			Detail: fmt.Sprintf("%d sessions hold the query; only the %d with the most phrases were ranked", res.Matched, maxInspect),
			Fix:    "find-session '<a longer, more distinctive passage>'"})
	}
	if len(res.Sessions) > 1 {
		second := res.Sessions[1]
		if second.Own() == top.Own() && second.Found == top.Found && tail(second) == tail(top) {
			diags = append(diags, runx.Diagnostic{Code: DiagAmbiguous, Severity: "warning",
				Detail: "the top sessions match equally — tell them apart by title, directory and time",
				Fix:    "find-session '<a longer passage>'"})
		}
	}
	if res.Scanned < res.Total {
		diags = append(diags, runx.Diagnostic{Code: DiagPartial, Severity: "info",
			Detail: fmt.Sprintf("searched the %d newest of %d sessions, stopping at the first that wrote it", res.Scanned, res.Total),
			Fix:    rerunFull(query)})
	}
	if top.Cwd == "" {
		diags = append(diags, runx.Diagnostic{Code: DiagCwdUnknown, Severity: "warning",
			Detail: "the transcript records no launch directory; run the line from the directory the session started in"})
	}
	if top.CwdMissing {
		diags = append(diags, runx.Diagnostic{Code: DiagCwdMissing, Severity: "warning",
			Detail: "the launch directory " + top.Cwd + " is gone; resume finds the transcript only from there",
			Fix:    "mkdir -p " + shellPath(top.Cwd, "") + " && " + top.Reopen})
	}
	return diags
}

// rerunFull is the full-scan rerun of a query: exact for a short one-line
// query, else a note to feed the same text again — also when the query holds
// a credential, which never reaches the output.
func rerunFull(query string) string {
	query = strings.TrimSpace(query)
	if strings.Contains(query, "\n") || len(query) > 120 || len(secretscan.Find(query, secretscan.All, nil)) > 0 {
		return "find-session --full  # with the same text on stdin"
	}
	return "find-session --full '" + strings.ReplaceAll(query, "'", `'\''`) + "'"
}
