package memo

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Event is one line of usage.jsonl: a row was cited, shown, its note read,
// or explicitly touched, by a session (empty for a manual CLI call).
type Event struct {
	Row     int    `json:"row"`
	Kind    string `json:"kind"`
	At      string `json:"at"`
	Session string `json:"session,omitempty"`
}

// Weights per event kind; unknown kinds score nothing.
var Weights = map[string]float64{"cite": 1, "show": 1, "read": 1, "touch": 2}

const (
	halfLifeDays = 90.0
	staleDays    = 180.0
)

// LoadEvents reads usage.jsonl; a missing file is an empty history.
func LoadEvents(home string) ([]Event, *Diag, error) {
	path := filepath.Join(home, UsageFile)
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	var events []Event
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	line := 0
	for sc.Scan() {
		line++
		if len(sc.Bytes()) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil || e.Row <= 0 || e.Kind == "" || e.At == "" {
			return nil, &Diag{Code: DiagLedgerInvalid, Severity: "error", Line: line, Detail: fmt.Sprintf("%s:%d is not a usage event", path, line), Fix: fmt.Sprintf("remove %s:%d by hand", path, line)}, nil
		}
		events = append(events, e)
	}
	return events, nil, sc.Err()
}

// AppendEvents appends the events not already present as (row, kind, session)
// and returns how many landed. Events without a session always land.
func AppendEvents(home string, existing, incoming []Event) (int, error) {
	seen := map[string]bool{}
	for _, e := range existing {
		seen[eventKey(e)] = true
	}
	f, err := os.OpenFile(filepath.Join(home, UsageFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	added := 0
	for _, e := range incoming {
		key := eventKey(e)
		if e.Session != "" && seen[key] {
			continue
		}
		seen[key] = true
		line, err := json.Marshal(e)
		if err != nil {
			return added, err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return added, err
		}
		added++
	}
	return added, nil
}

func eventKey(e Event) string { return fmt.Sprintf("%d|%s|%s", e.Row, e.Kind, e.Session) }

// NewEvent stamps an event at now for the session.
func NewEvent(row int, kind, session string, now time.Time) Event {
	return Event{Row: row, Kind: kind, At: now.UTC().Format(time.RFC3339), Session: session}
}

// Score is the decayed usage of one row: sum of weight * 0.5^(age/90d).
func Score(events []Event, row int, now time.Time) float64 {
	total := 0.0
	for _, e := range events {
		if e.Row != row {
			continue
		}
		at, err := time.Parse(time.RFC3339, e.At)
		if err != nil {
			continue
		}
		age := now.Sub(at).Hours() / 24
		total += Weights[e.Kind] * math.Pow(0.5, age/halfLifeDays)
	}
	return total
}

func newestEvent(events []Event, row int) (time.Time, bool) {
	var newest time.Time
	found := false
	for _, e := range events {
		if e.Row != row {
			continue
		}
		if at, err := time.Parse(time.RFC3339, e.At); err == nil && at.After(newest) {
			newest, found = at, true
		}
	}
	return newest, found
}

// Ranked is a row with its render order inputs.
type Ranked struct {
	Row   Row     `json:"row"`
	Score float64 `json:"score"`
}

// Order returns the renderable rows: active only, not stale, pinned first,
// then score descending, then newest id first.
func Order(l *Ledger, events []Event, now time.Time) []Ranked {
	var out []Ranked
	for _, r := range l.Rows {
		if status, _ := l.Status(r.ID); status != "active" {
			continue
		}
		if !r.Pinned && stale(r, events, now) {
			continue
		}
		out = append(out, Ranked{Row: r, Score: Score(events, r.ID, now)})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Row.Pinned != b.Row.Pinned {
			return a.Row.Pinned
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.Row.ID > b.Row.ID
	})
	return out
}

func stale(r Row, events []Event, now time.Time) bool {
	created, err := time.Parse("2006-01-02", r.Date)
	if err != nil || now.Sub(created).Hours()/24 <= staleDays {
		return false
	}
	newest, found := newestEvent(events, r.ID)
	return !found || now.Sub(newest).Hours()/24 > staleDays
}
