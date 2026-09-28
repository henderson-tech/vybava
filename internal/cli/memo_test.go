package cli

import (
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/memo"
)

// The --supersedes / --retires flags write the marker, accept one the author
// already wrote for the same row, and refuse one that names another row.
func TestMarkSentence(t *testing.T) {
	cases := []struct {
		name, sentence, want string
		supersedes, retires  int
		refused              bool
	}{
		{name: "flag writes the retires marker", sentence: "Fixed upstream.", retires: 7, want: "retires #7. Fixed upstream."},
		{name: "author's colon marker for the same row is kept", sentence: "retires #7: fixed upstream.", retires: 7, want: "retires #7: fixed upstream."},
		{name: "a marker for another row is refused", sentence: "retires #8: fixed upstream.", retires: 7, refused: true},
		{name: "flag writes the supersedes marker", sentence: "The new rule.", supersedes: 3, want: "supersedes #3: The new rule."},
		{name: "a malformed supersedes marker is refused", sentence: "supersedes #3 the new rule.", supersedes: 3, refused: true},
	}
	for _, c := range cases {
		got, problem, fix := markSentence(c.sentence, "", "feedback/git", c.supersedes, c.retires)
		if c.refused {
			if problem == "" || fix == "" {
				t.Errorf("%s: want a refusal with a fix, got %q", c.name, got)
			}
			continue
		}
		if problem != "" || got != c.want {
			t.Errorf("%s: got %q (%s), want %q", c.name, got, problem, c.want)
		}
	}
}

// A row's creation time is its `add` event, so add and import must stamp
// exactly one per new row and leave earlier events untouched.
func TestRecordAddedStampsOneAddEventPerRow(t *testing.T) {
	home := t.TempDir()
	env := memo.Env{Session: "sess-1"}
	now := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)

	first, err := recordAdded(env, home, []memo.Row{{ID: 1}, {ID: 2}}, now)
	if err != nil || len(first) != 2 {
		t.Fatalf("import of two rows: %d events, %v", len(first), err)
	}
	later, err := recordAdded(env, home, []memo.Row{{ID: 3}}, now.Add(time.Hour))
	if err != nil || len(later) != 3 {
		t.Fatalf("add after import: %d events, %v", len(later), err)
	}
	for i, e := range later {
		if e.Kind != "add" || e.Row != i+1 || e.Session != "sess-1" {
			t.Errorf("event %d = %+v", i, e)
		}
	}
	if later[0].At != now.Format(time.RFC3339) || later[2].At != now.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("timestamps: %v %v", later[0].At, later[2].At)
	}
	// Re-stamping the same row in the same session is a no-op, never a duplicate.
	again, err := recordAdded(env, home, []memo.Row{{ID: 3}}, now.Add(2*time.Hour))
	if err != nil || len(again) != 3 {
		t.Fatalf("duplicate add: %d events, %v", len(again), err)
	}
	if again[2].At != later[2].At || again[2].Row != 3 || again[2].Kind != "add" {
		t.Errorf("re-stamp must leave the original event untouched: %+v", again[2])
	}
}
