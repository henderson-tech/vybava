package analysis

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/framestats"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/xctrace"
)

// Kind is the evidence kind an input was read as.
type Kind string

const (
	KindIOSTrace   Kind = "ios-trace"
	KindFramestats Kind = "android-framestats"
	KindSidecar    Kind = "android-poll"
	KindPerfetto   Kind = "android-perfetto"
)

// Metrics is perflab's normalized reading of one piece of evidence.
type Metrics struct {
	Kind          Kind                       `json:"kind"`
	IOS           *xctrace.HitchMetrics      `json:"ios,omitempty"`
	IOSSteps      []xctrace.StepSummary      `json:"iosSteps,omitempty"`
	Profile       []xctrace.WindowProfile    `json:"profile,omitempty"`
	Display       *framestats.DisplayMetrics `json:"display,omitempty"`
	AndroidSteps  []framestats.StepRate      `json:"androidSteps,omitempty"`
	AndroidGroups []framestats.StepRate      `json:"androidGroups,omitempty"`
	ProfilerFps   *framestats.ProfilerFps    `json:"profilerFps,omitempty"`
	Framestats    *framestats.Summary        `json:"framestats,omitempty"`
	Present       *framestats.PresentMetrics `json:"present,omitempty"`
	// Slope is the last round's cost over the first's (hitch ms on iOS,
	// dropped-frame share on Android), nil without rounds or a zero first.
	Slope *float64 `json:"slope,omitempty"`
	// IOSRestMainMsPerS: an iOS rest probe's main-thread running ms per
	// recorded second (Time Profiler, whole recording): hitches alone pass
	// a screen that redraws smoothly at rest.
	IOSRestMainMsPerS *float64 `json:"iosRestMainMsPerS,omitempty"`
}

// Options tunes Analyze; every field is optional.
type Options struct {
	// Exec runs xctrace and trace_processor_shell (default os/exec).
	Exec xctrace.Runner
	// Reread ignores the cached xctrace exports.
	Reread bool
	// Steps: a marks file, or the runner log plus a step cycle and groups.
	Marks     string
	WdioLog   string
	StepCycle []string
	Groups    map[string][]string
	TapLag    time.Duration
	// Profile reads the time-profile table (iOS); Windows default to steps.
	Profile        bool
	ProfileOptions xctrace.ProfileOptions
	// Android: the app id, the display period, the layer to read.
	Package       string
	VsyncPeriodNs int64
	Layer         string
	// RecordedAtMs anchors a sidecar without host timestamps (default: the
	// stamp in its file name).
	RecordedAtMs float64
	// RecordLog is xctrace record's output for a bare .trace input.
	RecordLog string
	// SQL runs an embedded preset over a .pftrace input.
	SQL string
}

// Input is one analyzed input.
type Input struct {
	Path    string  `json:"path"`
	Kind    Kind    `json:"kind"`
	Metrics Metrics `json:"metrics"`
	// SQL is the preset's output when --sql was asked.
	SQL string `json:"sql,omitempty"`
}

// RecordResult is one record of a run dir, analyzed.
type RecordResult struct {
	Scenario   string    `json:"scenario"`
	Variant    string    `json:"variant"`
	Attempt    int       `json:"attempt"`
	RecordedAt time.Time `json:"recordedAt"`
	Build      Build     `json:"build"`
	Metrics    *Metrics  `json:"metrics,omitempty"`
	// Error is why the record could not be measured.
	Error *runx.Diagnostic `json:"error,omitempty"`
	// State carries the device snapshots for compare's warnings.
	Before *DeviceState `json:"-"`
	After  *DeviceState `json:"-"`
}

// RunAnalysis is a run dir, every record analyzed.
type RunAnalysis struct {
	RunDir     string         `json:"runDir"`
	Provenance Provenance     `json:"provenance"`
	Records    []RecordResult `json:"records"`
}

// Result is `perflab analyze`'s data.
type Result struct {
	Inputs []Input       `json:"inputs,omitempty"`
	Runs   []RunAnalysis `json:"runs,omitempty"`
}

// Analyze reads every input: a .trace bundle, a poll sidecar
// (frames/<stamp>.json), framestats dumps (read together as one window),
// a .pftrace, or a run dir. Warnings come back beside the result; an
// unusable bare input is an error, an unusable record of a run dir is that
// record's error diagnostic.
func Analyze(ctx context.Context, paths []string, opts Options) (Result, []runx.Diagnostic, error) {
	if len(paths) == 0 {
		return Result{}, nil, diag(DiagUsage, "analyze needs at least one input",
			"perflab analyze <run dir | .trace | frames/<stamp>.json | dump.txt | .pftrace>... --json")
	}
	var res Result
	var diags []runx.Diagnostic
	var dumps []string
	for _, p := range paths {
		kind, isRun, err := detect(p)
		if err != nil {
			return res, diags, err
		}
		if isRun {
			rf, err := LoadRunDir(p)
			if err != nil {
				return res, diags, err
			}
			ra, d := AnalyzeRunFile(ctx, rf, opts)
			res.Runs = append(res.Runs, ra)
			diags = append(diags, d...)
			continue
		}
		if kind == KindFramestats {
			dumps = append(dumps, p)
			continue
		}
		in, d, err := analyzeOne(ctx, kind, p, opts)
		if err != nil {
			return res, diags, err
		}
		res.Inputs = append(res.Inputs, in)
		diags = append(diags, d...)
	}
	if len(dumps) > 0 {
		in, d, err := analyzeDumps(dumps, opts)
		if err != nil {
			return res, diags, err
		}
		res.Inputs = append(res.Inputs, in)
		diags = append(diags, d...)
	}
	return res, diags, nil
}

// detect names an input's kind by its shape.
func detect(p string) (Kind, bool, error) {
	st, err := os.Stat(p)
	if err != nil {
		return "", false, diag(DiagFileUnreadable, fmt.Sprintf("cannot read %s: %v", p, err), "pass an existing run dir or evidence file")
	}
	if st.IsDir() {
		switch {
		case strings.HasSuffix(strings.TrimRight(p, "/"), ".trace"):
			return KindIOSTrace, false, nil
		case IsRunDir(p):
			return "", true, nil
		}
		return "", false, diag(DiagUsage, fmt.Sprintf("%s is neither a .trace bundle nor a run dir (no %s)", p, RunFileName),
			"perflab analyze <run dir | .trace | frames/<stamp>.json | dump.txt | .pftrace>... --json")
	}
	name := strings.ToLower(filepath.Base(p))
	if strings.HasSuffix(name, ".pftrace") || strings.HasSuffix(name, ".perfetto-trace") {
		return KindPerfetto, false, nil
	}
	head := make([]byte, 4096)
	fh, err := os.Open(p)
	if err != nil {
		return "", false, diag(DiagFileUnreadable, fmt.Sprintf("cannot read %s: %v", p, err), "pass an existing evidence file")
	}
	n, _ := fh.Read(head)
	fh.Close()
	head = head[:n]
	switch {
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}) || (strings.HasSuffix(name, ".json") && bytes.Contains(head, []byte(`"dumps"`))) || bytes.Contains(head, []byte(`"vsyncPeriodNs"`)):
		return KindSidecar, false, nil
	case bytes.Contains(head, []byte("Graphics Acceleration Info")) || bytes.Contains(head, []byte("---PROFILEDATA---")) || bytes.Contains(head, []byte("Uptime: ")):
		return KindFramestats, false, nil
	}
	return "", false, diag(DiagUsage, fmt.Sprintf("%s is no known evidence (a .trace bundle, frames/<stamp>.json, a framestats dump, a .pftrace)", p),
		"perflab analyze <run dir | .trace | frames/<stamp>.json | dump.txt | .pftrace>... --json")
}

func analyzeOne(ctx context.Context, kind Kind, p string, opts Options) (Input, []runx.Diagnostic, error) {
	switch kind {
	case KindIOSTrace:
		return analyzeTrace(ctx, p, opts)
	case KindSidecar:
		return analyzeSidecar(p, opts)
	case KindPerfetto:
		return analyzePerfetto(ctx, p, opts)
	}
	return Input{}, nil, diag(DiagUsage, "unknown evidence kind "+string(kind), "")
}

func analyzeTrace(ctx context.Context, p string, opts Options) (Input, []runx.Diagnostic, error) {
	x := xctrace.Exporter{Run: opts.Exec, Reread: opts.Reread}
	res, diags, err := xctrace.Analyze(ctx, x, p, xctrace.AnalyzeOptions{
		RecordLog: opts.RecordLog, Marks: opts.Marks, WdioLog: opts.WdioLog, StepCycle: opts.StepCycle,
		TapLag: opts.TapLag, Profile: opts.ProfileOptions, WithProfile: opts.Profile,
	})
	if err != nil {
		return Input{}, diags, err
	}
	m := Metrics{Kind: KindIOSTrace, IOS: &res.Metrics, IOSSteps: res.Steps, Profile: res.Profile}
	m.Slope = iosSlope(res.Hitches, res.StepSpan)
	return Input{Path: p, Kind: KindIOSTrace, Metrics: m}, diags, nil
}

func analyzeSidecar(p string, opts Options) (Input, []runx.Diagnostic, error) {
	sc, err := framestats.ReadSidecar(p)
	if err != nil {
		return Input{}, nil, err
	}
	so := framestats.StepOptions{Cycle: opts.StepCycle, Groups: opts.Groups, RecordedAtMs: opts.RecordedAtMs}
	if so.RecordedAtMs == 0 {
		so.RecordedAtMs, _ = framestats.RecordedAtFromName(filepath.Base(p))
	}
	var diags []runx.Diagnostic
	if opts.WdioLog != "" && len(opts.StepCycle) > 0 {
		raw, err := os.ReadFile(opts.WdioLog)
		if err != nil {
			return Input{}, nil, diag(DiagFileUnreadable, fmt.Sprintf("cannot read the runner log %s: %v", opts.WdioLog, err), "pass the run.log the runner wrote")
		}
		if so.TapsMs, err = framestats.ReadTapsMs(bytes.NewReader(raw)); err != nil {
			return Input{}, nil, diag(DiagFileUnreadable, fmt.Sprintf("%s: %v", opts.WdioLog, err), "pass the run.log the runner wrote")
		}
	}
	pr, d := framestats.AnalyzeSidecar(sc, so)
	diags = append(diags, d...)
	m := Metrics{Kind: KindSidecar, Display: &pr.Display, AndroidSteps: pr.Steps, AndroidGroups: pr.Groups, ProfilerFps: pr.ProfilerFps}
	return Input{Path: p, Kind: KindSidecar, Metrics: m}, diags, nil
}

func analyzeDumps(paths []string, opts Options) (Input, []runx.Diagnostic, error) {
	sum, err := framestats.ParseFiles(paths, framestats.ParseOptions{Window: opts.Package})
	if err != nil {
		return Input{}, nil, err
	}
	m := Metrics{Kind: KindFramestats, Framestats: &sum}
	var diags []runx.Diagnostic
	if opts.Package != "" {
		var dumps []framestats.Dump
		for _, p := range paths {
			fh, err := os.Open(p)
			if err != nil {
				return Input{}, nil, diag(DiagFileUnreadable, fmt.Sprintf("cannot read %s: %v", p, err), "")
			}
			d, dd, err := framestats.ParseDump(p, fh, opts.Package)
			fh.Close()
			if err != nil {
				return Input{}, nil, err
			}
			dumps = append(dumps, d)
			diags = append(diags, dd...)
		}
		sort.Slice(dumps, func(i, j int) bool { return dumps[i].UptimeMs < dumps[j].UptimeMs })
		if len(dumps) > 1 {
			dm := framestats.SummarizeDisplay(dumps, opts.VsyncPeriodNs)
			m.Display = &dm
		}
	}
	return Input{Path: strings.Join(paths, ","), Kind: KindFramestats, Metrics: m}, dedupeDiags(diags), nil
}

func analyzePerfetto(ctx context.Context, p string, opts Options) (Input, []runx.Diagnostic, error) {
	if opts.Package == "" {
		return Input{}, nil, diag(DiagUsage, "a Perfetto trace is read for one app", "perflab analyze "+p+" --package <app id> --json")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return Input{}, nil, diag(DiagFileUnreadable, fmt.Sprintf("cannot read %s: %v", p, err), "")
	}
	pm, diags, err := framestats.ReadPresent(raw, framestats.PresentOptions{Package: opts.Package, VsyncPeriodNs: opts.VsyncPeriodNs, Layer: opts.Layer})
	if err != nil {
		return Input{}, diags, err
	}
	in := Input{Path: p, Kind: KindPerfetto, Metrics: Metrics{Kind: KindPerfetto, Present: &pm, Display: pm.DisplayRate()}}
	if opts.SQL != "" {
		tool, err := framestats.TraceProcessor()
		if err != nil {
			return in, diags, err
		}
		run := framestats.Runner(opts.Exec)
		if run == nil {
			run = framestats.Runner(xctrace.ExecRunner)
		}
		period := opts.VsyncPeriodNs
		if period == 0 {
			period = int64(pm.VsyncPeriodMs * 1e6)
		}
		if in.SQL, err = framestats.RunPreset(ctx, run, tool, p, opts.SQL, framestats.PresetParams{Package: opts.Package, VsyncPeriodNs: period}); err != nil {
			return in, diags, err
		}
	}
	return in, diags, nil
}

// AnalyzeRunFile analyzes every record of a run dir with its own evidence;
// opts supplies only the reader switches (Exec, Reread, TapLag, Profile).
func AnalyzeRunFile(ctx context.Context, rf RunFile, opts Options) (RunAnalysis, []runx.Diagnostic) {
	ra := RunAnalysis{RunDir: rf.Dir, Provenance: rf.Provenance}
	var diags []runx.Diagnostic
	for _, rec := range rf.Runs {
		rr := RecordResult{Scenario: rec.Scenario, Variant: rec.Build.Variant, Attempt: rec.Attempt, RecordedAt: rec.RecordedAt,
			Build: rec.Build, Before: rec.Before, After: rec.After}
		if rec.Failed {
			ra.Records = append(ra.Records, rr)
			continue
		}
		ev := rec.Evidence
		ro := opts
		ro.Marks, ro.WdioLog, ro.RecordLog = rf.path(ev.Marks), rf.path(ev.Log), rf.path(ev.RecordLog)
		ro.StepCycle, ro.Groups = ev.StepCycle, ev.StepGroups
		ro.Package, ro.VsyncPeriodNs = ev.Package, ev.VsyncPeriodNs
		ro.RecordedAtMs = float64(rec.RecordedAt.UnixMilli())
		ro.SQL = ""
		var in Input
		var d []runx.Diagnostic
		var err error
		switch {
		case ev.Trace != "":
			kind, _, probe := probeScreen(rec.Scenario)
			rest := probe && kind == "rest"
			if rest {
				// The whole recording's per-thread totals (no windows, no classes).
				ro.Profile, ro.ProfileOptions = true, xctrace.ProfileOptions{}
			}
			in, d, err = analyzeTrace(ctx, rf.path(ev.Trace), ro)
			if err == nil && rest {
				in.Metrics.IOSRestMainMsPerS = restMainMsPerS(in.Metrics)
			}
		case ev.Frames != "":
			in, d, err = analyzeSidecar(rf.path(ev.Frames), ro)
		case ev.Pftrace != "":
			in, d, err = analyzePerfetto(ctx, rf.path(ev.Pftrace), ro)
		case len(ev.Dumps) > 0:
			paths := make([]string, len(ev.Dumps))
			for i, p := range ev.Dumps {
				paths[i] = rf.path(p)
			}
			in, d, err = analyzeDumps(paths, ro)
		default:
			err = diag(DiagFileUnreadable, fmt.Sprintf("%s attempt %d names no evidence", rec.Scenario, rec.Attempt), "perflab run "+rec.Scenario+" --resume "+rf.Dir)
		}
		for i := range d {
			d[i].Detail = fmt.Sprintf("%s #%d: %s", rec.Scenario, rec.Attempt, d[i].Detail)
		}
		diags = append(diags, d...)
		if err != nil {
			var de runx.DiagError
			if !errors.As(err, &de) {
				de = diag(runx.DiagInfraError, err.Error(), "")
			}
			e := de.Diag
			e.Detail = fmt.Sprintf("%s #%d: %s", rec.Scenario, rec.Attempt, e.Detail)
			rr.Error = &e
			diags = append(diags, e)
		} else {
			m := in.Metrics
			rr.Metrics = &m
		}
		if !rec.Build.ProductionEquivalent {
			diags = append(diags, info(DiagNotProductionEquivalent,
				fmt.Sprintf("%s #%d ran %s, not a build perflab can vouch for as the as-shipped binary (a packed variant runs with EXUpdatesEnabled NO; a native build counts only with production-equivalent provenance)", rec.Scenario, rec.Attempt, rec.Build.Variant),
				"for the as-shipped confirmation: perflab build native --kind bundled ..."))
		}
		ra.Records = append(ra.Records, rr)
	}
	return ra, dedupeDiags(diags)
}

// iosSlope: hitch ms of the last round over the first, rounds read from
// the step labels ("r3 multi").
func iosSlope(hitches []xctrace.Hitch, steps []xctrace.Step) *float64 {
	if len(steps) == 0 {
		return nil
	}
	byRound := map[int]float64{}
	maxRound := 0
	for _, s := range steps {
		var r int
		if _, err := fmt.Sscanf(s.Label, "r%d ", &r); err == nil && r > maxRound {
			maxRound = r
		}
	}
	for _, h := range hitches {
		var r int
		if h.Step != "" {
			if _, err := fmt.Sscanf(h.Step, "r%d ", &r); err == nil {
				byRound[r] += h.DurationMs
			}
		}
	}
	if maxRound < 2 || byRound[1] <= 0 {
		return nil
	}
	v := float64(int(byRound[maxRound]/byRound[1]*100+0.5)) / 100
	return &v
}

// dedupeDiags drops repeats of the same code and detail.
func dedupeDiags(in []runx.Diagnostic) []runx.Diagnostic {
	seen := map[string]bool{}
	var out []runx.Diagnostic
	for _, d := range in {
		k := d.Code + "\x00" + d.Detail
		if !seen[k] {
			seen[k] = true
			out = append(out, d)
		}
	}
	return out
}

// restMainMsPerS is the main thread's running ms per recorded second over
// the whole recording (the profile's one window), nil when the trace has no
// Time Profiler or no main-thread sample. FixIt's iPhone 11 at rest read
// 164 ms/s with a looping comet and 21 ms/s after it rested, 0 hitches both.
func restMainMsPerS(m Metrics) *float64 {
	if m.IOS == nil || m.IOS.RecordingMs <= 0 || len(m.Profile) == 0 {
		return nil
	}
	for _, th := range m.Profile[0].Threads {
		if th.Thread == "Main Thread" {
			v := round2(th.RunningMs / (m.IOS.RecordingMs / 1000))
			return &v
		}
	}
	return nil
}
