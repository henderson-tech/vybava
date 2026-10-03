package framestats

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// schedPacket writes sched_switch events (prev_pid=2, next_pid=6) on cpu
// and cpu_frequency events (state=1 kHz, cpu_id=2).
func schedPacket(cpu int, switches [][3]float64, freqs [][3]float64) []byte {
	var events [][]byte
	for _, s := range switches { // tsMs, prevPid, nextPid
		sw := pbMsg(pbVarint(2, int64(s[1])), pbVarint(6, int64(s[2])))
		events = append(events, pbBytes(2, pbMsg(pbVarint(1, ns(s[0])), pbBytes(4, sw))))
	}
	for _, f := range freqs { // tsMs, cpu, kHz
		cf := pbMsg(pbVarint(1, int64(f[2])), pbVarint(2, int64(f[1])))
		events = append(events, pbBytes(2, pbMsg(pbVarint(1, ns(f[0])), pbBytes(11, cf))))
	}
	bundle := pbMsg(append([][]byte{pbVarint(1, int64(cpu))}, events...)...)
	return pbBytes(fTracePacket, pbBytes(fFtraceBundle, bundle))
}

// presentTrace: five main-window frames (vsync 200..204) presenting 1, 2,
// 3 and 10 periods apart at 120 Hz, one popup frame, two RenderThread
// drawings (one on cpu 4 at 1.8 GHz, one on cpu 6 with no clock), a main
// thread eglSwapBuffers, a drawLayer, and SurfaceFlinger display frames of
// which one of two was GPU composited.
func presentTrace() []byte { return presentTraceFor(appPid) }

// presentTraceFor is presentTrace with the app's FrameTimeline surface
// frames attributed to surfacePid (another process when it is not appPid).
func presentTraceFor(surfacePid int) []byte {
	const P = 8.333
	main := []byte("TX - app.test/app.test.MainActivity$_4242#1")
	popup := []byte("TX - PopupWindow:1$_4242#2")
	presents := []float64{100, 100 + P, 100 + 3*P, 100 + 6*P, 100 + 16*P}
	var parts [][]byte
	var m []marker
	for i, at := range presents {
		id := int64(200 + i)
		cookie := int64(10 + i)
		parts = append(parts,
			timelinePacket(at-20, tlActualSurface, pbVarint(1, cookie), pbVarint(2, id), pbVarint(4, int64(surfacePid)), pbBytes(5, main), pbVarint(6, 1)),
			timelinePacket(at, tlFrameEnd, pbVarint(1, cookie)))
		mainMs := 2.0
		if i == 3 {
			mainMs = 12 // frame 203 ran long on the main thread
		}
		m = append(m, marker{at - 20, appPid, "B|4242|Choreographer#doFrame " + itoa(id)}, marker{at - 20 + mainMs, appPid, "E|4242"})
		m = append(m, marker{at - 15, renderTi, "B|4242|DrawFrames " + itoa(id)}, marker{at - 12, renderTi, "E|4242"})
	}
	parts = append(parts,
		timelinePacket(90, tlActualSurface, pbVarint(1, 50), pbVarint(2, 300), pbVarint(4, int64(surfacePid)), pbBytes(5, popup), pbVarint(6, 1)),
		timelinePacket(95, tlFrameEnd, pbVarint(1, 50)),
		// A dropped main-window frame never presents.
		timelinePacket(130, tlActualSurface, pbVarint(1, 51), pbVarint(2, 301), pbVarint(4, int64(surfacePid)), pbBytes(5, main), pbVarint(6, 4)),
		timelinePacket(140, tlFrameEnd, pbVarint(1, 51)),
		// SurfaceFlinger: the expected frame gives the period, one of two
		// actual frames was GPU composited.
		timelinePacket(80, tlExpectedDisplay, pbVarint(1, 60), pbVarint(2, 900)),
		timelinePacket(80+P, tlFrameEnd, pbVarint(1, 60)),
		timelinePacket(80, tlActualDisplay, pbVarint(1, 61), pbVarint(2, 900), pbVarint(6, 1)),
		timelinePacket(90, tlActualDisplay, pbVarint(1, 62), pbVarint(2, 901)),
	)
	m = append(m,
		marker{70, renderTi, "B|4242|Drawing 0.00 0.00 1080.00 2400.00"}, marker{74, renderTi, "E|4242"},
		marker{101, renderTi, "B|4242|Drawing 0.00 0.00 780.00 768.00"}, marker{103, renderTi, "E|4242"},
		marker{71, renderTi, "B|4242|drawLayer [g] 1080.0 x 2148.0"}, marker{72, renderTi, "E|4242"},
		marker{82, appPid, "B|4242|eglSwapBuffers"}, marker{82.5, appPid, "E|4242"},
	)
	parts = append(parts,
		ftracePacket(m),
		schedPacket(4, [][3]float64{{65, 0, renderTi}, {80, renderTi, 0}}, [][3]float64{{10, 4, 1_800_000}, {200, 4, 650_000}}),
		schedPacket(6, [][3]float64{{100, 0, renderTi}, {110, renderTi, 0}}, nil),
	)
	return pbMsg(parts...)
}

func TestReadPresentMeasuresWhatReachedTheGlass(t *testing.T) {
	pm, diags, err := ReadPresent(presentTrace(), PresentOptions{Package: "app.test"})
	if err != nil || len(diags) != 0 {
		t.Fatalf("err %v diags %+v", err, diags)
	}
	if !pm.FrameTimeline {
		t.Fatal("a trace with FrameTimeline packets must say so")
	}
	if pm.DisplayRate() == nil {
		t.Error("FrameTimeline presents carry their display-rate bins")
	}
	if !strings.Contains(pm.Layer, "MainActivity") || pm.Frames != 5 || pm.RefreshHz != 120 || pm.PeriodSource != "expected-display" {
		t.Fatalf("layer %q frames %d refresh %v (%s); the popup and the dropped frame stay out", pm.Layer, pm.Frames, pm.RefreshHz, pm.PeriodSource)
	}
	if pm.PresentGaps != (PresentGaps{OneVsync: 1, TwoVsync: 1, ThreeToFour: 1, Longer: 1}) {
		t.Errorf("gaps = %+v", pm.PresentGaps)
	}
	// Three gaps over 1.4 periods: 202 and 204 by neither thread, 203 by
	// the main thread's 12 ms doFrame (204's predecessor counts too).
	if pm.Drops != (Drops{Total: 3, Main: 2, Neither: 1}) {
		t.Errorf("drops = %+v", pm.Drops)
	}
	if pm.MainDoFrameMs.Count != 5 || val(t, "doFrame p50", pm.MainDoFrameMs.P50) != 2 || pm.MainEglSwapsPerFrame != 0.2 {
		t.Errorf("main = %+v, egl %v", pm.MainDoFrameMs, pm.MainEglSwapsPerFrame)
	}
	if pm.RTDrawMs.Count != 2 || val(t, "rt avg", pm.RTDrawMs.Avg) != 3 || len(pm.RTDrawByRect) != 2 || pm.RTDrawByRect[0].Rect != "0.00 0.00 1080.00 2400.00" {
		t.Errorf("rt = %+v by rect %+v", pm.RTDrawMs, pm.RTDrawByRect)
	}
	if len(pm.LayerDraws) != 1 || pm.LayerDraws[0].Count != 1 {
		t.Errorf("layer draws = %+v", pm.LayerDraws)
	}
	want := []RTCpu{{CPU: 4, MHz: 1800, Frames: 1, AvgDrawMs: 4}, {CPU: 6, MHz: 0, Frames: 1, AvgDrawMs: 2}}
	if len(pm.RTCpu) != 2 || pm.RTCpu[0] != want[0] || pm.RTCpu[1] != want[1] {
		t.Errorf("rtCpu = %+v", pm.RTCpu)
	}
	if val(t, "gpu share", pm.GPUCompositionShare) != 0.5 {
		t.Errorf("gpu share = %v", *pm.GPUCompositionShare)
	}
	if pm.RestFrames != 1 || pm.RestRunMs != 50 {
		t.Errorf("rest = %d frames, run %v ms; want the one frame after the 83 ms gap and the 50 ms opening run", pm.RestFrames, pm.RestRunMs)
	}
}

func TestReadPresentRefusesNonTraces(t *testing.T) {
	if _, _, err := ReadPresent([]byte("not a trace"), PresentOptions{Package: "app.test"}); err == nil || !strings.Contains(err.Error(), DiagNotATrace) {
		t.Errorf("err = %v", err)
	}
	if _, _, err := ReadPresent(presentTrace(), PresentOptions{Package: "app.absent"}); err == nil || !strings.Contains(err.Error(), DiagPackageNotInTrace) {
		t.Errorf("err = %v", err)
	}
}

// The S20 lab traces are too big for the repo (19 MB); this runs only with
// PERFLAB_FIXTURES_DIR pointing at a dir holding lab120-traces/.
func TestReadPresentOnTheS20LabTraces(t *testing.T) {
	dir := os.Getenv("PERFLAB_FIXTURES_DIR")
	if dir == "" {
		t.Skip("PERFLAB_FIXTURES_DIR unset")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "lab120-traces", "fixit-switch-before.pftrace"))
	if err != nil {
		t.Fatal(err)
	}
	pm, _, err := ReadPresent(raw, PresentOptions{Package: "app.fixit.client"})
	if err != nil {
		t.Fatal(err)
	}
	// The main window, never one of the four PopupWindow layers of the
	// mode menu; SurfaceFlinger's expected frames give 120 Hz.
	if !strings.Contains(pm.Layer, "app.fixit.client.MainActivity") || pm.RefreshHz != 120 || pm.Frames != 78 {
		t.Errorf("layer %q refresh %v frames %d", pm.Layer, pm.RefreshHz, pm.Frames)
	}
	if len(pm.LayerDraws) == 0 || !strings.Contains(pm.LayerDraws[0].Name, "PopupWindow") || len(pm.RTCpu) == 0 {
		t.Errorf("layer draws %+v, rtCpu %d rows", pm.LayerDraws, len(pm.RTCpu))
	}
}

// FrameTimeline packets of another process only are no evidence for the
// app: its present readings stay unread (frameTimeline false), never a
// passing zero.
func TestReadPresentWithOnlyAnotherProcessesSurfaces(t *testing.T) {
	// The app is pid 4242 (as `--pid` or process_tree names it); every
	// surface frame in the trace belongs to pid 9999.
	pm, diags, err := ReadPresent(presentTraceFor(9999), PresentOptions{Package: "app.test", PID: appPid})
	if err != nil {
		t.Fatal(err)
	}
	warned := false
	for _, d := range diags {
		warned = warned || d.Code == DiagNoFrameTimeline
	}
	if pm.FrameTimeline || pm.Frames != 0 || !warned {
		t.Fatalf("frameTimeline=%v frames=%d diags=%+v", pm.FrameTimeline, pm.Frames, diags)
	}
}

// A screen at rest presents nothing (the FixIt search tab: 20 s, no app
// surface frame, only the main thread's Choreographer tick). With
// FrameTimeline recording (display frames) and atrace reaching the app
// (its doFrames) and nothing in the app drawing, zero presents are a
// reading, so the rest budget can pass (a RenderThread DrawFrames that
// only synced draws nothing); one app Drawing without its own surface
// frames keeps them unread.
func TestReadPresentOfAScreenAtRest(t *testing.T) {
	trace := func(sync, draw bool) []byte {
		parts := [][]byte{
			timelinePacket(80, tlActualDisplay, pbVarint(1, 61), pbVarint(2, 900)),
			timelinePacket(88, tlActualDisplay, pbVarint(1, 62), pbVarint(2, 901)),
		}
		var m []marker
		for i := 0; i < 5; i++ {
			at := 100 + float64(i)*8.333
			m = append(m, marker{at, appPid, "B|4242|Choreographer#doFrame " + itoa(int64(200+i))}, marker{at + 0.8, appPid, "E|4242"})
		}
		if sync || draw {
			m = append(m, marker{101, renderTi, "B|4242|DrawFrames 200"}, marker{103, renderTi, "E|4242"})
		}
		if draw {
			m = append(m, marker{101.5, renderTi, "B|4242|Drawing 0.00 0.00 1080.00 2400.00"}, marker{102.5, renderTi, "E|4242"})
		}
		return pbMsg(append(parts, ftracePacket(m))...)
	}
	for _, c := range []struct {
		sync, draw bool
		timeline   bool
	}{{false, false, true}, {true, false, true}, {true, true, false}} {
		pm, diags, err := ReadPresent(trace(c.sync, c.draw), PresentOptions{Package: "app.test", PID: appPid, VsyncPeriodNs: 8_333_333})
		if err != nil {
			t.Fatal(err)
		}
		warned := false
		for _, d := range diags {
			warned = warned || d.Code == DiagNoFrameTimeline
		}
		if pm.FrameTimeline != c.timeline || warned == c.timeline || pm.Frames != 0 || pm.RestFrames != 0 || pm.RestRunMs != 0 {
			t.Errorf("sync=%v draw=%v: frameTimeline=%v (want %v) frames=%d rest=%d/%v diags=%+v", c.sync, c.draw, pm.FrameTimeline, c.timeline, pm.Frames, pm.RestFrames, pm.RestRunMs, diags)
		}
	}
}

// FixIt's customer home with a live search (2026-10-03): the comet drew
// for all 20 s, one run still going at the trace end, and the fix rests
// after one 2.9 s run then ticks the countdown once a second. A trace that
// never rests counts every frame, so the fix reads fewer rest frames, not
// more; a single run that stopped long before the end (the worker's
// waiting journey) rested.
func TestRestReadingOfATraceThatNeverRests(t *testing.T) {
	steady := func(n int, gap, from float64) []float64 {
		out := make([]float64, n)
		for i := range out {
			out[i] = from + float64(i)*gap
		}
		return out
	}
	ticks := append(steady(348, 8.333, 0), steady(15, 1000, 3900)...)
	for _, c := range []struct {
		name   string
		times  []float64
		endMs  float64
		frames int
	}{
		{"loops all trace", steady(2396, 8.333, 0), 19970, 2396},
		{"one run then a 1 Hz countdown", ticks, 20000, 15},
		{"one run then silence", steady(349, 8.333, 0), 20000, 0},
		{"a lone present", []float64{40}, 20000, 0},
		{"nothing", nil, 20000, 0},
	} {
		if got, _ := restReading(c.times, c.endMs); got != c.frames {
			t.Errorf("%s: %d rest frames, want %d", c.name, got, c.frames)
		}
	}
}
