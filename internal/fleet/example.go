package fleet

import (
	"time"

	"github.com/henderson-tech/vybava/internal/cmux"
	"github.com/henderson-tech/vybava/internal/runx"
)

// exampleScreen is a permission prompt as cmux reads it.
const exampleScreen = `⏺ Bash(touch notes.txt)
  ⎿  Waiting…

────────────────────────────────────────
 Bash command
 Create notes.txt
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 touch notes.txt
╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
 Do you want to proceed?
 ❯ 1. Yes
   2. Yes, and always allow access to notes/ from this project
   3. No

 Esc to cancel · Tab to amend`

// Example is a deterministic sample of every contract Fleet.app decodes —
// `fleet schema --example`. The app's tests decode it, so a Go-side change
// that the generated Swift cannot read fails there, not in front of Lukáš.
func Example() map[string]any {
	at := time.Date(2026, 10, 3, 14, 37, 18, 223000000, time.FixedZone("CEST", 2*3600))
	since := at.Add(-25 * time.Minute)
	started := at.Add(-3 * time.Hour)
	surface := func(n string) Surface { return Surface{SurfaceID: "SURFACE-" + n, WorkspaceID: "WORKSPACE-" + n} }
	session := func(id string, state State, waitingFor string) Session {
		return Session{SessionID: id, PID: 4242, Name: "vybava-" + id, Project: "vybava", Root: "/Users/me/Work/vybava",
			Worktree: "feat/fleet-actions-vt-5092", CWD: "/Users/me/Work/vybava/.worktrees/feat/fleet-actions-vt-5092",
			Status: string(state), State: state, WaitingFor: waitingFor, StatusSince: since, AgeSeconds: 1500,
			StartedAt: &started, Version: "2.1.287", Kind: "interactive", Entrypoint: "cli", Liveness: LivenessAlive,
			Resume: ResumeLine("/Users/me/Work/vybava", id)}
	}
	counts := Counts{Total: 2, Waiting: 1, Busy: 1}
	snapshot := Published{
		Version: PublishedVersion, GeneratedAt: at, CodexAt: &at,
		Cmux:     cmux.Status{State: cmux.StateOK, Socket: "/Users/me/.local/state/cmux/cmux.sock", Version: cmux.MinVersion, AccessMode: "automation"},
		Counts:   counts,
		Projects: []Project{{Project: "vybava", Root: "/Users/me/Work/vybava", Counts: counts}},
		Sessions: []Hosted{
			{Session: session("waiting-1", StateWaiting, "input needed"), Surface: surface("1"), Actions: []Action{ActionFocus, ActionScreen, ActionReply}},
			{Session: session("busy-1", StateBusy, ""), Surface: surface("2"), Actions: []Action{ActionFocus, ActionScreen}},
		},
		Codex: []HostedCodex{{CodexRow: CodexRow{PID: 5151, TTY: "ttys012", ThreadID: "thread-1", Name: "sidekick",
			Project: "vybava", Root: "/Users/me/Work/vybava", CWD: "/Users/me/Work/vybava", Branch: "main",
			StartedAt: &started, LastCallAt: &at, Calls: 12}, Surface: surface("3"), Actions: []Action{ActionFocus}}},
		Hidden:      3,
		Diagnostics: []runx.Diagnostic{{Code: DiagCodexPartial, Severity: "warning", Detail: "one rollout unreadable"}},
	}
	dialog := ParseDialog(exampleScreen)
	envelope := func(verb string, data any) runx.Envelope {
		return runx.Envelope{V: runx.EnvelopeVersion, OK: true, Verb: verb, Data: data, Diagnostics: []runx.Diagnostic{}, Next: []string{}}
	}
	return map[string]any{
		"snapshot": snapshot,
		"screen": envelope("screen", ScreenResult{SessionID: "waiting-1", PID: 4242, SurfaceID: "SURFACE-1",
			Text: exampleScreen, Dialog: dialog, ReadAt: at}),
		"reply": envelope("reply", ReplyResult{SessionID: "waiting-1", SurfaceID: "SURFACE-1", Mode: "option", Key: "1", Submitted: true}),
		"focus": envelope("focus", FocusResult{SessionID: "waiting-1", PID: 4242, SurfaceID: "SURFACE-1", WorkspaceID: "WORKSPACE-1", WindowID: "WINDOW-1"}),
		"refused": runx.Envelope{V: runx.EnvelopeVersion, OK: false, Verb: "reply",
			Diagnostics: []runx.Diagnostic{{Code: DiagDialogOpen, Severity: "error", Detail: "a permission dialog is open; text could answer it — choose an option instead", Fix: "fleet dialog --session waiting-1"}},
			Next:        []string{"fleet dialog --session waiting-1"}},
	}
}
