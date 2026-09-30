package journeys

import (
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func finishedPublicationFixture(t *testing.T, verdict string) (*Library, Store, Attempt, Summary) {
	t.Helper()
	l := fixture(t)
	p := planFor(t, l)
	p.Adapter = "test"
	p.Hash = p.computedHash()
	s := Store{Root: t.TempDir()}
	a, err := s.Begin(p, "journey", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"customer", "worker"} {
		if _, err := s.Append(a.ID, Event{Kind: "observation", Cell: "base", Actor: actor, Observed: "Private user observation", Evidence: []string{"private://" + actor}}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append(a.ID, Event{Kind: "verification", Cell: "base", Actor: "backend", Check: "assignment", Observed: "One assignment", Source: "read-only API", Result: "pass", Evidence: []string{"private://assignment"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(a.ID, Event{Kind: "verdict", Cell: "base", Actor: "tester", Verdict: verdict, Reason: "Private reason user@example.com"}); err != nil {
		t.Fatal(err)
	}
	summary, err := s.Finish(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	return l, s, a, summary
}

func TestPublicationKeepsStoryPinsAndWithholdsRawEvidence(t *testing.T) {
	l, s, a, _ := finishedPublicationFixture(t, "fail")
	l.Hash = "changed-library"
	l.names["main-journey"].Story = "foreign/999"
	p, err := s.PreparePublication(l, a.ID, "sample", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Story != a.Plan.Stories["sample"] || p.Cases[0].Verdict != "fail" {
		t.Fatalf("story/verdict changed: %+v", p)
	}
	b, _ := json.Marshal(p)
	if strings.Contains(string(b), "private://") || strings.Contains(string(b), "user@example.com") || strings.Contains(string(b), "Private user observation") {
		t.Fatal("raw evidence leaked")
	}
	if _, err := s.PreparePublication(l, a.ID, "../../outside", nil); err == nil {
		t.Fatal("path traversal mode accepted")
	}
	if _, err := s.PreparePublication(l, a.ID, "other-mode", nil); err == nil {
		t.Fatal("unselected mode accepted")
	}
	next, err := s.Begin(a.Plan, "journey", a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(next.ID, Event{Kind: "verdict", Cell: "base", Actor: "tester", Verdict: "blocked", Reason: "New attempt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Finish(next.ID); err != nil {
		t.Fatal(err)
	}
	q, err := s.PreparePublication(l, next.ID, "sample", nil)
	if err != nil || q.Cases[0].Key == p.Cases[0].Key || q.RetestOf != a.ID || q.RunID != a.RunID {
		t.Fatalf("retest erased failed attempt: %+v %v", q, err)
	}
}

func TestPublicationRequiresReviewedMatchingActorImagesAndCopy(t *testing.T) {
	l, s, a, summary := finishedPublicationFixture(t, "pass")
	if _, err := s.PreparePublication(l, a.ID, "sample", nil); err == nil {
		t.Fatal("unreviewed pass published")
	}
	presentation := Presentation{Version: 1, AttemptID: a.ID, JournalHash: summary.JournalHash, Cells: []PresentedCell{{Cell: "base", Note: "Both people saw the agreed assignment."}}}
	for _, actor := range []string{"customer", "worker"} {
		path := filepath.Join(t.TempDir(), actor+".png")
		f, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(path)
		presentation.Cells[0].Evidence = append(presentation.Cells[0].Evidence, PublicImage{Actor: actor, SourceRef: "private://" + actor, Path: path, SHA256: Digest(b), Surface: "ios", Route: "job/current", Caption: "Reviewed synthetic test screen", Reviewed: true})
	}
	p, err := s.PreparePublication(l, a.ID, "sample", &presentation)
	if err != nil || len(p.Cases[0].Evidence) != 2 || p.Cases[0].Evidence[0].SourceRef != "" {
		t.Fatalf("reviewed evidence rejected or private ref leaked: %+v %v", p, err)
	}
	for _, bad := range []string{"Reach user@example.com", "/Users/private/file", "Authorization: hidden"} {
		copy := presentation
		copy.Cells = append([]PresentedCell(nil), presentation.Cells...)
		copy.Cells[0].Note = bad
		if _, err := s.PreparePublication(l, a.ID, "sample", &copy); err == nil {
			t.Fatal("private public copy accepted")
		}
	}
	presentation.Cells[0].Evidence[0].Reviewed = false
	if _, err := s.PreparePublication(l, a.ID, "sample", &presentation); err == nil {
		t.Fatal("unreviewed source accepted")
	}
	presentation.Cells[0].Evidence[0].Reviewed = true
	presentation.Cells[0].Evidence[0].SourceRef = "private://worker"
	if _, err := s.PreparePublication(l, a.ID, "sample", &presentation); err == nil {
		t.Fatal("cross-role evidence accepted")
	}
	presentation.Cells[0].Evidence[0].SourceRef = "private://customer"
	if err := os.WriteFile(presentation.Cells[0].Evidence[0].Path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PreparePublication(l, a.ID, "sample", &presentation); err == nil {
		t.Fatal("changed reviewed image accepted")
	}
}

func TestPublicationAdapterProcess(t *testing.T) {
	if os.Getenv("JOURNEYS_PUBLISH_TEST") == "" {
		return
	}
	var req Request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil || req.Publication == nil {
		os.Exit(4)
	}
	p := req.Publication
	f, err := os.OpenFile("publications", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(4)
	}
	if _, err := f.WriteString(req.Key + "\n"); err != nil {
		os.Exit(4)
	}
	_ = f.Close()
	r := Receipt{Version: 1, OK: os.Getenv("JOURNEYS_PUBLISH_TEST") != "blocked", Operation: "publish", Diagnostics: []Diagnostic{}, Next: []string{}}
	if !r.OK {
		r.Diagnostics = append(r.Diagnostics, Diagnostic{Code: "PUBLICATION_PENDING", Message: "Interrupted publication"})
	} else {
		r.Publication = &Published{Hash: p.Hash, Story: p.Story, RunID: "server-run", TaskURL: "https://example.test/task", BoardURL: "https://example.test/board", Handback: "server returned handback", Cases: []PublishedCase{}}
		for _, c := range p.Cases {
			r.Publication.Cases = append(r.Publication.Cases, PublishedCase{Cell: c.Cell, Key: c.Key, URL: "https://example.test/case"})
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		os.Exit(4)
	}
	if !r.OK {
		os.Exit(3)
	}
	os.Exit(0)
}

func TestPublicationRetryUsesSameKeyAndPersistsFailure(t *testing.T) {
	l, s, a, _ := finishedPublicationFixture(t, "blocked")
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := privateJSON(filepath.Join(l.Root, ".journeys", "adapters.json"), Registry{Version: 1, Adapters: map[string]Adapter{"test": {Command: []string{exe, "-test.run=^TestPublicationAdapterProcess$"}, Env: []string{"JOURNEYS_PUBLISH_TEST"}}}}, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JOURNEYS_PUBLISH_TEST", "blocked")
	cp, err := s.Publish(l.Root, l, a.ID, "sample", nil, true, nil)
	if err == nil || cp.State != "pending" {
		t.Fatalf("lost pending publication: %+v %v", cp, err)
	}
	var saved PublicationCheckpoint
	if err := ReadJSON(filepath.Join(s.Root, a.ID, "publication-sample.json"), &saved); err != nil || len(saved.Receipt.Diagnostics) != 1 {
		t.Fatalf("failed receipt lost: %+v %v", saved, err)
	}
	exported := filepath.Join(t.TempDir(), "publication.json")
	if err := s.ExportPublication(l, a.ID, "sample", nil, exported); err == nil {
		t.Fatal("pending publication exported")
	}
	t.Setenv("JOURNEYS_PUBLISH_TEST", "ready")
	cp, err = s.Publish(l.Root, l, a.ID, "sample", nil, true, nil)
	if err != nil || cp.State != "done" {
		t.Fatalf("retry failed: %+v %v", cp, err)
	}
	if _, err := s.Publish(l.Root, l, a.ID, "sample", nil, true, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(l.Root, "publications"))
	lines := strings.Fields(string(b))
	if len(lines) != 2 || lines[0] != lines[1] {
		t.Fatalf("publication duplicated or key changed: %q", b)
	}
	if err := s.ExportPublication(l, a.ID, "sample", nil, exported); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(exported)
	if err != nil || !strings.Contains(string(b), "https://example.test/case") || strings.Contains(string(b), s.Root) || strings.Contains(string(b), "Private reason") || strings.Contains(string(b), "receiptRef") {
		t.Fatalf("unsafe or incomplete export: %v", err)
	}
	if err := s.ExportPublication(l, a.ID, "sample", nil, exported); err == nil {
		t.Fatal("overwrote immutable publication export")
	}
	cp.Receipt.Publication.Handback = "private user@example.com"
	if err := privateJSON(filepath.Join(s.Root, a.ID, "publication-sample.json"), cp, false); err != nil {
		t.Fatal(err)
	}
	if err := s.ExportPublication(l, a.ID, "sample", nil, filepath.Join(t.TempDir(), "unsafe.json")); err == nil {
		t.Fatal("exported private adapter handback")
	}
	cp.Receipt.Publication.Handback = "server returned handback"
	cp.Receipt.Publication.Cases[0].Key = "foreign"
	if err := privateJSON(filepath.Join(s.Root, a.ID, "publication-sample.json"), cp, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Publish(l.Root, l, a.ID, "sample", nil, true, nil); err == nil {
		t.Fatal("foreign cached case accepted")
	}
}
