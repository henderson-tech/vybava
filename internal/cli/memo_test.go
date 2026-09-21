package cli

import (
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/memo"
)

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
}
