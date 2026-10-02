package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSummaryKeepsTheWaitingSessionsAndPublishesAtomically(t *testing.T) {
	snap := Snapshot{
		GeneratedAt: now,
		Counts:      Counts{Total: 3, Waiting: 1, Busy: 1, Dead: 1},
		Sessions: []Session{
			{SessionID: "a", Project: "vybava", State: StateWaiting, WaitingFor: "input needed", AgeSeconds: 240},
			{SessionID: "b", Project: "fixit", State: StateBusy},
			{SessionID: "c", Project: "fixit", State: StateDead},
		},
	}
	path := filepath.Join(t.TempDir(), "fleet", "summary.json")
	if err := WriteSummary(path, Summarize(snap)); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got Summary
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Counts != snap.Counts || len(got.Waiting) != 1 || got.Waiting[0].SessionID != "a" || got.Waiting[0].AgeSeconds != 240 {
		t.Fatalf("summary = %+v", got)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}
