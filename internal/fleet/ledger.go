package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/henderson-tech/vybava/internal/runx"
)

// LedgerVersion is bumped on a breaking change of the ledger file.
const LedgerVersion = 1

// JobKind is what a session started in the background.
type JobKind string

const (
	JobWorkflow  JobKind = "workflow"
	JobShell     JobKind = "shell"
	JobMonitor   JobKind = "monitor"
	JobAgent     JobKind = "agent"
	JobLimitWait JobKind = "limit-wait"
	JobPark      JobKind = "park"
)

var jobKinds = []JobKind{JobWorkflow, JobShell, JobMonitor, JobAgent, JobLimitWait, JobPark}

// JobStatus is where a job stands. Only started is open.
type JobStatus string

const (
	JobStarted   JobStatus = "started"
	JobCompleted JobStatus = "completed"
	JobFailed    JobStatus = "failed"
	JobKilled    JobStatus = "killed"
	JobStopped   JobStatus = "stopped"
)

var jobStatuses = []JobStatus{JobStarted, JobCompleted, JobFailed, JobKilled, JobStopped}

const (
	maxJobs           = 200
	maxDescription    = 120
	maxIDLength       = 128
	maxScriptPath     = 1024
	maxEventBytes     = 64 << 10
	digestHexChars    = 12
	ledgerLockTimeout = 5 * time.Second
)

// Event is one line of news about a job, read from stdin by `fleet ledger
// record`. Command and CWD are input only: the command is hashed into a
// digest and never stored, the directory lands on the ledger itself.
type Event struct {
	Kind        JobKind    `json:"kind"`
	ID          string     `json:"id"`
	Status      JobStatus  `json:"status"`
	At          *time.Time `json:"at,omitempty"`
	RunID       string     `json:"runId,omitempty"`
	ScriptPath  string     `json:"scriptPath,omitempty"`
	Description string     `json:"description,omitempty"`
	Command     string     `json:"command,omitempty"`
	Until       *time.Time `json:"until,omitempty"`
	CWD         string     `json:"cwd,omitempty"`
}

// Job is a ledger row: one background job, keyed by kind and id.
type Job struct {
	Kind        JobKind    `json:"kind"`
	ID          string     `json:"id"`
	Status      JobStatus  `json:"status"`
	RunID       string     `json:"runId,omitempty"`
	ScriptPath  string     `json:"scriptPath,omitempty"`
	Description string     `json:"description,omitempty"`
	Digest      string     `json:"digest,omitempty"`
	Until       *time.Time `json:"until,omitempty"`
	StartedAt   time.Time  `json:"startedAt"`
	EndedAt     *time.Time `json:"endedAt,omitempty"`
	UpdatedAt   time.Time  `json:"updatedAt"`
}

// Open reports a job still running as far as the ledger knows.
func (j Job) Open() bool { return j.Status == JobStarted }

// Owner is the session process that wrote the ledger, taken from the
// registry, so a ledger outliving its registry record can still be proven
// dead by the same rule as a session.
type Owner struct {
	PID       int    `json:"pid"`
	ProcStart string `json:"procStart"`
	PIDDomain string `json:"pidDomain,omitempty"`
}

// Ledger is one session's record of the background jobs it started:
// <ledger dir>/<sessionId>.json, written only by that session through
// `fleet ledger record`.
type Ledger struct {
	Version   int       `json:"version"`
	SessionID string    `json:"sessionId"`
	CWD       string    `json:"cwd,omitempty"`
	Owner     *Owner    `json:"owner,omitempty"`
	UpdatedAt time.Time `json:"updatedAt"`
	Jobs      []Job     `json:"jobs"`
}

func (l Ledger) open() int {
	count := 0
	for _, job := range l.Jobs {
		if job.Open() {
			count++
		}
	}
	return count
}

// OpenJobs are the jobs still running as far as the ledger knows.
func (l Ledger) OpenJobs() []Job {
	jobs := []Job{}
	for _, job := range l.Jobs {
		if job.Open() {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)

// CheckSessionID refuses anything that is not a session id, which also keeps
// a ledger path inside the ledger directory.
func CheckSessionID(id string) error {
	if !sessionIDPattern.MatchString(id) {
		return runx.DiagError{Diag: runx.Diagnostic{Code: DiagSessionInvalid, Severity: "error",
			Detail: fmt.Sprintf("%q is not a session id", id), Fix: "pass the Claude Code session id: --session <uuid>"}}
	}
	return nil
}

// ReadEvent decodes exactly one event from stdin.
func ReadEvent(raw []byte) (Event, error) {
	var ev Event
	if len(raw) > maxEventBytes {
		return ev, invalidEvent(fmt.Sprintf("%d bytes on stdin; one event is at most %d", len(raw), maxEventBytes))
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ev); err != nil {
		return ev, invalidEvent("stdin is not one ledger event: " + err.Error())
	}
	if decoder.More() {
		return ev, invalidEvent("stdin carries more than one event")
	}
	return ev, nil
}

func invalidEvent(detail string) error {
	return runx.DiagError{Diag: runx.Diagnostic{Code: DiagLedgerEventInvalid, Severity: "error", Detail: detail,
		Fix: `echo '{"kind":"workflow","id":"<id>","status":"started"}' | fleet ledger record --session <id>`}}
}

func (ev Event) validate() error {
	if !containsKind(ev.Kind) {
		return invalidEvent(fmt.Sprintf("kind %q is not one of %v", ev.Kind, jobKinds))
	}
	if !containsStatus(ev.Status) {
		return invalidEvent(fmt.Sprintf("status %q is not one of %v", ev.Status, jobStatuses))
	}
	if ev.ID == "" || len(ev.ID) > maxIDLength {
		return invalidEvent(fmt.Sprintf("id must be 1–%d characters", maxIDLength))
	}
	if len(ev.RunID) > maxIDLength {
		return invalidEvent(fmt.Sprintf("runId is longer than %d characters", maxIDLength))
	}
	if len(ev.ScriptPath) > maxScriptPath {
		return invalidEvent(fmt.Sprintf("scriptPath is longer than %d characters", maxScriptPath))
	}
	return nil
}

// Record applies one event to the session's ledger under its lock and
// returns the ledger as written. The command text is reduced to a digest and
// the description to one short line before anything touches disk.
func Record(env Env, sessionID string, ev Event) (Ledger, error) {
	if err := CheckSessionID(sessionID); err != nil {
		return Ledger{}, err
	}
	if err := ev.validate(); err != nil {
		return Ledger{}, err
	}
	dir := env.ledgerDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Ledger{}, err
	}
	unlock, err := lockLedger(filepath.Join(dir, sessionID+".lock"), ledgerLockTimeout)
	if err != nil {
		if errors.Is(err, errLedgerBusy) {
			return Ledger{}, runx.DiagError{Diag: runx.Diagnostic{Code: DiagLedgerBusy, Severity: "error",
				Detail: fmt.Sprintf("another writer held %s's ledger for over %s", sessionID, ledgerLockTimeout)}}
		}
		return Ledger{}, err
	}
	defer unlock()

	ledger, err := loadLedger(dir, sessionID)
	if err != nil {
		return Ledger{}, runx.DiagError{Diag: runx.Diagnostic{Code: DiagLedgerUnreadable, Severity: "error", Detail: err.Error(),
			Fix: "inspect or remove " + ledgerPath(dir, sessionID)}}
	}
	at := env.Now
	if ev.At != nil && !ev.At.IsZero() {
		at = *ev.At
	}
	if ev.CWD != "" {
		ledger.CWD = ev.CWD
	}
	if owner, cwd, ok := ownerOf(env, sessionID); ok {
		ledger.Owner = &owner
		if ledger.CWD == "" {
			ledger.CWD = cwd
		}
	}
	ledger.apply(ev, at)
	ledger.UpdatedAt = env.Now
	if err := writeLedger(dir, ledger); err != nil {
		return Ledger{}, err
	}
	return ledger, nil
}

// Close marks every open job of a session stopped: the human dismissed them,
// or they were resumed elsewhere, so revive stops offering them.
func Close(env Env, sessionID string) (Ledger, error) {
	if err := CheckSessionID(sessionID); err != nil {
		return Ledger{}, err
	}
	ledger, err := Show(env, sessionID)
	if err != nil {
		return Ledger{}, err
	}
	for _, job := range ledger.OpenJobs() {
		if ledger, err = Record(env, sessionID, Event{Kind: job.Kind, ID: job.ID, Status: JobStopped}); err != nil {
			return Ledger{}, err
		}
	}
	return ledger, nil
}

// Show reads a session's ledger; a session that never recorded has an empty one.
func Show(env Env, sessionID string) (Ledger, error) {
	if err := CheckSessionID(sessionID); err != nil {
		return Ledger{}, err
	}
	ledger, err := loadLedger(env.ledgerDir(), sessionID)
	if err != nil {
		return Ledger{}, runx.DiagError{Diag: runx.Diagnostic{Code: DiagLedgerUnreadable, Severity: "error", Detail: err.Error(),
			Fix: "inspect or remove " + ledgerPath(env.ledgerDir(), sessionID)}}
	}
	return ledger, nil
}

// apply upserts the event's job, keyed by kind and id.
func (l *Ledger) apply(ev Event, at time.Time) {
	job := Job{Kind: ev.Kind, ID: ev.ID, StartedAt: at}
	index := -1
	for i, existing := range l.Jobs {
		if existing.Kind == ev.Kind && existing.ID == ev.ID {
			job, index = existing, i
			break
		}
	}
	job.Status = ev.Status
	job.UpdatedAt = at
	if ev.RunID != "" {
		job.RunID = ev.RunID
	}
	if ev.ScriptPath != "" {
		job.ScriptPath = ev.ScriptPath
	}
	if ev.Description != "" {
		job.Description = oneLine(ev.Description, maxDescription)
	}
	if ev.Command != "" {
		sum := sha256.Sum256([]byte(ev.Command))
		job.Digest = hex.EncodeToString(sum[:])[:digestHexChars]
	}
	if ev.Until != nil {
		until := *ev.Until
		job.Until = &until
	}
	if job.Open() {
		job.EndedAt = nil
	} else if job.EndedAt == nil {
		ended := at
		job.EndedAt = &ended
	}
	if index < 0 {
		l.Jobs = append(l.Jobs, job)
	} else {
		l.Jobs[index] = job
	}
	l.prune()
}

// prune keeps the ledger bounded by dropping the oldest finished jobs; open
// jobs are never dropped.
func (l *Ledger) prune() {
	for len(l.Jobs) > maxJobs {
		oldest := -1
		for i, job := range l.Jobs {
			if job.Open() {
				continue
			}
			if oldest < 0 || job.UpdatedAt.Before(l.Jobs[oldest].UpdatedAt) {
				oldest = i
			}
		}
		if oldest < 0 {
			return
		}
		l.Jobs = append(l.Jobs[:oldest], l.Jobs[oldest+1:]...)
	}
}

// ownerOf finds the session's registry record, the authority on which
// process writes the ledger.
func ownerOf(env Env, sessionID string) (Owner, string, bool) {
	records, _, err := readRegistry(env.registryDir())
	if err != nil {
		return Owner{}, "", false
	}
	for _, rec := range records {
		if rec.SessionID == sessionID {
			return Owner{PID: rec.PID, ProcStart: rec.ProcStart, PIDDomain: rec.PIDDomain}, rec.CWD, true
		}
	}
	return Owner{}, "", false
}

func ledgerPath(dir, sessionID string) string {
	return filepath.Join(dir, sessionID+".json")
}

// loadLedger reads a ledger; a missing file is an empty ledger.
func loadLedger(dir, sessionID string) (Ledger, error) {
	empty := Ledger{Version: LedgerVersion, SessionID: sessionID, Jobs: []Job{}}
	raw, err := os.ReadFile(ledgerPath(dir, sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	var ledger Ledger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return empty, fmt.Errorf("ledger %s: %w", ledgerPath(dir, sessionID), err)
	}
	if ledger.Version != LedgerVersion {
		return empty, fmt.Errorf("ledger %s is version %d; this fleet reads version %d", ledgerPath(dir, sessionID), ledger.Version, LedgerVersion)
	}
	if ledger.Jobs == nil {
		ledger.Jobs = []Job{}
	}
	return ledger, nil
}

// writeLedger replaces the file atomically: a reader sees the old ledger or
// the new one, never half of either.
func writeLedger(dir string, ledger Ledger) error {
	body, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(ledgerPath(dir, ledger.SessionID), append(body, '\n'))
}

// writeAtomic replaces path through a synced temp file in the same
// directory and one rename.
func writeAtomic(path string, body []byte) (err error) {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(file.Name())
		}
	}()
	if _, err = file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// listLedgers names every session that has a ledger.
func listLedgers(dir string) ([]string, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(paths))
	for _, path := range paths {
		id := strings.TrimSuffix(filepath.Base(path), ".json")
		if sessionIDPattern.MatchString(id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func oneLine(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit-1]) + "…"
}

func containsKind(kind JobKind) bool {
	for _, k := range jobKinds {
		if k == kind {
			return true
		}
	}
	return false
}

func containsStatus(status JobStatus) bool {
	for _, s := range jobStatuses {
		if s == status {
			return true
		}
	}
	return false
}
