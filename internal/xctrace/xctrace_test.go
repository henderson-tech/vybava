package xctrace

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/runx"
)

// fixture opens a testdata file, gunzipping a .gz one.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(name, ".gz") {
		return raw
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func tocOf(t *testing.T, name string) Run {
	t.Helper()
	toc, err := ParseTOC(bytes.NewReader(fixture(t, name)))
	if err != nil {
		t.Fatal(err)
	}
	run, ok := toc.Run(1)
	if !ok {
		t.Fatalf("%s: no run 1", name)
	}
	return run
}

func diagCode(t *testing.T, err error) string {
	t.Helper()
	var d runx.DiagError
	if !errors.As(err, &d) {
		t.Fatalf("want a DiagError, got %v", err)
	}
	return d.Diag.Code
}

func TestReadTableResolvesARefToTheSameNode(t *testing.T) {
	const xml = `<?xml version="1.0"?><trace-query-result><node><schema name="t"><col><mnemonic>a</mnemonic></col><col><mnemonic>b</mnemonic></col></schema>
<row><process id="1" fmt="App (7)"><pid id="2" fmt="7">7</pid></process><duration id="3">5</duration></row>
<row><process ref="1"/><duration ref="3"/></row></node></trace-query-result>`
	var rows []Row
	schema, err := ReadTable(strings.NewReader(xml), func(r Row) error { rows = append(rows, r); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if schema.Name != "t" || len(schema.Columns) != 2 || len(rows) != 2 {
		t.Fatalf("schema %+v, %d rows", schema, len(rows))
	}
	if rows[0].Cells[0] != rows[1].Cells[0] {
		t.Error("a ref must resolve to the id's node itself")
	}
	if n := len(rows[1].Cell("a").Children); n != 1 {
		t.Errorf("the referenced process has %d children, want 1 (a ref never appends into its target)", n)
	}
	if v, ok := rows[1].Cell("b").Int(); !ok || v != 5 {
		t.Errorf("ref duration = %d %v", v, ok)
	}
	_, err = ReadTable(strings.NewReader(`<r><schema name="t"><col><mnemonic>a</mnemonic></col></schema><row><x ref="9"/></row></r>`), func(Row) error { return nil })
	if !errors.Is(err, errUnresolvedRef) {
		t.Errorf("a ref to an unknown id = %v, want errUnresolvedRef", err)
	}
}

func TestPickHitchTableByDeviceGeneration(t *testing.T) {
	for _, tc := range []struct {
		toc, want string
	}{
		{"ios26-smooth.toc.xml", TableHitches},
		{"ios18-iphone11.toc.xml", TableHitchesSummary},
	} {
		got, err := PickHitchTable(tocOf(t, tc.toc))
		if err != nil || got != tc.want {
			t.Errorf("%s: %q %v, want %q", tc.toc, got, err, tc.want)
		}
	}
	// A run whose hitch tables are only the stage siblings fails closed.
	run := Run{Tables: []Table{{Schema: "hitches-frame-lifetimes"}, {Schema: "hitches-gpu"}, {Schema: "time-profile"}}}
	_, err := PickHitchTable(run)
	if diagCode(t, err) != DiagHitchTableMissing || !strings.Contains(err.Error(), "hitches-frame-lifetimes, hitches-gpu") {
		t.Errorf("err = %v", err)
	}
}

// The expected values are the published readings of the same traces
// (~/Exports/FixIt/perf/2026-10-01-calendar, result JSONs and smooth-log.md).
func TestHitchMetricsMatchThePublishedReadings(t *testing.T) {
	for _, tc := range []struct {
		name, toc, table                string
		count                           int
		timeMs, ratio, worst, recording float64
	}{
		// it4-confirm, iPhone Air on iOS 26: the `hitches` table.
		{"ios26", "ios26-smooth.toc.xml", "ios26-smooth.hitches.xml", 7, 75.03, 0.80, 16.67, 93275.37},
		// it4-iphone11 on iOS 18: `hitches-summary` beside its *-interval
		// siblings. The published 13.92 ms/s summed the siblings.
		{"ios18", "ios18-iphone11.toc.xml", "ios18-iphone11.hitches-summary.xml", 12, 200.48, 2.77, 16.79, 72453.75},
		// smooth-it1 calendar-mode-menu, the UIMenu floor: 25.01 ms over the
		// 27.97 s recorded (the published 0.71 divided by the 35 s limit).
		{"menu floor", "menu-floor.toc.xml", "menu-floor.hitches.xml", 3, 25.01, 0.89, 8.34, 27973.09},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := tocOf(t, tc.toc)
			hitches, schema, err := ReadHitches(bytes.NewReader(fixture(t, tc.table)))
			if err != nil {
				t.Fatal(err)
			}
			m := Summarize(run, schema, hitches)
			if m.HitchCount != tc.count || m.HitchTimeMs != tc.timeMs || m.HitchRatioMsPerS != tc.ratio ||
				m.WorstHitchMs != tc.worst || m.RecordingMs != tc.recording {
				t.Errorf("metrics = %+v", m)
			}
			if m.Device.Platform != "iOS" || m.Device.UDID == "" || m.Device.OS == "" {
				t.Errorf("device = %+v", m.Device)
			}
		})
	}
}

func TestHitchKindsSplitCommitFromRender(t *testing.T) {
	for narrative, want := range map[string]HitchKind{
		"Potentially expensive app update(s)": HitchCommit,
		"Pre-Commit(s) latency":               HitchCommit,
		"Expensive Commit(s)":                 HitchCommit, // iOS 18.7.8, iPhone 11, 2026-10-02 layer run
		"Potentially expensive render, 64 offscreen passes), Potentially expensive GPU work": HitchRender,
		"Commit to Render latency":                    HitchRender,
		"Commit to Render latency - Expensive GPU":    HitchRender,
		"Commit to Render latency - Delay Frame Swap": HitchRender,
		"": HitchUnknown,
	} {
		if got := classifyHitch(narrative); got != want {
			t.Errorf("%q = %s, want %s", narrative, got, want)
		}
	}
}

// stubExport serves fixtures as `xctrace export` output and counts calls.
type stubExport struct {
	files map[string]string // "toc" or schema -> fixture name
	calls int
	fail  []byte
}

func (s *stubExport) run(t *testing.T) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, []byte, int, error) {
		s.calls++
		if s.fail != nil {
			return nil, s.fail, 10, nil
		}
		kind := "toc"
		for i, a := range args {
			if a == "--xpath" {
				x := args[i+1]
				kind = x[strings.Index(x, `@schema="`)+9 : len(x)-2]
			}
		}
		f, ok := s.files[kind]
		if !ok {
			return []byte("<?xml version=\"1.0\"?>\n<trace-query-result>\n</trace-query-result>\n"), nil, 0, nil
		}
		return fixture(t, f), nil, 0, nil
	}
}

func fakeTrace(t *testing.T) string {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "x.trace")
	if err := os.Mkdir(trace, 0o755); err != nil {
		t.Fatal(err)
	}
	return trace
}

func ios26Stub() *stubExport {
	return &stubExport{files: map[string]string{
		"toc": "ios26-smooth.toc.xml", TableHitches: "ios26-smooth.hitches.xml",
		TableRenders: "ios26-smooth.hitches-renders.prefix.xml.gz", TableUpdates: "ios26-smooth.hitches-updates.prefix.xml.gz",
	}}
}

func TestExporterCachesOnlyTheTableItAskedFor(t *testing.T) {
	trace := fakeTrace(t)
	// A scratch tool left an empty export under the cache's name.
	if err := os.WriteFile(CachePath(trace, TableHitches), []byte("<?xml version=\"1.0\"?>\n<trace-query-result>\n</trace-query-result>\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stub := ios26Stub()
	x := Exporter{Run: stub.run(t)}
	res, _, err := Analyze(context.Background(), x, trace, AnalyzeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Metrics.HitchCount != 7 || stub.calls != 2 {
		t.Fatalf("hitches %d after %d exports, want 7 after 2 (toc + the re-exported table)", res.Metrics.HitchCount, stub.calls)
	}
	if _, _, err := Analyze(context.Background(), x, trace, AnalyzeOptions{}); err != nil || stub.calls != 2 {
		t.Errorf("second analyze ran %d exports (err %v), want the caches", stub.calls, err)
	}
	if _, _, err := Analyze(context.Background(), Exporter{Run: stub.run(t), Reread: true}, trace, AnalyzeOptions{}); err != nil || stub.calls != 4 {
		t.Errorf("--reread ran %d exports in total (err %v), want 4", stub.calls, err)
	}
}

func TestAnalyzeRefusesTracesThatCannotBeMeasured(t *testing.T) {
	for _, tc := range []struct {
		name string
		stub *stubExport
		opts AnalyzeOptions
		want string
	}{
		// before/calendar-view-switch-ios-1790880107904.trace: export exit 10.
		{"export fails", &stubExport{fail: fixture(t, "export-missing-template.stderr")}, AnalyzeOptions{}, DiagTraceUnreadable},
		{"record errors", ios26Stub(), AnalyzeOptions{RecordLog: filepath.Join("testdata", "record-malformed.log")}, DiagTraceRunErrors},
		{"stage siblings only", &stubExport{files: map[string]string{"toc": "synthetic-no-hitches.toc.xml"}}, AnalyzeOptions{}, DiagHitchTableMissing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Analyze(context.Background(), Exporter{Run: tc.stub.run(t)}, fakeTrace(t), tc.opts)
			if diagCode(t, err) != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
			if tc.want == DiagTraceUnreadable && !strings.Contains(err.Error(), "Document Missing Template Error") {
				t.Errorf("the detail must carry xctrace's own message: %v", err)
			}
		})
	}
}

func TestRunErrorsReadsTheRunnerLog(t *testing.T) {
	errs := RunErrors(string(fixture(t, "record-malformed.log")))
	if len(errs) != 1 || errs[0] != "Transferred trace file is malformed" {
		t.Fatalf("errs = %q", errs)
	}
	if RunErrors("[0-0] Recording completed. Saving output file...\n") != nil {
		t.Error("a clean recording has no run errors")
	}
}

var calendarCycle = strings.Split("menu,multi,menu,team,menu,list,menu,day,up-to-month,up-to-year,into-month,into-day", ",")

func TestTapsMapOntoTheStepsTheCampaignPublished(t *testing.T) {
	run := tocOf(t, "ios26-smooth.toc.xml")
	taps, err := ReadWdioTaps(bytes.NewReader(fixture(t, "ios26-run.log")))
	if err != nil {
		t.Fatal(err)
	}
	steps, dropped := StepsFromTaps(taps, calendarCycle, run.Start, DefaultTapLag, run.DurationS)
	if len(steps) != 36 || dropped != 0 {
		t.Fatalf("%d steps, %d dropped; want the 36 replay taps of this recording", len(steps), dropped)
	}
	var published []Step
	if err := json.Unmarshal(fixture(t, "ios26-smooth.steps.json"), &published); err != nil {
		t.Fatal(err)
	}
	// The published steps (maptaps.ts, no tap lag) open at the menu tap and
	// end 1.2 s after the item tap; ours open one step per tap, lag applied.
	for i, p := range published[:8] {
		menu := steps[2*i]
		if i >= 4 {
			menu = steps[8+(i-4)]
		}
		if d := menu.From - (p.From + DefaultTapLag.Seconds()); d < -0.001 || d > 0.001 {
			t.Errorf("%s opens at %.3f s, published %.3f s plus the tap lag", menu.Label, menu.From, p.From)
		}
	}
	hitches, _, _ := ReadHitches(bytes.NewReader(fixture(t, "ios26-smooth.hitches.xml")))
	LabelHitches(hitches, steps)
	for i, want := range map[int]string{0: "r1 menu", 2: "r1 up-to-year", 3: "r2 list"} {
		if hitches[i].Step != want {
			t.Errorf("hitch at %.2f ms labelled %q, want %q", hitches[i].StartMs, hitches[i].Step, want)
		}
	}
	if off := hitches[2].StepOffsetMs; off == nil || *off != 112.07 {
		t.Errorf("up-to-year hitch offset = %v", off)
	}
}

func TestMarksOnTheWallClockBecomeTraceSteps(t *testing.T) {
	run := tocOf(t, "ios26-smooth.toc.xml")
	at := func(s float64) *float64 { v := float64(run.Start.UnixMilli()) + s*1000; return &v }
	from, to := 2.0, 3.0
	steps, dropped := StepsFromMarks([]Mark{
		{Label: "open", AtMs: at(10)}, {Label: "close", AtMs: at(20)}, {Label: "late", AtMs: at(500)},
		{Label: "relative", From: &from, To: &to},
	}, run.Start, DefaultTapLag, run.DurationS)
	if dropped != 1 || len(steps) != 3 {
		t.Fatalf("steps %+v dropped %d", steps, dropped)
	}
	if steps[1].Label != "open" || steps[1].From != 10.25 || steps[1].To != 20.25 || steps[2].To != 500.25 {
		t.Errorf("steps = %+v", steps)
	}
}

func TestStepSummaryCarriesOffscreenPassesAndLongUpdates(t *testing.T) {
	stub := ios26Stub()
	logPath := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(logPath, fixture(t, "ios26-run.log"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, diags, err := Analyze(context.Background(), Exporter{Run: stub.run(t)}, fakeTrace(t), AnalyzeOptions{WdioLog: logPath, StepCycle: calendarCycle})
	if err != nil || len(diags) != 0 {
		t.Fatalf("err %v diags %+v", err, diags)
	}
	byKind := map[string]StepSummary{}
	for _, s := range res.Steps {
		byKind[s.Label] = s
	}
	menu, multi := byKind["menu"], byKind["multi"]
	if menu.Occurrences != 12 || menu.Hitches != 2 || menu.RenderHitches != 1 {
		t.Errorf("menu = %+v", menu)
	}
	// The renders and updates excerpts cover the first 10 s: r1 menu,
	// r1 multi, r1 menu (team) and the start of r1 team.
	if multi.OffscreenP50 == nil || *multi.OffscreenP50 != multiOffscreenP50 || multi.OffscreenMax == nil || *multi.OffscreenMax != multiOffscreenMax {
		t.Errorf("multi offscreen = %v / %v", ptr(multi.OffscreenP50), ptrI(multi.OffscreenMax))
	}
	if multi.LongUpdates != multiLongUpdates {
		t.Errorf("multi long updates = %d", multi.LongUpdates)
	}
}

func TestProfileClassifiesTheMainThread(t *testing.T) {
	// The first 300 samples of it4-confirm's time profile (binaries
	// stripped); expectations from classify.ts over the same rows.
	windows, err := ReadProfile(bytes.NewReader(fixture(t, "time-profile.prefix.xml.gz")), ProfileOptions{PID: 11077, Classify: true, Stacks: true})
	if err != nil {
		t.Fatal(err)
	}
	var main ThreadProfile
	for _, th := range windows[0].Threads {
		if th.Thread == "Main Thread" {
			main = th
		}
	}
	want := []Weighted{{"a11y-automation", 114}, {"ui-touch > menu", 16}, {"ca-commit", 9}, {"menu > ca-layout", 7}}
	for i, w := range want {
		if i >= len(main.Classes) || main.Classes[i] != w {
			t.Fatalf("main classes = %+v, want %+v first", main.Classes, want)
		}
	}
	if len(main.Leaves) == 0 || main.RunningMs < 200 {
		t.Errorf("main = %+v", main)
	}
}

func ptr(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func ptrI(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// Cross-checked with xt.ts (sax) over the same excerpt and step spans.
const (
	multiOffscreenP50 = 10
	multiOffscreenMax = 76
	multiLongUpdates  = 1
)

func TestParseWindowReadsTraceSeconds(t *testing.T) {
	w, err := ParseWindow("12.5-20")
	if err != nil || w.From != 12.5 || w.To != 20 {
		t.Errorf("window = %+v %v", w, err)
	}
	for _, bad := range []string{"20-12", "x-1", "5", "-3-4"} {
		if _, err := ParseWindow(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
}
