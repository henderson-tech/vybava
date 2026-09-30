package journeys

import (
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/henderson-tech/vybava/internal/secretscan"
)

// Presentation is an explicit reviewed public copy, never raw journal prose.
// SourceRef ties a redacted image back to an immutable journal evidence ref.
type Presentation struct {
	Version     int             `json:"v"`
	AttemptID   string          `json:"attemptId"`
	JournalHash string          `json:"journalHash"`
	Cells       []PresentedCell `json:"cells"`
}
type PresentedCell struct {
	Cell     string        `json:"cell"`
	Note     string        `json:"note"`
	Evidence []PublicImage `json:"evidence"`
}
type PublicImage struct {
	Actor     string `json:"actor"`
	SourceRef string `json:"sourceRef"`
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	Surface   string `json:"surface"`
	Route     string `json:"route"`
	Caption   string `json:"caption"`
	Reviewed  bool   `json:"reviewed"`
}
type PublishedCase struct {
	Cell string `json:"cell"`
	Key  string `json:"key"`
	URL  string `json:"url"`
}
type Published struct {
	Hash     string          `json:"hash"`
	Story    string          `json:"story"`
	RunID    string          `json:"runId"`
	TaskURL  string          `json:"taskUrl"`
	BoardURL string          `json:"boardUrl"`
	Cases    []PublishedCase `json:"cases"`
	Handback string          `json:"handback"`
}
type PublicCase struct {
	Cell     string        `json:"cell"`
	Key      string        `json:"key"`
	Title    string        `json:"title"`
	Verdict  string        `json:"verdict"`
	Note     string        `json:"note"`
	Evidence []PublicImage `json:"evidence"`
}
type Publication struct {
	Version     int          `json:"v"`
	AttemptID   string       `json:"attemptId"`
	RunID       string       `json:"runId"`
	RetestOf    string       `json:"retestOf,omitempty"`
	Phase       string       `json:"phase"`
	Mode        string       `json:"mode"`
	Story       string       `json:"story"`
	PlanHash    string       `json:"planHash"`
	JournalHash string       `json:"journalHash"`
	StateDir    string       `json:"stateDir"`
	Cases       []PublicCase `json:"cases"`
	Hash        string       `json:"hash"`
}

func (p Publication) computedHash() string {
	p.Hash = ""
	b, _ := json.Marshal(p)
	var canonical map[string]interface{}
	_ = json.Unmarshal(b, &canonical)
	b, _ = json.Marshal(canonical)
	return Digest(b)
}

var privateCopy = regexp.MustCompile(`(?i)(/Users/|/home/|file://|\bBearer\s|\bAuthorization\b|[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,})`)

func publicText(text string, a Attempt) error {
	if privateCopy.MatchString(text) || len(secretscan.Find(text, secretscan.All, nil)) > 0 {
		return problem("REDACTION_FAILED", "public copy contains credentials, contact details or private paths")
	}
	private := []string{a.Plan.Snapshot.Namespace}
	for _, origin := range a.Plan.Snapshot.Origins {
		private = append(private, origin)
	}
	for _, b := range a.Plan.Snapshot.Bindings {
		private = append(private, b.Device, b.SessionID, b.UserRef, b.CompanyRef, b.EmployeeRef)
	}
	for _, value := range private {
		if len(value) >= 6 && strings.Contains(text, value) {
			return problem("REDACTION_FAILED", "public copy contains a private plan identity")
		}
	}
	return nil
}

func publicationStory(l *Library, a Attempt, mode string) (string, error) {
	if target := a.Plan.Stories[mode]; target != "" && story.MatchString(target) {
		return target, nil
	}
	// Compatibility for plans sealed before story pins were introduced. A
	// changed library must never silently reroute an old attempt.
	if a.Plan.LibraryHash != l.Hash {
		return "", problem("PLAN_STALE", "historical plan has no story pin and its library changed")
	}
	for _, d := range l.Documents {
		if d.Type == "journey" && ModeKey(d.Mode) == mode {
			return d.Story, nil
		}
	}
	return "", problem("DOCUMENT_INVALID", "mode has no journey story")
}

func (s Store) PreparePublication(l *Library, id, mode string, presentation *Presentation) (Publication, error) {
	if !validID(mode) {
		return Publication{}, problem("DOCUMENT_INVALID", "invalid publication mode")
	}
	summary, err := s.Finish(id)
	if err != nil {
		return Publication{}, err
	}
	a, events, _, err := s.Read(id, false)
	if err != nil {
		return Publication{}, err
	}
	target, err := publicationStory(l, a, mode)
	if err != nil {
		return Publication{}, err
	}
	dir, err := s.dir(id)
	if err != nil {
		return Publication{}, err
	}
	p := Publication{Version: 1, AttemptID: id, RunID: a.RunID, RetestOf: a.RetestOf, Phase: a.Phase, Mode: mode, Story: target, PlanHash: a.Plan.Hash, JournalHash: summary.JournalHash, StateDir: dir, Cases: []PublicCase{}}
	public := map[string]PresentedCell{}
	if presentation != nil {
		if presentation.Version != 1 || presentation.AttemptID != id || presentation.JournalHash != summary.JournalHash {
			return p, problem("EVIDENCE_MISSING", "presentation belongs to a different immutable journal")
		}
		for _, c := range presentation.Cells {
			if _, exists := public[c.Cell]; exists {
				return p, problem("DOCUMENT_INVALID", "duplicate presentation cell")
			}
			public[c.Cell] = c
		}
	}
	for i, cell := range a.Plan.Cells {
		if cell.Mode != mode {
			continue
		}
		result := summary.Cells[i]
		copy, hasCopy := public[cell.ID]
		delete(public, cell.ID)
		if result.Verdict == "pass" && (!hasCopy || strings.TrimSpace(copy.Note) == "") {
			return p, problem("EVIDENCE_MISSING", "passing cells require reviewed public observations")
		}
		note := fmt.Sprintf("Phase: %s. Local verdict: %s. Attempt: %s. Journal sha256: %s. Required checks: %s. Latest successful checks: %s.", a.Phase, result.Verdict, id, summary.JournalHash, strings.Join(cell.Required, ", "), strings.Join(result.Checks, ", "))
		if a.RetestOf != "" {
			note += " Retest of: " + a.RetestOf + "; original attempt retained."
		}
		if a.Phase == "preflight" {
			note += " Startup evidence only; zero executed lifecycle coverage."
		}
		if copy.Note != "" {
			note += " " + copy.Note
		}
		if err := publicText(note, a); err != nil {
			return p, err
		}
		c := PublicCase{Cell: cell.ID, Key: "j-" + id + "-" + Digest([]byte(cell.ID))[:16], Title: fmt.Sprintf("%s: %s / %s (%s)", a.Phase, mode, cell.Scenario, id[:8]), Verdict: result.Verdict, Note: note, Evidence: []PublicImage{}}
		actors := map[string]bool{}
		for _, picture := range copy.Evidence {
			if !picture.Reviewed || !slices.Contains([]string{"customer", "worker"}, picture.Actor) || !slices.Contains([]string{"ios", "android", "web", "macos"}, picture.Surface) || picture.Caption == "" || picture.Route == "" {
				return p, problem("REDACTION_FAILED", "image needs explicit visual review, actor, surface, route and public caption")
			}
			known := false
			for _, event := range events {
				known = known || (event.Cell == cell.ID && event.Actor == picture.Actor && slices.Contains(event.Evidence, picture.SourceRef))
			}
			if !known {
				return p, problem("EVIDENCE_MISSING", "image source is not evidence for this cell and actor")
			}
			if err := publicText(picture.Caption+" "+picture.Route, a); err != nil {
				return p, err
			}
			if !filepath.IsAbs(picture.Path) {
				return p, problem("TARGET_UNSAFE", "public image path must be absolute")
			}
			st, err := os.Lstat(picture.Path)
			if err != nil || !st.Mode().IsRegular() || st.Size() > 25*1024*1024 {
				return p, problem("EVIDENCE_MISSING", "public image is missing, linked or too large")
			}
			b, err := os.ReadFile(picture.Path)
			if err != nil || Digest(b) != picture.SHA256 {
				return p, problem("EVIDENCE_MISSING", "reviewed image hash changed")
			}
			f, err := os.Open(picture.Path)
			if err != nil {
				return p, err
			}
			_, _, decodeErr := image.DecodeConfig(f)
			closeErr := f.Close()
			if decodeErr != nil || closeErr != nil {
				return p, problem("EVIDENCE_MISSING", "only reviewed PNG/JPEG screenshots are supported")
			}
			actors[picture.Actor] = true
			picture.SourceRef = "" // private journal reference never enters Vitrinka.
			c.Evidence = append(c.Evidence, picture)
		}
		if result.Verdict == "pass" && (!actors["customer"] || !actors["worker"]) {
			return p, problem("EVIDENCE_MISSING", "publish pass requires reviewed screenshots of both actors")
		}
		p.Cases = append(p.Cases, c)
	}
	if len(p.Cases) == 0 || len(public) != 0 {
		return p, problem("DOCUMENT_INVALID", "publication/presentation cells must belong to the selected mode")
	}
	p.Hash = p.computedHash()
	return p, nil
}

type PublicationCheckpoint struct {
	Hash    string  `json:"hash"`
	State   string  `json:"state"`
	Receipt Receipt `json:"receipt"`
}

// ExportPublication adds server references beside an immutable summary without
// copying adapter state, raw evidence paths or transport diagnostics.
func (s Store) ExportPublication(l *Library, id, mode string, presentation *Presentation, target string) error {
	p, err := s.PreparePublication(l, id, mode, presentation)
	if err != nil {
		return err
	}
	var cp PublicationCheckpoint
	if err := ReadJSON(filepath.Join(p.StateDir, "publication-"+mode+".json"), &cp); err != nil {
		return err
	}
	if cp.State != "done" || cp.Hash != p.Hash {
		return problem("PUBLICATION_PENDING", "publication is not complete for this projection")
	}
	if err := validatePublished(p, cp.Receipt); err != nil {
		return err
	}
	a, _, _, err := s.Read(id, false)
	if err != nil {
		return err
	}
	out := struct {
		Version     int        `json:"v"`
		AttemptID   string     `json:"attemptId"`
		RunID       string     `json:"runId"`
		RetestOf    string     `json:"retestOf,omitempty"`
		Phase       string     `json:"phase"`
		Mode        string     `json:"mode"`
		PlanHash    string     `json:"planHash"`
		JournalHash string     `json:"journalHash"`
		Publication *Published `json:"publication"`
	}{1, p.AttemptID, p.RunID, p.RetestOf, p.Phase, p.Mode, p.PlanHash, p.JournalHash, cp.Receipt.Publication}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	if err := publicText(string(b), a); err != nil {
		return err
	}
	return writeFile(target, append(b, '\n'), true)
}

func validatePublished(p Publication, r Receipt) error {
	got := r.Publication
	if !r.OK || r.Operation != "publish" || got == nil || got.Hash != p.Hash || got.Story != p.Story || got.RunID == "" || len(got.Cases) != len(p.Cases) || got.Handback == "" {
		return problem("ADAPTER_PROTOCOL", "publication receipt does not match this projection")
	}
	validLink := func(raw string) bool {
		u, err := url.Parse(raw)
		return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
	}
	if !validLink(got.TaskURL) || !validLink(got.BoardURL) {
		return problem("ADAPTER_PROTOCOL", "publication receipt has invalid server links")
	}
	seen := map[string]bool{}
	for _, c := range got.Cases {
		found := false
		for _, want := range p.Cases {
			found = found || (c.Cell == want.Cell && c.Key == want.Key)
		}
		if !found || seen[c.Cell] || !validLink(c.URL) {
			return problem("ADAPTER_PROTOCOL", "publication receipt has foreign or duplicate cases")
		}
		seen[c.Cell] = true
	}
	return nil
}

func (s Store) Publish(root string, l *Library, id, mode string, presentation *Presentation, apply bool, approvals []string) (PublicationCheckpoint, error) {
	p, err := s.PreparePublication(l, id, mode, presentation)
	if err != nil {
		return PublicationCheckpoint{}, err
	}
	if !apply {
		return PublicationCheckpoint{}, problem("TARGET_UNSAFE", "publication requires --apply after reviewing the public projection")
	}
	a, _, _, err := s.Read(id, false)
	if err != nil {
		return PublicationCheckpoint{}, err
	}
	unlock, err := lock(p.StateDir)
	if err != nil {
		return PublicationCheckpoint{}, err
	}
	defer unlock()
	path := filepath.Join(p.StateDir, "publication-"+mode+".json")
	var cp PublicationCheckpoint
	if err := ReadJSON(path, &cp); err == nil {
		if cp.Hash != p.Hash {
			return cp, problem("PLAN_STALE", "publication projection changed; preserve its original receipt")
		}
		if cp.State == "done" {
			return cp, validatePublished(p, cp.Receipt)
		}
	} else if !os.IsNotExist(err) {
		return cp, err
	}
	cp = PublicationCheckpoint{Hash: p.Hash, State: "pending"}
	if err := privateJSON(path, cp, false); err != nil {
		return cp, err
	}
	cp.Receipt, err = Invoke(root, a.Plan.Adapter, Request{Version: 1, Operation: "publish", Key: p.Hash + ":publish", Publication: &p, CaptureApprovals: approvals})
	if err == nil {
		err = validatePublished(p, cp.Receipt)
	}
	if err == nil {
		cp.State = "done"
	}
	if saveErr := privateJSON(path, cp, false); saveErr != nil {
		return cp, saveErr
	}
	return cp, err
}
