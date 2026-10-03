package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Summary is the small view every session's fleet mod reads without
// spawning a process: one producer (the watch daemon) writes it, every
// session only reads it.
type Summary struct {
	GeneratedAt time.Time        `json:"generatedAt"`
	Counts      Counts           `json:"counts"`
	Waiting     []SummarySession `json:"waiting"`
}

// SummarySession is one session waiting on the human.
type SummarySession struct {
	SessionID  string `json:"sessionId"`
	Name       string `json:"name,omitempty"`
	Project    string `json:"project"`
	WaitingFor string `json:"waitingFor,omitempty"`
	AgeSeconds int64  `json:"ageSeconds"`
}

// SummaryPath is where the daemon publishes the summary.
func SummaryPath(home string) string {
	return filepath.Join(home, ".local", "state", "vybava", "fleet", "summary.json")
}

// Summarize keeps the counts and the waiting sessions, in snapshot order
// (waiting first, oldest first).
func Summarize(snap Snapshot) Summary {
	summary := Summary{GeneratedAt: snap.GeneratedAt, Counts: snap.Counts, Waiting: []SummarySession{}}
	for _, session := range snap.Sessions {
		if session.State != StateWaiting {
			continue
		}
		summary.Waiting = append(summary.Waiting, SummarySession{
			SessionID:  session.SessionID,
			Name:       session.Name,
			Project:    session.Project,
			WaitingFor: session.WaitingFor,
			AgeSeconds: session.AgeSeconds,
		})
	}
	return summary
}

// WriteSummary publishes the summary atomically.
func WriteSummary(path string, summary Summary) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(body, '\n'))
}
