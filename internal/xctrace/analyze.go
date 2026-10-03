package xctrace

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// AnalyzeOptions tunes Analyze. Every field is optional.
type AnalyzeOptions struct {
	// RecordLog is `xctrace record`'s output saved beside the trace; its
	// `* [Error]` lines fail the trace (TRACE_RUN_ERRORS).
	RecordLog string
	// Marks is a marks file (LoadMarks). WdioLog plus StepCycle derive the
	// steps from the runner log's taps instead.
	Marks     string
	WdioLog   string
	StepCycle []string
	// TapLag shifts marks and taps onto the device (default DefaultTapLag).
	TapLag time.Duration
	// Profile reads the time-profile table per step (or for Windows).
	Profile ProfileOptions
	// WithProfile turns the profile reading on.
	WithProfile bool
}

// Result is perflab's iOS reading of one trace.
type Result struct {
	Trace    string          `json:"trace"`
	Metrics  HitchMetrics    `json:"metrics"`
	Steps    []StepSummary   `json:"steps,omitempty"`
	Hitches  []Hitch         `json:"hitches,omitempty"`
	Profile  []WindowProfile `json:"profile,omitempty"`
	StepSpan []Step          `json:"stepSpans,omitempty"`
}

// Analyze reads one .trace through the Exporter: TOC, run-error check, the
// one hitch table, then (with marks or taps) the per-step attribution over
// hitches-renders and hitches-updates, and the profile when asked. Warnings
// come back beside a usable result; an unusable trace is an error.
func Analyze(ctx context.Context, x Exporter, trace string, opts AnalyzeOptions) (Result, []runx.Diagnostic, error) {
	res := Result{Trace: trace}
	var diags []runx.Diagnostic
	if opts.TapLag == 0 {
		opts.TapLag = DefaultTapLag
	}
	if opts.RecordLog != "" {
		raw, err := os.ReadFile(opts.RecordLog)
		if err != nil {
			return res, nil, diag(DiagFileUnreadable, fmt.Sprintf("cannot read the record log %s: %v", opts.RecordLog, err), "pass the xctrace record output the runner saved")
		}
		if HitchesUnsupported(string(raw)) {
			return res, nil, diag(DiagHitchTableMissing, "xctrace refused to record hitches (\"Hitches is not supported\"): a simulator or an unsupported device", reRecordFix)
		}
		if errs := RunErrors(string(raw)); len(errs) > 0 {
			return res, nil, diag(DiagTraceRunErrors, fmt.Sprintf("xctrace record reported %d run error(s): %s", len(errs), strings.Join(errs, "; ")), reRecordFix)
		}
	}
	toc, err := x.TOC(ctx, trace)
	if err != nil {
		return res, nil, err
	}
	run, ok := toc.Run(1)
	if !ok || run.DurationS <= 0 {
		return res, nil, diag(DiagTraceEmpty, fmt.Sprintf("%s holds no recorded run (or a zero-length one)", trace), reRecordFix)
	}
	table, err := PickHitchTable(run)
	if err != nil {
		return res, nil, err
	}
	hitches, err := readHitchFile(ctx, x, trace, table)
	if err != nil {
		return res, nil, err
	}

	var steps []Step
	dropped := 0
	switch {
	case opts.Marks != "":
		marks, err := LoadMarks(opts.Marks)
		if err != nil {
			return res, nil, err
		}
		steps, dropped = StepsFromMarks(marks, run.Start, opts.TapLag, run.DurationS)
	case opts.WdioLog != "" && len(opts.StepCycle) > 0:
		fh, err := os.Open(opts.WdioLog)
		if err != nil {
			return res, nil, diag(DiagFileUnreadable, fmt.Sprintf("cannot read the runner log %s: %v", opts.WdioLog, err), "pass the run.log the runner wrote")
		}
		taps, err := ReadWdioTaps(fh)
		fh.Close()
		if err != nil {
			return res, nil, diag(DiagFileUnreadable, fmt.Sprintf("runner log %s: %v", opts.WdioLog, err), "pass the run.log the runner wrote")
		}
		steps, dropped = StepsFromTaps(taps, opts.StepCycle, run.Start, opts.TapLag, run.DurationS)
	}
	if dropped > 0 {
		diags = append(diags, warn(DiagStepsUnmapped, fmt.Sprintf("%d mark(s) or tap(s) fell outside the %.1f s recording", dropped, run.DurationS),
			"check the marks file belongs to this trace, or pass --tap-lag"))
	}
	if len(steps) > 0 {
		LabelHitches(hitches, steps)
		var renders []RenderPass
		var updates []Update
		if run.Has(TableRenders) {
			if renders, err = readRendersFile(ctx, x, trace); err != nil {
				return res, nil, err
			}
		}
		if run.Has(TableUpdates) {
			if updates, err = readUpdatesFile(ctx, x, trace); err != nil {
				return res, nil, err
			}
		}
		res.Steps = SummarizeSteps(steps, hitches, renders, updates, run.Process.Name)
		res.StepSpan = steps
	}
	res.Metrics = Summarize(run, table, hitches)
	res.Hitches = hitches
	if opts.WithProfile {
		if !run.Has(TableTimeProfile) {
			diags = append(diags, warn(DiagProfileMissing, "the trace has no time-profile table; no profile read", "record with the Time Profiler instrument"))
			return res, diags, nil
		}
		path, err := x.Table(ctx, trace, TableTimeProfile)
		if err != nil {
			return res, nil, err
		}
		popts := opts.Profile
		if popts.PID == 0 {
			popts.PID = run.Process.PID
		}
		if len(popts.Windows) == 0 {
			popts.Windows = steps
		}
		fh, err := os.Open(path)
		if err != nil {
			return res, nil, fmt.Errorf("read %s: %w", path, err)
		}
		defer fh.Close()
		if res.Profile, err = ReadProfile(fh, popts); err != nil {
			return res, nil, err
		}
	}
	return res, diags, nil
}

func readHitchFile(ctx context.Context, x Exporter, trace, table string) ([]Hitch, error) {
	path, err := x.Table(ctx, trace, table)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer fh.Close()
	hitches, _, err := ReadHitches(fh)
	return hitches, err
}

func readRendersFile(ctx context.Context, x Exporter, trace string) ([]RenderPass, error) {
	path, err := x.Table(ctx, trace, TableRenders)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer fh.Close()
	return ReadRenders(fh)
}

func readUpdatesFile(ctx context.Context, x Exporter, trace string) ([]Update, error) {
	path, err := x.Table(ctx, trace, TableUpdates)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	defer fh.Close()
	return ReadUpdates(fh)
}
