package watch

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProbe answers from a script of readings per ref and counts calls.
type fakeProbe struct {
	kind     string
	interval time.Duration
	cost     int

	mu    sync.Mutex
	calls map[string]int
	read  func(ref string, call int) (Observation, error)
}

func newFake(read func(ref string, call int) (Observation, error)) *fakeProbe {
	return &fakeProbe{kind: "fake", interval: time.Minute, calls: map[string]int{}, read: read}
}

func (f *fakeProbe) Kind() string            { return f.kind }
func (f *fakeProbe) Interval() time.Duration { return f.interval }
func (f *fakeProbe) Cost() int               { return f.cost }
func (f *fakeProbe) Conditions() map[string]Condition {
	return map[string]Condition{"done": {Holds: func(o Observation) bool { return o.Fields["status"] == "done" }}}
}
func (f *fakeProbe) Canonical(_ context.Context, ref, _ string) (string, error) {
	return strings.TrimPrefix(ref, "#"), nil
}
func (f *fakeProbe) Observe(_ context.Context, ref, _ string) (Observation, error) {
	f.mu.Lock()
	f.calls[ref]++
	n := f.calls[ref]
	f.mu.Unlock()
	return f.read(ref, n)
}
func (f *fakeProbe) count(ref string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[ref]
}

func status(s string) Observation {
	return Observation{Fields: map[string]string{"status": s}, Summary: "status " + s}
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)} }

func tick(e *Engine) {
	e.Tick(context.Background())
	e.Wait()
}

func drain(t *testing.T, e *Engine, session string) []Event {
	t.Helper()
	evs, err := e.Events(context.Background(), session, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return evs
}

func kinds(evs []Event) string {
	var k []string
	for _, e := range evs {
		k = append(k, e.Kind)
	}
	return strings.Join(k, ",")
}

func TestTwoSubscribersOnOneTargetCostOneProbePerInterval(t *testing.T) {
	c := newClock()
	f := newFake(func(_ string, n int) (Observation, error) {
		switch {
		case n < 3:
			return status("open"), nil
		case n == 3:
			return status("review"), nil
		}
		return status("done"), nil
	})
	e, err := NewEngine([]Probe{f}, Options{Now: c.now})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, s := range []string{"s1", "s2"} {
		if _, err := e.Add(ctx, AddRequest{Session: s, Target: "fake:#7", Until: "done"}); err != nil {
			t.Fatal(err)
		}
	}
	tick(e)
	tick(e) // same instant: nothing is due again
	if got := f.count("7"); got != 1 {
		t.Fatalf("two subscribers, one interval: %d probes, want 1", got)
	}
	c.advance(30 * time.Second)
	tick(e)
	if got := f.count("7"); got != 1 {
		t.Fatalf("half an interval later: %d probes, want 1", got)
	}
	c.advance(30 * time.Second)
	tick(e) // reading 2: unchanged
	if evs := drain(t, e, "s1"); len(evs) != 0 {
		t.Fatalf("an unchanged reading queued %v", evs)
	}
	c.advance(time.Minute)
	tick(e) // reading 3: review
	c.advance(time.Minute)
	tick(e) // reading 4: done
	for _, s := range []string{"s1", "s2"} {
		evs := drain(t, e, s)
		if kinds(evs) != "change,change,met" {
			t.Fatalf("%s got %s, want change,change,met", s, kinds(evs))
		}
		if evs[0].Changed[0] != "status" || evs[2].Summary != "status done" {
			t.Fatalf("%s events %+v", s, evs)
		}
	}
	if l := e.List(""); len(l.Subscriptions) != 0 || len(l.Targets) != 0 {
		t.Fatalf("met subscriptions linger: %+v", l)
	}
	c.advance(time.Hour)
	tick(e)
	if got := f.count("7"); got != 4 {
		t.Fatalf("a target nobody waits on was probed again: %d", got)
	}
}

func TestProbeErrorsBackOffAndTellSubscribersOnce(t *testing.T) {
	c := newClock()
	fail := true
	f := newFake(func(string, int) (Observation, error) {
		if fail {
			return Observation{}, errors.New("devbox status: connection refused")
		}
		return status("open"), nil
	})
	var log bytes.Buffer
	e, _ := NewEngine([]Probe{f}, Options{Now: c.now, Log: &log})
	if _, err := e.Add(context.Background(), AddRequest{Session: "s", Target: "fake:x", Until: "done"}); err != nil {
		t.Fatal(err)
	}
	wantGaps := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 8 * time.Minute}
	for i, gap := range wantGaps {
		tick(e)
		if got := f.count("x"); got != i+1 {
			t.Fatalf("failure %d: %d probes", i+1, got)
		}
		c.advance(gap - time.Second)
		tick(e)
		if got := f.count("x"); got != i+1 {
			t.Fatalf("probed before the %s backoff ran out", gap)
		}
		c.advance(time.Second)
	}
	evs := drain(t, e, "s")
	if kinds(evs) != "error" || !strings.Contains(evs[0].Error, "connection refused") {
		t.Fatalf("four failures queued %v, want one error event", evs)
	}
	if !strings.Contains(log.String(), "probe failed") {
		t.Fatalf("failures not logged: %q", log.String())
	}
	fail = false
	tick(e)
	if l := e.List("s"); l.Targets[0].Failures != 0 || l.Targets[0].NextAt != c.now().Add(time.Minute) {
		t.Fatalf("a good reading did not reset the backoff: %+v", l.Targets[0])
	}
}

func TestAPanickingProbeIsAFailedReadingNotADeadDaemon(t *testing.T) {
	c := newClock()
	f := newFake(func(string, int) (Observation, error) { panic("gitkit: index out of range") })
	var log bytes.Buffer
	e, _ := NewEngine([]Probe{f}, Options{Now: c.now, Log: &log})
	if _, err := e.Add(context.Background(), AddRequest{Session: "s", Target: "fake:x", Until: "done"}); err != nil {
		t.Fatal(err)
	}
	tick(e)
	l := e.List("s")
	if l.Targets[0].Failures != 1 || !strings.Contains(l.Targets[0].LastError, "panicked") {
		t.Fatalf("a panic was not recorded as a failed reading: %+v", l.Targets[0])
	}
	if l.Targets[0].NextAt != c.now().Add(2*time.Minute) {
		t.Fatalf("a panic skipped the backoff: next at %s", l.Targets[0].NextAt)
	}
}

func TestRateLimitBacksOffToTheMaximumAtOnce(t *testing.T) {
	c := newClock()
	f := newFake(func(string, int) (Observation, error) {
		return Observation{}, errors.New("gh: API rate limit exceeded (HTTP 403)")
	})
	e, _ := NewEngine([]Probe{f}, Options{Now: c.now})
	_, _ = e.Add(context.Background(), AddRequest{Session: "s", Target: "fake:x", Until: "done"})
	tick(e)
	if next := e.List("").Targets[0].NextAt; next != c.now().Add(8*time.Minute) {
		t.Fatalf("rate limit backoff to %s, want 8 intervals", next.Sub(c.now()))
	}
}

func TestGitHubBudgetDefersProbesItCannotPay(t *testing.T) {
	c := newClock()
	f := newFake(func(string, int) (Observation, error) { return status("open"), nil })
	f.cost = 5
	budget := NewBudget(5, 3600) // one probe's worth, refilled at 1 unit/s
	e, _ := NewEngine([]Probe{f}, Options{Now: c.now, Budget: budget})
	ctx := context.Background()
	_, _ = e.Add(ctx, AddRequest{Session: "s", Target: "fake:a", Until: "done"})
	_, _ = e.Add(ctx, AddRequest{Session: "s", Target: "fake:b", Until: "done"})
	tick(e)
	if f.count("a") != 1 || f.count("b") != 0 {
		t.Fatalf("budget for one probe ran a=%d b=%d", f.count("a"), f.count("b"))
	}
	c.advance(4 * time.Second)
	tick(e)
	if f.count("b") != 0 {
		t.Fatal("b ran before the budget refilled")
	}
	c.advance(time.Second)
	tick(e)
	if f.count("b") != 1 {
		t.Fatal("b did not run once the budget refilled")
	}
}

func TestSubscriptionsAndEventsSurviveARestart(t *testing.T) {
	c := newClock()
	reading := "open"
	f := newFake(func(string, int) (Observation, error) { return status(reading), nil })
	store := Store{Path: filepath.Join(t.TempDir(), "state.json")}
	e, _ := NewEngine([]Probe{f}, Options{Now: c.now, Store: store})
	if _, err := e.Add(context.Background(), AddRequest{Session: "s", Target: "fake:x", Until: "done"}); err != nil {
		t.Fatal(err)
	}
	tick(e)
	reading = "review"
	c.advance(time.Minute)
	tick(e)

	restarted, err := NewEngine([]Probe{f}, Options{Now: c.now, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, restarted, "s")
	if kinds(evs) != "change" {
		t.Fatalf("after restart the queue holds %s, want the undelivered change", kinds(evs))
	}
	tick(restarted) // same reading as before the restart: no replayed change
	if evs := drain(t, restarted, "s"); len(evs) != 1 {
		t.Fatalf("restart replayed a change: %v", kinds(evs))
	}
	if _, err := restarted.Events(context.Background(), "s", evs[0].Seq, 0); err != nil {
		t.Fatal(err)
	}
	again, _ := NewEngine([]Probe{f}, Options{Now: c.now, Store: store})
	if evs := drain(t, again, "s"); len(evs) != 0 {
		t.Fatalf("an acknowledged event came back: %v", evs)
	}
	if len(again.List("s").Subscriptions) != 1 {
		t.Fatal("the subscription did not survive")
	}
}

func TestEventsLongPollTimesOutAndWakesOnAnEvent(t *testing.T) {
	f := newFake(func(string, int) (Observation, error) { return status("done"), nil })
	e, _ := NewEngine([]Probe{f}, Options{})
	start := time.Now()
	evs, err := e.Events(context.Background(), "s", 0, 50*time.Millisecond)
	if err != nil || len(evs) != 0 || time.Since(start) < 50*time.Millisecond {
		t.Fatalf("empty long-poll: %v %v after %s", evs, err, time.Since(start))
	}
	_, _ = e.Add(context.Background(), AddRequest{Session: "s", Target: "fake:x", Until: "done"})
	got := make(chan []Event, 1)
	go func() {
		evs, _ := e.Events(context.Background(), "s", 0, 10*time.Second)
		got <- evs
	}()
	time.Sleep(20 * time.Millisecond)
	tick(e)
	select {
	case evs := <-got:
		if kinds(evs) != "met" {
			t.Fatalf("woke with %s", kinds(evs))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the long-poll did not wake on the event")
	}
}

func TestAddMeetsAtOnceFromAFreshReading(t *testing.T) {
	c := newClock()
	f := newFake(func(string, int) (Observation, error) { return status("done"), nil })
	e, _ := NewEngine([]Probe{f}, Options{Now: c.now})
	ctx := context.Background()
	_, _ = e.Add(ctx, AddRequest{Session: "a", Target: "fake:x", Until: "changed"})
	tick(e)
	res, err := e.Add(ctx, AddRequest{Session: "b", Target: "fake:x", Until: "done"})
	if err != nil {
		t.Fatal(err)
	}
	if kinds(res.Events) != "met" || f.count("x") != 1 {
		t.Fatalf("a held condition on a fresh reading answered %v after %d probes", res.Events, f.count("x"))
	}
	if subs := e.List("b").Subscriptions; len(subs) != 0 {
		t.Fatalf("met subscription kept: %+v", subs)
	}
}

func TestRemoveDropsTheQueueOfAnEndedSubscription(t *testing.T) {
	c := newClock()
	f := newFake(func(string, int) (Observation, error) { return status("done"), nil })
	e, _ := NewEngine([]Probe{f}, Options{Now: c.now})
	res, err := e.Add(context.Background(), AddRequest{Session: "until-1", Target: "fake:x", Until: "done"})
	if err != nil {
		t.Fatal(err)
	}
	tick(e)
	if _, err := e.Remove(res.Subscription.ID); err != nil {
		t.Fatal(err)
	}
	if events := drain(t, e, "until-1"); len(events) != 0 {
		t.Fatalf("a removed subscription's events stayed queued: %v", kinds(events))
	}
}

func TestUnacknowledgedEventsAgeOut(t *testing.T) {
	c := newClock()
	f := newFake(func(string, int) (Observation, error) { return status("done"), nil })
	e, _ := NewEngine([]Probe{f}, Options{Now: c.now})
	_, _ = e.Add(context.Background(), AddRequest{Session: "gone", Target: "fake:x", Until: "done"})
	tick(e)
	c.advance(eventTTL + time.Minute)
	tick(e)
	if events := drain(t, e, "gone"); len(events) != 0 {
		t.Fatalf("a queue nobody acknowledged for %s survived: %v", eventTTL, kinds(events))
	}
}

func TestAddRefusesUnknownKindsAndConditions(t *testing.T) {
	e, _ := NewEngine([]Probe{newFake(nil)}, Options{})
	ctx := context.Background()
	for _, req := range []AddRequest{
		{Session: "s", Target: "nope:1", Until: "done"},
		{Session: "s", Target: "fake:1", Until: "merged"},
		{Session: "s", Target: "fake", Until: "done"},
		{Target: "fake:1", Until: "done"},
	} {
		if _, err := e.Add(ctx, req); err == nil {
			t.Errorf("%+v was accepted", req)
		}
	}
	if _, err := e.Add(ctx, AddRequest{Session: "s", Target: "fake:1", Until: "status=review"}); err != nil {
		t.Fatalf("field=value refused: %v", err)
	}
}

func TestSubscriptionsExpire(t *testing.T) {
	c := newClock()
	f := newFake(func(string, int) (Observation, error) { return status("open"), nil })
	e, _ := NewEngine([]Probe{f}, Options{Now: c.now})
	_, _ = e.Add(context.Background(), AddRequest{Session: "s", Target: "fake:x", Until: "done", TTL: time.Hour})
	tick(e)
	c.advance(time.Hour + time.Second)
	tick(e)
	if kinds(drain(t, e, "s")) != "expired" || len(e.List("").Subscriptions) != 0 {
		t.Fatal("an expired subscription was not dropped with an expired event")
	}
}

func TestPeriodicTasksRunOnTheirIntervalAndFailuresAreLogged(t *testing.T) {
	c := newClock()
	var log bytes.Buffer
	e, _ := NewEngine(nil, Options{Now: c.now, Log: &log})
	runs := 0
	e.Every("fleet-summary", 5*time.Second, func(context.Context) error {
		runs++
		return errors.New("disk full")
	})
	tick(e)
	tick(e)
	c.advance(5 * time.Second)
	tick(e)
	if runs != 2 || !strings.Contains(log.String(), "task fleet-summary failed: disk full") {
		t.Fatalf("runs=%d log=%q", runs, log.String())
	}
	if h := e.Health(1); len(h.Tasks) != 1 || h.Tasks[0] != "fleet-summary" {
		t.Fatalf("health tasks %v", h.Tasks)
	}
}
