package codexusage

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

// tokenCountMarker gates JSON decoding. A rollout is mostly transcript — tens
// of megabytes of tool output per file — and only the handful of lines
// carrying this marker are usage evidence, so the substring test runs first.
var tokenCountMarker = []byte(`"token_count"`)

func usageOf(u transcripts.CodexUsage) Usage {
	return Usage{Input: u.Input, Cached: u.Cached, CacheWrite: u.CacheWrite, Output: u.Output, Reasoning: u.Reasoning}
}

func limitOf(p transcripts.TokenCount) *Limit {
	if p.RateLimits == nil || p.RateLimits.Primary == nil {
		return nil
	}
	return &Limit{
		Plan:          p.RateLimits.PlanType,
		ID:            p.RateLimits.LimitID,
		UsedPercent:   p.RateLimits.Primary.UsedPercent,
		WindowMinutes: p.RateLimits.Primary.WindowMinutes,
		ResetsAt:      time.Unix(p.RateLimits.Primary.ResetsAt, 0),
	}
}

// scanSessions reads every rollout that could hold a call inside the window.
// A single unreadable rollout is a warning, never a failure — a partial answer
// beats none when a limit is already burning.
func scanSessions(env Env, since time.Time) ([]Session, []string, error) {
	paths, err := transcripts.RolloutPaths(env.Home, since)
	if err != nil {
		return nil, nil, err
	}
	var (
		sessions []Session
		warnings []string
	)
	if env.Cache != nil {
		env.Cache.mu.Lock()
		defer env.Cache.mu.Unlock()
		env.Cache.retain(paths)
	}
	for _, path := range paths {
		state := env.Cache.state(path)
		session, err := readRollout(path, since, state)
		if err != nil {
			env.Cache.drop(path)
			warnings = append(warnings, fmt.Sprintf("%s: %v", filepath.Base(path), err))
			continue
		}
		sessions = append(sessions, session)
	}
	return sessions, warnings, nil
}

// Cache carries each rollout's parse from one Run to the next. Rollouts are
// append-only, so a long-lived reader (the watch daemon's fleet snapshot)
// reads only what was appended since — not the ~1.6 GB a day of rollouts
// every minute. The zero value is ready; a nil Cache reads every file whole.
type Cache struct {
	mu    sync.Mutex
	files map[string]*rolloutState
}

// rolloutState is one rollout read up to cursor. Samples are kept from floor
// on; a Run whose window starts later filters them, one starting earlier
// re-reads the file.
type rolloutState struct {
	cursor   transcripts.Cursor
	known    bool
	meta     bool
	floor    time.Time
	session  Session
	previous transcripts.CodexUsage
}

func (c *Cache) state(path string) *rolloutState {
	if c == nil {
		return &rolloutState{}
	}
	if c.files == nil {
		c.files = map[string]*rolloutState{}
	}
	if c.files[path] == nil {
		c.files[path] = &rolloutState{}
	}
	return c.files[path]
}

// drop forgets a rollout whose read failed part-way: its samples may be half
// applied, so the next Run reads it whole.
func (c *Cache) drop(path string) {
	if c != nil {
		delete(c.files, path)
	}
}

// retain forgets the rollouts that fell out of the window.
func (c *Cache) retain(paths []string) {
	keep := make(map[string]bool, len(paths))
	for _, path := range paths {
		keep[path] = true
	}
	for path := range c.files {
		if !keep[path] {
			delete(c.files, path)
		}
	}
}

// readRollout extracts a thread's identity and its in-window calls, reading
// only what follows state's cursor.
func readRollout(path string, since time.Time, state *rolloutState) (Session, error) {
	if state.known && since.Before(state.floor) {
		*state = rolloutState{}
	}
	if !state.known {
		*state = rolloutState{floor: since, session: Session{File: path}}
	}
	// The first line is session_meta and carries the model's base instructions,
	// so it can be past any record limit — read it unbounded, until it parses.
	if !state.meta {
		head, err := readHead(path)
		if err != nil {
			return Session{}, err
		}
		state.meta = applyMeta(&state.session, head)
	}
	opts := transcripts.ScanOptions{Budget: math.MaxInt64, SkipOversize: true}
	res, err := transcripts.Scan(path, state.cursor, state.known, opts, func(line []byte, offset int64) error {
		if offset > 0 && bytes.Contains(line, tokenCountMarker) {
			appendSample(&state.session, line, state.floor, &state.previous)
		}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	if res.Reset {
		// A replaced or truncated file: what the cache holds belongs to the old
		// one, and Scan may have handed over no first line to say so.
		*state = rolloutState{}
		return readRollout(path, since, state)
	}
	if res.Skipped && !state.known {
		return Session{}, fmt.Errorf("%s vanished or is not a regular file", filepath.Base(path))
	}
	state.cursor, state.known = res.Cursor, true

	session := state.session
	session.File = path
	session.Samples = nil
	for _, sample := range state.session.Samples {
		if !sample.At.Before(since) {
			session.Samples = append(session.Samples, sample)
		}
	}
	return session, nil
}

// readHead reads a rollout's first line, however long.
func readHead(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	head, err := bufio.NewReaderSize(file, 256*1024).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return head, nil
}

// applyMeta reads session_meta into session and reports whether it parsed.
func applyMeta(session *Session, line []byte) bool {
	var entry transcripts.RolloutLine
	if json.Unmarshal(line, &entry) != nil || entry.Type != "session_meta" {
		return false
	}
	var meta transcripts.SessionMeta
	if json.Unmarshal(entry.Payload, &meta) != nil {
		return false
	}
	session.ID, session.CWD, session.Version = meta.ID, meta.CWD, meta.CLIVersion
	if meta.Git != nil {
		session.Branch = meta.Git.Branch
	}
	if at, err := time.Parse(time.RFC3339, meta.Timestamp); err == nil {
		session.StartedAt = at.Local()
	}
	if session.ID == "" {
		session.ID = idFromPath(session.File)
	}
	return true
}

// appendSample records one call, skipping the repeat events Codex emits when
// only the rate limit refreshed. Those carry a stale last_token_usage that
// would be billed twice, but a fresh percentage worth keeping — so the limit
// is folded onto the existing sample instead.
func appendSample(session *Session, line []byte, since time.Time, previous *transcripts.CodexUsage) {
	var entry transcripts.RolloutLine
	if json.Unmarshal(line, &entry) != nil || entry.Type != "event_msg" {
		return
	}
	var payload transcripts.TokenCount
	if json.Unmarshal(entry.Payload, &payload) != nil || payload.Type != "token_count" {
		return
	}
	at, err := time.Parse(time.RFC3339, entry.Timestamp)
	if err != nil {
		return
	}
	local := at.Local()
	limit := limitOf(payload)

	if local.Before(since) {
		if payload.Info != nil {
			*previous = payload.Info.Total
		}
		return
	}
	// An unchanged session total means no new call happened — this event only
	// refreshed the quota reading. Recording it unbilled keeps the percentage
	// trajectory intact without charging the call it echoes a second time.
	if payload.Info == nil || payload.Info.Total == *previous {
		if limit != nil {
			session.Samples = append(session.Samples, Sample{At: local, Limit: limit})
		}
		return
	}
	*previous = payload.Info.Total
	session.Samples = append(session.Samples, Sample{
		At:            local,
		Usage:         usageOf(payload.Info.Last),
		ContextWindow: payload.Info.ContextWindow,
		Limit:         limit,
		Billed:        true,
	})
}

// idFromPath recovers a thread id from its filename when session_meta is
// unreadable: rollout-<RFC3339-ish timestamp>-<uuid>.jsonl.
func idFromPath(path string) string {
	name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if parts := strings.SplitN(strings.TrimPrefix(name, "rollout-"), "-", 4); len(parts) == 4 {
		return parts[3]
	}
	return ""
}

// loadNames reads the thread titles Codex keeps outside the rollouts. The
// index is append-only and a thread is renamed by appending, so the last entry
// for an id wins.
func loadNames(env Env) map[string]string {
	names := map[string]string{}
	file, err := os.Open(filepath.Join(env.Home, ".codex", "session_index.jsonl"))
	if err != nil {
		return names
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var entry struct {
			ID   string `json:"id"`
			Name string `json:"thread_name"`
		}
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && entry.ID != "" && entry.Name != "" {
			names[entry.ID] = entry.Name
		}
	}
	return names
}
