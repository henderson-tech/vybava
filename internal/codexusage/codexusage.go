// Package codexusage explains where a Codex CLI plan limit went.
//
// Codex writes one JSONL rollout per thread under ~/.codex/sessions and stamps
// every model call with a token_count event carrying both the call's usage and
// the account's live rate-limit percentage. Reading the two together answers
// the only question that matters when a weekly limit drains in an afternoon:
// which thread spent it, and how much runway is left.
package codexusage

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Usage is one call's token accounting. Cached is a SUBSET of Input, never a
// sibling — summing the two double-counts every cache hit. Fresh() is the
// uncached remainder.
type Usage struct {
	Input      int64 `json:"input"`
	Cached     int64 `json:"cached"`
	CacheWrite int64 `json:"cache_write"`
	Output     int64 `json:"output"`
	Reasoning  int64 `json:"reasoning"`
}

func (u Usage) Fresh() int64 { return u.Input - u.Cached }

// Total is what the plan limit actually meters: input (cache reads included)
// plus output. Verified against the reported rate-limit percentage — see
// docs/codexusage.md.
func (u Usage) Total() int64 { return u.Input + u.Output }

func (u Usage) CacheRate() float64 {
	if u.Input == 0 {
		return 0
	}
	return float64(u.Cached) / float64(u.Input) * 100
}

func (u *Usage) add(o Usage) {
	u.Input += o.Input
	u.Cached += o.Cached
	u.CacheWrite += o.CacheWrite
	u.Output += o.Output
	u.Reasoning += o.Reasoning
}

// Limit is the account-wide quota snapshot Codex attaches to a token_count
// event. Codex reports the percentage consumed, never the allowance, so the
// allowance is derived by regression against observed spend.
type Limit struct {
	Plan          string    `json:"plan"`
	ID            string    `json:"id"`
	UsedPercent   float64   `json:"used_percent"`
	WindowMinutes int       `json:"window_minutes"`
	ResetsAt      time.Time `json:"resets_at"`
}

// window keys a limit to its quota period. resets_at drifts a second or two
// between reports, so the hour is the stable identity.
func (l Limit) window() string {
	return l.Plan + "|" + l.ResetsAt.Truncate(time.Hour).Format(time.RFC3339)
}

// Sample is one observation on a thread. Billed samples are model calls;
// unbilled ones are limit refreshes, which carry a fresher quota percentage
// but no new tokens.
type Sample struct {
	At            time.Time `json:"at"`
	Usage         Usage     `json:"usage"`
	ContextWindow int64     `json:"context_window"`
	Limit         *Limit    `json:"limit,omitempty"`
	Billed        bool      `json:"billed"`
}

// Session is one Codex thread: its rollout file plus the calls inside the
// reporting window. A resumed thread keeps appending to its original file, so
// StartedAt can predate the window by days — that gap is the whole story
// behind an expensive session.
type Session struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	File      string    `json:"file"`
	CWD       string    `json:"cwd"`
	Branch    string    `json:"branch"`
	Version   string    `json:"cli_version"`
	StartedAt time.Time `json:"started_at"`
	Samples   []Sample  `json:"-"`
}

// SessionReport is one Codex process's share of the window. A process that
// compacts mid-window rolls over to a fresh rollout file, so one terminal tab
// can own several — they are folded into a single row, because the tab is what
// the user acts on.
type SessionReport struct {
	Session
	Files         []string  `json:"files"`
	Threads       int       `json:"threads"`
	PID           int       `json:"pid,omitempty"`
	TTY           string    `json:"tty,omitempty"`
	ProcessStart  time.Time `json:"process_start,omitempty"`
	Live          bool      `json:"live"`
	Calls         int       `json:"calls"`
	Usage         Usage     `json:"usage"`
	AvgContext    int64     `json:"avg_context"`
	ContextWindow int64     `json:"context_window"`
	Points        float64   `json:"limit_points"`
	Plan          string    `json:"plan,omitempty"`
	First         time.Time `json:"first_call"`
	Last          time.Time `json:"last_call"`

	// byWindow splits spend across quota windows, so a row that straddles an
	// account switch is charged to each window at that window's own rate.
	byWindow map[string]int64
}

// Resumed reports whether the window opened on a transcript that was already
// under way — the pattern that makes a thread expensive, because every call
// re-bills the inherited context.
func (s SessionReport) Resumed() bool {
	return !s.StartedAt.IsZero() && s.First.Sub(s.StartedAt) > time.Hour
}

// merge folds another rollout of the same process into this row.
func (s *SessionReport) merge(o SessionReport) {
	s.Files = append(s.Files, o.Files...)
	s.Threads += o.Threads
	s.Calls += o.Calls
	s.Usage.add(o.Usage)
	if o.ContextWindow > s.ContextWindow {
		s.ContextWindow = o.ContextWindow
	}
	// The oldest rollout carries the thread's identity; a rollover file is
	// unnamed and inherits it.
	if !o.StartedAt.IsZero() && (s.StartedAt.IsZero() || o.StartedAt.Before(s.StartedAt)) {
		s.StartedAt, s.ID = o.StartedAt, o.ID
		if o.Name != "" {
			s.Name = o.Name
		}
		if s.CWD == "" {
			s.CWD = o.CWD
		}
	}
	if s.Name == "" {
		s.Name = o.Name
	}
	if o.First.Before(s.First) || s.First.IsZero() {
		s.First = o.First
	}
	if o.Last.After(s.Last) {
		s.Last = o.Last
	}
	for window, tokens := range o.byWindow {
		s.byWindow[window] += tokens
	}
	if s.Calls > 0 {
		s.AvgContext = s.Usage.Input / int64(s.Calls)
	}
}

// PlanReport is one quota window's burn, and the runway left in it.
type PlanReport struct {
	Plan          string        `json:"plan"`
	Window        string        `json:"window"`
	WindowMinutes int           `json:"window_minutes"`
	ResetsAt      time.Time     `json:"resets_at"`
	StartPercent  float64       `json:"start_percent"`
	EndPercent    float64       `json:"end_percent"`
	Points        float64       `json:"points_used"`
	Usage         Usage         `json:"usage"`
	Calls         int           `json:"calls"`
	Elapsed       time.Duration `json:"elapsed"`
	Exhausted     bool          `json:"exhausted"`
}

// TokensPerPoint is the observed cost of one percent of the quota.
func (p PlanReport) TokensPerPoint() int64 {
	if p.Points <= 0 {
		return 0
	}
	return int64(float64(p.Usage.Total()) / p.Points)
}

// Allowance estimates the quota's full size. It is a projection from this
// window's spend, not a figure Codex publishes.
func (p PlanReport) Allowance() int64 { return p.TokensPerPoint() * 100 }

// PointsPerHour is the burn rate over the measured window.
func (p PlanReport) PointsPerHour() float64 {
	if p.Elapsed <= 0 || p.Points <= 0 {
		return 0
	}
	return p.Points / p.Elapsed.Hours()
}

// Runway is how long the remaining quota lasts at the measured burn rate, and
// whether that estimate is meaningful at all.
func (p PlanReport) Runway() (time.Duration, bool) {
	rate := p.PointsPerHour()
	if rate <= 0 || p.EndPercent >= 100 {
		return 0, false
	}
	return time.Duration((100 - p.EndPercent) / rate * float64(time.Hour)), true
}

// Report is the whole picture for one reporting window.
type Report struct {
	Since    time.Time       `json:"since"`
	Until    time.Time       `json:"until"`
	Usage    Usage           `json:"usage"`
	Calls    int             `json:"calls"`
	Plans    []PlanReport    `json:"plans"`
	Sessions []SessionReport `json:"sessions"`
	Idle     []SessionReport `json:"idle"`
	Warnings []string        `json:"warnings,omitempty"`
}

// Env is the machine the report is read from.
type Env struct {
	Home string
	Now  time.Time
	// Exec runs process-inspection helpers (ps, lsof). A nil Exec, or one that
	// fails, degrades the report to file evidence only — never an error, since
	// historical usage is answerable without any live process.
	Exec func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Cache, when set, keeps each rollout's parse between Runs so a repeat
	// Run reads only appended bytes. Nil reads every rollout whole.
	Cache *Cache
}

// Options bounds the report.
type Options struct {
	Since       time.Time
	IncludeIdle bool
	Top         int
}

// Run reads the rollout tree and returns the window's report. Rows are one
// per live Codex process (falling back to one per rollout for threads whose
// process is gone), ranked by spend.
func Run(ctx context.Context, env Env, opts Options) (Report, error) {
	report := Report{Since: opts.Since, Until: env.Now}

	sessions, warnings, err := scanSessions(env, opts.Since)
	if err != nil {
		return report, err
	}
	report.Warnings = warnings
	report.Plans = summarizePlans(sessions)

	names := loadNames(env)
	live, liveWarn := scanLive(ctx, env)
	report.Warnings = append(report.Warnings, liveWarn...)

	// A live process is the unit the user can act on, so its rollouts collapse
	// into one row; an ended thread keeps its own.
	groups := map[string]*SessionReport{}
	var order []string
	for _, session := range sessions {
		if name, ok := names[session.ID]; ok {
			session.Name = name
		}
		entry := summarize(session)
		key := session.File
		if process, ok := live[session.File]; ok {
			entry.PID, entry.TTY, entry.ProcessStart, entry.Live = process.PID, process.TTY, process.Start, true
			key = fmt.Sprintf("pid:%d", process.PID)
		}
		if existing, ok := groups[key]; ok {
			existing.merge(entry)
			continue
		}
		row := entry
		groups[key] = &row
		order = append(order, key)
	}

	rates := map[string]float64{}
	for _, plan := range report.Plans {
		if perPoint := plan.TokensPerPoint(); perPoint > 0 {
			rates[plan.Window] = float64(perPoint)
		}
	}

	var active, idle []SessionReport
	for _, key := range order {
		row := *groups[key]
		var dominant int64
		for window, tokens := range row.byWindow {
			if rate, ok := rates[window]; ok {
				row.Points += float64(tokens) / rate
			}
			if tokens > dominant {
				dominant, row.Plan = tokens, planOf(report.Plans, window)
			}
		}
		if row.Calls == 0 {
			if row.Live {
				idle = append(idle, row)
			}
			continue
		}
		active = append(active, row)
		report.Usage.add(row.Usage)
		report.Calls += row.Calls
	}

	sort.Slice(active, func(i, j int) bool { return active[i].Usage.Total() > active[j].Usage.Total() })
	sort.Slice(idle, func(i, j int) bool { return idle[i].StartedAt.Before(idle[j].StartedAt) })
	if opts.Top > 0 && len(active) > opts.Top {
		active = active[:opts.Top]
	}
	report.Sessions = active
	if opts.IncludeIdle {
		report.Idle = idle
	}
	return report, nil
}

func planOf(plans []PlanReport, window string) string {
	for _, plan := range plans {
		if plan.Window == window {
			return plan.Plan
		}
	}
	return ""
}

// summarize folds one rollout's in-window samples into a row.
func summarize(session Session) SessionReport {
	entry := SessionReport{
		Session:  session,
		Files:    []string{session.File},
		Threads:  1,
		byWindow: map[string]int64{},
	}
	window := ""
	for _, sample := range session.Samples {
		if sample.Limit != nil {
			window = sample.Limit.window()
		}
		if !sample.Billed {
			continue
		}
		entry.Calls++
		entry.Usage.add(sample.Usage)
		if sample.ContextWindow > entry.ContextWindow {
			entry.ContextWindow = sample.ContextWindow
		}
		entry.byWindow[window] += sample.Usage.Total()
		if entry.First.IsZero() {
			entry.First = sample.At
		}
		entry.Last = sample.At
	}
	if entry.Calls > 0 {
		entry.AvgContext = entry.Usage.Input / int64(entry.Calls)
	}
	return entry
}

// summarizePlans attributes spend to quota windows. Every session reports the
// same account-wide percentage, so the percentage span comes from the extremes
// across all sessions while the tokens are summed — that pairing is what makes
// tokens-per-point measurable.
func summarizePlans(sessions []Session) []PlanReport {
	type bucket struct {
		report PlanReport
		first  time.Time
		last   time.Time
	}
	buckets := map[string]*bucket{}
	for _, session := range sessions {
		for _, sample := range session.Samples {
			if sample.Limit == nil {
				continue
			}
			key := sample.Limit.window()
			b, ok := buckets[key]
			if !ok {
				b = &bucket{report: PlanReport{
					Plan:          sample.Limit.Plan,
					Window:        key,
					WindowMinutes: sample.Limit.WindowMinutes,
					ResetsAt:      sample.Limit.ResetsAt,
					StartPercent:  sample.Limit.UsedPercent,
					EndPercent:    sample.Limit.UsedPercent,
				}, first: sample.At, last: sample.At}
				buckets[key] = b
			}
			if sample.At.Before(b.first) {
				b.first, b.report.StartPercent = sample.At, sample.Limit.UsedPercent
			}
			if !sample.At.Before(b.last) {
				b.last, b.report.EndPercent = sample.At, sample.Limit.UsedPercent
			}
			if sample.Billed {
				b.report.Usage.add(sample.Usage)
				b.report.Calls++
			}
		}
	}

	plans := make([]PlanReport, 0, len(buckets))
	for _, b := range buckets {
		// The reported percentage is integral, so it lags the first call of a
		// window by up to a point. Measuring from the low-water mark keeps the
		// span honest rather than optimistic.
		b.report.Points = b.report.EndPercent - b.report.StartPercent
		b.report.Elapsed = b.last.Sub(b.first)
		b.report.Exhausted = b.report.EndPercent >= 100
		plans = append(plans, b.report)
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].ResetsAt.Before(plans[j].ResetsAt) })
	return plans
}

// ParseSince resolves a window start. It accepts a clock time today ("7am",
// "07:00"), a lookback duration ("3h", "90m"), "today", "yesterday", or an
// explicit date/timestamp — because the question is usually "since I sat
// down", not a timestamp anyone wants to type.
func ParseSince(value string, now time.Time) (time.Time, error) {
	text := strings.ToLower(strings.TrimSpace(value))
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	switch text {
	case "", "today":
		return midnight, nil
	case "yesterday":
		return midnight.AddDate(0, 0, -1), nil
	case "week":
		return midnight.AddDate(0, 0, -7), nil
	}
	if d, err := time.ParseDuration(text); err == nil {
		if d <= 0 {
			return time.Time{}, fmt.Errorf("lookback %q must be positive", value)
		}
		return now.Add(-d), nil
	}
	for _, layout := range []string{"3pm", "3:04pm", "15:04", "15:04:05"} {
		if t, err := time.Parse(layout, text); err == nil {
			return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), t.Second(), 0, now.Location()), nil
		}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, strings.ToUpper(value), now.Location()); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q: use 7am, 3h, today, yesterday, or 2006-01-02T15:04", value)
}

// Human renders a token count at the scale a limit conversation happens in.
func Human(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.0fk", math.Round(float64(n)/1e3))
	default:
		return fmt.Sprintf("%d", n)
	}
}

// Duration renders a span compactly: "3h04m", "21m", "6d 21h".
func Duration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd %dh", int(d.Hours())/24, int(d.Hours())%24)
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}
