package journeys

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestPublicationPreviewDoesNotFinishAttempt(t *testing.T) {
	l := fixture(t)
	p := planFor(t, l)
	s := Store{Root: t.TempDir()}
	a, err := s.Begin(p, "journey", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Append(a.ID, Event{Cell: "base", Kind: "verdict", Actor: "tester", Verdict: "blocked", Reason: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PreparePublication(l, a.ID, "sample", nil); err == nil {
		t.Fatal("unfinished publication accepted")
	}
	if _, err = os.Stat(filepath.Join(s.Root, a.ID, "summary.json")); !os.IsNotExist(err) {
		t.Fatalf("preview finalized attempt: %v", err)
	}
	if _, err = s.Finish(a.ID); err != nil {
		t.Fatalf("explicit finish failed: %v", err)
	}
	if _, err = s.PreparePublication(l, a.ID, "sample", nil); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkJournalDecode(b *testing.B) {
	for _, count := range []int{500, 2000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			var buf bytes.Buffer
			for i := 1; i <= count; i++ {
				value, _ := json.Marshal(Event{Sequence: i, At: time.Unix(1, 0), Kind: "observation", Cell: "base", Actor: "customer", Observed: "fixture"})
				buf.Write(value)
				buf.WriteByte('\n')
			}
			path := filepath.Join(b.TempDir(), "journal.jsonl")
			if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, _, _, err := readJournal(path); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestCoverageRejectsTamperedSeal(t *testing.T) {
	l := fixture(t)
	p := planFor(t, l)
	p.Snapshot.Namespace = "tampered"
	if _, err := (Store{Root: t.TempDir()}).CoverageAt(l, &p, &p.Snapshot); err == nil {
		t.Fatal("tampered coverage plan accepted")
	}
}

func TestProtocolFailureWithholdsDecodedData(t *testing.T) {
	l, p, _ := startupFixture(t)
	for _, mode := range []string{"trailing", "version"} {
		t.Setenv("JOURNEYS_TEST_INVALID_RECEIPT", mode)
		receipt, err := Invoke(l.Root, p.Adapter, Request{Version: 1, Operation: "snapshot", Plan: &p})
		if err == nil || !reflect.DeepEqual(receipt, Receipt{}) {
			t.Fatalf("invalid receipt exposed fields: %+v %v", receipt, err)
		}
	}
}
