package fleet

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/cmux"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/secretscan"
)

// Cmux is what fleet needs from the cmux terminal; cmux.Client satisfies it.
type Cmux interface {
	Check(ctx context.Context) cmux.Status
	Resolve(ctx context.Context, pid int) (cmux.Target, error)
	ReadText(ctx context.Context, surfaceID string, lines int) (cmux.Screen, error)
	Paste(ctx context.Context, surfaceID, text string, submit bool) (cmux.PasteResult, error)
	SendText(ctx context.Context, surfaceID, text string) error
	Focus(ctx context.Context, windowID, workspaceID, surfaceID string) error
}

// Action is what Fleet.app may do with a listed row. Go decides; the app
// renders exactly these.
type Action string

const (
	ActionFocus  Action = "focus"
	ActionScreen Action = "screen"
	ActionReply  Action = "reply"
)

// MaxReplyBytes bounds a reply read from stdin.
const MaxReplyBytes = 32 << 10

// Target names what an action is for: a Claude session by id, or (focus
// only) any agent process by pid — how Codex rows are addressed.
type Target struct {
	SessionID string
	PID       int
}

// located is a live session found at action time, its surface resolved by
// pid just now — never from the published snapshot.
type located struct {
	sessionID string
	pid       int
	state     State
	codex     bool
	surface   cmux.Target
}

// ScreenResult is `fleet screen`: what a session's terminal shows, redacted.
type ScreenResult struct {
	SessionID  string    `json:"sessionId"`
	PID        int       `json:"pid"`
	SurfaceID  string    `json:"surfaceId"`
	Text       string    `json:"text"`
	Redactions int       `json:"redactions"`
	Dialog     *Dialog   `json:"dialog,omitempty"`
	ReadAt     time.Time `json:"readAt"`
}

// ReplyResult is `fleet reply`: what was sent.
type ReplyResult struct {
	SessionID string `json:"sessionId"`
	SurfaceID string `json:"surfaceId"`
	// Mode is "text" (pasted and submitted) or "option" (one key pressed).
	Mode      string `json:"mode"`
	Key       string `json:"key,omitempty"`
	Bytes     int    `json:"bytes,omitempty"`
	Submitted bool   `json:"submitted"`
}

// FocusResult is `fleet focus`: where cmux was brought to.
type FocusResult struct {
	SessionID   string `json:"sessionId,omitempty"`
	PID         int    `json:"pid"`
	SurfaceID   string `json:"surfaceId"`
	WorkspaceID string `json:"workspaceId"`
	WindowID    string `json:"windowId,omitempty"`
}

func actionError(code, detail, fix string) error {
	return runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
}

// locate finds the target alive and resolves its surface by live pid.
func (env Env) locate(ctx context.Context, t Target) (located, error) {
	if env.Cmux == nil {
		return located{}, actionError(DiagCmuxUnavailable, "no cmux client configured", "")
	}
	var found located
	switch {
	case t.SessionID != "":
		records, _, err := readRegistry(env.registryDir())
		if err != nil {
			return found, err
		}
		var rec *record
		for i := range records {
			if records[i].SessionID == t.SessionID {
				rec = &records[i]
				break
			}
		}
		if rec == nil {
			return found, actionError(DiagSessionNotFound, "no live registry entry for session "+t.SessionID, "fleet --json")
		}
		view, _ := env.processView(ctx)
		liveness := view.classify(rec.PID, rec.ProcStart, rec.PIDDomain, env.goos(), env.Now.Location())
		if liveness == LivenessUnknown {
			return found, actionError(DiagLivenessUnavailable, fmt.Sprintf("cannot prove session %s alive (pid %d): the process table is unreadable or the record is from another OS", t.SessionID, rec.PID), "")
		}
		if liveness != LivenessAlive {
			return found, actionError(DiagSessionGone, fmt.Sprintf("session %s is %s (pid %d)", t.SessionID, liveness, rec.PID), "fleet revive")
		}
		found = located{sessionID: rec.SessionID, pid: rec.PID, state: stateOf(rec.Status, liveness)}
	case t.PID > 0:
		view, diags := env.processView(ctx)
		if len(diags) > 0 {
			return found, actionError(DiagLivenessUnavailable, diags[0].Detail, "")
		}
		process, ok := view.table[t.PID]
		if !ok {
			return found, actionError(DiagSessionGone, fmt.Sprintf("no process %d", t.PID), "")
		}
		base := strings.ToLower(filepath.Base(process.Command))
		switch {
		case strings.Contains(base, "codex"):
			found = located{pid: t.PID, codex: true}
		case couldBeClaude(process.Command):
			found = located{pid: t.PID}
		default:
			return found, actionError(DiagSessionInvalid, fmt.Sprintf("pid %d runs %s, not an agent", t.PID, base), "")
		}
	default:
		return found, actionError(DiagSessionInvalid, "name a session (--session <id>) or a process (--pid <n>)", "fleet --json")
	}
	surface, err := env.Cmux.Resolve(ctx, found.pid)
	if err != nil {
		return found, cmuxError(err, found.pid)
	}
	found.surface = surface
	return found, nil
}

func cmuxError(err error, pid int) error {
	if cmux.ErrNotFound(err) {
		return actionError(DiagNotInCmux, fmt.Sprintf("pid %d does not run in a cmux surface", pid), "")
	}
	var unreachable *cmux.UnreachableError
	if errors.As(err, &unreachable) {
		return actionError(DiagCmuxUnavailable, err.Error(), "cmux Settings → Socket Control → Automation")
	}
	return err
}

// Screen reads a Claude session's visible terminal, redacted, with the
// dialog open on it if any. lines > 0 reads that much scrollback instead.
func Screen(ctx context.Context, env Env, sessionID string, lines int) (ScreenResult, error) {
	found, err := env.locate(ctx, Target{SessionID: sessionID})
	if err != nil {
		return ScreenResult{}, err
	}
	screen, err := env.Cmux.ReadText(ctx, found.surface.SurfaceID, lines)
	if err != nil {
		return ScreenResult{}, cmuxError(err, found.pid)
	}
	out := ScreenResult{SessionID: found.sessionID, PID: found.pid, SurfaceID: found.surface.SurfaceID, ReadAt: env.Now}
	out.Text, out.Redactions = redact(screen.Text)
	if dialog := ParseDialog(screen.Text); dialog != nil {
		out.Dialog = dialog.redacted()
	}
	return out, nil
}

// OpenDialog reads a Claude session's screen and returns the dialog open on
// it, or nil.
func OpenDialog(ctx context.Context, env Env, sessionID string) (*Dialog, error) {
	out, err := Screen(ctx, env, sessionID, 0)
	return out.Dialog, err
}

// Reply is what `fleet reply` sends: text for a plain input wait, or one
// option key of the dialog the human chose from (Expect is its fingerprint).
type Reply struct {
	Text   string
	Option string
	Expect string
}

// SendReply answers a Claude session in place. Text is pasted and submitted
// once — refused while any dialog is open, so typing can never approve a
// tool call. An option is one key press, sent only while the screen still
// shows the dialog named by Expect.
func SendReply(ctx context.Context, env Env, t Target, reply Reply) (ReplyResult, error) {
	if t.SessionID == "" {
		if found, err := env.locate(ctx, t); err != nil {
			return ReplyResult{}, err
		} else if found.codex {
			return ReplyResult{}, actionError(DiagReplyRefused, "Codex sessions are read-only: no waiting signal tells when a reply is safe", fmt.Sprintf("fleet focus --pid %d", t.PID))
		}
		return ReplyResult{}, actionError(DiagSessionInvalid, "a reply names a Claude session (--session <id>)", "fleet --json")
	}
	sessionID := t.SessionID
	text := strings.TrimSuffix(reply.Text, "\n")
	switch {
	case reply.Option == "" && strings.TrimSpace(text) == "":
		return ReplyResult{}, actionError(DiagReplyInvalid, "nothing to send: give --option <key> --expect <fingerprint>, or text on stdin", "")
	case reply.Option != "" && text != "":
		return ReplyResult{}, actionError(DiagReplyInvalid, "an option and text together: a dialog takes a key, a plain wait takes text", "")
	case len(reply.Text) > MaxReplyBytes:
		// Measured before the newline is trimmed: the CLI reads one byte past
		// the cap, and that byte must never be what makes a longer text fit.
		return ReplyResult{}, actionError(DiagReplyInvalid, fmt.Sprintf("text is over %d bytes", MaxReplyBytes), "")
	case reply.Option != "" && reply.Expect == "":
		return ReplyResult{}, actionError(DiagReplyInvalid, "--option needs --expect <fingerprint> of the dialog it answers", "fleet dialog --session "+sessionID)
	}
	found, err := env.locate(ctx, t)
	if err != nil {
		return ReplyResult{}, err
	}
	surface := found.surface.SurfaceID
	screen, err := env.Cmux.ReadText(ctx, surface, 0)
	if err != nil {
		return ReplyResult{}, cmuxError(err, found.pid)
	}
	dialog := ParseDialog(screen.Text)
	out := ReplyResult{SessionID: found.sessionID, SurfaceID: surface}

	if reply.Option != "" {
		switch {
		case dialog == nil:
			return out, actionError(DiagNoDialog, "no dialog is open on the session's screen any more", "fleet screen --session "+sessionID)
		case dialog.Fingerprint != reply.Expect:
			return out, actionError(DiagDialogChanged, "the open dialog is not the one the option was chosen from", "fleet dialog --session "+sessionID)
		case !dialog.Answerable:
			return out, actionError(DiagOptionInvalid, "this dialog cannot be answered with one key (multi-select or more than 9 options)", "fleet focus --session "+sessionID)
		}
		if _, ok := dialog.Option(reply.Option); !ok {
			return out, actionError(DiagOptionInvalid, fmt.Sprintf("option %q is not an answer of the open dialog", reply.Option), "fleet dialog --session "+sessionID)
		}
		if err := env.Cmux.SendText(ctx, surface, reply.Option); err != nil {
			return out, cmuxError(err, found.pid)
		}
		out.Mode, out.Key, out.Submitted = "option", reply.Option, true
		return out, nil
	}

	switch {
	case dialog != nil:
		return out, actionError(DiagDialogOpen, fmt.Sprintf("a %s dialog is open; text could answer it — choose an option instead", dialog.Kind), "fleet dialog --session "+sessionID)
	case found.state != StateWaiting && found.state != StateIdle:
		return out, actionError(DiagReplyRefused, fmt.Sprintf("the session is %s, not at its prompt", found.state), "fleet focus --session "+sessionID)
	case !AtPrompt(screen.Text):
		return out, actionError(DiagReplyRefused, "the screen does not show the conversation's prompt (the agents view, a picker, or a turn running)", "fleet focus --session "+sessionID)
	}
	pasted, err := env.Cmux.Paste(ctx, surface, text, true)
	if err != nil {
		return out, cmuxError(err, found.pid)
	}
	out.Mode, out.Bytes, out.Submitted = "text", len(text), pasted.Submitted
	if !pasted.Submitted {
		// The text is at the prompt already; sending it again would double it.
		return out, actionError(DiagNotSubmitted, "the text was pasted but not submitted ("+orUnknown(pasted.SubmitError)+")", "fleet focus --session "+sessionID)
	}
	return out, nil
}

// Focus brings cmux to the target's window, workspace and surface,
// resolved by live pid now. Codex rows are addressed by pid.
func Focus(ctx context.Context, env Env, t Target) (FocusResult, error) {
	found, err := env.locate(ctx, t)
	if err != nil {
		return FocusResult{}, err
	}
	// read_text is the one cheap call that names the surface's window.
	screen, err := env.Cmux.ReadText(ctx, found.surface.SurfaceID, 1)
	if err != nil {
		return FocusResult{}, cmuxError(err, found.pid)
	}
	if err := env.Cmux.Focus(ctx, screen.WindowID, found.surface.WorkspaceID, found.surface.SurfaceID); err != nil {
		return FocusResult{}, cmuxError(err, found.pid)
	}
	if env.Activate != nil {
		if err := env.Activate(ctx); err != nil {
			return FocusResult{}, err
		}
	}
	return FocusResult{SessionID: found.sessionID, PID: found.pid, SurfaceID: found.surface.SurfaceID,
		WorkspaceID: found.surface.WorkspaceID, WindowID: screen.WindowID}, nil
}

// redact withholds every secret shape secretscan knows before screen text
// leaves Go.
func redact(text string) (string, int) {
	spans := secretscan.Find(text, secretscan.All, nil)
	if len(spans) == 0 {
		return text, 0
	}
	return secretscan.Redact(text, spans), len(spans)
}

func (d *Dialog) redacted() *Dialog {
	out := *d
	out.Title, _ = redact(d.Title)
	out.Question, _ = redact(d.Question)
	out.Detail, _ = redact(d.Detail)
	out.Tabs = make([]DialogTab, len(d.Tabs))
	for i, tab := range d.Tabs {
		tab.Label, _ = redact(tab.Label)
		out.Tabs[i] = tab
	}
	out.Options = make([]DialogOption, len(d.Options))
	for i, o := range d.Options {
		o.Label, _ = redact(o.Label)
		o.Description, _ = redact(o.Description)
		out.Options[i] = o
	}
	return &out
}

func orUnknown(s string) string {
	if s == "" {
		return "no reason given"
	}
	return s
}
