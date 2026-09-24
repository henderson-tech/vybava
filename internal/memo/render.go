package memo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MaxIndexLines is the hard cap on the rendered MEMORY.md, header included.
// The harness loads the file whole and drops content past 200 lines; 100
// keeps the loaded surface small enough to earn every line.
const MaxIndexLines = 100

// indexHeading and citeLead open every render; isRenderedIndex keys on them
// to tell a render from a hand-written v2 index.
const (
	indexHeading = "# Memory"
	citeLead     = "Cite a row as #"
)

// Render produces the deterministic MEMORY.md for a ledger.
func Render(l *Ledger, events []Event, now time.Time) string {
	lines := []string{indexHeading, ""}
	prefix := ""
	if l.Kind == KindTeam {
		prefix = "t"
	}
	if l.Kind == KindPersonal && l.Repo != "" {
		lines = append(lines, "Team memory: "+filepath.Join(l.Repo, ".claude", "memory", IndexFile), "")
	}
	lines = append(lines,
		fmt.Sprintf(citeLead+"%sNN when you act on it; `memo show %sNN` prints the row and its notes.", prefix, prefix),
		"`memo find <words>` searches the whole ledger; `memo add` captures a new row.",
		"",
		"## Pinned",
	)
	ranked := Order(l, events, now)
	var pinned, hot []string
	for _, r := range ranked {
		if r.Row.Pinned {
			pinned = append(pinned, r.Row.Format())
		} else {
			hot = append(hot, r.Row.Format())
		}
	}
	budget := MaxIndexLines - len(lines) - 2 // "" + "## Hot"
	pinned = cap(pinned, budget)
	lines = append(lines, pinned...)
	lines = append(lines, "", "## Hot")
	lines = append(lines, cap(hot, budget-len(pinned))...)
	return strings.Join(lines, "\n") + "\n"
}

func cap(rows []string, budget int) []string {
	if budget < 0 {
		budget = 0
	}
	if len(rows) > budget {
		return rows[:budget]
	}
	return rows
}

// WriteIndex renders and writes MEMORY.md, reporting whether the file changed.
// In a team home it first makes sure the hot surface is gitignored, so every
// writer (add, import, render, touch, the Stop harvest) keeps it local. A
// team MEMORY.md that git tracks is never written: rewriting it dirtied every
// checkout that still commits it (deploys refuse a dirty tree) and clobbered
// hand-written v2 indexes. It stays as committed and the returned
// SURFACE_TRACKED warning names the fix.
func WriteIndex(l *Ledger, events []Event, now time.Time) (bool, *Diag, error) {
	if _, err := EnsureGitignore(l.Home(), l.Kind); err != nil {
		return false, nil, err
	}
	if d := TrackedIndex(l.Home(), l.Kind); d != nil {
		return false, d, nil
	}
	want := Render(l, events, now)
	path := filepath.Join(l.Home(), IndexFile)
	if have, err := os.ReadFile(path); err == nil && string(have) == want {
		return false, nil, nil
	}
	return true, nil, os.WriteFile(path, []byte(want), 0o644)
}

// CheckIndex reports whether MEMORY.md on disk equals the render.
func CheckIndex(l *Ledger, events []Event, now time.Time) (bool, error) {
	have, err := os.ReadFile(filepath.Join(l.Home(), IndexFile))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return string(have) == Render(l, events, now), nil
}
