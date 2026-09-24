// Package gitmerge is the leaf both merge drivers share (lok's catalog driver
// and merge-assist's generated-file driver): the journal a driver appends
// what it did to, and the text-merge fallback that keeps git's default
// behaviour for anything a driver will not own. It imports nothing from
// vybava so lok can depend on it without a cycle.
package gitmerge

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// JournalEnv redirects the journal to a file — `merge-assist merge --dry-run`
// points it at a scratch file so a `git merge-tree` preview, which runs the
// drivers too, leaves no trace in the real merge's state.
const JournalEnv = "VYBAVA_MERGE_JOURNAL"

// Classes and outcomes a driver records.
const (
	ClassCatalog   = "catalog"
	ClassGenerated = "generated"
	ClassMigration = "migration"
	// ClassRegen events mark a regen command run; Path is the command.
	ClassRegen = "regen"

	OutcomeResolved = "resolved"
	OutcomeConflict = "conflict"
	OutcomeFailed   = "failed"
)

// Event is one driver invocation: which path, what it did, and the regen
// command that must run once the merge has settled ("" = none).
type Event struct {
	Path    string `json:"path"`
	Class   string `json:"class"`
	Outcome string `json:"outcome"`
	Detail  string `json:"detail,omitempty"`
	Regen   string `json:"regen,omitempty"`
}

// JournalPath is $VYBAVA_MERGE_JOURNAL, else <git-dir>/vybava-merge.jsonl —
// per worktree, because each worktree has its own git dir.
func JournalPath(dir string) (string, error) {
	if p := os.Getenv(JournalEnv); p != "" {
		return p, nil
	}
	cmd := exec.Command("git", "rev-parse", "--absolute-git-dir")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("locate git dir: %w", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "vybava-merge.jsonl"), nil
}

// Record appends one event.
func Record(dir string, e Event) error {
	p, err := JournalPath(dir)
	if err != nil {
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Events reads the journal in write order.
func Events(dir string) ([]Event, error) {
	p, err := JournalPath(dir)
	if err != nil {
		return nil, err
	}
	return ReadJournal(p)
}

// ReadJournal reads one journal file; a missing file is an empty journal.
func ReadJournal(p string) ([]Event, error) {
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		out = append(out, e)
	}
	return out, sc.Err()
}

// Latest keeps the last event per path, in first-seen order: a re-run driver
// or a `lok merge` resolution supersedes the first attempt.
func Latest(events []Event) []Event {
	var out []Event
	at := map[string]int{}
	for _, e := range events {
		if i, ok := at[e.Path]; ok {
			out[i] = e
			continue
		}
		at[e.Path] = len(out)
		out = append(out, e)
	}
	return out
}

// Clear drops the journal before a new merge.
func Clear(dir string) error {
	p, err := JournalPath(dir)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// TextMerge is git's own line merge into ours (conflict markers included) —
// what git would have done without a driver. conflict reports markers left.
func TextMerge(dir, base, ours, theirs string, stderr io.Writer) (conflict bool, err error) {
	cmd := exec.Command("git", "merge-file", "-L", "ours", "-L", "base", "-L", "theirs", ours, base, theirs)
	cmd.Dir = dir
	cmd.Stderr = stderr
	err = cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 && exit.ExitCode() < 128 {
		return true, nil // exit code = number of conflicts
	}
	return false, err
}
