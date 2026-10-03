package fleet

import (
	"context"
	"time"

	"github.com/henderson-tech/vybava/internal/codexusage"
)

// CodexRow is one live Codex CLI, shown read-only: Codex has no mods, so a
// fleet view can see it but never reply to it.
type CodexRow struct {
	PID        int        `json:"pid"`
	TTY        string     `json:"tty,omitempty"`
	ThreadID   string     `json:"threadId"`
	Name       string     `json:"name,omitempty"`
	Project    string     `json:"project"`
	Root       string     `json:"root,omitempty"`
	CWD        string     `json:"cwd"`
	Branch     string     `json:"branch,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	LastCallAt *time.Time `json:"lastCallAt,omitempty"`
	Calls      int        `json:"calls"`
}

// CodexReader lists live Codex processes. Warnings are partial failures the
// rows still stand on; an error means no rows at all. Either way the fleet
// snapshot goes on — Codex rows are an enrichment.
type CodexReader func(ctx context.Context, now time.Time) ([]CodexRow, []string, error)

// codexWindow is how far back rollouts are scanned for live threads. A Codex
// thread untouched for longer is not listed.
const codexWindow = 24 * time.Hour

// CodexUsage reads live Codex processes through codexusage, the existing
// reader of ~/.codex rollouts and its ps/lsof enrichment — never a second
// parser of Codex logs. The reader keeps its rollout parses between calls,
// so a long-lived caller reads only what Codex appended since.
func CodexUsage(home string, exec func(ctx context.Context, name string, args ...string) ([]byte, error)) CodexReader {
	cache := &codexusage.Cache{}
	return func(ctx context.Context, now time.Time) ([]CodexRow, []string, error) {
		report, err := codexusage.Run(ctx, codexusage.Env{Home: home, Now: now, Exec: exec, Cache: cache},
			codexusage.Options{Since: now.Add(-codexWindow), IncludeIdle: true})
		if err != nil {
			return nil, nil, err
		}
		rows := []CodexRow{}
		for _, group := range [][]codexusage.SessionReport{report.Sessions, report.Idle} {
			for _, s := range group {
				if !s.Live {
					continue
				}
				row := CodexRow{PID: s.PID, TTY: s.TTY, ThreadID: s.ID, Name: s.Name, CWD: s.CWD, Branch: s.Branch, Calls: s.Calls}
				if !s.ProcessStart.IsZero() {
					started := s.ProcessStart
					row.StartedAt = &started
				}
				if !s.Last.IsZero() {
					last := s.Last
					row.LastCallAt = &last
				}
				rows = append(rows, row)
			}
		}
		return rows, report.Warnings, nil
	}
}
