package transcripts

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RolloutRoots are the trees under ~/.codex that hold rollouts. Archived
// threads still count against a limit spent before they were archived.
var RolloutRoots = []string{"sessions", "archived_sessions"}

// RolloutLine is the envelope of every rollout record.
type RolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

// SessionMeta is a rollout's session_meta payload. The FIRST one names the
// rollout's owner; forks then serialize copied ancestor headers after it.
type SessionMeta struct {
	ID         string `json:"id"`
	Timestamp  string `json:"timestamp"`
	CWD        string `json:"cwd"`
	CLIVersion string `json:"cli_version"`
	Git        *struct {
		Branch        string `json:"branch"`
		CommitHash    string `json:"commit_hash"`
		RepositoryURL string `json:"repository_url"`
	} `json:"git"`
}

// TurnContext is a turn_context payload: the model (session_meta carries none)
// and the working directory of the turns that follow.
type TurnContext struct {
	Model string `json:"model"`
	CWD   string `json:"cwd"`
}

// CodexUsage is Codex's token accounting. Cached — and the cache writes some
// producers report — are SUBSETS of Input, never siblings: summing them
// double-counts every cache hit. Reasoning is part of Output.
type CodexUsage struct {
	Input      int64 `json:"input_tokens"`
	Cached     int64 `json:"cached_input_tokens"`
	CacheWrite int64 `json:"cache_write_input_tokens"`
	Output     int64 `json:"output_tokens"`
	Reasoning  int64 `json:"reasoning_output_tokens"`
	Total      int64 `json:"total_tokens"`
}

// Valid rejects counters whose cache subsets exceed the input they belong to.
func (u CodexUsage) Valid() bool {
	return u.Input >= 0 && u.Output >= 0 && u.Cached >= 0 && u.CacheWrite >= 0 && u.Cached+u.CacheWrite <= u.Input
}

// TokenCount is an event_msg token_count payload: the call's usage and the
// account's live rate-limit reading. A nil Info is a rate-limit refresh only.
type TokenCount struct {
	Type string `json:"type"`
	Info *struct {
		Total         CodexUsage `json:"total_token_usage"`
		Last          CodexUsage `json:"last_token_usage"`
		ContextWindow int64      `json:"model_context_window"`
	} `json:"info"`
	RateLimits *struct {
		LimitID  string `json:"limit_id"`
		PlanType string `json:"plan_type"`
		Primary  *struct {
			UsedPercent   float64 `json:"used_percent"`
			WindowMinutes int     `json:"window_minutes"`
			ResetsAt      int64   `json:"resets_at"`
		} `json:"primary"`
	} `json:"rate_limits"`
}

// UsageRecord is a token_usage_record payload (Codex CLI 0.153+): one exact
// receipt per model response. Copied fork history keeps the ORIGINAL owner's
// ThreadID, which is how a reader tells a copy from new work.
type UsageRecord struct {
	ThreadID   string     `json:"thread_id"`
	ResponseID string     `json:"response_id"`
	Usage      CodexUsage `json:"usage"`
}

// Rollout record types a usage reader cares about.
const (
	RolloutSessionMeta = "session_meta"
	RolloutTurnContext = "turn_context"
	RolloutUsageRecord = "token_usage_record"
	RolloutEventMsg    = "event_msg"
	EventTokenCount    = "token_count"
)

// RolloutUsageLine is the cheap prefilter: a rollout is mostly transcript, and
// only these record types carry identity, model or usage evidence.
// The whole line is searched: key order is the writer's choice.
func RolloutUsageLine(line []byte) bool {
	for _, marker := range rolloutMarkers {
		if bytes.Contains(line, marker) {
			return true
		}
	}
	return false
}

var rolloutMarkers = [][]byte{[]byte(`"session_meta"`), []byte(`"turn_context"`), []byte(`"token_usage_record"`), []byte(`"token_count"`)}

// RolloutPaths lists every rollout-*.jsonl under home/.codex whose
// modification time is not before since (the zero time lists everything).
// A rollout untouched since before a window cannot hold a call inside it.
func RolloutPaths(home string, since time.Time) ([]string, error) {
	return RolloutPathsIn(filepath.Join(home, ".codex"), since)
}

// RolloutPathsIn is RolloutPaths for an explicit Codex directory ($CODEX_HOME).
func RolloutPathsIn(codexDir string, since time.Time) ([]string, error) {
	var paths []string
	for _, root := range RolloutRoots {
		dir := filepath.Join(codexDir, root)
		err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if entry.IsDir() || !strings.HasPrefix(entry.Name(), "rollout-") || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				if since.IsZero() {
					paths = append(paths, path) // a full listing keeps it; the caller's stat decides
				}
				return nil
			}
			if info.ModTime().Before(since) {
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	sort.Strings(paths)
	return paths, nil
}
