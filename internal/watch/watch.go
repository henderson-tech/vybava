// Package watch is the one machine-wide poller behind `vybava watch`: sessions
// and scripts subscribe to a target (a PR, a devbox box or workspace run, a
// vitrinka task, a deployik environment) and a condition, and the daemon
// (`watch serve`, the LaunchAgent vybava.watchd) probes each DISTINCT target
// once per interval however many subscribers it has, under one GitHub budget,
// and queues per-session events: change, met, error, expired.
//
// The domain decisions stay with their owners: a PR's CI, review, bot (Eve)
// and mergeability state is gitkit's merge-precheck, read in-process; devbox,
// vitrinka and deployik are their own CLIs' --json. This package only
// schedules, dedupes, backs off and delivers. Rules: docs/watch.md.
package watch

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// StateVersion is the persisted state file's schema; a file written by
// another version is refused, never silently re-read.
const StateVersion = 1

// Paths are the daemon's files: the 0700 state dir holding the 0600 socket
// and the atomically replaced state file, plus the LaunchAgent's log dir.
type Paths struct {
	Dir    string
	Socket string
	State  string
	Logs   string
}

// DefaultPaths keeps the socket path short: macOS caps a unix socket path at
// 104 bytes.
func DefaultPaths(home string) Paths {
	dir := filepath.Join(home, ".local", "state", "vybava", "watch")
	return Paths{
		Dir:    dir,
		Socket: filepath.Join(dir, "watchd.sock"),
		State:  filepath.Join(dir, "state.json"),
		Logs:   filepath.Join(home, "Library", "Logs", "vybava"),
	}
}

// Target is `<kind>:<ref>`; the probe for the kind canonicalizes the ref, and
// the canonical form is the dedupe key.
type Target struct {
	Kind string
	Ref  string
}

var kindPattern = regexp.MustCompile(`^[a-z][a-z-]*$`)

// ParseTarget splits `<kind>:<ref>`; whether the kind exists is the engine's
// question (it holds the probes).
func ParseTarget(s string) (Target, error) {
	kind, ref, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok || !kindPattern.MatchString(kind) || strings.TrimSpace(ref) == "" {
		return Target{}, fmt.Errorf("target %q is not <kind>:<ref> (e.g. pr:1684, devbox:b, vitrinka:fixit/4759, deployik:luko/production)", s)
	}
	return Target{Kind: kind, Ref: strings.TrimSpace(ref)}, nil
}

func (t Target) String() string { return t.Kind + ":" + t.Ref }

// Observation is one probe's reading. Fields are compared to decide a
// change, so they carry state only — never a timestamp that moves on its own.
type Observation struct {
	Fields  map[string]string `json:"fields"`
	Summary string            `json:"summary"`
}

// Subscription is one waiter: a session (or an ephemeral `until-…` id)
// waiting for Until on Target. Baseline is the first reading after it
// subscribed (what `changed` compares to); Seen is the last reading it was
// told about (what a change event compares to). Both persist, so a daemon
// restart neither replays nor swallows a change.
type Subscription struct {
	ID        string            `json:"id"`
	Session   string            `json:"session"`
	Target    string            `json:"target"`
	Until     string            `json:"until"`
	Dir       string            `json:"dir,omitempty"`
	CreatedAt time.Time         `json:"createdAt"`
	ExpiresAt time.Time         `json:"expiresAt"`
	Baseline  map[string]string `json:"baseline,omitempty"`
	Seen      map[string]string `json:"seen,omitempty"`
}

// Event kinds.
const (
	EventChange  = "change"  // the target's fields moved since this subscriber last heard
	EventMet     = "met"     // the condition holds; the subscription is gone
	EventError   = "error"   // the probe failed errorStreak times in a row (once per streak)
	EventExpired = "expired" // the subscription's TTL ran out; it is gone
)

// Event is what a session receives, in Seq order.
type Event struct {
	Seq          int64             `json:"seq"`
	At           time.Time         `json:"at"`
	Session      string            `json:"session"`
	Subscription string            `json:"subscription"`
	Target       string            `json:"target"`
	Until        string            `json:"until"`
	Kind         string            `json:"kind"`
	Summary      string            `json:"summary,omitempty"`
	Fields       map[string]string `json:"fields,omitempty"`
	Changed      []string          `json:"changed,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// Condition is a named predicate a probe offers beside the generic ones.
type Condition struct {
	Doc   string
	Holds func(Observation) bool
}

// Generic conditions every kind takes: `changed` (any field differs from the
// subscription's baseline) and `<field>=<value>`.
const ConditionChanged = "changed"

func validateCondition(p Probe, cond string) error {
	if cond == ConditionChanged {
		return nil
	}
	if field, _, ok := strings.Cut(cond, "="); ok && field != "" {
		return nil
	}
	if _, ok := p.Conditions()[cond]; ok {
		return nil
	}
	return fmt.Errorf("%s targets take %s, changed or <field>=<value>; got %q", p.Kind(), strings.Join(conditionNames(p), ", "), cond)
}

func conditionNames(p Probe) []string {
	return slices.Sorted(maps.Keys(p.Conditions()))
}

func holds(p Probe, cond string, obs Observation, baseline map[string]string) bool {
	if cond == ConditionChanged {
		return baseline != nil && !maps.Equal(baseline, obs.Fields)
	}
	if field, value, ok := strings.Cut(cond, "="); ok && field != "" {
		return obs.Fields[field] == value
	}
	if c, ok := p.Conditions()[cond]; ok {
		return c.Holds(obs)
	}
	return false
}

// changedKeys lists the fields that differ, sorted; nil prev changes nothing.
func changedKeys(prev, curr map[string]string) []string {
	if prev == nil {
		return nil
	}
	var keys []string
	for k, v := range curr {
		if old, ok := prev[k]; !ok || old != v {
			keys = append(keys, k)
		}
	}
	for k := range prev {
		if _, ok := curr[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}
