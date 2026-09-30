package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/henderson-tech/vybava/internal/journeys"
)

func TestJourneyCoverageAdapterProcess(t *testing.T) {
	mode := os.Getenv("JOURNEYS_COVERAGE_TEST")
	if mode == "" {
		return
	}
	var req journeys.Request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil || req.Plan == nil {
		os.Exit(4)
	}
	// A pure pin snapshot succeeds even when readiness fails. The CLI must
	// choose readiness, or identical pins incorrectly keep a past pass fresh.
	r := journeys.Receipt{Version: 1, OK: req.Operation != "readiness" || mode == "ready", Operation: req.Operation, Snapshot: &req.Plan.Snapshot, Diagnostics: []journeys.Diagnostic{}, Next: []string{}}
	if !r.OK {
		r.Diagnostics = append(r.Diagnostics, journeys.Diagnostic{Code: "CAPABILITY_MISSING", Message: "API unavailable"})
	}
	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		os.Exit(4)
	}
	if !r.OK {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestJourneyCoverageRequiresLiveReadiness(t *testing.T) {
	root, private := t.TempDir(), t.TempDir()
	write := func(path string, b []byte) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSONFile := func(path string, value interface{}) {
		t.Helper()
		b, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		write(path, b)
	}
	for _, d := range []struct{ name, kind, extra string }{
		{"run", "journey", "mode: SAMPLE\nactors: [customer, worker]\nincludes: [shared]\nseed: seed.md\nverify: verify.md\nstory: test/1\n"},
		{"shared", "reference", ""}, {"seed", "seed", ""}, {"verify", "verification", ""},
	} {
		write(filepath.Join(root, "pack", d.name+".md"), []byte(fmt.Sprintf("---\nschema: user-journeys/v1\nname: %s\ndescription: Exercise a meaningful outcome.\ntype: %s\nstatus: draft\ntags: [qa]\naliases: []\nlast-verified: \"2026-09-26\"\n%s---\n\nBody.\n", d.name, d.kind, d.extra)))
	}
	l, err := journeys.Load(root, "pack")
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Valid(); err != nil {
		t.Fatal(err)
	}
	p := journeys.Plan{Adapter: "test", Snapshot: journeys.Snapshot{Revision: "rev", SourceHash: "source", Namespace: "owned", TargetFingerprint: "target"}, Cells: []journeys.Cell{{ID: "base", Mode: "sample", Scenario: "lifecycle", Topology: "two-device", Lane: "live-sandbox", Condition: "foreground", Required: []string{"assignment"}}}}
	if err := p.Seal(l); err != nil {
		t.Fatal(err)
	}
	planPath := filepath.Join(private, "plan.json")
	writeJSONFile(planPath, p)
	s := journeys.Store{Root: filepath.Join(private, "attempts")}
	a, err := s.Begin(p, "journey", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []journeys.Event{
		{Kind: "observation", Actor: "customer", Observed: "Completed", Evidence: []string{"private://customer"}},
		{Kind: "observation", Actor: "worker", Observed: "Completed", Evidence: []string{"private://worker"}},
		{Kind: "verification", Actor: "backend", Check: "assignment", Observed: "One assignment", Source: "read-only API", Result: "pass", Evidence: []string{"private://assignment"}},
		{Kind: "verdict", Actor: "tester", Verdict: "pass"},
	} {
		event.Cell = "base"
		if _, err := s.Append(a.ID, event); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Finish(a.ID); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeJSONFile(filepath.Join(root, ".journeys", "adapters.json"), journeys.Registry{Version: 1, Adapters: map[string]journeys.Adapter{"test": {Command: []string{exe, "-test.run=^TestJourneyCoverageAdapterProcess$"}, Env: []string{"JOURNEYS_COVERAGE_TEST"}}}})
	for _, mode := range []string{"ready", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("JOURNEYS_COVERAGE_TEST", mode)
			var out bytes.Buffer
			cmd, err := (App{Stdout: &out, Stderr: &out}).Command("journeys")
			if err != nil {
				t.Fatal(err)
			}
			cmd.SetArgs([]string{"coverage", "--root", root, "--library", "pack", "--plan", planPath, "--private", s.Root, "--json"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("coverage failed: %v: %s", err, &out)
			}
			var envelope struct {
				OK   bool              `json:"ok"`
				Data journeys.Coverage `json:"data"`
			}
			if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
				t.Fatal(err)
			}
			wantPass, wantStale := 1, 0
			if mode == "blocked" {
				wantPass, wantStale = 0, 1
			}
			if !envelope.OK || envelope.Data.Counts["pass"] != wantPass || envelope.Data.Counts["stale"] != wantStale || envelope.Data.Stale != wantStale {
				t.Fatalf("incorrect freshness: %+v", envelope)
			}
		})
	}
}
