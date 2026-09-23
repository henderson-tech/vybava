// Package operator records observations, Codex delivery receipts and human-rated
// proposals. It deliberately has no message sending or automatic promotion API.
package operator

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

type Source string

const (
	Claude   Source = "claude"
	Codex    Source = "codex"
	WhatsApp Source = "whatsapp"
	Messages Source = "messages"
)

type Observation struct {
	Source     Source    `json:"source"`
	Key        string    `json:"key"`
	Revision   string    `json:"revision"`
	Text       string    `json:"text"`
	ObservedAt time.Time `json:"observed_at"`
	SessionID  string    `json:"session_id,omitempty"`
	Cwd        string    `json:"cwd,omitempty"`
}

type Rating struct {
	Score      int       `json:"score"`
	Correction string    `json:"correction,omitempty"`
	At         time.Time `json:"at"`
}

type Proposal struct {
	Text       string          `json:"text"`
	CreatedAt  time.Time       `json:"created_at"`
	Superseded bool            `json:"superseded"`
	Rating     *Rating         `json:"rating,omitempty"`
	Feedback   []Feedback      `json:"feedback,omitempty"`
	Decision   *ReviewDecision `json:"decision,omitempty"`
}

type Event struct {
	ID string `json:"id"`
	Observation
	Delivery       string     `json:"delivery"`
	Thread         string     `json:"thread,omitempty"`
	Receipt        string     `json:"receipt,omitempty"`
	Error          string     `json:"error,omitempty"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	Superseded     bool       `json:"superseded"`
	Proposals      []Proposal `json:"proposals,omitempty"`
	Outcomes       []Outcome  `json:"outcomes,omitempty"`
}

// Cursor is the shared incremental read position; its JSON shape is part of
// the persisted state.
type Cursor = transcripts.Cursor

type State struct {
	Version    int               `json:"version"`
	Roots      map[string]bool   `json:"roots"`
	Cursors    map[string]Cursor `json:"cursors"`
	Events     []Event           `json:"events"`
	LastScanAt *time.Time        `json:"last_scan_at,omitempty"`
	Messages   *MessagesState    `json:"messages,omitempty"`
}

type Store struct{ Dir string }

// View reads the last atomically published state without waiting for a scan.
// Callback changes are local only; writers must use With.
func (s Store) View(fn func(*State) error) error {
	if s.Indexed() {
		return s.indexedState(false, "ORDER BY seq", nil, fn)
	}
	if err := s.prepare(); err != nil {
		return err
	}
	state, _, err := s.load()
	if err != nil {
		return err
	}
	return fn(&state)
}

func (s Store) prepare() error {
	if err := os.MkdirAll(s.Dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(s.Dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("operator state directory must be a private directory (0700): %s", s.Dir)
	}
	return nil
}

func (s Store) load() (State, []byte, error) {
	state := State{Version: 4, Roots: map[string]bool{}, Cursors: map[string]Cursor{}}
	path := filepath.Join(s.Dir, "state.json")
	var original []byte
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return State{}, nil, errors.New("operator state file must be a private regular file")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return State{}, nil, err
		}
		original = data
		if err := json.Unmarshal(data, &state); err != nil {
			return State{}, nil, fmt.Errorf("read operator state: %w", err)
		}
		if (state.Version < 1 || state.Version > 4) || state.Roots == nil || state.Cursors == nil {
			return State{}, nil, errors.New("unsupported or incomplete operator state")
		}
		if state.Messages != nil && (state.Messages.Database == "" || state.Messages.Baseline < 0 || state.Messages.Records == nil) {
			return State{}, nil, errors.New("incomplete Messages source state")
		}
		if state.Messages != nil && state.Messages.Initialized && state.Messages.Baseline > 0 && state.Messages.AnchorGUID == "" {
			return State{}, nil, errors.New("Messages baseline identity is missing")
		}
		// Older binaries must not silently drop Messages cursors/coverage.
		state.Version = 4
	} else if !errors.Is(err, os.ErrNotExist) {
		return State{}, nil, err
	}
	return state, original, nil
}

func (s Store) With(fn func(*State) error) error {
	if s.Indexed() {
		return s.indexedState(true, "ORDER BY seq", nil, fn)
	}
	if err := s.prepare(); err != nil {
		return err
	}
	unlock, err := lock(filepath.Join(s.Dir, "state.lock"))
	if err != nil {
		return err
	}
	defer unlock()
	state, original, err := s.load()
	if err != nil {
		return err
	}
	path := filepath.Join(s.Dir, "state.json")
	if err := fn(&state); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if bytes.Equal(original, data) {
		return nil
	}
	f, err := os.CreateTemp(s.Dir, ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(s.Dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func digest(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}

func (s *State) Observe(o Observation) (string, bool, error) {
	if o.Source != Claude && o.Source != Codex && o.Source != WhatsApp && o.Source != Messages {
		return "", false, errors.New("source must be claude, codex, whatsapp or messages")
	}
	if strings.TrimSpace(o.Key) == "" || strings.TrimSpace(o.Revision) == "" || strings.TrimSpace(o.Text) == "" {
		return "", false, errors.New("key, revision and text are required")
	}
	if len(o.Text) > 64000 {
		return "", false, errors.New("observation exceeds 64000 bytes; select a smaller conversation excerpt")
	}
	if o.ObservedAt.IsZero() {
		o.ObservedAt = time.Now().UTC()
	}
	identity, _ := json.Marshal([]string{string(o.Source), o.Key, o.Revision})
	id := digest(string(identity))[:24]
	for i := range s.Events {
		e := &s.Events[i]
		if e.ID == id {
			if e.Text != o.Text {
				return "", false, errors.New("same revision has different text; capture a new revision")
			}
			return id, false, nil
		}
	}
	for i := range s.Events {
		e := &s.Events[i]
		if e.Source == o.Source && e.Key == o.Key {
			e.Superseded = true
			for j := range e.Proposals {
				e.Proposals[j].Superseded = true
			}
		}
	}
	s.Events = append(s.Events, Event{ID: id, Observation: o, Delivery: "pending"})
	return id, true, nil
}

func (s *State) Find(id string) (*Event, error) {
	for i := range s.Events {
		if s.Events[i].ID == id {
			return &s.Events[i], nil
		}
	}
	return nil, fmt.Errorf("unknown event %q", id)
}

// NextDelivery coalesces changed context and allows only one unacknowledged
// submission. A missing receiver must not grow an unbounded Codex queue.
func (s *State) NextDelivery() string {
	for _, e := range s.Events {
		if e.AcknowledgedAt == nil && (e.Delivery == "queued" || e.Delivery == "submitting" || e.Delivery == "failed") {
			return ""
		}
	}
	for _, e := range s.Events {
		if !e.Superseded && e.Delivery == "pending" && e.AcknowledgedAt == nil {
			return e.ID
		}
	}
	return ""
}

func (s *State) Acknowledge(id string) error {
	e, err := s.Find(id)
	if err != nil {
		return err
	}
	if e.AcknowledgedAt == nil {
		now := time.Now().UTC()
		e.AcknowledgedAt = &now
	}
	return nil
}

func (s *State) Propose(id, text string) error {
	e, err := s.Find(id)
	if err != nil {
		return err
	}
	if err := s.requireMessagesCoverage(e.Source); err != nil {
		return err
	}
	if e.Superseded {
		return errors.New("context changed; prepare a reply for the latest event")
	}
	if strings.TrimSpace(text) == "" || len(text) > 64000 {
		return errors.New("proposal must contain 1–64000 bytes")
	}
	for i := range e.Proposals {
		e.Proposals[i].Superseded = true
	}
	e.Proposals = append(e.Proposals, Proposal{Text: text, CreatedAt: time.Now().UTC()})
	return nil
}

func (s *State) Rate(id string, proposal, score int, correction string) error {
	e, err := s.Find(id)
	if err != nil {
		return err
	}
	if score < 1 || score > 5 {
		return errors.New("score must be 1–5")
	}
	if proposal < 1 || proposal > len(e.Proposals) {
		return errors.New("proposal number does not exist (numbers start at 1)")
	}
	p := &e.Proposals[proposal-1]
	if p.Rating != nil {
		return errors.New("proposal already scored; recorded ratings are immutable")
	}
	p.Rating = &Rating{Score: score, Correction: correction, At: time.Now().UTC()}
	return nil
}

type Summary struct {
	DecisionsPending int     `json:"decisions_pending"`
	Events           int     `json:"events"`
	Pending          int     `json:"pending"`
	Queued           int     `json:"queued"`
	Acknowledged     int     `json:"acknowledged"`
	DeliveryProblems int     `json:"delivery_problems"`
	Proposals        int     `json:"proposals"`
	Scored           int     `json:"scored"`
	Average          float64 `json:"average_score"`
	Sending          string  `json:"sending"`
}

func (s *State) Summary() Summary {
	r := Summary{Events: len(s.Events), Sending: "human-only"}
	total := 0
	for _, e := range s.Events {
		if e.AcknowledgedAt != nil {
			r.Acknowledged++
		}
		if e.AcknowledgedAt == nil {
			switch e.Delivery {
			case "pending":
				if !e.Superseded {
					r.Pending++
				}
			case "queued":
				r.Queued++
			case "submitting", "failed":
				r.DeliveryProblems++
			}
		}
		for _, p := range e.Proposals {
			if !e.Superseded && !p.Superseded && p.Decision == nil {
				r.DecisionsPending++
			}
			r.Proposals++
			if p.Rating != nil {
				r.Scored++
				total += p.Rating.Score
			}
		}
	}
	if r.Scored > 0 {
		r.Average = float64(total) / float64(r.Scored)
	}
	return r
}
