package fleet

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

func TestLedgerRecordShowRoundTrip(t *testing.T) {
	f := newFixture(t)
	f.register(t, 71, "s1", "busy", f.vybava, time.Minute, nil)
	env := f.env(table(claude(71)))

	secret := "devbox run -- env TOKEN=abc123 make verify"
	_, err := Record(env, "s1", Event{Kind: JobShell, ID: "bg-1", Status: JobStarted, Command: secret,
		Description: "Run   the\nverify suite " + strings.Repeat("x", 200)})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := Show(env, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if ledger.open() != 1 || ledger.Owner == nil || ledger.Owner.PID != 71 || ledger.CWD != f.vybava {
		t.Fatalf("ledger = %+v", ledger)
	}
	job := ledger.Jobs[0]
	if len(job.Digest) != digestHexChars || !strings.HasPrefix(job.Description, "Run the verify suite") || len([]rune(job.Description)) != maxDescription {
		t.Fatalf("job = %+v", job)
	}
	raw, err := os.ReadFile(filepath.Join(f.ledgers, "s1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "abc123") || strings.Contains(string(raw), "devbox run") {
		t.Fatalf("the ledger stored command text: %s", raw)
	}

	ledger, err = Record(env, "s1", Event{Kind: JobShell, ID: "bg-1", Status: JobCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if ledger.open() != 0 || ledger.Jobs[0].EndedAt == nil || !ledger.Jobs[0].StartedAt.Equal(job.StartedAt) {
		t.Fatalf("completed job = %+v", ledger.Jobs[0])
	}

	empty, err := Show(env, "never-recorded")
	if err != nil || len(empty.Jobs) != 0 || empty.Version != LedgerVersion {
		t.Fatalf("empty ledger = %+v, %v", empty, err)
	}
}

func TestLedgerRefusesBadInput(t *testing.T) {
	f := newFixture(t)
	env := f.env(table())
	cases := []struct {
		session string
		event   Event
		code    string
	}{
		{"../escape", Event{Kind: JobShell, ID: "a", Status: JobStarted}, DiagSessionInvalid},
		{"s", Event{Kind: "cron", ID: "a", Status: JobStarted}, DiagLedgerEventInvalid},
		{"s", Event{Kind: JobShell, ID: "a", Status: "paused"}, DiagLedgerEventInvalid},
		{"s", Event{Kind: JobShell, ID: "", Status: JobStarted}, DiagLedgerEventInvalid},
	}
	for _, c := range cases {
		_, err := Record(env, c.session, c.event)
		var derr runx.DiagError
		if !errors.As(err, &derr) || derr.Diag.Code != c.code {
			t.Errorf("Record(%q, %+v) = %v, want %s", c.session, c.event, err, c.code)
		}
	}
	if _, err := ReadEvent([]byte(`{"kind":"shell","id":"a","status":"started","command_text":"x"}`)); err == nil {
		t.Fatal("an unknown field must be refused")
	}
	if _, err := ReadEvent([]byte(`{"kind":"shell","id":"a","status":"started"}{"kind":"shell","id":"b","status":"started"}`)); err == nil {
		t.Fatal("two events on stdin must be refused")
	}
}

func TestLedgerConcurrentRecordsAllLandAtomically(t *testing.T) {
	f := newFixture(t)
	env := f.env(table())
	const writers = 24
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := Record(env, "busy-session", Event{Kind: JobAgent, ID: fmt.Sprintf("agent-%02d", i), Status: JobStarted})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	ledger, err := Show(env, "busy-session")
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.Jobs) != writers {
		t.Fatalf("%d jobs landed, want %d — a read-modify-write interleaved", len(ledger.Jobs), writers)
	}
	leftovers, _ := filepath.Glob(filepath.Join(f.ledgers, ".*.tmp"))
	if len(leftovers) != 0 {
		t.Fatalf("temp files left behind: %v", leftovers)
	}
}

func TestLedgerPruneKeepsOpenJobs(t *testing.T) {
	ledger := Ledger{Jobs: []Job{}}
	ledger.apply(Event{Kind: JobWorkflow, ID: "open", Status: JobStarted}, now)
	for i := 0; i < maxJobs+10; i++ {
		ledger.apply(Event{Kind: JobShell, ID: fmt.Sprintf("done-%03d", i), Status: JobCompleted}, now.Add(time.Duration(i)*time.Second))
	}
	if len(ledger.Jobs) != maxJobs || ledger.Jobs[0].ID != "open" || ledger.open() != 1 {
		t.Fatalf("pruned to %d jobs, first %q, open %d", len(ledger.Jobs), ledger.Jobs[0].ID, ledger.open())
	}
}

func TestLedgerCloseStopsOpenJobs(t *testing.T) {
	f := newFixture(t)
	env := f.env(table())
	for _, id := range []string{"a", "b"} {
		if _, err := Record(env, "s", Event{Kind: JobMonitor, ID: id, Status: JobStarted}); err != nil {
			t.Fatal(err)
		}
	}
	ledger, err := Close(env, "s")
	if err != nil {
		t.Fatal(err)
	}
	if ledger.open() != 0 || ledger.Jobs[0].Status != JobStopped {
		t.Fatalf("ledger = %+v", ledger)
	}
}
