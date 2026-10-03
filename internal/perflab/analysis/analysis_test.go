package analysis

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/framestats"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/xctrace"
)

// The fixtures are the readers' own real excerpts (see their testdata
// READMEs): the it4-confirm iPhone Air trace exports and the lab120 S20
// poll sidecars.
const (
	xctraceData    = "../../xctrace/testdata"
	framestatsData = "../../framestats/testdata"
	beforeSidecar  = "lab120-before.calendar-view-switch-smooth-android-2026-10-02T10-33-10-748Z.json.gz"
	layerSidecar   = "lab120-layer.calendar-view-switch-smooth-android-2026-10-02T10-48-32-456Z.json.gz"
)

var calendarCycle = strings.Split("menu,multi,menu,team,menu,list,menu,day,up-to-month,up-to-year,into-month,into-day", ",")

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	raw, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func gunzip(t *testing.T, path string) []byte {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	zr, err := gzip.NewReader(fh)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// xctraceStub answers `xctrace export` from the reader's fixtures.
func xctraceStub(t *testing.T) xctrace.Runner {
	files := map[string]string{"toc": "ios26-smooth.toc.xml", "hitches": "ios26-smooth.hitches.xml",
		"hitches-renders": "ios26-smooth.hitches-renders.prefix.xml.gz", "hitches-updates": "ios26-smooth.hitches-updates.prefix.xml.gz"}
	return func(_ context.Context, _ string, args ...string) ([]byte, []byte, int, error) {
		kind := "toc"
		for i, a := range args {
			if a == "--xpath" {
				x := args[i+1]
				kind = x[strings.Index(x, `@schema="`)+9 : len(x)-2]
			}
		}
		name, ok := files[kind]
		if !ok {
			return []byte("<?xml version=\"1.0\"?>\n<trace-query-result>\n</trace-query-result>\n"), nil, 0, nil
		}
		path := filepath.Join(xctraceData, name)
		if strings.HasSuffix(name, ".gz") {
			return gunzip(t, path), nil, 0, nil
		}
		raw, err := os.ReadFile(path)
		return raw, nil, 0, err
	}
}

// writeRunDir lays out a run dir: an iPhone Air trace record and two S20
// sidecar records (before and layer variants), as perflab run writes them.
func writeRunDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "ios", "x.trace"), 0o755); err != nil {
		t.Fatal(err)
	}
	copyFile(t, filepath.Join(xctraceData, "ios26-run.log"), filepath.Join(dir, "ios", "run.log"))
	copyFile(t, filepath.Join(framestatsData, beforeSidecar), filepath.Join(dir, "a", "frames", beforeSidecar))
	copyFile(t, filepath.Join(framestatsData, "lab120-before.taps.log"), filepath.Join(dir, "a", "run.log"))
	copyFile(t, filepath.Join(framestatsData, layerSidecar), filepath.Join(dir, "b", "frames", layerSidecar))
	copyFile(t, filepath.Join(framestatsData, "lab120-layer.taps.log"), filepath.Join(dir, "b", "run.log"))
	groups := map[string][]string{"modes": {"multi", "team", "list", "day"}}
	rf := RunFile{Version: RunFileVersion, Provenance: Provenance{Device: "s20", Platform: PlatformAndroid, RefreshHz: 120, InputSource: InputW3C}}
	rf.Runs = []RunRecord{
		{Scenario: "calendar-view-switch-smooth", Attempt: 1, RecordedAt: time.Date(2026, 10, 2, 1, 30, 18, 0, time.UTC),
			Build:    Build{Variant: "air", NativeKey: "pf1-k", ProductionEquivalent: true},
			Evidence: Evidence{Trace: "ios/x.trace", Log: "ios/run.log", StepCycle: calendarCycle}},
		{Scenario: "calendar-view-switch-smooth", Attempt: 1, RecordedAt: time.Date(2026, 10, 2, 10, 33, 10, 748e6, time.UTC),
			Build: Build{Variant: "before", NativeKey: "pf1-k"}, After: &DeviceState{ThermalStatus: ptrInt(2)},
			Evidence: Evidence{Frames: "a/frames/" + beforeSidecar, Log: "a/run.log", StepCycle: calendarCycle, StepGroups: groups, Package: "app.fixit.client"}},
		{Scenario: "calendar-view-switch-smooth", Attempt: 2, RecordedAt: time.Date(2026, 10, 2, 10, 48, 32, 456e6, time.UTC),
			Build:    Build{Variant: "layer", NativeKey: "pf1-k"},
			Evidence: Evidence{Frames: "b/frames/" + layerSidecar, Log: "b/run.log", StepCycle: calendarCycle, StepGroups: groups, Package: "app.fixit.client"}},
		{Scenario: "calendar-mode-menu", Attempt: 1, Failed: true, Build: Build{Variant: "layer"}},
	}
	raw, _ := json.Marshal(rf)
	if err := os.WriteFile(filepath.Join(dir, RunFileName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func ptrInt(v int) *int       { return &v }
func ptrF(v float64) *float64 { return &v }
func code(err error) string {
	var d runx.DiagError
	if errors.As(err, &d) {
		return d.Diag.Code
	}
	return ""
}

func TestMetricDirectionsCoverTheVocabulary(t *testing.T) {
	if len(MetricDirections) != len(AllMetrics) {
		t.Fatalf("%d directions for %d metrics", len(MetricDirections), len(AllMetrics))
	}
	for _, m := range AllMetrics {
		if _, ok := MetricDirections[m]; !ok {
			t.Errorf("%s has no direction", m)
		}
	}
}

func TestAnalyzeRereadsARunDirFromItsEvidence(t *testing.T) {
	dir := writeRunDir(t)
	res, diags, err := Analyze(context.Background(), []string{dir}, Options{Exec: xctraceStub(t)})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Runs) != 1 || len(res.Runs[0].Records) != 4 {
		t.Fatalf("runs = %+v", res.Runs)
	}
	recs := res.Runs[0].Records
	ios := recs[0].Metrics
	if ios == nil || ios.IOS.HitchCount != 7 || ios.IOS.HitchRatioMsPerS != 0.8 || len(ios.IOSSteps) == 0 {
		t.Fatalf("ios record = %+v (err %+v)", ios, recs[0].Error)
	}
	before, layer := recs[1].Metrics, recs[2].Metrics
	if before == nil || before.Display.AnimatingFpsP10 != 90.8 || layer == nil || layer.Display.AnimatingFpsP10 != 98 {
		t.Fatalf("android records = %+v / %+v", before, layer)
	}
	modes := before.AndroidGroups[0]
	if modes.Label != "modes" || int(modes.FpsMedian+0.5) != 92 || int(modes.FpsP10+0.5) != 77 {
		t.Errorf("before modes = %+v", modes)
	}
	if recs[3].Metrics != nil || recs[3].Error != nil {
		t.Errorf("a failed attempt is listed unmeasured, without an error: %+v", recs[3])
	}
	var notProd int
	for _, d := range diags {
		if d.Code == DiagNotProductionEquivalent {
			notProd++
		}
		if d.Severity == "error" {
			t.Errorf("unexpected error diagnostic %+v", d)
		}
	}
	if notProd != 2 {
		t.Errorf("%d NOT_PRODUCTION_EQUIVALENT infos, want the two swapped variants", notProd)
	}
}

func TestAnalyzeNamesAnInputItCannotRead(t *testing.T) {
	junk := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(junk, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{nil, {junk}, {filepath.Join(t.TempDir(), "absent")}} {
		_, _, err := Analyze(context.Background(), paths, Options{})
		if c := code(err); c != DiagUsage && c != DiagFileUnreadable {
			t.Errorf("%v: err %v", paths, err)
		}
	}
}

// Noise and threshold tables ported from appium/perf/__tests__/stats.test.ts.
func TestNoiseRuleMatchesTheAppiumPort(t *testing.T) {
	for _, tc := range []struct {
		samples []float64
		noisy   bool
		reason  string
	}{
		{[]float64{100, 102, 98, 101, 99}, false, ""},
		{[]float64{100, 102, 101, 160, 165, 170}, true, "half-run drift"},
		{[]float64{100, 101}, true, "requires at least 5"},
	} {
		n := AnalyzeNoise(tc.samples, LatencyNoiseRule)
		if n.Noisy != tc.noisy || (tc.reason != "" && !strings.Contains(strings.Join(n.Reasons, ";"), tc.reason)) {
			t.Errorf("%v: %+v", tc.samples, n)
		}
	}
	if n := AnalyzeNoise([]float64{0, 0}, DeviceNoiseRule); n.Noisy {
		t.Errorf("two rests at 0 frames are not noise: %+v", n)
	}
	for _, tc := range []struct {
		b    float64
		want Verdict
	}{{115, VerdictWithinNoise}, {116, VerdictRegressed}, {84, VerdictImproved}} {
		row := judge("s", "d", MetricHitchRatio, []float64{100, 100}, []float64{tc.b, tc.b}, CompareOptions{Threshold: 0.15, Noise: DeviceNoiseRule})
		if row.Verdict != tc.want {
			t.Errorf("100 -> %v: %s, want %s", tc.b, row.Verdict, tc.want)
		}
	}
	// Higher is better: an fps drop past the threshold regresses.
	if row := judge("s", "d", MetricAnimatingFpsP10, []float64{100, 100}, []float64{80, 80}, CompareOptions{Threshold: 0.15, Noise: DeviceNoiseRule}); row.Verdict != VerdictRegressed {
		t.Errorf("fps 100 -> 80 = %s", row.Verdict)
	}
}

func sideOf(label string, ra RunAnalysis, variant string) Side {
	return Side{Label: label, Runs: []RunAnalysis{ra}, Variant: variant}
}

func TestCompareJudgesTheLabVariantsAndRefusesConfounders(t *testing.T) {
	dir := writeRunDir(t)
	rf, err := LoadRunDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	ra, _ := AnalyzeRunFile(context.Background(), rf, Options{Exec: xctraceStub(t)})
	a, b := sideOf("before", ra, "before"), sideOf("layer", ra, "layer")

	c, diags, err := Compare(a, b, CompareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range c.Rows {
		if r.Verdict != VerdictTooFewRuns {
			t.Errorf("one run a side must read too-few-runs: %+v", r)
		}
	}
	if !hasCode(diags, DiagTooFewRuns) || !hasCode(diags, DiagThermalHot) {
		t.Errorf("diags = %+v", diags)
	}

	c, _, err = Compare(a, b, CompareOptions{MinRuns: 1})
	if err != nil {
		t.Fatal(err)
	}
	got := map[Metric]Verdict{}
	for _, r := range c.Rows {
		got[r.Metric] = r.Verdict
	}
	// jankyPct 26.6 -> 17.8 (-33 %) improved; the animating worst decile
	// 90.8 -> 98 (+7.9 %) stays inside the 15 % threshold.
	if got[MetricJankyPct] != VerdictImproved || got[MetricAnimatingFpsP10] != VerdictWithinNoise {
		t.Errorf("verdicts = %v", got)
	}
	var modes *StepCompare
	for i := range c.Steps {
		if c.Steps[i].Step == "modes" {
			modes = &c.Steps[i]
		}
	}
	if modes == nil || int(modes.A+0.5) != 77 || int(modes.B+0.5) != 108 {
		t.Errorf("modes step = %+v", modes)
	}

	// A different native key confounds, unless allowed.
	ra.Records[2].Build.NativeKey = "pf1-other"
	_, _, err = Compare(a, sideOf("layer", ra, "layer"), CompareOptions{MinRuns: 1})
	if code(err) != DiagConfounded || !strings.Contains(err.Error(), "nativeKey") {
		t.Fatalf("err = %v", err)
	}
	c, diags, err = Compare(a, sideOf("layer", ra, "layer"), CompareOptions{MinRuns: 1, AllowConfound: []ConfoundField{ConfoundNativeKey}})
	if err != nil || len(c.Confounders) != 1 || !c.Confounders[0].Allowed || !hasCode(diags, DiagConfounded) {
		t.Errorf("allowed confound: err %v confounders %+v", err, c.Confounders)
	}
	if _, err := ParseConfoundField("variant"); code(err) != DiagUsage {
		t.Errorf("variant is no confounder field: %v", err)
	}
}

func hasCode(diags []runx.Diagnostic, c string) bool {
	for _, d := range diags {
		if d.Code == c {
			return true
		}
	}
	return false
}

func TestReportGatesTheNewestRecordAgainstItsBudget(t *testing.T) {
	dir := writeRunDir(t)
	rf, _ := LoadRunDir(dir)
	ra, _ := AnalyzeRunFile(context.Background(), rf, Options{Exec: xctraceStub(t)})
	budget, err := ParseBudget("calendar-view-switch-smooth", json.RawMessage(`{"androidAnimatingFpsP10MinShare":0.75,"androidJankyPctMax":20,"androidFpsP10Min":50,"iosHitchRatioMsPerSMax":5}`))
	if err != nil {
		t.Fatal(err)
	}
	data, _, err := Report([]RunAnalysis{ra}, ReportOptions{Budgets: map[string]Budget{"calendar-view-switch-smooth": budget}, Gate: true})
	if err != nil {
		t.Fatalf("gate: %v", err)
	}
	var smooth, menu ReportRow
	for _, r := range data.Rows {
		switch r.Scenario {
		case "calendar-view-switch-smooth":
			smooth = r
		case "calendar-mode-menu":
			menu = r
		}
	}
	// The newest smooth record is the layer variant (17.8 % janky).
	if smooth.Variant != "layer" || smooth.Verdict != RowPass || menu.Verdict != RowFailed {
		t.Fatalf("rows = %+v", data.Rows)
	}
	for _, c := range smooth.Checks {
		if c.Key == "androidFpsP10Min" && c.Gating {
			t.Errorf("the 60-capped key never gates a 120 Hz display: %+v", c)
		}
	}
	tight, _ := ParseBudget("calendar-view-switch-smooth", json.RawMessage(`{"androidJankyPctMax":10}`))
	if _, _, err := Report([]RunAnalysis{ra}, ReportOptions{Budgets: map[string]Budget{"calendar-view-switch-smooth": tight}, Gate: true}); code(err) != DiagOverBudget || !strings.Contains(err.Error(), "androidJankyPctMax 17.8 > 10") {
		t.Errorf("tight budget: %v", err)
	}
	if _, _, err := Report(nil, ReportOptions{Gate: true}); code(err) != DiagNothingMeasured {
		t.Errorf("nothing measured: %v", err)
	}
	if _, err := ParseBudget("x", json.RawMessage(`{"androidFpsP10":50}`)); code(err) != DiagConfigInvalid {
		t.Errorf("an unknown budget key must be CONFIG_INVALID: %v", err)
	}
	if md := data.Markdown(); !strings.Contains(md, "| calendar-view-switch-smooth | s20 | layer |") {
		t.Errorf("markdown = %s", md)
	}
}

func TestBudgetUsesThe120HzRatioOnAProMotionDevice(t *testing.T) {
	m := Metrics{IOS: &xctrace.HitchMetrics{HitchRatioMsPerS: 3, WorstAppHitchMs: 16.67}}
	b := Budget{IOSHitchRatioMsPerSMax: ptrF(5), IOSHitchRatio120MsPerSMax: ptrF(2), IOSWorstHitchMsMax: ptrF(16.7)}
	if v := verdictOf(checkBudget(b, m, 120)); v != RowFail {
		t.Errorf("3 ms/s on a 120 Hz device against 2 = %s", v)
	}
	if v := verdictOf(checkBudget(b, m, 60)); v != RowPass {
		t.Errorf("3 ms/s on a 60 Hz device against 5 = %s", v)
	}
}

// A pftrace without FrameTimeline leaves the present readings unread: the
// rest and fling budgets get no check (so the row is unbudgeted, never a
// pass on zeros) and compare gets no zero samples.
func TestPresentBudgetsNeedFrameTimeline(t *testing.T) {
	b := Budget{AndroidRestFramesMax: ptrF(0), AndroidRestRunMsMax: ptrF(6000), AndroidFlingTwoVsyncGapsMax: ptrF(3)}
	for _, c := range []struct {
		timeline bool
		want     RowVerdict
	}{{false, RowUnbudgeted}, {true, RowPass}} {
		m := Metrics{Kind: KindPerfetto, Present: &framestats.PresentMetrics{FrameTimeline: c.timeline}}
		if v := verdictOf(checkBudget(b, m, 120)); v != c.want {
			t.Errorf("frameTimeline=%v: verdict %s, want %s", c.timeline, v, c.want)
		}
		if _, ok := m.Scalars()[MetricRestFrames]; ok != c.timeline {
			t.Errorf("frameTimeline=%v: restFrames sample present=%v", c.timeline, ok)
		}
	}
}

// A sweep's probes: report applies the default probe budget per kind (no
// adapter row names them) and merges one screen's rest, drag and fling
// probes into ONE board row, each column read from its own kind (a rest
// trace's draw cost is never a drag column). A scenario row fills only the
// columns its budget names.
func TestReportJudgesProbesAndMergesAScreensBoardRow(t *testing.T) {
	at := time.Date(2026, 10, 3, 0, 45, 0, 0, time.UTC)
	rec := func(scenario string, p framestats.PresentMetrics, animatingP10 float64) RecordResult {
		m := &Metrics{Kind: KindPerfetto, Present: &p}
		if animatingP10 > 0 {
			m.Display = &framestats.DisplayMetrics{RefreshHz: 120, Frames: 2000, AnimatingBins: 30, AnimatingFpsP10: animatingP10}
		}
		at = at.Add(time.Minute)
		return RecordResult{Scenario: scenario, Variant: "before", RecordedAt: at, Metrics: m}
	}
	rest := framestats.PresentMetrics{FrameTimeline: true, RestFrames: 0, RestRunMs: 4937, RTDrawMs: framestats.DurationStats{Avg: ptrF(3.6)}}
	ambient := framestats.PresentMetrics{FrameTimeline: true, RestFrames: 2300, RestRunMs: 19000, RTDrawMs: framestats.DurationStats{Avg: ptrF(2)}}
	drag := framestats.PresentMetrics{FrameTimeline: true, RestFrames: 0, RTDrawMs: framestats.DurationStats{Avg: ptrF(5.1)}}
	fling := framestats.PresentMetrics{FrameTimeline: true, RestFrames: 1878, PresentGaps: framestats.PresentGaps{TwoVsync: 18}, RTDrawMs: framestats.DurationStats{Avg: ptrF(2.6)}}
	ra := RunAnalysis{RunDir: "sweep", Provenance: Provenance{Device: "s20", Platform: PlatformAndroid, RefreshHz: 120}, Records: []RecordResult{
		rec("probe-rest-worker-hub", rest, 0),
		rec("probe-drag-worker-hub", drag, 100),
		rec("probe-fling-worker-hub", fling, 96),
		rec("probe-rest-chat", ambient, 0),
		rec("worker-hub-drag", drag, 0),
	}}
	scenarioBudget, _ := ParseBudget("worker-hub-drag", json.RawMessage(`{"androidDragRtDrawMsMax":6}`))
	data, _, err := Report([]RunAnalysis{ra}, ReportOptions{Budgets: map[string]Budget{"worker-hub-drag": scenarioBudget}})
	if err != nil {
		t.Fatal(err)
	}
	verdicts := map[string]RowVerdict{}
	for _, r := range data.Rows {
		verdicts[r.Scenario] = r.Verdict
	}
	want := map[string]RowVerdict{"probe-rest-worker-hub": RowPass, "probe-drag-worker-hub": RowFail, "probe-fling-worker-hub": RowPass,
		"probe-rest-chat": RowFail, "worker-hub-drag": RowPass}
	for s, v := range want {
		if verdicts[s] != v {
			t.Errorf("%s: verdict %s, want %s (drag 5.1 ms > 4; fling 96 of 120 Hz >= 0.75; 2300 rest frames > 0)", s, verdicts[s], v)
		}
	}
	if len(data.BoardRows) != 3 {
		t.Fatalf("board rows = %+v, want one per screen (worker-hub, chat, worker-hub-drag)", data.BoardRows)
	}
	byScreen := map[string]BoardRow{}
	for _, b := range data.BoardRows {
		byScreen[b.Screen] = b
	}
	hub := byScreen["worker-hub on s20"]
	if hub.Verdict != "fail" || hub.RestFrames == nil || *hub.RestFrames != 0 || hub.DragDrawMs == nil || *hub.DragDrawMs != 5.1 ||
		hub.DragFpsP10 == nil || *hub.DragFpsP10 != 100 || hub.FlingGaps == nil || *hub.FlingGaps != 18 || hub.FlingFpsP10 == nil || *hub.FlingFpsP10 != 96 {
		t.Errorf("hub row = %+v", hub)
	}
	if chat := byScreen["chat on s20"]; chat.DragDrawMs != nil || chat.FlingGaps != nil || chat.RestFrames == nil || *chat.RestFrames != 2300 {
		t.Errorf("a rest probe fills only the rest column: %+v", chat)
	}
	if s := byScreen["worker-hub-drag on s20"]; s.RestFrames != nil || s.FlingGaps != nil || s.DragDrawMs == nil || *s.DragDrawMs != 5.1 {
		t.Errorf("a scenario fills the columns its budget names: %+v", s)
	}
}

// An iOS rest probe is judged on the main thread's work at rest: FixIt's
// iPhone 11 read 0 hitches both ways, but 3.3 s of main thread per 20 s
// with a looping comet and 0.42 s once it rested.
func TestIOSRestProbeIsJudgedOnMainThreadWork(t *testing.T) {
	budget, err := ParseBudget("probe-rest-ios-dispatch-home", ProbeBudget("rest", PlatformIOS))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		mainMs  float64
		perS    float64
		verdict RowVerdict
	}{{"comet looping", 3296, 164.8, RowFail}, {"rested", 418, 20.9, RowPass}} {
		m := Metrics{Kind: KindIOSTrace, IOS: &xctrace.HitchMetrics{RecordingMs: 20000},
			Profile: []xctrace.WindowProfile{{Threads: []xctrace.ThreadProfile{
				{Thread: "com.facebook.react.runtime.JavaScript", RunningMs: 209}, {Thread: "Main Thread", RunningMs: c.mainMs}}}}}
		got := restMainMsPerS(m)
		if got == nil || *got != c.perS {
			t.Fatalf("%s: %v ms/s, want %v", c.name, got, c.perS)
		}
		m.IOSRestMainMsPerS = got
		if v := verdictOf(checkBudget(budget, m, 60)); v != c.verdict {
			t.Errorf("%s: verdict %s, want %s (0 hitches either way)", c.name, v, c.verdict)
		}
	}
	if restMainMsPerS(Metrics{IOS: &xctrace.HitchMetrics{RecordingMs: 20000}}) != nil {
		t.Error("a trace without Time Profiler reads nothing, never 0")
	}
}
