// Package fleet is the one reader of Claude Code's session registry
// (~/.claude/sessions/<pid>.json): every session's state for the /fleet pane,
// proven-dead detection for `fleet revive`, Codex rows read-only, and the
// per-session job ledger the lazarus mod writes. The registry is the engine's
// and is never written here. Contract and rules: docs/fleet.md.
package fleet

import (
	"context"
	"path/filepath"
	goruntime "runtime"
	"sort"
	"time"

	"github.com/henderson-tech/vybava/internal/plugingc"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/shellword"
	"github.com/henderson-tech/vybava/internal/transcripts"
)

// Env is the machine a snapshot is read from. Every outside effect is a
// field, so tests drive fixtures and never the live process table.
type Env struct {
	Home string
	Now  time.Time
	// Registry overrides <Home>/.claude/sessions.
	Registry string
	// LedgerDir overrides <Home>/.local/state/vybava/fleet/ledger.
	LedgerDir string
	// Processes reads the live process table; nil means ps.
	Processes plugingc.ProcessLister
	// Codex reads live Codex processes; nil leaves the Codex rows out.
	Codex CodexReader
	// GOOS is the pidDomain this machine can judge; "" means runtime.GOOS.
	GOOS string
}

func (env Env) registryDir() string {
	if env.Registry != "" {
		return env.Registry
	}
	return filepath.Join(env.Home, ".claude", "sessions")
}

func (env Env) ledgerDir() string {
	if env.LedgerDir != "" {
		return env.LedgerDir
	}
	return filepath.Join(env.Home, ".local", "state", "vybava", "fleet", "ledger")
}

func (env Env) goos() string {
	if env.GOOS != "" {
		return env.GOOS
	}
	return goruntime.GOOS
}

// State is what the fleet view shows for a session: the registry's status,
// or dead/ended once its process is proven gone.
type State string

const (
	StateWaiting State = "waiting"
	StateBusy    State = "busy"
	StateIdle    State = "idle"
	StateShell   State = "shell"
	// StateDead is a session whose process is proven gone while it was busy
	// or waiting: work was lost.
	StateDead State = "dead"
	// StateEnded is a session whose process is proven gone while it was idle
	// or at its shell: nothing was running.
	StateEnded State = "ended"
	// StateUnknown is a registry status this reader does not know yet.
	StateUnknown State = "unknown"
)

// rank orders sessions for the view: what needs the human first.
var rank = map[State]int{StateWaiting: 0, StateDead: 1, StateBusy: 2, StateUnknown: 3, StateIdle: 4, StateShell: 5, StateEnded: 6}

// Session is one registry record as the fleet view shows it.
type Session struct {
	SessionID   string     `json:"sessionId"`
	PID         int        `json:"pid"`
	Name        string     `json:"name,omitempty"`
	Project     string     `json:"project"`
	Root        string     `json:"root,omitempty"`
	Worktree    string     `json:"worktree,omitempty"`
	CWD         string     `json:"cwd"`
	Status      string     `json:"status"`
	State       State      `json:"state"`
	WaitingFor  string     `json:"waitingFor,omitempty"`
	StatusSince time.Time  `json:"statusSince"`
	AgeSeconds  int64      `json:"ageSeconds"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	Version     string     `json:"version,omitempty"`
	Kind        string     `json:"kind,omitempty"`
	Entrypoint  string     `json:"entrypoint,omitempty"`
	Liveness    Liveness   `json:"liveness"`
	OpenJobs    int        `json:"openJobs"`
	Resume      string     `json:"resume"`
}

// Counts tallies sessions by state.
type Counts struct {
	Total   int `json:"total"`
	Waiting int `json:"waiting"`
	Busy    int `json:"busy"`
	Idle    int `json:"idle"`
	Shell   int `json:"shell"`
	Dead    int `json:"dead"`
	Ended   int `json:"ended"`
	Unknown int `json:"unknown"`
}

func (c *Counts) add(state State) {
	c.Total++
	switch state {
	case StateWaiting:
		c.Waiting++
	case StateBusy:
		c.Busy++
	case StateIdle:
		c.Idle++
	case StateShell:
		c.Shell++
	case StateDead:
		c.Dead++
	case StateEnded:
		c.Ended++
	default:
		c.Unknown++
	}
}

// Project is one repository's share of the fleet.
type Project struct {
	Project string `json:"project"`
	Root    string `json:"root,omitempty"`
	Counts  Counts `json:"counts"`
}

// Snapshot is the whole fleet at one instant: Sessions flat and
// waiting-first, Projects ordered the same way, Codex read-only.
type Snapshot struct {
	GeneratedAt time.Time  `json:"generatedAt"`
	Registry    string     `json:"registry"`
	Counts      Counts     `json:"counts"`
	Projects    []Project  `json:"projects"`
	Sessions    []Session  `json:"sessions"`
	Codex       []CodexRow `json:"codex"`
}

// Read builds the snapshot. Warnings come back as diagnostics; the only
// error that fails it is a registry whose shape as a whole is unknown, or a
// directory that cannot be listed.
func Read(ctx context.Context, env Env) (Snapshot, []runx.Diagnostic, error) {
	snap := Snapshot{GeneratedAt: env.Now, Registry: env.registryDir(), Projects: []Project{}, Sessions: []Session{}, Codex: []CodexRow{}}
	records, diags, err := readRegistry(env.registryDir())
	if err != nil {
		return snap, diags, err
	}
	view, viewDiag := env.processView(ctx)
	diags = append(diags, viewDiag...)

	roots := resolver{home: env.Home, cache: map[string][2]string{}}
	projects := map[string]*Project{}
	for _, rec := range records {
		liveness := view.classify(rec.PID, rec.ProcStart, rec.PIDDomain, env.goos(), env.Now.Location())
		session := roots.session(rec, liveness, env.Now)
		ledger, err := loadLedger(env.ledgerDir(), rec.SessionID)
		if err != nil {
			diags = append(diags, warning(DiagLedgerUnreadable, err.Error()))
		}
		session.OpenJobs = ledger.open()
		snap.Sessions = append(snap.Sessions, session)
		snap.Counts.add(session.State)
		project, ok := projects[session.Root+"\x00"+session.Project]
		if !ok {
			project = &Project{Project: session.Project, Root: session.Root}
			projects[session.Root+"\x00"+session.Project] = project
		}
		project.Counts.add(session.State)
	}
	sortSessions(snap.Sessions)
	for _, project := range projects {
		snap.Projects = append(snap.Projects, *project)
	}
	sort.Slice(snap.Projects, func(i, j int) bool {
		a, b := snap.Projects[i].Counts, snap.Projects[j].Counts
		if a.Waiting != b.Waiting {
			return a.Waiting > b.Waiting
		}
		if a.Dead != b.Dead {
			return a.Dead > b.Dead
		}
		if a.Busy != b.Busy {
			return a.Busy > b.Busy
		}
		return snap.Projects[i].Project < snap.Projects[j].Project
	})

	if env.Codex != nil {
		rows, warnings, err := env.Codex(ctx, env.Now)
		if err != nil {
			diags = append(diags, warning(DiagCodexUnavailable, err.Error()))
		}
		for _, w := range warnings {
			diags = append(diags, warning(DiagCodexPartial, w))
		}
		for i := range rows {
			rows[i].Project, rows[i].Root = roots.project(rows[i].CWD)
		}
		if rows != nil {
			snap.Codex = rows
		}
	}
	return snap, diags, nil
}

// sortSessions orders what needs the human first, the longest-waiting first
// within a state.
func sortSessions(sessions []Session) {
	sort.SliceStable(sessions, func(i, j int) bool {
		a, b := sessions[i], sessions[j]
		if rank[a.State] != rank[b.State] {
			return rank[a.State] < rank[b.State]
		}
		if !a.StatusSince.Equal(b.StatusSince) {
			return a.StatusSince.Before(b.StatusSince)
		}
		return a.SessionID < b.SessionID
	})
}

// resolver maps each working directory to its repository once per read:
// dozens of sessions share a handful of checkouts.
type resolver struct {
	home  string
	cache map[string][2]string
}

// project names the repository a directory belongs to; a linked worktree
// folds into its main checkout. A directory outside any repository is its
// own project, and the home directory is "~".
func (r resolver) project(cwd string) (project, root string) {
	if hit, ok := r.cache[cwd]; ok {
		return hit[0], hit[1]
	}
	root, _ = transcripts.GitRoot(cwd)
	switch {
	case root == "":
		project = ""
	case isRepo(root), root != filepath.Clean(cwd):
		// A repository, or a removed worktree folded lexically into the
		// directory that held it.
		project = filepath.Base(root)
	case cwd == r.home:
		project, root = "~", ""
	default:
		project, root = filepath.Base(cwd), ""
	}
	r.cache[cwd] = [2]string{project, root}
	return project, root
}

func (r resolver) session(rec record, liveness Liveness, now time.Time) Session {
	project, root := r.project(rec.CWD)
	since := time.UnixMilli(rec.StatusUpdatedAt).In(now.Location())
	session := Session{
		SessionID: rec.SessionID, PID: rec.PID, Name: rec.Name,
		Project: project, Root: root, Worktree: worktreeLabel(rec.CWD, root), CWD: rec.CWD,
		Status: rec.Status, State: stateOf(rec.Status, liveness), WaitingFor: rec.WaitingFor,
		StatusSince: since, AgeSeconds: int64(now.Sub(since) / time.Second),
		Version: rec.Version, Kind: rec.Kind, Entrypoint: rec.Entrypoint,
		Liveness: liveness, Resume: ResumeLine(rec.CWD, rec.SessionID),
	}
	if session.AgeSeconds < 0 {
		session.AgeSeconds = 0
	}
	if rec.StartedAt > 0 {
		started := time.UnixMilli(rec.StartedAt).In(now.Location())
		session.StartedAt = &started
	}
	return session
}

// stateOf maps a registry status and a liveness verdict to the view's state.
func stateOf(status string, liveness Liveness) State {
	state := State(status)
	switch state {
	case StateWaiting, StateBusy, StateIdle, StateShell:
	default:
		state = StateUnknown
	}
	if !liveness.Dead() {
		return state
	}
	if state == StateIdle || state == StateShell {
		return StateEnded
	}
	return StateDead
}

// ResumeLine is the command that brings a session back in its own directory.
func ResumeLine(cwd, sessionID string) string {
	if cwd == "" {
		return "claude --resume " + shellword.Quote(sessionID)
	}
	return "cd " + shellword.Quote(cwd) + " && claude --resume " + shellword.Quote(sessionID)
}

func warning(code, detail string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "warning", Detail: detail}
}
