package reconcile

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// State is the per-box state directory:
//
//	applied.tsv    <repo path>\t<sha256 last applied>   — the hotfix detector
//	pending-hooks  hooks that failed and retry next tick
//	last-good      commit that last converged fully with hooks passing
//	pin            commit `rollback` pinned the box to (run honors it)
//	history.jsonl  one entry per run/force/rollback (UI + metrics read it)
//	last-alert.*   per-channel digest dedup
//	backups/       live files `force` overwrote
type State struct{ Dir string }

func (s State) applied() string      { return filepath.Join(s.Dir, "applied.tsv") }
func (s State) pendingHooks() string { return filepath.Join(s.Dir, "pending-hooks") }
func (s State) lastGood() string     { return filepath.Join(s.Dir, "last-good") }
func (s State) pin() string          { return filepath.Join(s.Dir, "pin") }
func (s State) history() string      { return filepath.Join(s.Dir, "history.jsonl") }
func (s State) alertMarker(channel string) string {
	return filepath.Join(s.Dir, "last-alert."+channel)
}
func (s State) Backups() string { return filepath.Join(s.Dir, "backups") }

// Ensure creates the directory and applied.tsv (run/force/rollback only —
// status never creates state).
func (s State) Ensure() error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.applied(), os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// AppliedSHA returns the last-applied sha for a repo path ("" if none).
func (s State) AppliedSHA(rp string) string {
	rows, _ := s.readApplied()
	last := ""
	for _, r := range rows {
		if r[0] == rp {
			last = r[1]
		}
	}
	return last
}

func (s State) readApplied() ([][2]string, error) {
	f, err := os.Open(s.applied())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var rows [][2]string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		p, sha, _ := strings.Cut(line, "\t")
		rows = append(rows, [2]string{p, sha})
	}
	return rows, sc.Err()
}

// RecordApplied rewrites the row for rp (first-field match, never a suffix).
func (s State) RecordApplied(rp, sha string) error {
	return s.rewriteApplied(rp, &sha)
}

// UnrecordApplied drops every row for rp.
func (s State) UnrecordApplied(rp string) error {
	return s.rewriteApplied(rp, nil)
}

func (s State) rewriteApplied(rp string, sha *string) error {
	rows, err := s.readApplied()
	if err != nil {
		return err
	}
	var b strings.Builder
	for _, r := range rows {
		if r[0] != rp {
			b.WriteString(r[0] + "\t" + r[1] + "\n")
		}
	}
	if sha != nil {
		b.WriteString(rp + "\t" + *sha + "\n")
	}
	return atomicWrite(s.applied(), []byte(b.String()), 0o644)
}

// PendingHooks reads the retry queue (deduplicated, order kept).
func (s State) PendingHooks() []string {
	raw, err := os.ReadFile(s.pendingHooks())
	if err != nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, l := range strings.Split(string(raw), "\n") {
		if l == "" || seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, l)
	}
	return out
}

// WritePendingHooks replaces the retry queue; an empty list removes the file.
func (s State) WritePendingHooks(hooks []string) error {
	if len(hooks) == 0 {
		err := os.Remove(s.pendingHooks())
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return atomicWrite(s.pendingHooks(), []byte(strings.Join(hooks, "\n")+"\n"), 0o644)
}

// QueueHook appends one hook to the retry queue (force's reload failure).
func (s State) QueueHook(hook string) error {
	return s.WritePendingHooks(append(s.PendingHooks(), hook))
}

func (s State) LastGood() string { return readTrim(s.lastGood()) }
func (s State) SetLastGood(sha string) error {
	return atomicWrite(s.lastGood(), []byte(sha+"\n"), 0o644)
}

func (s State) Pin() string { return readTrim(s.pin()) }
func (s State) SetPin(sha string) error {
	if sha == "" {
		err := os.Remove(s.pin())
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return atomicWrite(s.pin(), []byte(sha+"\n"), 0o644)
}

func (s State) AlertMarker(channel string) string { return readTrim(s.alertMarker(channel)) }
func (s State) SetAlertMarker(channel, sha string) error {
	return atomicWrite(s.alertMarker(channel), []byte(sha+"\n"), 0o644)
}
func (s State) ClearAlertMarkers(channels []string) {
	for _, c := range channels {
		_ = os.Remove(s.alertMarker(c))
	}
}

// HistoryEntry is one line of history.jsonl.
type HistoryEntry struct {
	Time        time.Time `json:"time"`
	Action      string    `json:"action"` // run | force | rollback
	Commit      string    `json:"commit"`
	Mode        string    `json:"mode"`
	OK          bool      `json:"ok"`
	Applied     []string  `json:"applied,omitempty"`
	Pending     []string  `json:"pending,omitempty"`
	Held        []string  `json:"held,omitempty"`
	CertHeld    []string  `json:"cert_held,omitempty"`
	Errors      []Issue   `json:"errors,omitempty"`
	RollNotes   []string  `json:"roll_manually,omitempty"`
	RollSteps   []string  `json:"roll_steps,omitempty"`
	SkippedApps []string  `json:"skipped_apps,omitempty"`
	FailedHooks []string  `json:"failed_hooks,omitempty"`
	Path        string    `json:"path,omitempty"` // force target
	LastGood    string    `json:"last_good,omitempty"`
	Pin         string    `json:"pin,omitempty"`
}

func (s State) AppendHistory(e HistoryEntry) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.history(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

// History returns the newest `limit` entries, newest first (0 = all).
func (s State) History(limit int) ([]HistoryEntry, error) {
	f, err := os.Open(s.history())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var all []HistoryEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		var e HistoryEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue // a torn line never blinds the UI
		}
		all = append(all, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}

func readTrim(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// atomicWrite lands content via a same-directory temp file + rename.
func atomicWrite(path string, content []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(content); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(name, path); err != nil {
		cleanup()
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}
