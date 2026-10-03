package framestats

import (
	"bytes"
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const appID = "app.fixit.client"

// a13 is the head-s20 kept mode-switch dump (Android 13, Samsung S20):
// its header lists FrameInterval before FrameStartTime, its rows the other
// way round.
func a13(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "a13-switch-multi-kept.framestats.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// withPopup appends a popup window block (synthetic: the real block's rows
// under a PopupWindow title, shifted 1 ms) so the window filter is tested.
func withPopup(t *testing.T, dump string) string {
	t.Helper()
	start := strings.Index(dump, "---PROFILEDATA---")
	end := strings.LastIndex(dump, "---PROFILEDATA---")
	block := dump[start : end+len("---PROFILEDATA---")]
	var shifted []string
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, "0,") {
			parts := strings.Split(line, ",")
			parts[2] = parts[2][:len(parts[2])-7] + "1000000"
			line = strings.Join(parts, ",")
		}
		shifted = append(shifted, line)
	}
	return dump + "\nWindow: PopupWindow:7b84b7f\n" + strings.Join(shifted, "\n") + "\n"
}

func TestParseDumpReadsTheAndroid13ColumnSwapAndSkipsPopups(t *testing.T) {
	d, diags, err := ParseDump("a13", strings.NewReader(withPopup(t, a13(t))), appID)
	if err != nil {
		t.Fatal(err)
	}
	if d.UptimeMs != 241104677 || len(d.Frames) != 16 {
		t.Fatalf("uptime %v, %d frames; want 241104677 and the 16 MainActivity rows (the popup's are left out)", d.UptimeMs, len(d.Frames))
	}
	if d.Frames[0].FrameIntervalNs != 8328022 || d.Frames[0].DisplayPresentNs != 241103123651126 {
		t.Errorf("first frame = %+v", d.Frames[0])
	}
	if len(diags) != 1 || diags[0].Code != DiagColumnOrderSwapped || diags[0].Severity != "info" {
		t.Errorf("diags = %+v", diags)
	}
	// The same dump through the summary parser: the window filter keeps the
	// popup's rows out there too.
	s, err := Parse([]Input{{Name: "a13", Reader: strings.NewReader(withPopup(t, a13(t)))}}, ParseOptions{Window: appID})
	if err != nil || s.Frames != 16 {
		t.Errorf("Parse with Window = %d frames, %v", s.Frames, err)
	}
	all, _ := Parse([]Input{{Name: "a13", Reader: strings.NewReader(withPopup(t, a13(t)))}}, ParseOptions{})
	if all.Frames != 32 {
		t.Errorf("Parse without Window = %d frames, want both windows' 32", all.Frames)
	}
}

func TestParseDumpDropsSkippedAndLayoutChangeRows(t *testing.T) {
	dump := a13(t)
	lines := strings.Split(dump, "\n")
	flagged := 0
	for i, line := range lines {
		if strings.HasPrefix(line, "0,") && flagged < 3 {
			lines[i] = []string{"1", "8", "4"}[flagged] + line[1:]
			flagged++
		}
	}
	d, _, err := ParseDump("flags", strings.NewReader(strings.Join(lines, "\n")), appID)
	if err != nil {
		t.Fatal(err)
	}
	// Flags 1 (layout changed) and 8 (skipped) go; 4 (SurfaceCanvas) stays.
	if len(d.Frames) != 14 {
		t.Errorf("%d frames, want 14", len(d.Frames))
	}
}

func TestParseVsyncPeriodReadsSurfaceFlingersFirstLine(t *testing.T) {
	for in, want := range map[string]int64{"8333333\n0\t0\t0\n": 8333333, "16666666": 16666666, "": 0, "241103123651126\n": 0} {
		got, ok := ParseVsyncPeriodNs(in)
		if got != want || ok != (want > 0) {
			t.Errorf("%q = %d %v", in, got, ok)
		}
	}
}

// lab120 sidecars: the calendar-view-switch-smooth worker window polled on
// the S20 at 120 Hz, compacted (each frame kept once, in the last dump that
// listed it, which is the frame the reader keeps). Expectations are the
// published result JSONs and smooth-log.md's per-step table, except where a
// frame's DisplayPresentTime lies seconds before its own vsync (8 frames in
// before, 12 in syncprops and layer): the TypeScript reader placed those
// frames there, this one falls back to FrameCompleted. Published values that
// moved are noted beside the corrected ones.
var lab120 = []struct {
	dir, file            string
	display              DisplayMetrics
	modes, zooms         [2]float64 // median / worst decile @120
	modesTaps, zoomsTaps int
}{
	{"lab120-before", "calendar-view-switch-smooth-android-2026-10-02T10-33-10-748Z.json.gz",
		// published fpsP10 99.6, jankyPct 26.9, modes 91 / 77
		DisplayMetrics{RefreshHz: 120, Frames: 817, FpsP10: 100, FpsMedian: 120, AnimatingBins: 33, AnimatingFpsP10: 90.8, AnimatingFpsMedian: 104, JankyPct: 26.6},
		[2]float64{92, 77}, [2]float64{111, 104}, 12, 12},
	{"lab120-syncprops", "calendar-view-switch-smooth-android-2026-10-02T10-44-10-902Z.json.gz",
		DisplayMetrics{RefreshHz: 120, Frames: 993, FpsP10: 110, FpsMedian: 120, AnimatingBins: 36, AnimatingFpsP10: 102, AnimatingFpsMedian: 112, JankyPct: 13.1},
		[2]float64{110, 102}, [2]float64{112, 100}, 12, 12},
	{"lab120-layer", "calendar-view-switch-smooth-android-2026-10-02T10-48-32-456Z.json.gz",
		// published fpsP10 103.6, zooms 108 / 94
		DisplayMetrics{RefreshHz: 120, Frames: 1000, FpsP10: 104, FpsMedian: 120, AnimatingBins: 38, AnimatingFpsP10: 98, AnimatingFpsMedian: 111, JankyPct: 17.8},
		[2]float64{114, 108}, [2]float64{106, 94}, 12, 12},
}

var calendarCycle = strings.Split("menu,multi,menu,team,menu,list,menu,day,up-to-month,up-to-year,into-month,into-day", ",")

func TestSidecarDisplayMetricsMatchThePublishedLab(t *testing.T) {
	for _, tc := range lab120 {
		t.Run(tc.dir, func(t *testing.T) {
			sc, err := ReadSidecar(filepath.Join("testdata", tc.dir+"."+tc.file))
			if err != nil {
				t.Fatal(err)
			}
			if got := SummarizeDisplay(sc.Dumps, sc.PeriodNs()); got != tc.display {
				t.Errorf("display = %+v\nwant      %+v", got, tc.display)
			}
		})
	}
}

func TestSidecarStepsMatchThePublishedModeSwitchRates(t *testing.T) {
	for _, tc := range lab120 {
		t.Run(tc.dir, func(t *testing.T) {
			sc, err := ReadSidecar(filepath.Join("testdata", tc.dir+"."+tc.file))
			if err != nil {
				t.Fatal(err)
			}
			logRaw, err := os.ReadFile(filepath.Join("testdata", tc.dir+".taps.log"))
			if err != nil {
				t.Fatal(err)
			}
			taps, err := ReadTapsMs(bytes.NewReader(logRaw))
			if err != nil {
				t.Fatal(err)
			}
			recordedAt, ok := RecordedAtFromName(tc.file)
			if !ok {
				t.Fatal("no stamp in the sidecar name")
			}
			res, diags := AnalyzeSidecar(sc, StepOptions{TapsMs: taps, Cycle: calendarCycle, RecordedAtMs: recordedAt,
				Groups: map[string][]string{"modes": {"multi", "team", "list", "day"}, "zooms": {"up-to-month", "up-to-year", "into-month", "into-day"}}})
			// The before sidecar predates host timestamps (clock from the result
			// stamp); the later ones carry them.
			wantClock := map[bool]string{true: "recordedAt", false: "dumps"}[tc.dir == "lab120-before"]
			if len(diags) != 0 || res.ClockSource != wantClock || res.TapsInWindow != 36 {
				t.Fatalf("diags %+v clock %s taps %d", diags, res.ClockSource, res.TapsInWindow)
			}
			got := map[string]StepRate{}
			for _, g := range res.Groups {
				got[g.Label] = g
			}
			for name, want := range map[string][2]float64{"modes": tc.modes, "zooms": tc.zooms} {
				g := got[name]
				if math.Round(g.FpsMedian) != want[0] || math.Round(g.FpsP10) != want[1] {
					t.Errorf("%s = %.1f / %.1f, want %v", name, g.FpsMedian, g.FpsP10, want)
				}
				if g.Taps != tc.modesTaps || g.InBurstFps <= 0 || g.WorstGapAfterTapMs <= 0 {
					t.Errorf("%s = %+v", name, g)
				}
			}
		})
	}
}

func TestCappedProfilerFpsNeverStandsInForTheDisplay(t *testing.T) {
	sc, err := ReadSidecar(filepath.Join("testdata", lab120[2].dir+"."+lab120[2].file))
	if err != nil {
		t.Fatal(err)
	}
	sixty := 60.0
	sc.ProfilerSamples = []ProfilerSample{{AtMs: 1, Fps: &sixty}, {AtMs: 2, Fps: &sixty}}
	res, diags := AnalyzeSidecar(sc, StepOptions{})
	if res.ProfilerFps == nil || !res.ProfilerFps.Capped || len(diags) != 1 || diags[0].Code != DiagCappedFpsSource {
		t.Fatalf("profiler %+v diags %+v", res.ProfilerFps, diags)
	}
}

func TestClockOffsetPrefersTimedDumps(t *testing.T) {
	at := func(v float64) *float64 { return &v }
	sc := Sidecar{Dumps: []Dump{
		{UptimeMs: 1000, AtMs: at(100)}, {UptimeMs: 1600, AtMs: at(702)}, {UptimeMs: 2200, AtMs: at(1300), AdbMs: at(900)},
		{UptimeMs: 2800, AtMs: at(1898)},
	}}
	if off, src := ClockOffset(sc, 0); off != 900 || src != "dumps" {
		t.Errorf("offset %v from %s, want the median 900 of the fast dumps", off, src)
	}
	if off, src := ClockOffset(Sidecar{Dumps: []Dump{{UptimeMs: 5000}}}, 1000); off != 3850 || src != "recordedAt" {
		t.Errorf("fallback offset %v from %s", off, src)
	}
}

func TestPresetsRenderTheirPlaceholders(t *testing.T) {
	names := Presets()
	if len(names) != 14 {
		t.Fatalf("presets = %v", names)
	}
	for _, name := range names {
		sql, err := RenderPreset(name, PresetParams{Package: "app.test"})
		if err != nil || strings.Contains(sql, "{{") || !strings.HasPrefix(sql, "-- "+name+": ") {
			t.Errorf("%s: %v\n%.120s", name, err, sql)
		}
	}
	if sql, _ := RenderPreset("present", PresetParams{Package: "app.test", VsyncPeriodNs: 16_666_666}); !strings.Contains(sql, "1.5 * 16666666") {
		t.Error("present.sql must bucket gaps by the passed period")
	}
	if _, err := RenderPreset("nope", PresetParams{Package: "app.test"}); err == nil || !strings.Contains(err.Error(), "PRESET_UNKNOWN") {
		t.Errorf("unknown preset = %v", err)
	}
	if _, err := RenderPreset("jank", PresetParams{Package: "x' OR 1=1 --"}); err == nil {
		t.Error("a quoted package must be refused")
	}
	var gotArgs []string
	out, err := RunPreset(context.Background(), func(_ context.Context, name string, args ...string) ([]byte, []byte, int, error) {
		gotArgs = append([]string{name}, args...)
		sql, _ := os.ReadFile(args[1])
		if !strings.Contains(string(sql), "'app.test'") {
			t.Errorf("the rendered SQL file holds %q", sql)
		}
		return []byte("\"jank_type\",\"n\"\n\"None\",4\n"), nil, 0, nil
	}, "/opt/tp", "/t/x.pftrace", "jank", PresetParams{Package: "app.test"})
	if err != nil || !strings.Contains(out, "None") || gotArgs[0] != "/opt/tp" || gotArgs[1] != "-q" || gotArgs[3] != "/t/x.pftrace" {
		t.Errorf("RunPreset = %q %v, args %v", out, err, gotArgs)
	}
}

func TestAStalePresentTimeFallsBackToTheFramesCompletion(t *testing.T) {
	// The last four rows of the A13 dump carry a DisplayPresentTime ~3.5 s
	// before their own IntendedVsync.
	d, _, err := ParseDump("a13", strings.NewReader(a13(t)), appID)
	if err != nil {
		t.Fatal(err)
	}
	last := d.Frames[len(d.Frames)-1]
	if last.DisplayPresentNs >= last.IntendedVsyncNs || last.presentMs() != float64(last.FrameCompletedNs)/1e6 {
		t.Errorf("last frame %+v placed at %v", last, last.presentMs())
	}
	if first := d.Frames[0]; first.presentMs() != float64(first.DisplayPresentNs)/1e6 {
		t.Errorf("a sane present must be kept: %+v", first)
	}
}

// A probe's Perfetto presents get the sidecar's display-rate bins: two
// steady 120 Hz bins, a bin with five two-vsync gaps (120 x (1 - 5 x 8.33 /
// 500) = 110), 2 s of rest, then one more steady bin. Rest bins never count
// as animating; fewer than two presents read nothing.
func TestSummarizePresentsBinsAProbesFrames(t *testing.T) {
	period := 1000.0 / 120
	presents := []float64{0}
	add := func(n int, gap float64) {
		for i := 0; i < n; i++ {
			presents = append(presents, presents[len(presents)-1]+gap)
		}
	}
	add(120, period)
	add(5, 2*period)
	add(50, period)
	add(1, 2000)
	add(61, period)
	d := SummarizePresents(presents, period)
	if d.AnimatingBins != 4 || d.AnimatingFpsP10 != 113 || d.AnimatingFpsMedian != 120 || d.RefreshHz != 120 || d.JankyPct == 0 {
		t.Errorf("display = %+v; want 4 animating bins, p10 113 (110 x 0.7 + 120 x 0.3), median 120", d)
	}
	if z := SummarizePresents(presents[:1], period); z != (DisplayMetrics{}) {
		t.Errorf("one present reads nothing: %+v", z)
	}
}
