package cmux

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCall is one request the fake socket received.
type fakeCall struct {
	Method string
	Params map[string]any
}

// fake is a cmux socket: serve answers each request with the lines it
// returns (none = close unanswered); a stream keeps writing after the ack.
type fake struct {
	t      *testing.T
	socket string
	mu     sync.Mutex
	calls  []fakeCall
	serve  func(call fakeCall, n int) []string
}

func newFake(t *testing.T, serve func(call fakeCall, n int) []string) *fake {
	t.Helper()
	// macOS caps unix socket paths at 104 bytes; t.TempDir() is too deep.
	dir, err := os.MkdirTemp("", "cmux")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	f := &fake{t: t, socket: filepath.Join(dir, "s.sock"), serve: serve}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.handle(conn)
		}
	}()
	return f
}

func (f *fake) handle(conn net.Conn) {
	defer conn.Close()
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		return
	}
	var req struct {
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		return
	}
	call := fakeCall{Method: req.Method, Params: req.Params}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	n := 0
	for _, c := range f.calls {
		if c.Method == call.Method {
			n++
		}
	}
	f.mu.Unlock()
	for _, out := range f.serve(call, n) {
		if _, err := conn.Write([]byte(out + "\n")); err != nil {
			return
		}
	}
}

func (f *fake) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

func (f *fake) client() Client {
	return Client{Socket: f.socket, Timeout: 2 * time.Second, Sleep: func(time.Duration) {}}
}

func ok(result string) []string { return []string{`{"id":"x","ok":true,"result":` + result + `}`} }

func fail(code, message, data string) []string {
	return []string{`{"id":"x","ok":false,"error":{"code":"` + code + `","message":"` + message + `","data":` + data + `}}`}
}

func TestResolveFindsTheSurfaceAndReportsAnUnhostedPid(t *testing.T) {
	f := newFake(t, func(call fakeCall, _ int) []string {
		if call.Params["pid"] == float64(42) {
			return ok(`{"surface_id":"S1","workspace_id":"W1","pid_resolution":"corroborated","source":"pid"}`)
		}
		return fail("not_found", "No live delivery target", `{}`)
	})
	target, err := f.client().Resolve(context.Background(), 42)
	if err != nil || target != (Target{SurfaceID: "S1", WorkspaceID: "W1", Resolution: "corroborated"}) {
		t.Fatalf("Resolve(42) = %+v, %v", target, err)
	}
	if _, err := f.client().Resolve(context.Background(), 7); !ErrNotFound(err) {
		t.Fatalf("Resolve(7) err = %v, want not_found", err)
	}
}

func TestPasteIsSentOnceOnceCmuxTookIt(t *testing.T) {
	f := newFake(t, func(call fakeCall, n int) []string {
		if n == 1 {
			// The pool was saturated: the command never ran, so a retry is safe.
			return fail("overloaded", "busy", `{"retryable":true,"retry_after_ms":5}`)
		}
		return ok(`{"submitted":false,"submit_error":"no_agent"}`)
	})
	result, err := f.client().Paste(context.Background(), "S1", "hello", true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Submitted || result.SubmitError != "no_agent" || f.count("terminal.paste") != 2 {
		t.Fatalf("result %+v after %d pastes, want the unsubmitted warning after exactly 2", result, f.count("terminal.paste"))
	}
	last := f.calls[len(f.calls)-1].Params
	if last["submit_key"] != "return" || last["text"] != "hello" || last["surface_id"] != "S1" {
		t.Fatalf("params = %v", last)
	}

	unknown := newFake(t, func(fakeCall, int) []string {
		// Started on the main thread, result unknown: never re-sent.
		return fail("timeout", "main actor", `{"retryable":false}`)
	})
	if _, err := unknown.client().Paste(context.Background(), "S1", "hello", true); err == nil || unknown.count("terminal.paste") != 1 {
		t.Fatalf("err %v after %d pastes, want one failed paste", err, unknown.count("terminal.paste"))
	}
}

func TestReadTextAsksForScrollbackOnlyWithLines(t *testing.T) {
	f := newFake(t, func(call fakeCall, _ int) []string {
		return ok(`{"text":"❯ 1. Yes","surface_id":"S1","workspace_id":"W1","window_id":"N1"}`)
	})
	screen, err := f.client().ReadText(context.Background(), "S1", 0)
	if err != nil || screen.Text != "❯ 1. Yes" || screen.WindowID != "N1" {
		t.Fatalf("ReadText = %+v, %v", screen, err)
	}
	if _, has := f.calls[0].Params["scrollback"]; has {
		t.Fatalf("visible read sent %v", f.calls[0].Params)
	}
	if _, err := f.client().ReadText(context.Background(), "S1", 40); err != nil {
		t.Fatal(err)
	}
	if p := f.calls[1].Params; p["lines"] != float64(40) || p["scrollback"] != true {
		t.Fatalf("lines read sent %v", p)
	}
}

func TestCheckNamesWhatStopsActions(t *testing.T) {
	bundle := t.TempDir()
	if err := os.MkdirAll(filepath.Join(bundle, "Contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeVersion := func(v string) {
		plist := `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleShortVersionString</key><string>` + v + `</string></dict></plist>`
		if err := os.WriteFile(filepath.Join(bundle, "Contents", "Info.plist"), []byte(plist), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	all, _ := json.Marshal(RequiredMethods)
	capsWith := func(mode string, methods []byte) func(fakeCall, int) []string {
		return func(call fakeCall, _ int) []string {
			if call.Method == "system.identify" {
				id, _ := json.Marshal(map[string]string{"app_bundle_path": bundle})
				return ok(string(id))
			}
			return ok(`{"access_mode":"` + mode + `","protocol":"cmux-socket","version":2,"methods":` + string(methods) + `}`)
		}
	}
	ctx := context.Background()

	writeVersion("0.64.25")
	if s := newFake(t, capsWith("automation", all)).client().Check(ctx); s.State != StateOK || s.Version != "0.64.25" {
		t.Fatalf("healthy cmux = %+v", s)
	}
	if s := newFake(t, capsWith("cmuxOnly", all)).client().Check(ctx); s.State != StateRestricted || s.Fix == "" {
		t.Fatalf("cmuxOnly = %+v, want restricted with a fix", s)
	}
	fewer, _ := json.Marshal(RequiredMethods[1:])
	if s := newFake(t, capsWith("automation", fewer)).client().Check(ctx); s.State != StateOutdated || len(s.Missing) != 1 {
		t.Fatalf("missing method = %+v", s)
	}
	writeVersion("0.64.9")
	if s := newFake(t, capsWith("automation", all)).client().Check(ctx); s.State != StateOutdated {
		t.Fatalf("0.64.9 = %+v, want outdated", s)
	}
	if s := newFake(t, func(fakeCall, int) []string { return nil }).client().Check(ctx); s.State != StateDenied {
		t.Fatalf("closed unanswered = %+v, want denied", s)
	}
	if s := (Client{Socket: filepath.Join(t.TempDir(), "none.sock")}).Check(ctx); s.State != StateUnreachable {
		t.Fatalf("no socket = %+v, want unreachable", s)
	}
}

func TestFollowResumesAndReportsLostEvents(t *testing.T) {
	event := func(seq int) string {
		return `{"type":"event","seq":` + itoa(seq) + `,"name":"agent.hook.Notification","category":"agent","surface_id":"S1","boot_id":"A"}`
	}
	var mu sync.Mutex
	var afters []any
	f := newFake(t, func(call fakeCall, n int) []string {
		mu.Lock()
		afters = append(afters, call.Params["after_seq"])
		mu.Unlock()
		switch n {
		case 1:
			return []string{`{"type":"ack","boot_id":"A","resume":{"gap":false}}`, `{"type":"heartbeat"}`, event(5), event(6)}
		case 2:
			return []string{`{"type":"ack","boot_id":"A","resume":{"gap":true}}`, event(9)}
		default:
			return []string{`{"type":"ack","boot_id":"B","resume":{"gap":false}}`}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var seqs []int64
	var gaps []string
	client := f.client()
	done := make(chan error, 1)
	go func() {
		done <- client.Follow(ctx, []string{"agent"}, func(e Event) { seqs = append(seqs, e.Seq) }, func(reason string) {
			gaps = append(gaps, reason)
			if len(gaps) == 2 {
				cancel()
			}
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Follow did not stop")
	}
	if len(seqs) != 3 || seqs[0] != 5 || seqs[2] != 9 {
		t.Fatalf("delivered %v, want 5 6 9", seqs)
	}
	if len(gaps) != 2 || !strings.Contains(gaps[0], "after 6") || gaps[1] != "cmux restarted" {
		t.Fatalf("gaps %q", gaps)
	}
	mu.Lock()
	defer mu.Unlock()
	if afters[0] != nil || afters[1] != float64(6) || afters[2] != float64(9) {
		t.Fatalf("after_seq per connect = %v, want live edge, 6, 9", afters)
	}
}

func TestSocketPathPrefersTheEnvThenWhatCmuxLastWrote(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	if got := SocketPath(getenv, home); got != filepath.Join(home, ".local/state/cmux/cmux.sock") {
		t.Fatalf("default = %s", got)
	}
	dir := filepath.Join(home, ".local", "state", "cmux")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "last-socket-path"), []byte("/x/last.sock\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := SocketPath(getenv, home); got != "/x/last.sock" {
		t.Fatalf("last-socket-path = %s", got)
	}
	env["CMUX_SOCKET_PATH"] = "/x/env.sock"
	if got := SocketPath(getenv, home); got != "/x/env.sock" {
		t.Fatalf("env = %s", got)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestCheckTellsABusyCmuxFromARefusingOne(t *testing.T) {
	f := newFake(t, func(fakeCall, int) []string {
		time.Sleep(300 * time.Millisecond)
		return ok(`{}`)
	})
	client := f.client()
	client.Timeout = 50 * time.Millisecond
	if s := client.Check(context.Background()); s.State != StateUnreachable || !strings.Contains(s.Detail, "did not answer in time") {
		t.Fatalf("a slow reply = %+v, want unreachable (busy), never denied", s)
	}
}

func TestFollowBacksOffAStreamThatEndsUnheard(t *testing.T) {
	// Acks, then closes at once — forever.
	f := newFake(t, func(fakeCall, int) []string { return []string{`{"type":"ack","boot_id":"A","resume":{"gap":false}}`} })
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	_ = f.client().Follow(ctx, nil, func(Event) {}, func(string) {})
	if n := f.count("events.stream"); n > 2 {
		t.Fatalf("%d subscriptions in 1.5 s: a stream that delivers nothing must back off", n)
	}
}
