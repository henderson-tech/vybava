// Package reclaim is the emergency disk reclaimer: when a dev Mac is minutes
// away from processes crashing on a full disk, it must free the most space in
// the fewest seconds, visibly, without asking anything.
//
// It never scans first. It runs a FIXED ladder of steps, tiered by risk and
// ordered by expected yield, every step deleting something that regenerates on
// its own (build caches, package caches, prunable Docker layers, derived
// data). Steps in a tier run concurrently and each one reports the moment it
// finishes, with the volume's free space at that instant — partial wins land
// while later steps are still working. A target (`--until`) stops the ladder
// as soon as enough is free.
//
// What it will never touch: Docker volumes and containers (a paused worktree's
// database lives there), screen recordings and anything else that is user
// data, and anything it cannot classify. Those belong to a human decision.
package reclaim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Tier is the risk ladder. Every tier is reversible; higher means the
// regeneration is slower or costs a re-download (DeviceSupport re-syncs from
// a plugged phone, a simulator runtime re-downloads through Xcode).
type Tier int

const (
	// TierBulk is seconds-to-run, huge-yield build and package caches.
	TierBulk Tier = 1
	// TierCaches is tool and app caches, logs, prunable browsers.
	TierCaches Tier = 2
	// TierAggressive is reversible but costlier to rebuild.
	TierAggressive Tier = 3
)

// Step is one deletion the ladder can perform.
type Step struct {
	ID    string `json:"id"`
	Tier  Tier   `json:"tier"`
	Title string `json:"title"`
	// Regenerates says what brings the data back, so the report is honest
	// about cost.
	Regenerates string `json:"regenerates"`
	// Paths are removed whole (globs allowed, ~ expanded).
	Paths []string `json:"paths,omitempty"`
	// Aged removes only files older than KeepDays under Paths, never the
	// tree itself — the "warm cache is not garbage" rule.
	Aged bool `json:"aged,omitempty"`
	// Except drops glob matches containing any of these substrings, so a
	// generic step never re-counts a tree that has its own step.
	Except []string `json:"except,omitempty"`
	// Quit names a macOS app to quit before deleting (its sandbox tmp gets
	// rewritten while it runs).
	Quit string `json:"quit,omitempty"`
	// Needs is a binary that must be on PATH for the step to apply.
	Needs string `json:"needs,omitempty"`
	// Run performs a non-path step (Docker prune, simctl) and returns the
	// bytes it reclaimed when the tool reports them, else 0.
	Run func(ctx context.Context, env Env) (int64, error) `json:"-"`
	// Size estimates the step's yield for a dry run when Paths is empty.
	Size func(ctx context.Context, env Env) (int64, error) `json:"-"`
}

// Options steer one run.
type Options struct {
	// MaxTier caps the ladder (default TierAggressive).
	MaxTier Tier
	// Until stops the ladder once the volume has at least this many bytes
	// free; zero runs everything.
	Until int64
	// DryRun sizes instead of deleting.
	DryRun bool
	// Only / Skip filter by step ID.
	Only, Skip []string
	// KeepDays is the age below which Aged steps keep files (default 60).
	KeepDays int
	// StepTimeout bounds one step (default 15 minutes: tier-1 trees of millions
	// of small files — bun/go-build caches — took over 5 minutes while Docker
	// prune ran beside them and were left half-deleted).
	StepTimeout time.Duration
}

// Status of a step after the run.
type Status string

const (
	StatusDone    Status = "done"
	StatusDry     Status = "dry-run"
	StatusSkipped Status = "skipped"
	StatusFailed  Status = "failed"
	StatusStopped Status = "stopped"
)

// Result is one step's outcome.
type Result struct {
	ID          string  `json:"id"`
	Tier        Tier    `json:"tier"`
	Title       string  `json:"title"`
	Status      Status  `json:"status"`
	Bytes       int64   `json:"bytes"`
	Seconds     float64 `json:"seconds"`
	FreeAfter   int64   `json:"free_after"`
	Regenerates string  `json:"regenerates"`
	Error       string  `json:"error,omitempty"`
	// Reason explains a skip (missing binary, filtered, target reached).
	Reason string `json:"reason,omitempty"`
}

// skipError lets a Run step decline at run time on a condition only the run
// can see (an install in flight); the step reports skipped, not failed.
type skipError string

func (e skipError) Error() string { return string(e) }

// Note is a by-hand item the run surfaces but never deletes.
type Note struct {
	Title  string `json:"title"`
	Detail string `json:"detail"`
	Bytes  int64  `json:"bytes,omitempty"`
	Action string `json:"action,omitempty"`
}

// Report is the whole run.
type Report struct {
	Volume     string   `json:"volume"`
	FreeBefore int64    `json:"free_before"`
	FreeAfter  int64    `json:"free_after"`
	Total      int64    `json:"total"`
	Seconds    float64  `json:"seconds"`
	DryRun     bool     `json:"dry_run"`
	Until      int64    `json:"until,omitempty"`
	Reached    bool     `json:"until_reached,omitempty"`
	Results    []Result `json:"results"`
	Notes      []Note   `json:"notes,omitempty"`
}

// Freed is the ground truth: the volume's df delta, not a sum of du figures
// (APFS clones and hardlinks make du lie).
func (r Report) Freed() int64 { return r.FreeAfter - r.FreeBefore }

// Progress receives each step's result the moment it finishes and a marker
// when a tier completes; nil is fine.
type Progress interface {
	Step(Result)
	TierDone(tier Tier, free int64, elapsed time.Duration)
}

// Env is the machine the ladder runs against; tests substitute it.
type Env struct {
	Home     string
	Volume   string
	Now      time.Time
	LookPath func(string) (string, error)
	Free     func(volume string) (int64, int64, error)
	Exec     func(ctx context.Context, name string, args ...string) ([]byte, error)
	Stderr   func(string)
	GOOS     string
	KeepDays int
}

// Plan selects and orders the steps a run will attempt. Order inside a tier
// is by expected yield, which is also the print order for --list.
func Plan(env Env, opts Options) []Step {
	if opts.MaxTier == 0 {
		opts.MaxTier = TierAggressive
	}
	only := set(opts.Only)
	skip := set(opts.Skip)
	var plan []Step
	for _, step := range Ladder(env) {
		if step.Tier > opts.MaxTier && len(only) == 0 {
			continue
		}
		if len(only) > 0 && !only[step.ID] {
			continue
		}
		if skip[step.ID] {
			continue
		}
		plan = append(plan, step)
	}
	return plan
}

// Run executes the plan tier by tier. Steps within a tier run concurrently;
// the ladder stops early once opts.Until is satisfied.
func Run(ctx context.Context, env Env, opts Options, progress Progress) (Report, error) {
	start := env.Now
	if start.IsZero() {
		start = time.Now()
	}
	if opts.KeepDays <= 0 {
		opts.KeepDays = 60
	}
	env.KeepDays = opts.KeepDays
	if opts.StepTimeout <= 0 {
		opts.StepTimeout = 15 * time.Minute
	}
	free, total, err := env.Free(env.Volume)
	if err != nil {
		return Report{}, fmt.Errorf("stat %s: %w", env.Volume, err)
	}
	report := Report{Volume: env.Volume, FreeBefore: free, Total: total, DryRun: opts.DryRun, Until: opts.Until}
	plan := Plan(env, opts)
	byTier := map[Tier][]Step{}
	var tiers []Tier
	for _, step := range plan {
		if _, seen := byTier[step.Tier]; !seen {
			tiers = append(tiers, step.Tier)
		}
		byTier[step.Tier] = append(byTier[step.Tier], step)
	}
	sort.Slice(tiers, func(i, j int) bool { return tiers[i] < tiers[j] })

	var mu sync.Mutex
	reached := func() bool {
		if opts.Until <= 0 {
			return false
		}
		f, _, err := env.Free(env.Volume)
		return err == nil && f >= opts.Until
	}
	emit := func(res Result) {
		mu.Lock()
		defer mu.Unlock()
		report.Results = append(report.Results, res)
		if progress != nil {
			progress.Step(res)
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, tier := range tiers {
		if reached() {
			report.Reached = true
			for _, step := range byTier[tier] {
				emit(skipped(step, "target reached", free))
			}
			continue
		}
		tierStart := time.Now()
		var wg sync.WaitGroup
		for _, step := range byTier[tier] {
			if path, missing := needsMissing(env, step); missing {
				f, _, _ := env.Free(env.Volume)
				emit(skipped(step, path+" not installed", f))
				continue
			}
			if runCtx.Err() != nil {
				f, _, _ := env.Free(env.Volume)
				emit(skipped(step, "stopped", f))
				continue
			}
			wg.Add(1)
			go func(step Step) {
				defer wg.Done()
				res := runStep(runCtx, env, opts, step)
				emit(res)
				if !opts.DryRun && reached() {
					mu.Lock()
					report.Reached = true
					mu.Unlock()
					cancel()
				}
			}(step)
		}
		wg.Wait()
		f, _, _ := env.Free(env.Volume)
		if progress != nil {
			progress.TierDone(tier, f, time.Since(tierStart))
		}
		if runCtx.Err() != nil && ctx.Err() == nil {
			// target reached mid-tier: mark the rest of the ladder
			for _, later := range tiers {
				if later <= tier {
					continue
				}
				for _, step := range byTier[later] {
					emit(skipped(step, "target reached", f))
				}
			}
			break
		}
		if ctx.Err() != nil {
			return report, ctx.Err()
		}
	}
	report.Notes = Notes(ctx, env)
	report.FreeAfter, _, _ = env.Free(env.Volume)
	report.Seconds = time.Since(start).Seconds()
	sort.SliceStable(report.Results, func(i, j int) bool { return report.Results[i].Tier < report.Results[j].Tier })
	return report, nil
}

func runStep(ctx context.Context, env Env, opts Options, step Step) Result {
	started := time.Now()
	res := Result{ID: step.ID, Tier: step.Tier, Title: step.Title, Regenerates: step.Regenerates, Status: StatusDone}
	if opts.DryRun {
		res.Status = StatusDry
	}
	stepCtx, cancel := context.WithTimeout(ctx, opts.StepTimeout)
	defer cancel()

	var bytes int64
	var err error
	switch {
	case len(step.Paths) > 0:
		bytes, err = runPaths(stepCtx, env, opts, step)
	case opts.DryRun && step.Size != nil:
		bytes, err = step.Size(stepCtx, env)
	case opts.DryRun:
		res.Reason = "size unknown until run"
	case step.Run != nil:
		bytes, err = step.Run(stepCtx, env)
	}
	res.Bytes = bytes
	res.Seconds = time.Since(started).Seconds()
	res.FreeAfter, _, _ = env.Free(env.Volume)
	var skip skipError
	switch {
	case err == nil:
	case errors.As(err, &skip):
		res.Status = StatusSkipped
		res.Reason = string(skip)
	case errors.Is(err, context.Canceled):
		res.Status = StatusStopped
		res.Reason = "target reached"
	default:
		res.Status = StatusFailed
		res.Error = err.Error()
	}
	return res
}

func runPaths(ctx context.Context, env Env, opts Options, step Step) (int64, error) {
	if step.Quit != "" && !opts.DryRun {
		quitApp(ctx, env, step.Quit)
	}
	var total int64
	var errs []error
	for _, pattern := range expand(env.Home, step.Paths) {
		matches, _ := filepath.Glob(pattern)
		for _, path := range matches {
			if excepted(path, step.Except) {
				continue
			}
			var n int64
			var err error
			switch {
			case step.Aged:
				n, err = removeAged(ctx, path, env.Now.AddDate(0, 0, -env.KeepDays), opts.DryRun)
			default:
				n, err = removeTree(ctx, path, opts.DryRun)
			}
			total += n
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("%s: %w", path, err))
			}
		}
	}
	return total, errors.Join(errs...)
}

func excepted(path string, except []string) bool {
	for _, e := range except {
		if strings.Contains(path, e) {
			return true
		}
	}
	return false
}

func needsMissing(env Env, step Step) (string, bool) {
	if step.Needs == "" {
		return "", false
	}
	if _, err := env.LookPath(step.Needs); err != nil {
		return step.Needs, true
	}
	return "", false
}

func skipped(step Step, reason string, free int64) Result {
	return Result{ID: step.ID, Tier: step.Tier, Title: step.Title, Regenerates: step.Regenerates, Status: StatusSkipped, Reason: reason, FreeAfter: free}
}

func expand(home string, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if strings.HasPrefix(p, "~/") {
			p = filepath.Join(home, p[2:])
		}
		out = append(out, p)
	}
	return out
}

func set(ids []string) map[string]bool {
	m := map[string]bool{}
	for _, id := range ids {
		for _, part := range strings.Split(id, ",") {
			if part = strings.TrimSpace(part); part != "" {
				m[part] = true
			}
		}
	}
	return m
}

// quitApp asks a macOS app to quit and waits briefly; failure is not fatal,
// the aged delete still only touches old files.
func quitApp(ctx context.Context, env Env, app string) {
	if env.GOOS != "darwin" {
		return
	}
	if _, err := env.LookPath("osascript"); err != nil {
		return
	}
	_, _ = env.Exec(ctx, "osascript", "-e", fmt.Sprintf(`quit app %q`, app))
	time.Sleep(1500 * time.Millisecond)
}
