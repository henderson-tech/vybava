package journeys

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProbePreservesPassedAndFailedObservations(t *testing.T) {
	for _, mode := range []string{"pass", "fail", "forged", "evidence", "missing"} {
		t.Run(mode, func(t *testing.T) {
			l, p, _ := startupFixture(t)
			t.Setenv("JOURNEYS_TEST_VERIFY", mode)
			path := filepath.Join(t.TempDir(), "probe.json")
			r, err := Probe(l.Root, l, p, json.RawMessage("{\n\"check\":\"fixture\"\n}"), path)
			if (err == nil) != (mode == "pass") || r.OK != (mode == "pass") || r.State != "done" {
				t.Fatalf("wrong probe result: %+v %v", r, err)
			}
			var saved ProbeRecord
			if err := ReadJSON(path, &saved); err != nil {
				t.Fatal(err)
			}
			if saved.PlanHash != p.Hash || saved.RequestHash != Digest([]byte(`{"check":"fixture"}`)) || saved.OK != r.OK || saved.State != "done" {
				t.Fatalf("lost receipt: %+v", saved)
			}
			if mode == "fail" && (saved.Receipt.Verification == nil || saved.Receipt.Verification.Result != "fail") {
				t.Fatal("failed evidence discarded")
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Probe(l.Root, l, p, json.RawMessage(`{"check":"fixture"}`), path); err == nil {
				t.Fatal("overwrote prior observation")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("retry changed immutable receipt")
			}
			calls, err := os.ReadFile(filepath.Join(l.Root, "calls"))
			if err != nil || string(calls) != "snapshot\nverify\n" {
				t.Fatalf("retry invoked adapter: %s %v", calls, err)
			}
			st, err := os.Stat(path)
			if err != nil || st.Mode().Perm() != 0600 {
				t.Fatal("receipt is not private")
			}
		})
	}
}

func TestProbeRejectsEditedSealAndChangedLivePins(t *testing.T) {
	t.Run("edited", func(t *testing.T) {
		l, p, _ := startupFixture(t)
		p.Snapshot.Namespace = "changed"
		_, err := Probe(l.Root, l, p, json.RawMessage(`{"check":"fixture"}`), filepath.Join(t.TempDir(), "probe.json"))
		if err == nil || !strings.Contains(err.Error(), "PLAN_STALE") {
			t.Fatalf("edited plan accepted: %v", err)
		}
		if _, err := os.Stat(filepath.Join(l.Root, "calls")); !os.IsNotExist(err) {
			t.Fatal("adapter ran for edited plan")
		}
	})
	t.Run("live drift", func(t *testing.T) {
		l, p, _ := startupFixture(t)
		t.Setenv("JOURNEYS_TEST_DRIFT", "1")
		if err := os.WriteFile(filepath.Join(l.Root, "running"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "probe.json")
		r, err := Probe(l.Root, l, p, json.RawMessage(`{"check":"fixture"}`), path)
		if err == nil || !strings.Contains(err.Error(), "PLAN_STALE") || r.OK {
			t.Fatalf("drift accepted: %+v %v", r, err)
		}
		calls, err := os.ReadFile(filepath.Join(l.Root, "calls"))
		if err != nil || string(calls) != "snapshot\n" {
			t.Fatal("verification ran after drift")
		}
	})
}
