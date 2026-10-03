package watch

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortSocket keeps the path under macOS's 104-byte unix socket limit, which
// t.TempDir's /var/folders paths can exceed.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "watch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "d", "watchd.sock")
}

func serve(t *testing.T, e *Engine, socket string) {
	t.Helper()
	ln, err := Listen(socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, e, ln, 10*time.Millisecond, io.Discard) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
}

func TestSocketAPIRoundTrip(t *testing.T) {
	socket := shortSocket(t)
	f := newFake(func(_ string, n int) (Observation, error) {
		if n < 2 {
			return status("open"), nil
		}
		return status("done"), nil
	})
	f.interval = 20 * time.Millisecond
	e, _ := NewEngine([]Probe{f}, Options{})
	serve(t, e, socket)

	info, err := os.Stat(socket)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %v (%v)", info.Mode().Perm(), err)
	}
	if dir, _ := os.Stat(filepath.Dir(socket)); dir.Mode().Perm() != 0o700 {
		t.Fatalf("state dir mode %v", dir.Mode().Perm())
	}

	c := NewClient(socket)
	ctx := context.Background()
	if h, err := c.Health(ctx); err != nil || !h.OK {
		t.Fatalf("health %+v %v", h, err)
	}
	res, err := c.Add(ctx, AddRequest{Session: "s", Target: "fake:#9", Until: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Subscription.Target != "fake:9" {
		t.Fatalf("canonical target %q", res.Subscription.Target)
	}
	l, err := c.List(ctx, "s")
	if err != nil || len(l.Subscriptions) != 1 || len(l.Targets) != 1 {
		t.Fatalf("list %+v %v", l, err)
	}
	var got []Event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(kinds(got), "met") {
		evs, err := c.Events(ctx, "s", 0, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		got = evs
	}
	if kinds(got) != "change,met" {
		t.Fatalf("events %s", kinds(got))
	}
	if evs, _ := c.Events(ctx, "s", got[len(got)-1].Seq, 0); len(evs) != 0 {
		t.Fatalf("acknowledged events came back: %v", evs)
	}

	var apiErr *APIError
	if _, err := c.Add(ctx, AddRequest{Session: "s", Target: "fake:1", Until: "merged"}); !errors.As(err, &apiErr) || apiErr.Status != 422 {
		t.Fatalf("a bad condition answered %v", err)
	}
	res, _ = c.Add(ctx, AddRequest{Session: "s", Target: "fake:2", Until: "status=never"})
	if err := c.Remove(ctx, res.Subscription.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Remove(ctx, res.Subscription.ID); !errors.As(err, &apiErr) || apiErr.Status != 404 {
		t.Fatalf("second remove answered %v", err)
	}
}

func TestListenRefusesALiveDaemonAndReplacesAStaleSocket(t *testing.T) {
	socket := shortSocket(t)
	e, _ := NewEngine(nil, Options{})
	serve(t, e, socket)
	if _, err := Listen(socket); err == nil || !strings.Contains(err.Error(), "already serves") {
		t.Fatalf("a second daemon was allowed: %v", err)
	}

	stale := shortSocket(t)
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close() // the file stays, nobody answers
	ln, err = Listen(stale)
	if err != nil {
		t.Fatalf("stale socket: %v", err)
	}
	ln.Close()
}

func TestClientSaysTheDaemonIsDown(t *testing.T) {
	_, err := NewClient(shortSocket(t)).Health(context.Background())
	if !errors.Is(err, ErrDaemonDown) {
		t.Fatalf("no daemon answered %v", err)
	}
}

func TestUntilThroughTheDaemonExitsZeroWhenMet(t *testing.T) {
	socket := shortSocket(t)
	f := newFake(func(_ string, n int) (Observation, error) {
		if n < 3 {
			return status("open"), nil
		}
		return status("done"), nil
	})
	f.interval = 20 * time.Millisecond
	e, _ := NewEngine([]Probe{f}, Options{})
	serve(t, e, socket)

	var out, errOut bytes.Buffer
	code, err := Until(context.Background(), NewClient(socket), nil, UntilOptions{
		Target: "fake:x", Until: "done", Timeout: 10 * time.Second, Out: &out, Err: &errOut,
	})
	if err != nil || code != ExitMet {
		t.Fatalf("until: %d %v (stderr %q)", code, err, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], " change fake:x status done") || !strings.Contains(lines[1], " met fake:x") {
		t.Fatalf("until printed %q", out.String())
	}
	if l := e.List(""); len(l.Subscriptions) != 0 {
		t.Fatalf("until left its subscription: %+v", l.Subscriptions)
	}
}

func TestUntilProbesDirectlyWithoutADaemon(t *testing.T) {
	f := newFake(func(_ string, n int) (Observation, error) {
		if n < 2 {
			return status("open"), nil
		}
		return status("done"), nil
	})
	f.interval = 10 * time.Millisecond
	var out, errOut bytes.Buffer
	code, err := Until(context.Background(), NewClient(shortSocket(t)), []Probe{f}, UntilOptions{
		Target: "fake:x", Until: "done", Timeout: 5 * time.Second, JSON: true, Out: &out, Err: &errOut,
	})
	if err != nil || code != ExitMet {
		t.Fatalf("direct until: %d %v", code, err)
	}
	if !strings.Contains(errOut.String(), "probing directly") {
		t.Fatalf("the fallback was not announced: %q", errOut.String())
	}
	if !strings.Contains(out.String(), `"kind":"met"`) {
		t.Fatalf("json lines %q", out.String())
	}
}

func TestUntilTimesOutWith124(t *testing.T) {
	f := newFake(func(string, int) (Observation, error) { return status("open"), nil })
	f.interval = 10 * time.Millisecond
	code, err := Until(context.Background(), NewClient(shortSocket(t)), []Probe{f}, UntilOptions{
		Target: "fake:x", Until: "done", Timeout: 80 * time.Millisecond, Out: io.Discard, Err: io.Discard,
	})
	if err != nil || code != ExitTimeout {
		t.Fatalf("timeout answered %d %v", code, err)
	}
}
