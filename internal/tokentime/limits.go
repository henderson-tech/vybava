package tokentime

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// LimitWindow is one rate-limit window as a Codex response left it: its
// length in minutes, when it resets (unix seconds) and how much of it is used.
type LimitWindow struct {
	Minutes  int     `json:"minutes"`
	ResetsAt int64   `json:"resetsAt"`
	Pct      float64 `json:"pct"`
}

// LimitPoint is one Codex token_count's rate-limit reading and the call it
// followed. Input keeps Codex's meaning: it INCLUDES Cached and CacheWrite.
// A refresh — no call, just a new reading — has every token count zero.
type LimitPoint struct {
	TS         int64         `json:"ts"` // unix milliseconds
	Thread     string        `json:"thread"`
	Plan       string        `json:"plan"`
	Model      string        `json:"model"`
	Input      int64         `json:"input"`
	Cached     int64         `json:"cached"`
	CacheWrite int64         `json:"cacheWrite"`
	Output     int64         `json:"output"`
	Reasoning  int64         `json:"reasoning"`
	Windows    []LimitWindow `json:"windows"`
}

// LimitsBackfill is the points backlog: rollout bytes indexed before points
// existed, still owed their points as of the last completed pass.
type LimitsBackfill struct {
	Done         bool  `json:"done"`
	PendingBytes int64 `json:"pendingBytes"`
}

// Limits is the `tokentime limits --json` payload, a contract with
// claude-switcheroo's Arcade accounts.
type Limits struct {
	Points   []LimitPoint   `json:"points"`
	Cursor   int64          `json:"cursor"`
	Backfill LimitsBackfill `json:"backfill"`
}

// Limits reads every limit point strictly after since (unix milliseconds),
// oldest first, without indexing: never the lock, never a write. Cursor is
// the newest point's ts, or since when there is none. The backlog recovers
// older points than a cursor already returned until Backfill is done.
func (s *Store) Limits(since int64) (Limits, error) {
	if s.db != nil && s.version > 0 && s.version < limitsSchema {
		return Limits{}, fmt.Errorf("%w: schema %d records no limit points, this binary reads %d or later", ErrStaleSchema, s.version, limitsSchema)
	}
	tx, err := s.begin()
	if err != nil {
		return Limits{}, err
	}
	defer tx.Rollback()
	out := Limits{Points: []LimitPoint{}, Cursor: since}
	rows, err := tx.Query(`SELECT ts, thread, plan, model, input, cached, cache_write, output, reasoning, windows
		FROM limit_points WHERE ts > ? ORDER BY ts, id`, since)
	if err != nil {
		return Limits{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var p LimitPoint
		var windows string
		if err := rows.Scan(&p.TS, &p.Thread, &p.Plan, &p.Model, &p.Input, &p.Cached, &p.CacheWrite, &p.Output, &p.Reasoning, &windows); err != nil {
			return Limits{}, err
		}
		if err := json.Unmarshal([]byte(windows), &p.Windows); err != nil {
			return Limits{}, fmt.Errorf("limit point at %d: windows: %w", p.TS, err)
		}
		out.Points = append(out.Points, p)
		out.Cursor = max(out.Cursor, p.TS)
	}
	if err := rows.Err(); err != nil {
		return Limits{}, err
	}
	pending, err := meta(tx, "points_pending_bytes")
	if err != nil {
		return Limits{}, err
	}
	// Written by every completed pass; missing only before the first one ends.
	out.Backfill.PendingBytes, _ = strconv.ParseInt(pending, 10, 64)
	out.Backfill.Done = pending != "" && out.Backfill.PendingBytes == 0
	return out, nil
}
