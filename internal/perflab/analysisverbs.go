package perflab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/perflab/analysis"
	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/shellword"
	"github.com/henderson-tech/vybava/internal/xctrace"
)

// AnalyzeOptions are the analyze flags.
type AnalyzeOptions struct {
	Marks         string
	WdioLog       string
	StepCycle     []string
	TapLag        time.Duration
	Reread        bool
	SQL           string
	Window        string
	Classify      bool
	Stacks        bool
	Package       string
	Layer         string
	VsyncPeriodNs int64
	RecordLog     string
	Timeout       time.Duration
}

// xctraceRunner adapts perflab's process seam to the readers' Runner: own
// process group, a hard timeout (default 15 min: big Time Profiler
// exports).
func (t *Tool) xctraceRunner(timeout time.Duration) xctrace.Runner {
	if timeout == 0 {
		timeout = 15 * time.Minute
	}
	return func(ctx context.Context, name string, args ...string) ([]byte, []byte, int, error) {
		res, err := t.Exec.Run(ctx, hostexec.Cmd{Argv: append([]string{name}, args...), Timeout: timeout})
		return res.Stdout, res.Stderr, res.Exit, err
	}
}

func (t *Tool) analysisOptions(o AnalyzeOptions) (analysis.Options, error) {
	opts := analysis.Options{
		Exec: t.xctraceRunner(o.Timeout), Reread: o.Reread, Marks: o.Marks, WdioLog: o.WdioLog, StepCycle: o.StepCycle,
		TapLag: o.TapLag, Package: o.Package, Layer: o.Layer, VsyncPeriodNs: o.VsyncPeriodNs, RecordLog: o.RecordLog, SQL: o.SQL,
	}
	if opts.TapLag == 0 {
		opts.TapLag = xctrace.DefaultTapLag
	}
	if o.Window != "" || o.Classify || o.Stacks {
		opts.Profile = true
		opts.ProfileOptions = xctrace.ProfileOptions{Classify: o.Classify, Stacks: o.Stacks}
		if o.Window != "" {
			w, err := xctrace.ParseWindow(o.Window)
			if err != nil {
				return opts, diag(DiagUsage, fmt.Sprintf("--window %q: %v", o.Window, err), "pass --window <fromS>-<toS>, e.g. --window 12.5-14")
			}
			opts.ProfileOptions.Windows = []xctrace.Step{w}
		}
	}
	return opts, nil
}

// Analyze reads stored evidence: .trace, sidecars, dumps, .pftrace, run dirs.
func (t *Tool) Analyze(ctx context.Context, paths []string, o AnalyzeOptions) (Result, error) {
	if len(paths) == 0 {
		return Result{}, diag(DiagUsage, "analyze needs at least one path", "perflab analyze <runDir|.trace|.pftrace|sidecar> --json")
	}
	opts, err := t.analysisOptions(o)
	if err != nil {
		return Result{}, err
	}
	res, diags, err := analysis.Analyze(ctx, paths, opts)
	if err != nil {
		return Result{}, err
	}
	next := []string{}
	var runDirs []string
	for _, p := range paths {
		if analysis.IsRunDir(p) {
			runDirs = append(runDirs, shellword.Quote(p))
		}
	}
	if len(runDirs) > 0 {
		next = append(next, "perflab compare "+strings.Join(runDirs, " ")+" --json", "perflab report "+strings.Join(runDirs, " ")+" --gate --json")
	}
	return Result{Data: res, Diagnostics: diags, Next: next}, nil
}

// ledgerPath is runs.jsonl, the append-only run index.
func (t *Tool) ledgerPath() string { return filepath.Join(t.Lab.StateDir, "runs.jsonl") }

// resolveRunDir takes a run dir, or a variant label the run index knows
// (its newest run dir).
func (t *Tool) resolveRunDir(arg string) (string, error) {
	if analysis.IsRunDir(arg) {
		return arg, nil
	}
	entries, _, err := analysis.ReadLedger(t.ledgerPath())
	if err == nil {
		if dirs := analysis.LatestRunDirs(entries, "", arg); len(dirs) > 0 {
			return dirs[0], nil
		}
	}
	return "", diag(DiagUsage, fmt.Sprintf("%q is neither a run dir (no %s) nor a variant label in %s", arg, analysis.RunFileName, t.ledgerPath()),
		"perflab compare <runDirA> <runDirB> --json")
}

func (t *Tool) analyzeRunDir(ctx context.Context, dir string, opts analysis.Options) (analysis.RunAnalysis, []runx.Diagnostic, error) {
	rf, err := analysis.LoadRunDir(dir)
	if err != nil {
		return analysis.RunAnalysis{}, nil, err
	}
	ra, diags := analysis.AnalyzeRunFile(ctx, rf, opts)
	return ra, diags, nil
}

// CompareOptions are the compare flags.
type CompareOptions struct {
	Scenario      string
	Threshold     float64
	MinRuns       int
	AllowConfound []string
	Reread        bool
}

// Compare judges B against A: two run dirs (or variant labels), or one run
// dir holding exactly two variants (A is the first one run).
func (t *Tool) Compare(ctx context.Context, args []string, o CompareOptions) (Result, error) {
	if len(args) == 0 || len(args) > 2 {
		return Result{}, diag(DiagUsage, "compare takes one run dir with two variants, or two run dirs", "perflab compare <runDirA> <runDirB> --json")
	}
	opts, err := t.analysisOptions(AnalyzeOptions{Reread: o.Reread})
	if err != nil {
		return Result{}, err
	}
	var allowed []analysis.ConfoundField
	for _, f := range o.AllowConfound {
		cf, err := analysis.ParseConfoundField(f)
		if err != nil {
			return Result{}, diag(DiagUsage, err.Error(), "pass --allow-confound nativeKey|publicEnvHash|device|inputSource|productionEquivalent")
		}
		allowed = append(allowed, cf)
	}
	var a, b analysis.Side
	var diags []runx.Diagnostic
	if len(args) == 2 {
		for i, arg := range args {
			dir, err := t.resolveRunDir(arg)
			if err != nil {
				return Result{}, err
			}
			ra, d, err := t.analyzeRunDir(ctx, dir, opts)
			if err != nil {
				return Result{}, err
			}
			diags = append(diags, d...)
			side := analysis.Side{Label: filepath.Base(dir), Runs: []analysis.RunAnalysis{ra}}
			if i == 0 {
				a = side
			} else {
				b = side
			}
		}
	} else {
		dir, err := t.resolveRunDir(args[0])
		if err != nil {
			return Result{}, err
		}
		ra, d, err := t.analyzeRunDir(ctx, dir, opts)
		if err != nil {
			return Result{}, err
		}
		diags = append(diags, d...)
		var variants []string
		seen := map[string]bool{}
		for _, rec := range ra.Records {
			if !seen[rec.Variant] {
				seen[rec.Variant] = true
				variants = append(variants, rec.Variant)
			}
		}
		if len(variants) != 2 {
			return Result{}, diag(DiagUsage, fmt.Sprintf("%s holds %d variants (%s); one-dir compare needs exactly two", dir, len(variants), strings.Join(variants, ", ")),
				"perflab compare <runDirA> <runDirB> --json")
		}
		a = analysis.Side{Label: variants[0], Runs: []analysis.RunAnalysis{ra}, Variant: variants[0]}
		b = analysis.Side{Label: variants[1], Runs: []analysis.RunAnalysis{ra}, Variant: variants[1]}
	}
	cmp, d, err := analysis.Compare(a, b, analysis.CompareOptions{Scenario: o.Scenario, Threshold: o.Threshold, MinRuns: o.MinRuns, AllowConfound: allowed})
	if err != nil {
		return Result{}, err
	}
	diags = append(diags, d...)
	next := []string{"perflab report " + strings.Join(quoteAll(args), " ") + " --gate --json"}
	for _, dg := range diags {
		if dg.Code == analysis.DiagTooFewRuns || dg.Code == analysis.DiagNoisy {
			next = append([]string{"perflab run <scenario> --device <id> --lease <token> --variant <a> --variant <b> --alternate --repeat 2 --json"}, next...)
			break
		}
	}
	return Result{Data: cmp, Diagnostics: diags, Next: next}, nil
}

func quoteAll(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellword.Quote(a)
	}
	return out
}

// ScenarioRow is one row the adapter's scenarios command prints.
type ScenarioRow struct {
	Name       string              `json:"name"`
	WindowMs   int                 `json:"windowMs"`
	Platforms  []string            `json:"platforms,omitempty"`
	Budget     json.RawMessage     `json:"budget,omitempty"`
	StepCycle  []string            `json:"stepCycle,omitempty"`
	StepGroups map[string][]string `json:"stepGroups,omitempty"`
	World      string              `json:"world,omitempty"`
	Account    string              `json:"account,omitempty"`
	Route      string              `json:"route,omitempty"`
}

// Scenarios runs the adapter's scenarios command and decodes its rows
// (unknown keys and budget keys are CONFIG_INVALID).
func (t *Tool) Scenarios(ctx context.Context, platform string) ([]ScenarioRow, error) {
	c, err := t.Cfg()
	if err != nil {
		return nil, err
	}
	v := t.baseVars(ctx, platform)
	cmd, err := v.Expand("scenarios", c.Scenarios, true)
	if err != nil {
		return nil, err
	}
	out, err := t.adapterCommand(ctx, "scenarios", cmd, nil, 2*time.Minute)
	if err != nil {
		return nil, err
	}
	return parseScenarioRows(out, t.byHand(cmd))
}

func parseScenarioRows(out, fix string) ([]ScenarioRow, error) {
	dec := json.NewDecoder(strings.NewReader(strings.TrimSpace(out)))
	dec.DisallowUnknownFields()
	var rows []ScenarioRow
	if err := dec.Decode(&rows); err != nil {
		return nil, diag(DiagConfigInvalid, "the scenarios command did not print a JSON array of {name, windowMs, budget, stepCycle, stepGroups}: "+err.Error(), fix)
	}
	seen := map[string]bool{}
	for i, r := range rows {
		if r.Name == "" || r.WindowMs <= 0 {
			return nil, diag(DiagConfigInvalid, fmt.Sprintf("scenario row %d needs a name and a positive windowMs", i), fix)
		}
		if seen[r.Name] {
			return nil, diag(DiagConfigInvalid, "scenario "+r.Name+" is listed twice", fix)
		}
		seen[r.Name] = true
		if len(r.Budget) > 0 {
			if _, err := analysis.ParseBudget(r.Name, r.Budget); err != nil {
				return nil, diag(DiagConfigInvalid, err.Error(), fix)
			}
		}
	}
	return rows, nil
}

// ReportOptions are the report flags.
type ReportOptions struct {
	Gate   bool
	MD     string
	Reread bool
}

// Report checks the newest result per scenario x device against the
// adapter's budgets; without dirs it reads the run index.
func (t *Tool) Report(ctx context.Context, dirs []string, o ReportOptions) (Result, error) {
	if len(dirs) == 0 {
		entries, _, err := analysis.ReadLedger(t.ledgerPath())
		if err != nil && !os.IsNotExist(err) {
			return Result{}, err
		}
		for _, e := range entries {
			if analysis.IsRunDir(e.RunDir) {
				dirs = append(dirs, e.RunDir)
			}
		}
	}
	opts, err := t.analysisOptions(AnalyzeOptions{Reread: o.Reread})
	if err != nil {
		return Result{}, err
	}
	var runs []analysis.RunAnalysis
	var diags []runx.Diagnostic
	for _, dir := range dirs {
		ra, d, err := t.analyzeRunDir(ctx, dir, opts)
		if err != nil {
			return Result{}, err
		}
		runs = append(runs, ra)
		diags = append(diags, d...)
	}
	budgets := map[string]analysis.Budget{}
	if t.Config != nil {
		rows, err := t.Scenarios(ctx, "")
		if err != nil {
			code := CodeOf(err)
			if code == "" {
				code = DiagAdapterCommandFailed
			}
			diags = append(diags, warn(code, "budgets unavailable: "+err.Error(), adapterFix))
		}
		for _, r := range rows {
			if len(r.Budget) > 0 {
				if b, err := analysis.ParseBudget(r.Name, r.Budget); err == nil {
					budgets[r.Name] = b
				}
			}
		}
	}
	data, d, err := analysis.Report(runs, analysis.ReportOptions{Budgets: budgets, Gate: o.Gate})
	if err != nil {
		return Result{}, err
	}
	diags = append(diags, d...)
	if o.MD != "" {
		if err := os.WriteFile(o.MD, []byte(data.Markdown()), 0o644); err != nil {
			return Result{}, err
		}
	}
	return Result{Data: data, Diagnostics: diags, Lines: strings.Split(strings.TrimRight(data.Markdown(), "\n"), "\n")}, nil
}
