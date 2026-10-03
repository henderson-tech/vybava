package fleet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/cmux"
)

func readPublished(t *testing.T, path string) Published {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out Published
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPublishedListsOnlyCmuxHostedSessionsWithTheirActions(t *testing.T) {
	f := newFixture(t)
	f.register(t, 10, "in-cmux", "waiting", f.vybava, time.Minute, nil)
	f.register(t, 11, "in-warp", "waiting", f.vybava, time.Minute, nil)
	f.register(t, 12, "busy", "busy", f.fixit, time.Minute, nil)
	cx := newFakeCmux()
	cx.host(10, "S10", promptScreen)
	cx.host(12, "S12", promptScreen)
	cx.host(20, "S20", promptScreen)
	env := f.env(table(claude(10), claude(11), claude(12)))
	env.Cmux = cx
	p := &Publisher{Env: env, Clock: func() time.Time { return now }, ReadCodex: func(context.Context, time.Time) ([]CodexRow, []string, error) {
		return []CodexRow{{PID: 20, ThreadID: "t1", CWD: f.fixit}, {PID: 21, ThreadID: "t2", CWD: f.fixit}}, nil, nil
	}}
	p.RefreshCodex(context.Background())
	view, err := p.Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, s := range view.Sessions {
		ids = append(ids, s.SessionID)
	}
	if strings.Join(ids, ",") != "in-cmux,busy" || view.Hidden != 2 || view.Counts.Waiting != 1 {
		t.Fatalf("sessions %v, hidden %d, waiting %d", ids, view.Hidden, view.Counts.Waiting)
	}
	if !slices.Equal(view.Sessions[0].Actions, []Action{ActionFocus, ActionScreen, ActionReply}) || slices.Contains(view.Sessions[1].Actions, ActionReply) {
		t.Fatalf("actions %v / %v: reply only at a prompt", view.Sessions[0].Actions, view.Sessions[1].Actions)
	}
	if len(view.Codex) != 1 || !slices.Equal(view.Codex[0].Actions, []Action{ActionFocus}) || view.Codex[0].Surface.SurfaceID != "S20" {
		t.Fatalf("codex rows %+v: focus only", view.Codex)
	}
}

func TestPublishedShowsWhyNothingIsListedWhenCmuxRefuses(t *testing.T) {
	f := newFixture(t)
	f.register(t, 10, "s", "waiting", f.vybava, time.Minute, nil)
	cx := newFakeCmux()
	cx.status = cmux.Status{State: cmux.StateDenied, Detail: "cmux closed the socket", Fix: "Automation"}
	env := f.env(table(claude(10)))
	env.Cmux = cx
	view, err := (&Publisher{Env: env, Clock: func() time.Time { return now }}).Build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Cmux.State != cmux.StateDenied || view.Hidden != 1 || len(view.Sessions) != 0 || len(view.Diagnostics) == 0 || view.Diagnostics[len(view.Diagnostics)-1].Code != DiagCmuxUnavailable {
		t.Fatalf("view = %+v", view)
	}
}

// The summary rides the snapshot's read; it lists every waiting session,
// hosted by cmux or not.
func TestPublishWritesTheSummaryFromTheSameRead(t *testing.T) {
	f := newFixture(t)
	f.register(t, 10, "s", "waiting", f.vybava, time.Minute, nil)
	cx := newFakeCmux()
	cx.status = cmux.Status{State: cmux.StateDenied, Detail: "cmux closed the socket"}
	env := f.env(table(claude(10)))
	env.Cmux = cx
	dir := t.TempDir()
	p := &Publisher{Env: env, Path: filepath.Join(dir, "snapshot.json"), SummaryPath: filepath.Join(dir, "summary.json"), Clock: func() time.Time { return now }}
	if err := p.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p.SummaryPath)
	if err != nil {
		t.Fatal(err)
	}
	var summary Summary
	if err := json.Unmarshal(raw, &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Waiting) != 1 || summary.Waiting[0].SessionID != "s" || readPublished(t, p.Path).Hidden != 1 {
		t.Fatalf("summary %+v", summary)
	}
}

func TestAWaitingSessionIsPublishedOnTheCmuxEvent(t *testing.T) {
	f := newFixture(t)
	f.register(t, 10, "s", "busy", f.vybava, time.Minute, nil)
	cx := newFakeCmux()
	cx.host(10, "S10", promptScreen)
	env := f.env(table(claude(10)))
	env.Cmux = cx
	path := filepath.Join(f.home, "snapshot.json")
	p := &Publisher{Env: env, Path: path, Clock: func() time.Time { return now }}
	if err := p.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := readPublished(t, path).Sessions[0].State; got != StateBusy {
		t.Fatalf("first publish state %s", got)
	}

	// The session starts waiting; cmux reports its Notification hook — the
	// 15 s timer never fires in this test.
	f.register(t, 10, "s", "waiting", f.vybava, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := func(ctx context.Context, _ []string, handle func(cmux.Event), _ func(string)) error {
		handle(cmux.Event{Name: "agent.hook.PreToolUse"})
		handle(cmux.Event{Name: "agent.hook.Notification", SurfaceID: "S10"})
		<-ctx.Done()
		return ctx.Err()
	}
	go p.Follow(ctx, events, 5*time.Millisecond, func(err error) { t.Error(err) })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if view := readPublished(t, path); len(view.Sessions) == 1 && view.Sessions[0].State == StateWaiting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the waiting session never reached snapshot.json after its cmux event")
}

func TestSwiftDeclaresEveryContractType(t *testing.T) {
	swift := Swift()
	for _, named := range swiftNamed {
		if !strings.Contains(swift, "struct "+named.name+": Codable, Sendable, Equatable {") {
			t.Fatalf("missing struct %s", named.name)
		}
	}
	for _, enum := range swiftEnums {
		if !strings.Contains(swift, "enum "+enum.name+": String, Codable, Sendable, CaseIterable {") {
			t.Fatalf("missing enum %s", enum.name)
		}
	}
	// Hosted flattens its embedded Session the way encoding/json does.
	session := swift[strings.Index(swift, "struct FleetSession:"):]
	session = session[:strings.Index(session, "}")]
	for _, field := range []string{"let sessionId: String", "let state: FleetState", "let surface: FleetSurface", "let actions: [FleetAction]"} {
		if !strings.Contains(session, field) {
			t.Fatalf("FleetSession lacks %q:\n%s", field, session)
		}
	}
}
