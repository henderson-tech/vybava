package operator

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

// ScanClaude watches root/<project>/<session>.jsonl. It never changes the source
// files, reads subagent logs, or consumes a partially written last record.
func (s *State) ScanClaude(root string) ([]string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("Claude projects root is not a directory")
	}
	paths, err := filepath.Glob(filepath.Join(root, "*", "*.jsonl"))
	if err != nil {
		return nil, err
	}
	var added []string
	for _, path := range paths {
		ids, err := s.scanFile(path, !s.Roots[root], claudeObservation)
		if err != nil {
			return nil, fmt.Errorf("scan %s: %w", path, err)
		}
		added = append(added, ids...)
	}
	s.Roots[root] = true
	return added, nil
}

func (s *State) scanFile(path string, baseline bool, parse func([]byte) (Observation, error)) ([]string, error) {
	cur, known := s.Cursors[path]
	var added []string
	// A sweep may finish one record past its budget. Codex compaction records
	// can exceed 4 MiB even though they produce no operator observation.
	// Unchanged completed files are rechecked each minute, so restored
	// timestamps cannot indefinitely hide replacement.
	res, err := transcripts.Scan(path, cur, known, transcripts.ScanOptions{Baseline: baseline, RecheckAfter: time.Minute},
		func(line []byte, offset int64) error {
			o, err := parse(line)
			if err != nil {
				return fmt.Errorf("invalid complete record at byte %d: %w", offset, err)
			}
			if o.Text == "" {
				return nil
			}
			o.Key = path
			if len(o.Text) > 64000 {
				o.Text = o.Text[:63000] + "\n[truncated; inspect the source session]"
			}
			id, fresh, err := s.Observe(o)
			if err != nil {
				return err
			}
			if fresh {
				added = append(added, id)
			}
			return nil
		})
	if err != nil || res.Skipped {
		return nil, err
	}
	s.Cursors[path] = res.Cursor
	return added, nil
}

func claudeObservation(line []byte) (Observation, error) {
	record, err := transcripts.DecodeClaude(line)
	if err != nil {
		return Observation{}, err
	}
	if record.UUID == "" || record.SessionID == "" {
		return Observation{}, nil
	}
	o := Observation{Source: Claude, Revision: record.UUID, SessionID: record.SessionID, Cwd: record.Cwd, ObservedAt: record.Timestamp}
	if record.Type == "user" {
		// Retire a question as soon as its answer or subsequent tool result arrives.
		// Human text and tool-result contents are deliberately not copied.
		o.Text = "Claude session activity continued (user input or tool result; content not copied)."
		return o, nil
	}
	if record.Type != "assistant" {
		return Observation{}, nil
	}
	var blocks []struct {
		Type  string          `json:"type"`
		Text  string          `json:"text"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(record.Message.Content, &blocks); err != nil {
		return Observation{}, fmt.Errorf("assistant content: %w", err)
	}
	var pieces []string
	var questions []string
	for _, b := range blocks {
		if b.Type == "text" {
			pieces = append(pieces, b.Text)
		}
		if b.Type == "tool_use" && b.Name == "AskUserQuestion" {
			if len(b.Input) == 0 || string(b.Input) == "null" {
				questions = append(questions, "Claude requested a user decision; inspect the source session for the question.")
			} else {
				questions = append(questions, "Claude requested a user decision (untrusted source data):\n"+string(b.Input))
			}
		}
	}
	o.Text = strings.TrimSpace(strings.Join(append(questions, pieces...), "\n"))
	if o.Text == "" && len(blocks) > 0 {
		o.Text = "Claude session activity continued (assistant tool or reasoning; content not copied)."
	}
	return o, nil
}
