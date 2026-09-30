package journeys

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/henderson-tech/vybava/internal/secretscan"
)

type Cell struct {
	ID        string   `json:"id"`
	Mode      string   `json:"mode"`
	Scenario  string   `json:"scenario"`
	Topology  string   `json:"topology"`
	Lane      string   `json:"lane"`
	Condition string   `json:"condition"`
	Required  []string `json:"required"`
}
type Binding struct {
	SessionID   string `json:"sessionId,omitempty"`
	Actor       string `json:"actor"`
	Device      string `json:"device"`
	Platform    string `json:"platform"`
	UserRef     string `json:"userRef"`
	CompanyRef  string `json:"companyRef,omitempty"`
	EmployeeRef string `json:"employeeRef,omitempty"`
	AppID       string `json:"appId"`
	NativeHash  string `json:"nativeHash"`
	DriverPort  int    `json:"driverPort"`
	AppiumPort  int    `json:"appiumPort,omitempty"`
}
type Snapshot struct {
	Revision          string            `json:"revision"`
	SourceHash        string            `json:"sourceHash"`
	Namespace         string            `json:"namespace"`
	TargetFingerprint string            `json:"targetFingerprint"`
	Origins           map[string]string `json:"origins"`
	Capabilities      map[string]bool   `json:"capabilities"`
	Bindings          []Binding         `json:"bindings"`
}
type Plan struct {
	Version     int               `json:"v"`
	ID          string            `json:"id"`
	At          time.Time         `json:"at"`
	LibraryHash string            `json:"libraryHash"`
	Snapshot    Snapshot          `json:"snapshot"`
	Cells       []Cell            `json:"cells"`
	Adapter     string            `json:"adapter"`
	Operations  []string          `json:"operations"`
	Stories     map[string]string `json:"stories,omitempty"`
	Hash        string            `json:"hash"`
}
type Attempt struct {
	Version  int       `json:"schemaVersion"`
	RunID    string    `json:"runId"`
	ID       string    `json:"attemptId"`
	RetestOf string    `json:"retestOf,omitempty"`
	At       time.Time `json:"at"`
	Phase    string    `json:"phase"`
	Plan     Plan      `json:"plan"`
}
type Money struct {
	Minor    int64  `json:"minor"`
	Currency string `json:"currency"`
}
type Event struct {
	Sequence int       `json:"sequence"`
	At       time.Time `json:"at"`
	Kind     string    `json:"kind"`
	Cell     string    `json:"cell"`
	Step     string    `json:"step,omitempty"`
	Actor    string    `json:"actor"`
	Expected string    `json:"expected,omitempty"`
	Observed string    `json:"observed,omitempty"`
	Check    string    `json:"check,omitempty"`
	Result   string    `json:"result,omitempty"`
	Source   string    `json:"source,omitempty"`
	Evidence []string  `json:"evidence,omitempty"`
	Money    *Money    `json:"money,omitempty"`
	Verdict  string    `json:"verdict,omitempty"`
	Reason   string    `json:"reason,omitempty"`
	Approver string    `json:"approver,omitempty"`
}
type CellResult struct {
	ID        string     `json:"id"`
	Verdict   string     `json:"verdict"`
	Observed  bool       `json:"observed"`
	Checks    []string   `json:"checks"`
	Exclusion *Exclusion `json:"exclusion,omitempty"`
}
type Exclusion struct {
	Reason   string    `json:"reason"`
	Approver string    `json:"approver"`
	At       time.Time `json:"at"`
}

// Summary deliberately omits raw prose, actor identifiers, origins and local evidence paths.
type Summary struct {
	Version     int          `json:"schemaVersion"`
	RunID       string       `json:"runId"`
	AttemptID   string       `json:"attemptId"`
	RetestOf    string       `json:"retestOf,omitempty"`
	Phase       string       `json:"phase"`
	PlanHash    string       `json:"planHash"`
	LibraryHash string       `json:"libraryHash"`
	Started     time.Time    `json:"started"`
	Ended       time.Time    `json:"ended"`
	JournalHash string       `json:"journalHash"`
	Cells       []CellResult `json:"cells"`
	Denominator int          `json:"denominator"`
}
type Store struct{ Root string }

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func validID(s string) bool { return len(s) <= 80 && slug.MatchString(s) }
func Decode(b []byte, out interface{}) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return problem("DOCUMENT_INVALID", "invalid JSON contract")
	}
	if err := d.Decode(new(interface{})); err != io.EOF {
		return problem("DOCUMENT_INVALID", "trailing JSON content")
	}
	return nil
}
func ReadJSON(path string, out interface{}) error {
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return problem("TARGET_UNSAFE", "JSON record is a symlink")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return Decode(b, out)
}
func privateJSON(path string, v interface{}, exclusive bool) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'), exclusive)
}
func writeFile(path string, b []byte, exclusive bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if exclusive {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(b)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil {
			return err
		}
		return closeErr
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".journeys-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func (s Store) dir(id string) (string, error) {
	if !validID(id) {
		return "", problem("TARGET_UNSAFE", "invalid attempt ID")
	}
	root, err := filepath.Abs(s.Root)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	p := filepath.Join(root, id)
	if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return "", problem("TARGET_UNSAFE", "attempt directory is a symlink")
	}
	return p, nil
}
func lock(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "writer.lock")
	if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, problem("TARGET_UNSAFE", "lock is a symlink")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, problem("WRITER_BUSY", "attempt has an active writer")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
func (p Plan) computedHash() string { p.Hash = ""; b, _ := json.Marshal(p); return Digest(b) }
func (p Plan) CheckSeal() error {
	if p.Hash == "" || p.Hash != p.computedHash() {
		return problem("PLAN_STALE", "saved plan was edited or has no seal")
	}
	return nil
}
func (p *Plan) Seal(l *Library) error {
	p.Version = 1
	p.At = time.Now().UTC()
	p.ID = NewID()
	p.LibraryHash = l.Hash
	p.Stories = map[string]string{}
	for _, d := range l.Documents {
		if d.Type == "journey" {
			p.Stories[ModeKey(d.Mode)] = d.Story
		}
	}
	if err := p.Validate(l); err != nil {
		return err
	}
	encoded, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if len(secretscan.Find(string(encoded), secretscan.All, nil)) > 0 {
		return problem("REDACTION_FAILED", "plan contains credentials; use private credential references")
	}
	p.Hash = p.computedHash()
	return nil
}
func (p Plan) Validate(l *Library) error {
	if p.Version != 1 || p.LibraryHash != l.Hash {
		return problem("PLAN_STALE", "library revision changed")
	}
	if len(p.Cells) == 0 {
		return problem("DOCUMENT_INVALID", "select at least one reviewed coverage cell")
	}
	ids, keys := map[string]bool{}, map[string]bool{}
	for _, c := range p.Cells {
		key := strings.Join([]string{c.Mode, c.Scenario, c.Topology, c.Lane, c.Condition}, "|")
		if !validID(c.ID) || ids[c.ID] || keys[key] {
			return problem("DOCUMENT_INVALID", "duplicate or invalid cell")
		}
		ids[c.ID] = true
		keys[key] = true
		if !slices.Contains([]string{"two-device", "three-device", "sequential-device", "api-assisted"}, c.Topology) || !slices.Contains([]string{"live-sandbox", "scripted", "checkpoint"}, c.Lane) || c.Condition == "" || len(c.Required) == 0 {
			return problem("DOCUMENT_INVALID", "cell needs explicit topology, lane, condition and checks")
		}
		known := false
		for _, d := range l.Documents {
			if d.Type == "journey" && ModeKey(d.Mode) == c.Mode && c.Scenario == "lifecycle" {
				known = true
			}
		}
		for _, sc := range l.Scenarios {
			if sc.ID == c.Scenario && slices.Contains(sc.Modes, c.Mode) {
				known = true
			}
		}
		if !known {
			return problem("DOCUMENT_INVALID", "scenario/mode is not authored")
		}
		checkIDs := map[string]bool{}
		for _, k := range c.Required {
			if !validID(k) || checkIDs[k] {
				return problem("DOCUMENT_INVALID", "invalid or duplicate required check")
			}
			checkIDs[k] = true
		}
	}
	return nil
}
func (s Store) Begin(p Plan, phase, retest string) (Attempt, error) {
	if p.Hash == "" || p.Hash != p.computedHash() {
		return Attempt{}, problem("PLAN_STALE", "plan hash does not match")
	}
	if phase != "preflight" && phase != "journey" {
		return Attempt{}, problem("DOCUMENT_INVALID", "phase must be preflight or journey")
	}
	a := Attempt{Version: 1, RunID: NewID(), ID: NewID(), RetestOf: retest, At: time.Now().UTC(), Phase: phase, Plan: p}
	if retest != "" {
		old, _, _, err := s.Read(retest, false)
		if err != nil {
			return a, err
		}
		dir, _ := s.dir(retest)
		if _, err := os.Stat(filepath.Join(dir, "summary.json")); err != nil {
			return a, problem("EVIDENCE_MISSING", "finish the earlier attempt before retesting")
		}
		a.RunID = old.RunID
	}
	dir, err := s.dir(a.ID)
	if err != nil {
		return a, err
	}
	unlock, err := lock(dir)
	if err != nil {
		return a, err
	}
	defer unlock()
	if err = privateJSON(filepath.Join(dir, "attempt.json"), a, true); err != nil {
		return a, err
	}
	return a, writeFile(filepath.Join(dir, "events.jsonl"), nil, true)
}
func readJournal(path string) ([]Event, []byte, []byte, error) {
	if st, err := os.Lstat(path); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return nil, nil, nil, problem("TARGET_UNSAFE", "journal is a symlink")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, nil, err
	}
	end := bytes.LastIndexByte(b, '\n') + 1
	complete, tail := b[:end], b[end:]
	events := []Event{}
	for i, line := range bytes.Split(complete, []byte("\n")) {
		if i == bytes.Count(complete, []byte("\n")) {
			break
		}
		var e Event
		if err := Decode(line, &e); err != nil || e.Sequence != i+1 || e.At.IsZero() {
			return nil, nil, nil, problem("JOURNAL_CORRUPT", fmt.Sprintf("invalid journal event %d", i+1))
		}
		events = append(events, e)
	}
	return events, complete, tail, nil
}
func (s Store) Read(id string, recoverTail bool) (Attempt, []Event, int, error) {
	dir, err := s.dir(id)
	if err != nil {
		return Attempt{}, nil, 0, err
	}
	unlock, err := lock(dir)
	if err != nil {
		return Attempt{}, nil, 0, err
	}
	defer unlock()
	return readAttempt(dir, recoverTail)
}
func readAttempt(dir string, recoverTail bool) (Attempt, []Event, int, error) {
	var a Attempt
	if err := ReadJSON(filepath.Join(dir, "attempt.json"), &a); err != nil {
		return a, nil, 0, err
	}
	if a.ID != filepath.Base(dir) || a.Version != 1 || a.Plan.Hash != a.Plan.computedHash() {
		return a, nil, 0, problem("JOURNAL_CORRUPT", "attempt identity or plan changed")
	}
	events, b, tail, err := readJournal(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return a, nil, 0, err
	}
	if len(tail) > 0 {
		if !recoverTail {
			return a, events, len(tail), problem("JOURNAL_TRUNCATED", "resume with --recover to archive the incomplete final record")
		}
		if _, err := os.Stat(filepath.Join(dir, "summary.json")); err == nil {
			return a, events, len(tail), problem("ATTEMPT_FINISHED", "finished evidence cannot be repaired")
		}
		if err := writeFile(filepath.Join(dir, "recovered-"+NewID()+".raw"), tail, true); err != nil {
			return a, events, len(tail), err
		}
		if err := writeFile(filepath.Join(dir, "events.jsonl"), b, false); err != nil {
			return a, events, len(tail), err
		}
	}
	for i, e := range events {
		if err := validateEvent(a, events[:i], e); err != nil {
			return a, nil, 0, problem("JOURNAL_CORRUPT", fmt.Sprintf("invalid event contract at %d", i+1))
		}
	}
	return a, events, len(tail), nil
}

var verdicts = []string{"pass", "fail", "blocked", "needs-product-decision", "not-applicable", "not-run"}

func validateEvent(a Attempt, prior []Event, e Event) error {
	if !slices.Contains([]string{"action", "observation", "verification", "artifact", "issue", "intervention", "verdict", "other"}, e.Kind) {
		return problem("DOCUMENT_INVALID", "unknown event kind; use other with observed text")
	}
	var cell *Cell
	for i := range a.Plan.Cells {
		if a.Plan.Cells[i].ID == e.Cell {
			cell = &a.Plan.Cells[i]
		}
	}
	if cell == nil {
		return problem("DOCUMENT_INVALID", "event cell is not in the frozen plan")
	}
	for _, p := range prior {
		if p.Cell == e.Cell && p.Kind == "verdict" {
			return problem("ATTEMPT_FINISHED", "cell verdict is immutable; make a new attempt")
		}
	}
	if e.Actor == "" || (e.Observed == "" && e.Kind != "verdict") {
		return problem("DOCUMENT_INVALID", "event needs actor and observed text")
	}
	if e.Kind == "verification" && (!slices.Contains(cell.Required, e.Check) || !slices.Contains([]string{"pass", "fail", "pending", "unknown"}, e.Result) || e.Source == "" || len(e.Evidence) == 0) {
		return problem("EVIDENCE_MISSING", "verification needs a planned check, result, source and evidence")
	}
	for _, ref := range e.Evidence {
		if strings.TrimSpace(ref) == "" {
			return problem("EVIDENCE_MISSING", "empty evidence reference")
		}
	}
	if e.Money != nil && (len(e.Money.Currency) != 3 || strings.ToUpper(e.Money.Currency) != e.Money.Currency) {
		return problem("DOCUMENT_INVALID", "money needs integer minor units and uppercase ISO currency")
	}
	if e.Kind == "verdict" {
		if !slices.Contains(verdicts, e.Verdict) {
			return problem("DOCUMENT_INVALID", "unknown verdict")
		}
		if e.Verdict == "pass" {
			if err := passEvidence(*cell, prior); err != nil {
				return err
			}
		} else if e.Reason == "" {
			return problem("EVIDENCE_MISSING", "non-pass verdict needs a reason")
		}
		if e.Verdict == "not-applicable" && (e.Approver == "" || e.At.IsZero()) {
			return problem("EVIDENCE_MISSING", "exclusion needs approver and date")
		}
	}
	b, _ := json.Marshal(e)
	if len(b) > 256*1024 {
		return problem("DOCUMENT_INVALID", "event exceeds 256 KiB; store raw evidence privately")
	}
	if len(secretscan.Find(string(b), secretscan.All, nil)) > 0 {
		return problem("REDACTION_FAILED", "event contains secret material; content withheld")
	}
	return nil
}
func passEvidence(c Cell, events []Event) error {
	checks := map[string]Event{}
	actors := map[string]bool{}
	for _, e := range events {
		if e.Cell != c.ID {
			continue
		}
		if e.Kind == "verification" {
			checks[e.Check] = e
		}
		if e.Kind == "observation" && len(e.Evidence) > 0 {
			actors[e.Actor] = true
		}
		if e.Kind == "intervention" && c.Lane == "live-sandbox" {
			return problem("EVIDENCE_MISSING", "intervened cell cannot pass as a full live journey")
		}
	}
	for _, check := range c.Required {
		e, ok := checks[check]
		if !ok || e.Result != "pass" {
			return problem("EVIDENCE_MISSING", "required verification is missing, pending or failed: "+check)
		}
	}
	if !actors["customer"] || !actors["worker"] {
		return problem("EVIDENCE_MISSING", "both actor observations need evidence")
	}
	backend := false
	for _, e := range checks {
		backend = backend || e.Actor == "backend"
	}
	if !backend {
		return problem("EVIDENCE_MISSING", "independent backend verification is required")
	}
	return nil
}
func (s Store) Append(id string, e Event) (Event, error) {
	dir, err := s.dir(id)
	if err != nil {
		return e, err
	}
	unlock, err := lock(dir)
	if err != nil {
		return e, err
	}
	defer unlock()
	if _, err := os.Stat(filepath.Join(dir, "summary.json")); err == nil {
		return e, problem("ATTEMPT_FINISHED", "start a new attempt")
	} else if !errors.Is(err, os.ErrNotExist) {
		return e, err
	}
	a, prior, _, err := readAttempt(dir, false)
	if err != nil {
		return e, err
	}
	e.Sequence = len(prior) + 1
	e.At = time.Now().UTC()
	if err = validateEvent(a, prior, e); err != nil {
		return e, err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return e, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "events.jsonl"), os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return e, err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return e, err
	}
	return e, closeErr
}
func (s Store) Finish(id string) (Summary, error) {
	dir, err := s.dir(id)
	if err != nil {
		return Summary{}, err
	}
	unlock, err := lock(dir)
	if err != nil {
		return Summary{}, err
	}
	defer unlock()
	a, events, _, err := readAttempt(dir, false)
	if err != nil {
		return Summary{}, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		return Summary{}, err
	}
	var existing Summary
	finished := false
	if err = ReadJSON(filepath.Join(dir, "summary.json"), &existing); err == nil {
		if existing.JournalHash != Digest(b) {
			return existing, problem("JOURNAL_CORRUPT", "finished journal changed")
		}
		finished = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return existing, err
	}
	summary := Summary{Version: 1, RunID: a.RunID, AttemptID: a.ID, RetestOf: a.RetestOf, Phase: a.Phase, PlanHash: a.Plan.Hash, LibraryHash: a.Plan.LibraryHash, Started: a.At, Ended: time.Now().UTC(), JournalHash: Digest(b), Denominator: len(a.Plan.Cells), Cells: []CellResult{}}
	if finished {
		summary.Ended = existing.Ended
	}
	for _, c := range a.Plan.Cells {
		r := CellResult{ID: c.ID, Verdict: "not-run", Checks: []string{}}
		found := false
		latestChecks := map[string]string{}
		for _, e := range events {
			if e.Cell != c.ID {
				continue
			}
			if e.Kind == "verification" {
				latestChecks[e.Check] = e.Result
				if e.Result == "pass" && !slices.Contains(r.Checks, e.Check) {
					r.Checks = append(r.Checks, e.Check)
				}
			}
			if e.Kind == "verdict" {
				r.Verdict = e.Verdict
				found = true
				if e.Verdict == "not-applicable" {
					r.Exclusion = &Exclusion{"Private reviewed reason sha256:" + Digest([]byte(e.Reason)), "reviewer-" + Digest([]byte(e.Approver))[:12], e.At}
				}
			}
		}
		if !found {
			return summary, problem("EVIDENCE_MISSING", "every selected cell needs an explicit verdict")
		}
		r.Checks = slices.DeleteFunc(r.Checks, func(check string) bool { return latestChecks[check] != "pass" })
		r.Observed = observedEvidence(c, events)
		summary.Cells = append(summary.Cells, r)
	}
	if finished {
		x, _ := json.Marshal(summary)
		y, _ := json.Marshal(existing)
		if !bytes.Equal(x, y) {
			return summary, problem("JOURNAL_CORRUPT", "summary disagrees with journal")
		}
		return summary, nil
	}
	return summary, privateJSON(filepath.Join(dir, "summary.json"), summary, true)
}

func observedEvidence(c Cell, events []Event) bool {
	actors, checks := map[string]bool{}, map[string]Event{}
	for _, e := range events {
		if e.Cell != c.ID {
			continue
		}
		if e.Kind == "observation" && len(e.Evidence) > 0 {
			actors[e.Actor] = true
		}
		if e.Kind == "verification" {
			checks[e.Check] = e
		}
	}
	if !actors["customer"] || !actors["worker"] {
		return false
	}
	backend := false
	for _, name := range c.Required {
		e, ok := checks[name]
		if !ok || !slices.Contains([]string{"pass", "fail"}, e.Result) {
			return false
		}
		backend = backend || e.Actor == "backend"
	}
	return backend
}
func (s Store) Export(id, target string) error {
	summary, err := s.Finish(id)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return err
	}
	if len(secretscan.Find(string(b), secretscan.All, nil)) > 0 {
		return problem("REDACTION_FAILED", "summary contains secret material")
	}
	return writeFile(target, append(b, '\n'), true)
}
