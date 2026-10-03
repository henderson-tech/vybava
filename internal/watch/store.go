package watch

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

// state is everything that must survive a daemon restart: the
// subscriptions (with their baselines) and the events not yet acknowledged.
// Probe readings, backoff and the budget are rebuilt — a restart re-probes.
type state struct {
	Version       int            `json:"version"`
	Seq           int64          `json:"seq"`
	Subscriptions []Subscription `json:"subscriptions"`
	Events        []Event        `json:"events"`
}

// Store persists state at Path; an empty Path keeps it in memory (the
// direct `until` fallback and tests).
type Store struct{ Path string }

func (s Store) load() (state, error) {
	empty := state{Version: StateVersion, Subscriptions: []Subscription{}, Events: []Event{}}
	if s.Path == "" {
		return empty, nil
	}
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return state{}, err
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return state{}, fmt.Errorf("%s: %w", s.Path, err)
	}
	if st.Version != StateVersion {
		return state{}, fmt.Errorf("%s was written by watch state version %d, this build reads %d — move it aside to start empty", s.Path, st.Version, StateVersion)
	}
	if st.Subscriptions == nil {
		st.Subscriptions = []Subscription{}
	}
	if st.Events == nil {
		st.Events = []Event{}
	}
	return st, nil
}

// save replaces the file atomically: a reader (or a crash) sees the old
// state or the new one, never half of either.
func (s Store) save(st state) error {
	if s.Path == "" {
		return nil
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.Path)
}

// Budget is the GitHub request budget every gh-backed probe shares: a token
// bucket holding Capacity units, refilled at PerHour. A probe that cannot
// pay waits for the refill instead of polling — N watched PRs stretch their
// intervals rather than exhaust the user's rate limit, which every Claude
// session and gh call on the Mac shares.
type Budget struct {
	Capacity float64
	PerHour  float64
	tokens   float64
	at       time.Time
}

// NewBudget starts full.
func NewBudget(capacity, perHour float64) *Budget {
	return &Budget{Capacity: capacity, PerHour: perHour, tokens: capacity}
}

// Take spends cost units at now, or answers when they will be there.
func (b *Budget) Take(now time.Time, cost int) (bool, time.Time) {
	b.refill(now)
	need := float64(cost)
	if b.tokens >= need {
		b.tokens -= need
		return true, now
	}
	if b.PerHour <= 0 {
		return false, now.Add(time.Hour)
	}
	wait := (need - b.tokens) / b.PerHour * float64(time.Hour)
	return false, now.Add(time.Duration(math.Ceil(wait)))
}

// Remaining is the units available at now.
func (b *Budget) Remaining(now time.Time) float64 {
	b.refill(now)
	return b.tokens
}

func (b *Budget) refill(now time.Time) {
	if !b.at.IsZero() && now.After(b.at) {
		b.tokens = min(b.Capacity, b.tokens+now.Sub(b.at).Hours()*b.PerHour)
	}
	if b.at.IsZero() || now.After(b.at) {
		b.at = now
	}
}
