package fleet

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/henderson-tech/vybava/internal/cmux"
	"github.com/henderson-tech/vybava/internal/plugingc"
	"github.com/henderson-tech/vybava/internal/runx"
)

// fakeCmux is cmux in memory: which pid each surface hosts and what each
// screen shows, with every write recorded.
type fakeCmux struct {
	mu       sync.Mutex
	status   cmux.Status
	surfaces map[int]cmux.Target
	screens  map[string]string
	unsubmit bool
	resolves int
	pastes   []string
	keys     []string
	focused  []string
}

func newFakeCmux() *fakeCmux {
	return &fakeCmux{status: cmux.Status{State: cmux.StateOK, Socket: "/tmp/cmux.sock"}, surfaces: map[int]cmux.Target{}, screens: map[string]string{}}
}

func (f *fakeCmux) host(pid int, surface, screen string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.surfaces[pid] = cmux.Target{SurfaceID: surface, WorkspaceID: "W-" + surface, Resolution: "corroborated"}
	f.screens[surface] = screen
}

func (f *fakeCmux) Check(context.Context) cmux.Status { return f.status }

func (f *fakeCmux) Resolve(_ context.Context, pid int) (cmux.Target, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resolves++
	if t, ok := f.surfaces[pid]; ok {
		return t, nil
	}
	return cmux.Target{}, &cmux.Error{Method: "agent.resolve_delivery_target", Code: "not_found"}
}

func (f *fakeCmux) ReadText(_ context.Context, surface string, _ int) (cmux.Screen, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cmux.Screen{Text: f.screens[surface], SurfaceID: surface, WindowID: "N1"}, nil
}

func (f *fakeCmux) Paste(_ context.Context, surface, text string, submit bool) (cmux.PasteResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pastes = append(f.pastes, surface+"|"+text)
	return cmux.PasteResult{Submitted: submit && !f.unsubmit}, nil
}

func (f *fakeCmux) SendText(_ context.Context, surface, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = append(f.keys, surface+"|"+key)
	return nil
}

func (f *fakeCmux) Focus(_ context.Context, _, _, surface string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.focused = append(f.focused, surface)
	return nil
}

const promptScreen = "⏺ Which branch should I merge?\n\n────────────\n❯ \n────────────\n  vybava │ 4% │ acct\n"

// actionFixture is one waiting Claude session (pid 10) hosted in surface S10.
func actionFixture(t *testing.T, status, screen string) (Env, *fakeCmux) {
	t.Helper()
	f := newFixture(t)
	f.register(t, 10, "s-wait", status, f.vybava, 0, nil)
	cx := newFakeCmux()
	cx.host(10, "S10", screen)
	env := f.env(table(claude(10), plugingc.Process{PID: 20, Started: started(20), Command: "/opt/homebrew/bin/codex"}))
	env.Cmux = cx
	return env, cx
}

func diagCode(err error) string {
	var derr runx.DiagError
	if errors.As(err, &derr) {
		return derr.Diag.Code
	}
	return ""
}

func TestReplyRefusesTextWhileADialogIsOpen(t *testing.T) {
	env, cx := actionFixture(t, "waiting", screenFixture(t, "permission-bash.txt"))
	_, err := SendReply(context.Background(), env, Target{SessionID: "s-wait"}, Reply{Text: "yes please"})
	if diagCode(err) != DiagDialogOpen || len(cx.pastes) != 0 || len(cx.keys) != 0 {
		t.Fatalf("err %v, pastes %v, keys %v: typing must never reach an open dialog", err, cx.pastes, cx.keys)
	}
}

func TestReplyPastesTextOnceAndNeverRetries(t *testing.T) {
	env, cx := actionFixture(t, "waiting", promptScreen)
	out, err := SendReply(context.Background(), env, Target{SessionID: "s-wait"}, Reply{Text: "merge main\n"})
	if err != nil || !out.Submitted || out.Mode != "text" {
		t.Fatalf("reply = %+v, %v", out, err)
	}
	if len(cx.pastes) != 1 || cx.pastes[0] != "S10|merge main" {
		t.Fatalf("pastes = %q, want the text once without its trailing newline", cx.pastes)
	}

	cx.unsubmit = true
	_, err = SendReply(context.Background(), env, Target{SessionID: "s-wait"}, Reply{Text: "again"})
	if diagCode(err) != DiagNotSubmitted || len(cx.pastes) != 2 {
		t.Fatalf("err %v after %d pastes: an accepted paste is never re-sent", err, len(cx.pastes))
	}
}

func TestReplyPressesAnOptionOnlyForTheDialogItWasChosenFrom(t *testing.T) {
	env, cx := actionFixture(t, "waiting", screenFixture(t, "permission-bash.txt"))
	dialog, err := OpenDialog(context.Background(), env, "s-wait")
	if err != nil || dialog == nil {
		t.Fatalf("dialog = %+v, %v", dialog, err)
	}
	_, err = SendReply(context.Background(), env, Target{SessionID: "s-wait"}, Reply{Option: "1", Expect: "0000000000000000"})
	if diagCode(err) != DiagDialogChanged || len(cx.keys) != 0 {
		t.Fatalf("stale fingerprint: err %v, keys %v", err, cx.keys)
	}
	out, err := SendReply(context.Background(), env, Target{SessionID: "s-wait"}, Reply{Option: "4", Expect: dialog.Fingerprint})
	if err != nil || out.Key != "4" || len(cx.keys) != 1 || cx.keys[0] != "S10|4" || len(cx.pastes) != 0 {
		t.Fatalf("reply %+v, %v, keys %v, pastes %v", out, err, cx.keys, cx.pastes)
	}
}

func TestFocusResolvesTheSurfaceByLivePidEachTime(t *testing.T) {
	env, cx := actionFixture(t, "waiting", promptScreen)
	if _, err := Focus(context.Background(), env, Target{SessionID: "s-wait"}); err != nil {
		t.Fatal(err)
	}
	// The session moved to another surface since: focus must follow it.
	cx.host(10, "S11", promptScreen)
	out, err := Focus(context.Background(), env, Target{SessionID: "s-wait"})
	if err != nil || out.SurfaceID != "S11" || cx.resolves != 2 {
		t.Fatalf("focus %+v, %v after %d resolves", out, err, cx.resolves)
	}
	if strings.Join(cx.focused, ",") != "S10,S11" {
		t.Fatalf("focused %v", cx.focused)
	}
}

func TestScreenRedactsSecretsBeforeTheyLeaveGo(t *testing.T) {
	token := "ghp_" + strings.Repeat("A1b2C3d4E5", 4)[:36]
	env, _ := actionFixture(t, "waiting", "export GITHUB_TOKEN="+token+"\n❯ \n")
	out, err := Screen(context.Background(), env, "s-wait", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.Text, token) || !strings.Contains(out.Text, "[REDACTED:") || out.Redactions == 0 {
		t.Fatalf("screen text %q (%d redactions) leaks the token", out.Text, out.Redactions)
	}
}

func TestCodexTargetsTakeFocusButNoReply(t *testing.T) {
	env, cx := actionFixture(t, "waiting", promptScreen)
	cx.host(20, "S20", promptScreen)
	_, err := SendReply(context.Background(), env, Target{PID: 20}, Reply{Text: "hi"})
	if diagCode(err) != DiagReplyRefused || len(cx.pastes) != 0 {
		t.Fatalf("codex reply: err %v, pastes %v", err, cx.pastes)
	}
	if out, err := Focus(context.Background(), env, Target{PID: 20}); err != nil || out.SurfaceID != "S20" {
		t.Fatalf("codex focus = %+v, %v", out, err)
	}
}
