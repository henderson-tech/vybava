package watch

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"sync"
	"time"
)

const (
	// errorStreak failed probes in a row tell every subscriber once.
	errorStreak = 3
	// maxBackoff caps the error backoff at this many intervals.
	maxBackoff = 8
	// probeTimeout bounds one Observe.
	probeTimeout = 2 * time.Minute
	// DefaultTTL is how long a subscription waits before it expires.
	DefaultTTL = 24 * time.Hour
	// DefaultMaxEvents is the per-session queue bound; the oldest go first.
	DefaultMaxEvents = 100
	// eventTTL drops an event nobody acknowledged for this long.
	eventTTL = 7 * 24 * time.Hour
	// MaxWait caps one long-poll.
	MaxWait = 60 * time.Second
)

var rateLimitish = regexp.MustCompile(`(?i)rate limit|secondary rate|\b429\b|\b403\b`)

// AddRequest subscribes Session to Until on Target.
type AddRequest struct {
	Session string        `json:"session"`
	Target  string        `json:"target"`
	Until   string        `json:"until"`
	Dir     string        `json:"dir,omitempty"`
	TTL     time.Duration `json:"ttl,omitempty"`
}

// AddResult is the new subscription plus any event the cached reading
// already produced (a condition that holds at once is met here).
type AddResult struct {
	Subscription Subscription `json:"subscription"`
	Events       []Event      `json:"events"`
}

// TargetStatus is the daemon's view of one deduped target.
type TargetStatus struct {
	Target      string    `json:"target"`
	Subscribers int       `json:"subscribers"`
	Summary     string    `json:"summary,omitempty"`
	ObservedAt  time.Time `json:"observedAt,omitzero"`
	NextAt      time.Time `json:"nextAt,omitzero"`
	Failures    int       `json:"failures"`
	LastError   string    `json:"lastError,omitempty"`
}

// Listing is `watch ls`.
type Listing struct {
	Subscriptions []Subscription `json:"subscriptions"`
	Targets       []TargetStatus `json:"targets"`
}

// Health is the daemon's self-report.
type Health struct {
	OK            bool     `json:"ok"`
	PID           int      `json:"pid"`
	Subscriptions int      `json:"subscriptions"`
	Targets       int      `json:"targets"`
	Budget        float64  `json:"githubBudget"`
	Tasks         []string `json:"tasks"`
}

type target struct {
	key, kind, ref string
	nextAt         time.Time
	failures       int
	inflight       bool
	errorSent      bool
	last           *Observation
	lastAt         time.Time
	lastErr        string
}

type task struct {
	name     string
	every    time.Duration
	fn       func(context.Context) error
	nextAt   time.Time
	inflight bool
}

// Engine is the scheduler and the event queues; Serve puts it behind the
// socket, Until runs a private one when no daemon answers.
type Engine struct {
	probes    map[string]Probe
	store     Store
	budget    *Budget
	now       func() time.Time
	log       io.Writer
	maxEvents int

	mu      sync.Mutex
	st      state
	targets map[string]*target
	tasks   []*task
	wake    chan struct{}
	wg      sync.WaitGroup
}

// Options configure NewEngine; zero values take the defaults.
type Options struct {
	Store     Store
	Budget    *Budget
	Now       func() time.Time
	Log       io.Writer
	MaxEvents int
}

// NewEngine loads the persisted state and schedules every subscribed target
// for an immediate probe.
func NewEngine(probes []Probe, opts Options) (*Engine, error) {
	e := &Engine{
		probes:    map[string]Probe{},
		store:     opts.Store,
		budget:    opts.Budget,
		now:       opts.Now,
		log:       opts.Log,
		maxEvents: opts.MaxEvents,
		targets:   map[string]*target{},
		wake:      make(chan struct{}),
	}
	if e.now == nil {
		e.now = time.Now
	}
	if e.log == nil {
		e.log = io.Discard
	}
	if e.maxEvents <= 0 {
		e.maxEvents = DefaultMaxEvents
	}
	for _, p := range probes {
		e.probes[p.Kind()] = p
	}
	st, err := e.store.load()
	if err != nil {
		return nil, err
	}
	e.st = st
	e.syncTargets(e.now())
	return e, nil
}

// Kinds lists the probe kinds this engine serves.
func (e *Engine) Kinds() []string { return slices.Sorted(maps.Keys(e.probes)) }

// Every registers a periodic task the serve loop runs beside the probes (the
// fleet summary publisher is one); a failure is logged, never swallowed.
func (e *Engine) Every(name string, every time.Duration, fn func(context.Context) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tasks = append(e.tasks, &task{name: name, every: every, fn: fn})
}

func newID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "w" + hex.EncodeToString(b)
}

// Add validates and canonicalizes the target, then subscribes. A fresh
// reading of the same target is reused, so a condition that already holds
// is met in the answer itself.
func (e *Engine) Add(ctx context.Context, req AddRequest) (AddResult, error) {
	if req.Session == "" {
		return AddResult{}, errors.New("a subscription needs a session")
	}
	t, err := ParseTarget(req.Target)
	if err != nil {
		return AddResult{}, err
	}
	p, ok := e.probes[t.Kind]
	if !ok {
		return AddResult{}, fmt.Errorf("no probe for %q targets (kinds: %v)", t.Kind, e.Kinds())
	}
	if err := validateCondition(p, req.Until); err != nil {
		return AddResult{}, err
	}
	ref, err := p.Canonical(ctx, t.Ref, req.Dir)
	if err != nil {
		return AddResult{}, err
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	sub := Subscription{
		ID: newID(), Session: req.Session, Target: Target{Kind: t.Kind, Ref: ref}.String(),
		Until: req.Until, Dir: req.Dir, CreatedAt: now, ExpiresAt: now.Add(ttl),
	}
	e.st.Subscriptions = append(e.st.Subscriptions, sub)
	e.syncTargets(now)
	tg := e.targets[sub.Target]
	var events []Event
	if tg.last != nil && now.Sub(tg.lastAt) < p.Interval() {
		var met bool
		last := len(e.st.Subscriptions) - 1
		events, _, met = e.observeFor(last, p, *tg.last, now)
		sub = e.st.Subscriptions[last]
		if met {
			e.drop(map[string]bool{sub.ID: true})
		}
	}
	if err := e.persist(); err != nil {
		return AddResult{}, err
	}
	if len(events) > 0 {
		e.broadcast()
	}
	if events == nil {
		events = []Event{}
	}
	return AddResult{Subscription: sub, Events: events}, nil
}

// Remove drops a subscription; false when no such id.
func (e *Engine) Remove(id string) (bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	queued := len(e.st.Events)
	// The client is done with it: its unacknowledged events go too, or a
	// subscription that already ended (met, expired) and whose session never
	// polls again (`watch until` uses a fresh one each run) leaves them in
	// state.json for good.
	e.st.Events = slices.DeleteFunc(e.st.Events, func(ev Event) bool { return ev.Subscription == id })
	i := slices.IndexFunc(e.st.Subscriptions, func(s Subscription) bool { return s.ID == id })
	if i < 0 {
		if len(e.st.Events) == queued {
			return false, nil
		}
		return false, e.persist()
	}
	e.st.Subscriptions = slices.Delete(e.st.Subscriptions, i, i+1)
	e.syncTargets(e.now())
	return true, e.persist()
}

// List answers the subscriptions (one session's, or all) and the targets
// they share.
func (e *Engine) List(session string) Listing {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := Listing{Subscriptions: []Subscription{}, Targets: []TargetStatus{}}
	keys := map[string]bool{}
	for _, s := range e.st.Subscriptions {
		if session == "" || s.Session == session {
			out.Subscriptions = append(out.Subscriptions, s)
			keys[s.Target] = true
		}
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		t := e.targets[k]
		ts := TargetStatus{Target: k, NextAt: t.nextAt, Failures: t.failures, LastError: t.lastErr}
		for _, s := range e.st.Subscriptions {
			if s.Target == k {
				ts.Subscribers++
			}
		}
		if t.last != nil {
			ts.Summary, ts.ObservedAt = t.last.Summary, t.lastAt
		}
		out.Targets = append(out.Targets, ts)
	}
	return out
}

// Health is the daemon's self-report.
func (e *Engine) Health(pid int) Health {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := Health{OK: true, PID: pid, Subscriptions: len(e.st.Subscriptions), Targets: len(e.targets), Tasks: []string{}}
	if e.budget != nil {
		h.Budget = e.budget.Remaining(e.now())
	}
	for _, t := range e.tasks {
		h.Tasks = append(h.Tasks, t.name)
	}
	return h
}

// Events acknowledges everything of session up to and including after
// (deleting it), then answers the session's newer events — waiting up to
// timeout for the first one. Delivery is at-least-once: an event is gone
// only once a later call acknowledges it.
func (e *Engine) Events(ctx context.Context, session string, after int64, timeout time.Duration) ([]Event, error) {
	if session == "" {
		return nil, errors.New("events need a session")
	}
	timeout = min(max(timeout, 0), MaxWait)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		e.mu.Lock()
		before := len(e.st.Events)
		e.st.Events = slices.DeleteFunc(e.st.Events, func(ev Event) bool { return ev.Session == session && ev.Seq <= after })
		var err error
		if len(e.st.Events) != before {
			err = e.persist()
		}
		pending := []Event{}
		for _, ev := range e.st.Events {
			if ev.Session == session {
				pending = append(pending, ev)
			}
		}
		wake := e.wake
		e.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if len(pending) > 0 || timeout == 0 {
			return pending, nil
		}
		select {
		case <-wake:
		case <-deadline.C:
			return []Event{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Tick expires subscriptions and starts every due probe and task; probes run
// concurrently and apply their reading when done (Wait blocks for them).
func (e *Engine) Tick(ctx context.Context) {
	e.mu.Lock()
	now := e.now()
	e.expire(now)
	type job struct {
		t        *target
		p        Probe
		ref, dir string
	}
	var jobs []job
	for _, k := range slices.Sorted(maps.Keys(e.targets)) {
		t := e.targets[k]
		if t.inflight || now.Before(t.nextAt) {
			continue
		}
		p := e.probes[t.kind]
		if cost := p.Cost(); cost > 0 && e.budget != nil {
			if ok, ready := e.budget.Take(now, cost); !ok {
				t.nextAt = ready
				fmt.Fprintf(e.log, "watch: %s waits for the GitHub budget until %s\n", k, ready.Format(time.RFC3339))
				continue
			}
		}
		t.inflight = true
		jobs = append(jobs, job{t: t, p: p, ref: t.ref, dir: e.anchor(k)})
	}
	var tasks []*task
	for _, tk := range e.tasks {
		if !tk.inflight && !now.Before(tk.nextAt) {
			tk.inflight = true
			tk.nextAt = now.Add(tk.every)
			tasks = append(tasks, tk)
		}
	}
	e.mu.Unlock()

	for _, j := range jobs {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			pctx, cancel := context.WithTimeout(ctx, probeTimeout)
			obs, err := observe(pctx, j.p, j.ref, j.dir)
			cancel()
			e.apply(j.t, j.p, obs, err)
		}()
	}
	for _, tk := range tasks {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			err := runTask(ctx, tk)
			e.mu.Lock()
			tk.inflight = false
			e.mu.Unlock()
			if err != nil {
				fmt.Fprintf(e.log, "watch: task %s failed: %v\n", tk.name, err)
			}
		}()
	}
}

// observe runs one probe. A panic inside it (the pr probe runs gitkit
// in-process) becomes that reading's error and goes through the backoff,
// never the daemon's death.
func observe(ctx context.Context, p Probe, ref, dir string) (obs Observation, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s probe panicked on %s: %v", p.Kind(), ref, r)
		}
	}()
	return p.Observe(ctx, ref, dir)
}

// runTask runs one periodic task with the same panic boundary as a probe.
func runTask(ctx context.Context, tk *task) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panicked: %v", r)
		}
	}()
	return tk.fn(ctx)
}

// Wait blocks until every probe and task Tick started has finished.
func (e *Engine) Wait() { e.wg.Wait() }

// NextDue is the earliest moment a probe or task is due (zero when nothing
// is scheduled).
func (e *Engine) NextDue() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	var next time.Time
	consider := func(at time.Time) {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	for _, t := range e.targets {
		if !t.inflight {
			consider(t.nextAt)
		}
	}
	for _, tk := range e.tasks {
		consider(tk.nextAt)
	}
	return next
}

func (e *Engine) apply(t *target, p Probe, obs Observation, err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	t.inflight = false
	if e.targets[t.key] != t {
		return // every subscriber left while the probe ran
	}
	if errors.Is(err, ErrUnsettled) {
		// The source is mid-computation: the last reading stands and is
		// asked again next interval — no change, no backoff, no event.
		t.nextAt = now.Add(p.Interval())
		return
	}
	if err != nil {
		t.failures++
		factor := min(1<<min(t.failures, 3), maxBackoff)
		if rateLimitish.MatchString(err.Error()) {
			factor = maxBackoff
		}
		t.nextAt = now.Add(p.Interval() * time.Duration(factor))
		t.lastErr = err.Error()
		fmt.Fprintf(e.log, "watch: %s probe failed (%d in a row, next in %s): %v\n", t.key, t.failures, t.nextAt.Sub(now), err)
		if t.failures >= errorStreak && !t.errorSent {
			t.errorSent = true
			for _, s := range e.st.Subscriptions {
				if s.Target == t.key {
					e.enqueue(Event{At: now, Session: s.Session, Subscription: s.ID, Target: s.Target, Until: s.Until, Kind: EventError, Error: t.lastErr})
				}
			}
			e.persistLogged()
			e.broadcast()
		}
		return
	}
	t.failures, t.lastErr, t.errorSent = 0, "", false
	t.nextAt = now.Add(p.Interval())
	t.last, t.lastAt = &obs, now
	var events []Event
	dirty := false
	met := map[string]bool{}
	for i := range e.st.Subscriptions {
		if e.st.Subscriptions[i].Target != t.key {
			continue
		}
		evs, changed, done := e.observeFor(i, p, obs, now)
		events = append(events, evs...)
		dirty = dirty || changed
		if done {
			met[e.st.Subscriptions[i].ID] = true
		}
	}
	e.drop(met)
	if dirty {
		e.persistLogged()
	}
	if len(events) > 0 {
		e.broadcast()
	}
}

// observeFor applies one reading to subscription i: the first reading is its
// baseline, a later different one is a change event, a holding condition is
// a met event. It answers the events, whether the subscription changed, and
// whether it is met (the caller drops it). Called with the lock held.
func (e *Engine) observeFor(i int, p Probe, obs Observation, now time.Time) ([]Event, bool, bool) {
	s := &e.st.Subscriptions[i]
	var events []Event
	dirty := false
	base := Event{At: now, Session: s.Session, Subscription: s.ID, Target: s.Target, Until: s.Until, Summary: obs.Summary, Fields: maps.Clone(obs.Fields)}
	if s.Baseline == nil {
		s.Baseline, dirty = maps.Clone(obs.Fields), true
	} else if changed := changedKeys(s.Seen, obs.Fields); len(changed) > 0 {
		ev := base
		ev.Kind, ev.Changed = EventChange, changed
		events, dirty = append(events, e.enqueue(ev)), true
	}
	s.Seen = maps.Clone(obs.Fields)
	met := holds(p, s.Until, obs, s.Baseline)
	if met {
		ev := base
		ev.Kind = EventMet
		events, dirty = append(events, e.enqueue(ev)), true
	}
	return events, dirty, met
}

// drop removes the given subscriptions (met ones) and retires their targets
// when nobody else waits on them.
func (e *Engine) drop(ids map[string]bool) {
	if len(ids) == 0 {
		return
	}
	e.st.Subscriptions = slices.DeleteFunc(e.st.Subscriptions, func(s Subscription) bool { return ids[s.ID] })
	e.syncTargets(e.now())
}

func (e *Engine) expire(now time.Time) {
	queued := len(e.st.Events)
	// A queue nobody acknowledged for eventTTL belongs to a session that is
	// gone (a crashed wake, an interrupted until).
	e.st.Events = slices.DeleteFunc(e.st.Events, func(ev Event) bool { return now.Sub(ev.At) > eventTTL })
	expired := len(e.st.Events) != queued
	e.st.Subscriptions = slices.DeleteFunc(e.st.Subscriptions, func(s Subscription) bool {
		if !now.After(s.ExpiresAt) {
			return false
		}
		e.enqueue(Event{At: now, Session: s.Session, Subscription: s.ID, Target: s.Target, Until: s.Until, Kind: EventExpired})
		expired = true
		return true
	})
	if expired {
		e.syncTargets(now)
		e.persistLogged()
		e.broadcast()
	}
}

// enqueue stamps the next seq and bounds the session's queue.
func (e *Engine) enqueue(ev Event) Event {
	e.st.Seq++
	ev.Seq = e.st.Seq
	e.st.Events = append(e.st.Events, ev)
	n := 0
	for _, x := range e.st.Events {
		if x.Session == ev.Session {
			n++
		}
	}
	for drop := n - e.maxEvents; drop > 0; drop-- {
		i := slices.IndexFunc(e.st.Events, func(x Event) bool { return x.Session == ev.Session })
		e.st.Events = slices.Delete(e.st.Events, i, i+1)
	}
	return ev
}

// syncTargets makes the target table match the subscriptions: one entry per
// distinct target (due now when new), none for a target nobody waits on.
func (e *Engine) syncTargets(now time.Time) {
	wanted := map[string]bool{}
	for _, s := range e.st.Subscriptions {
		wanted[s.Target] = true
		if _, ok := e.targets[s.Target]; ok {
			continue
		}
		t, _ := ParseTarget(s.Target)
		e.targets[s.Target] = &target{key: s.Target, kind: t.Kind, ref: t.Ref, nextAt: now}
	}
	for k := range e.targets {
		if !wanted[k] {
			delete(e.targets, k)
		}
	}
}

// anchor is the directory a target is probed from: the oldest live
// subscriber's that names one.
func (e *Engine) anchor(key string) string {
	for _, s := range e.st.Subscriptions {
		if s.Target == key && s.Dir != "" {
			return s.Dir
		}
	}
	return ""
}

func (e *Engine) persist() error { return e.store.save(e.st) }

func (e *Engine) persistLogged() {
	if err := e.persist(); err != nil {
		fmt.Fprintf(e.log, "watch: persisting state failed: %v\n", err)
	}
}

func (e *Engine) broadcast() {
	close(e.wake)
	e.wake = make(chan struct{})
}
