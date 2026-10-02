package journeys

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func doc(name, kind, extra, body string) []byte {
	return []byte("---\nschema: user-journeys/v1\nname: " + name + "\ndescription: Exercise a meaningful outcome.\ntype: " + kind + "\nstatus: draft\ntags: [qa]\naliases: []\nlast-verified: \"2026-09-26\"\n" + extra + "---\n\n" + body + "\n")
}
func fixture(t *testing.T) *Library {
	t.Helper()
	root := t.TempDir()
	files := map[string][]byte{
		"run.md":    doc("main-journey", "journey", "mode: SAMPLE\nactors: [customer, worker]\nincludes: [shared]\nseed: seed.md\nverify: verify.md\nstory: test/1\n", "# Main\n[[pack/shared#A heading|read]]\n[[pack/shared#^block]]\n"),
		"shared.md": doc("shared", "reference", "", "# A heading\nText ^block\n\n| ID | Modes | Moment | Person's intent or event | Expected observation |\n|---|---|---|---|---|\n| B1 | all | Before | A \\| B | Both see it |\n"),
		"seed.md":   doc("seed", "seed", "", "Seed prerequisites."), "verify.md": doc("verify", "verification", "", "Read independently.")}
	for name, b := range files {
		if err := writeFile(filepath.Join(root, "pack", name), b, true); err != nil {
			t.Fatal(err)
		}
	}
	l, err := Load(root, "pack")
	if err != nil {
		t.Fatal(err)
	}
	if err = l.Valid(); err != nil {
		t.Fatalf("%v: %+v", err, l.Diagnostics)
	}
	return l
}
func TestDocuments(t *testing.T) {
	l := fixture(t)
	if l.CandidateCount() != 1 || len(l.Scenarios) != 1 || l.Scenarios[0].Intent != "A | B" {
		t.Fatalf("table: %+v", l.Scenarios)
	}
	paths, err := l.Compose("main-journey")
	if err != nil || len(paths) != 2 || !strings.HasSuffix(paths[0], "shared.md") {
		t.Fatalf("compose: %v %v", paths, err)
	}
	for _, d := range l.Documents {
		one, err := Format(d)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := Parse(d.Path, one)
		if err != nil {
			t.Fatal(err)
		}
		two, err := Format(parsed)
		if err != nil || !bytes.Equal(one, two) || !bytes.Equal(d.Body, parsed.Body) {
			t.Fatal("formatter changed body or was not idempotent")
		}
	}
}

func TestLoadThroughSymlinkedRoot(t *testing.T) {
	l := fixture(t)
	alias := filepath.Join(t.TempDir(), "linked-checkout")
	if err := os.Symlink(l.Root, alias); err != nil {
		t.Fatal(err)
	}
	linked, err := Load(alias, "pack")
	if err != nil {
		t.Fatal(err)
	}
	if err := linked.Valid(); err != nil {
		t.Fatalf("linked root rejected: %v: %+v", err, linked.Diagnostics)
	}
	if linked.Root != l.Root || linked.Hash != l.Hash || len(linked.Documents) != len(l.Documents) {
		t.Fatal("linked root changed library identity")
	}
	outside := filepath.Join(t.TempDir(), "outside.md")
	if err := os.WriteFile(outside, doc("outside", "reference", "", "Outside the root."), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(l.Root, "pack", "escape.md")); err != nil {
		t.Fatal(err)
	}
	unsafe, err := Load(alias, "pack")
	if err != nil {
		t.Fatal(err)
	}
	if err := unsafe.Valid(); err == nil {
		t.Fatal("linked root allowed a document to escape")
	}
	if _, err := SafePath(alias, "pack/../pack/run.md"); err == nil {
		t.Fatal("linked root allowed parent traversal")
	}
}

func TestFormatPreservesBodyEndings(t *testing.T) {
	for _, body := range []string{"", "No final newline", "One final newline\n", "Keep intentional blank lines\n\n\n"} {
		d, err := Parse("reference.md", doc("reference", "reference", "", body))
		if err != nil {
			t.Fatal(err)
		}
		formatted, err := Format(d)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := Parse(d.Path, formatted)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(parsed.Body, d.Body) {
			t.Fatalf("body changed: got %q, want %q", parsed.Body, d.Body)
		}
		again, err := Format(parsed)
		if err != nil || !bytes.Equal(formatted, again) {
			t.Fatalf("formatter is not idempotent: %v", err)
		}
	}
}
func TestMalformedProperties(t *testing.T) {
	for _, change := range []struct{ name, old, new string }{{"missing", "name: a\n", ""}, {"duplicate", "name: a", "name: a\nname: b"}, {"date", "2026-09-26", "2026-02-30"}, {"nested", "tags: [qa]", "tags: {x: y}"}, {"unknown", "status: draft", "status: passed"}, {"extension nested", "status: draft", "status: draft\nx-future: {a: b}"}} {
		t.Run(change.name, func(t *testing.T) {
			b := strings.Replace(string(doc("a", "reference", "", "body")), change.old, change.new, 1)
			if _, err := Parse("x.md", []byte(b)); err == nil {
				t.Fatal("invalid document accepted")
			}
		})
	}
}
func TestLinksAndCycles(t *testing.T) {
	for _, body := range []string{"[[../outside]]", "[[pack/shared#missing]]", "[[missing]]"} {
		t.Run(body, func(t *testing.T) {
			l := fixture(t)
			d := l.names["main-journey"]
			d.Body = []byte(body)
			l.links(d)
			if len(l.Diagnostics) == 0 {
				t.Fatal("unresolved link accepted")
			}
		})
	}
	l := fixture(t)
	l.names["shared"].Includes = []string{"main-journey"}
	if _, err := l.Compose("main-journey"); err == nil {
		t.Fatal("cycle accepted")
	}
	l.names["shared"].Includes = nil
	l.names["main-journey"].Includes = []string{"shared", "shared"}
	paths, err := l.Compose("main-journey")
	if err != nil || len(paths) != 2 {
		t.Fatal("repeated include duplicated")
	}
	d := l.names["main-journey"]
	d.Body = []byte("```md\n[[missing]]\n```\nInline `[[missing]]`\n")
	l.links(d)
	if len(l.Diagnostics) > 0 {
		t.Fatal("code example treated as link")
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(l.Root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := SafePath(l.Root, "escape"); err == nil {
		t.Fatal("symlink escape allowed")
	}
}
func planFor(t *testing.T, l *Library) Plan {
	t.Helper()
	p := Plan{Cells: []Cell{{ID: "base", Mode: "sample", Scenario: "lifecycle", Topology: "two-device", Lane: "live-sandbox", Condition: "foreground", Required: []string{"assignment"}}}}
	if err := p.Seal(l); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestJournalRecoveryAndImmutableAttempts(t *testing.T) {
	l := fixture(t)
	p := planFor(t, l)
	s := Store{Root: t.TempDir()}
	a, err := s.Begin(p, "journey", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Append(a.ID, Event{Kind: "verdict", Cell: "base", Actor: "tester", Verdict: "pass"}); err == nil {
		t.Fatal("evidence-free pass accepted")
	}
	for _, actor := range []string{"customer", "worker"} {
		if _, err = s.Append(a.ID, Event{Kind: "observation", Cell: "base", Actor: actor, Observed: "Visible outcome", Evidence: []string{"private://capture/" + actor}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = s.Append(a.ID, Event{Kind: "verification", Cell: "base", Actor: "backend", Check: "assignment", Observed: "One assignment", Source: "read-only API", Result: "pass", Evidence: []string{"private://assignment"}}); err != nil {
		t.Fatal(err)
	}
	dir, _ := s.dir(a.ID)
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(`{"sequence":4`)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, _, _, err = s.Read(a.ID, false); err == nil {
		t.Fatal("incomplete tail silently accepted")
	}
	_, events, n, err := s.Read(a.ID, true)
	if err != nil || n == 0 || len(events) != 3 {
		t.Fatalf("recovery: %d %d %v", len(events), n, err)
	}
	if _, err = s.Append(a.ID, Event{Kind: "verdict", Cell: "base", Actor: "tester", Verdict: "fail", Reason: "Known mismatch"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.Finish(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Finish(a.ID)
	if err != nil || first.JournalHash != again.JournalHash || first.Ended != again.Ended {
		t.Fatal("finish changed the attempt")
	}
	if _, err = s.Append(a.ID, Event{Kind: "other", Cell: "base", Actor: "tester", Observed: "rewrite"}); err == nil {
		t.Fatal("finished attempt changed")
	}
	next, err := s.Begin(p, "journey", a.ID)
	if err != nil || next.ID == a.ID || next.RunID != a.RunID {
		t.Fatal("retest erased identity")
	}
	r, err := s.Coverage(l, &p)
	if err != nil || r.FailedAttempts != 1 || r.Counts["pass"] != 0 {
		t.Fatalf("coverage: %+v %v", r, err)
	}
	first.Cells[0].Verdict = "pass"
	if err := privateJSON(filepath.Join(dir, "summary.json"), first, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(a.ID); err == nil {
		t.Fatal("forged summary verdict accepted")
	}
}
func TestInteriorCorruptionAndWriterLock(t *testing.T) {
	l := fixture(t)
	s := Store{Root: t.TempDir()}
	a, err := s.Begin(planFor(t, l), "preflight", "")
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := s.dir(a.ID)
	unlock, err := lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = s.Read(a.ID, false); err == nil {
		t.Fatal("concurrent writer accepted")
	}
	unlock()
	if err := os.WriteFile(filepath.Join(dir, "events.jsonl"), []byte("bad\n{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err = s.Read(a.ID, true); err == nil {
		t.Fatal("interior corruption repaired silently")
	}
}

func TestSummaryUsesLatestVerificationOutcome(t *testing.T) {
	for _, latest := range []string{"fail", "pending", "unknown", "pass"} {
		t.Run(latest, func(t *testing.T) {
			l := fixture(t)
			s := Store{Root: t.TempDir()}
			a, err := s.Begin(planFor(t, l), "journey", "")
			if err != nil {
				t.Fatal(err)
			}
			for _, result := range []string{"pass", latest} {
				_, err := s.Append(a.ID, Event{Kind: "verification", Cell: "base", Actor: "backend", Check: "assignment", Observed: "Current assignment", Source: "read-only API", Result: result, Evidence: []string{"private://assignment"}})
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Append(a.ID, Event{Kind: "verdict", Cell: "base", Actor: "tester", Verdict: "blocked", Reason: "Actor observations incomplete"}); err != nil {
				t.Fatal(err)
			}
			got, err := s.Finish(a.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if latest == "pass" {
				want = 1
			}
			if len(got.Cells[0].Checks) != want {
				t.Fatalf("obsolete pass survives %s: %+v", latest, got.Cells[0])
			}
		})
	}
}
func TestPreflightDoesNotPassCoverage(t *testing.T) {
	l := fixture(t)
	p := planFor(t, l)
	s := Store{Root: t.TempDir()}
	a, err := s.Begin(p, "preflight", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Append(a.ID, Event{Kind: "verdict", Cell: "base", Actor: "tester", Verdict: "blocked", Reason: "Driver missing"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Finish(a.ID); err != nil {
		t.Fatal(err)
	}
	r, err := s.Coverage(l, &p)
	if err != nil || r.Preflight != 1 || r.Attempts != 0 || r.Counts["not-run"] != 1 {
		t.Fatalf("preflight inflated coverage: %+v %v", r, err)
	}
}

func TestCoveragePreservesUntouchedCellsAcrossBoundedAttempts(t *testing.T) {
	l := fixture(t)
	p := planFor(t, l)
	p.Cells = append(p.Cells, Cell{ID: "second", Mode: "sample", Scenario: "lifecycle", Topology: "two-device", Lane: "live-sandbox", Condition: "background", Required: []string{"assignment"}})
	if err := p.Seal(l); err != nil {
		t.Fatal(err)
	}
	s := Store{Root: t.TempDir()}
	first, err := s.Begin(p, "journey", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"customer", "worker"} {
		if _, err := s.Append(first.ID, Event{Kind: "observation", Cell: "base", Actor: actor, Observed: "Visible", Evidence: []string{"private://capture/" + actor}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append(first.ID, Event{Kind: "verification", Cell: "base", Actor: "backend", Check: "assignment", Observed: "Assigned", Source: "read-only API", Result: "pass", Evidence: []string{"private://assignment"}}); err != nil {
		t.Fatal(err)
	}
	for _, verdict := range []struct{ cell, result, reason string }{{"base", "pass", ""}, {"second", "not-run", "Bounded attempt"}} {
		if _, err := s.Append(first.ID, Event{Kind: "verdict", Cell: verdict.cell, Actor: "tester", Verdict: verdict.result, Reason: verdict.reason}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Finish(first.ID); err != nil {
		t.Fatal(err)
	}
	second, err := s.Begin(p, "journey", "")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CoverageAt(l, &p, &p.Snapshot)
	if err != nil || r.Counts["pass"] != 1 || r.Counts["not-run"] != 1 || r.Attempted != 1 {
		t.Fatalf("untouched cell lost its earlier result: %+v %v", r, err)
	}
	if _, err := s.Append(second.ID, Event{Kind: "observation", Cell: "base", Actor: "customer", Observed: "Retest underway", Evidence: []string{"private://capture/retest"}}); err != nil {
		t.Fatal(err)
	}
	r, err = s.CoverageAt(l, &p, &p.Snapshot)
	if err != nil || r.Counts["pass"] != 0 || r.Counts["not-run"] != 2 {
		t.Fatalf("retested cell inherited prior pass: %+v %v", r, err)
	}
}
func TestPlanSafety(t *testing.T) {
	l := fixture(t)
	p := planFor(t, l)
	p.Cells = append(p.Cells, p.Cells[0])
	if err := p.Validate(l); err == nil {
		t.Fatal("duplicate cell accepted")
	}
	p = planFor(t, l)
	p.LibraryHash = "changed"
	if err := p.Validate(l); err == nil {
		t.Fatal("stale library accepted")
	}
	if err := ValidateBindings(Snapshot{}); err == nil {
		t.Fatal("missing target accepted")
	}
}
