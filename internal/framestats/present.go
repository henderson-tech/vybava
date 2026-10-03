package framestats

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/runx"
)

// The present reading of a Perfetto trace: what reached the glass and what
// held each late frame up. It ports the PR #1770 kit's SQL (present, perframe,
// rtfreq, layer, egl-anc) and drops.ts onto the native decoder, so `perflab
// probe` reads a trace without trace_processor_shell.
const (
	rtDrawingPrefix = "Drawing "
	drawLayerPrefix = "drawLayer"
	eglSwapPrefix   = "eglSwapBuffers"
	// DropVsyncs: a present gap above this many periods is a dropped frame.
	DropVsyncs = 1.4
	// presentTypeDropped: a surface frame SurfaceFlinger never presented.
	presentTypeDropped = 4
)

// PresentOptions tunes ReadPresent.
type PresentOptions struct {
	Package string
	// PID overrides the package lookup when > 0.
	PID int
	// VsyncPeriodNs is the display period (`dumpsys SurfaceFlinger
	// --latency`); 0 derives it from the presents.
	VsyncPeriodNs int64
	// Layer keeps the app layers whose name contains it; empty keeps the
	// layer with the most frames (the main window, never a popup).
	Layer string
}

// PresentGaps buckets consecutive app presents by vsync periods: one, two,
// three to four, longer (rest included).
type PresentGaps struct {
	OneVsync    int `json:"oneVsync"`
	TwoVsync    int `json:"twoVsync"`
	ThreeToFour int `json:"threeToFour"`
	Longer      int `json:"longer"`
}

// DurationStats are slice durations, ms (nil without samples).
type DurationStats struct {
	Count int `json:"count"`
	Avg   Ms  `json:"avg"`
	P50   Ms  `json:"p50"`
	P90   Ms  `json:"p90"`
	P99   Ms  `json:"p99"`
}

// Drops blames each dropped frame on the thread that ran over a period on
// it or the frame before: main (doFrame), rt (DrawFrames), both, neither.
type Drops struct {
	Total   int `json:"total"`
	Main    int `json:"main"`
	RT      int `json:"rt"`
	Both    int `json:"both"`
	Neither int `json:"neither"`
}

// LayerDraw counts a RenderThread drawLayer slice name (one per hardware
// layer redraw).
type LayerDraw struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// RectDraw is the RenderThread Drawing slices of one damage rect.
type RectDraw struct {
	Rect  string  `json:"rect"`
	Count int     `json:"count"`
	AvgMs float64 `json:"avgMs"`
}

// RTCpu is RenderThread drawing placed on a CPU at a clock.
type RTCpu struct {
	CPU       int     `json:"cpu"`
	MHz       int64   `json:"mhz"`
	Frames    int     `json:"frames"`
	AvgDrawMs float64 `json:"avgDrawMs"`
}

// PresentMetrics is `perflab probe`'s reading of one trace.
type PresentMetrics struct {
	Package       string  `json:"package"`
	PID           int     `json:"pid"`
	Layer         string  `json:"layer"`
	RefreshHz     float64 `json:"refreshHz"`
	VsyncPeriodMs float64 `json:"vsyncPeriodMs"`
	PeriodSource  string  `json:"periodSource"`
	// Frames are the app's presents on the layer; SpanMs first to last.
	Frames int `json:"frames"`
	SpanMs Ms  `json:"spanMs"`
	// RestFrames are presents after the first rest gap (the opening run
	// settled); RestRunMs is the longest run of presents with no rest gap.
	RestFrames    int           `json:"restFrames"`
	RestRunMs     float64       `json:"restRunMs"`
	PresentGaps   PresentGaps   `json:"presentGaps"`
	RTDrawMs      DurationStats `json:"rtDrawMs"`
	MainDoFrameMs DurationStats `json:"mainDoFrameMs"`
	Drops         Drops         `json:"drops"`
	// MainEglSwapsPerFrame: main-thread eglSwapBuffers slices per doFrame;
	// above zero at rest a Skia/GL canvas keeps redrawing.
	MainEglSwapsPerFrame float64     `json:"mainEglSwapsPerFrame"`
	LayerDraws           []LayerDraw `json:"layerDraws"`
	// RTDrawByRect splits the Drawing slices by their damage rect (the
	// whole window, a popup, a partial redraw), heaviest first.
	RTDrawByRect []RectDraw `json:"rtDrawByRect"`
	RTCpu        []RTCpu    `json:"rtCpu"`
	// GPUCompositionShare: SurfaceFlinger display frames composited on the
	// GPU, nil without FrameTimeline display frames.
	GPUCompositionShare Ms `json:"gpuCompositionShare"`
	// FrameTimeline: the trace carries FrameTimeline surface frames of the
	// app's pid, or proves the app presented nothing (display frames, the
	// app's doFrames, no Drawing: a screen at rest reads zero). Otherwise
	// (no FrameTimeline at all, or only another process's) Frames,
	// RestFrames, RestRunMs, PresentGaps and Drops are unread, not zero: a
	// budget never passes on them (NO_FRAME_TIMELINE).
	FrameTimeline bool `json:"frameTimeline"`
	// display is the display-rate reading of the same presents (DisplayRate).
	display *DisplayMetrics
}

// DisplayRate is the presents' display-rate bins (SummarizePresents), nil
// without the app's FrameTimeline surface frames: unread, never zero.
func (m PresentMetrics) DisplayRate() *DisplayMetrics { return m.display }

// ReadPresent measures the app's presents, its per-frame thread work and
// the RenderThread's CPU placement. Warnings come back beside a usable
// reading; an unusable input is an error.
func ReadPresent(raw []byte, opts PresentOptions) (PresentMetrics, []runx.Diagnostic, error) {
	t, err := decodeTrace(raw)
	if t.packets == 0 {
		return PresentMetrics{}, nil, diag(DiagNotATrace, "no TracePacket decoded; pass the binary .pftrace perfetto wrote, not a text or JSON export",
			"adb pull /data/misc/perfetto-traces/<name>.pftrace")
	}
	if err != nil {
		return PresentMetrics{}, nil, diag(DiagTraceIncomplete,
			fmt.Sprintf("the protobuf stream breaks (%v): the file was pulled while perfetto was still writing, or is corrupt", err),
			"adb pull /data/misc/perfetto-traces/<name>.pftrace")
	}
	m := PresentMetrics{Package: opts.Package, PID: opts.PID, LayerDraws: []LayerDraw{}, RTDrawByRect: []RectDraw{}, RTCpu: []RTCpu{}}
	if m.PID <= 0 {
		m.PID, _ = resolvePID(t, opts.Package)
	}
	if m.PID <= 0 {
		return m, nil, diag(DiagPackageNotInTrace,
			fmt.Sprintf("no process_tree entry, FrameTimeline layer or main-thread comm names %q", opts.Package),
			fmt.Sprintf("perflab analyze <trace.pftrace> --package %s --pid <pid from adb shell pidof %s>", opts.Package, opts.Package))
	}
	var diags []runx.Diagnostic

	// Presents: the app's actual surface frames on one layer, each ending
	// at its own FrameEnd (actual_frame_timeline_slice ts + dur).
	ends := map[int64]int64{}
	starts := map[int64]timelineEvent{}
	expected := map[int64]int64{} // cookie -> expected display frame start
	var displayFrames, gpuComposed int
	for _, e := range t.timeline {
		switch e.kind {
		case tlActualSurface:
			if e.pid == m.PID {
				starts[e.cookie] = e
			}
		case tlExpectedDisplay:
			expected[e.cookie] = e.ts
		case tlActualDisplay:
			displayFrames++
			if e.gpuComposition {
				gpuComposed++
			}
		case tlFrameEnd:
			// A packet whose timestamp is not on the trace clock (seen as a
			// negative int64 on an S20) places no present.
			if e.ts > 0 {
				ends[e.cookie] = e.ts
			}
		}
	}
	m.FrameTimeline = len(starts) > 0
	if displayFrames > 0 {
		m.GPUCompositionShare = ms(float64(gpuComposed) / float64(displayFrames))
	}
	layers := map[string]int{}
	for _, e := range starts {
		layers[e.layer]++
	}
	m.Layer = pickLayer(layers, opts.Layer)
	type present struct {
		at    int64
		token int64
	}
	var presents []present
	for cookie, e := range starts {
		end, ok := ends[cookie]
		if !ok || e.layer != m.Layer || e.presentType == presentTypeDropped {
			continue
		}
		presents = append(presents, present{at: end, token: e.token})
	}
	sort.Slice(presents, func(i, j int) bool { return presents[i].at < presents[j].at })
	m.Frames = len(presents)
	gaps := make([]float64, 0, len(presents))
	for i := 1; i < len(presents); i++ {
		gaps = append(gaps, nsToMs(presents[i].at-presents[i-1].at))
	}
	if len(presents) > 1 {
		m.SpanMs = ms(nsToMs(presents[len(presents)-1].at - presents[0].at))
	}
	period := float64(opts.VsyncPeriodNs) / 1e6
	m.PeriodSource = "flag"
	if period <= 0 {
		period, m.PeriodSource = derivePeriod(expectedPeriods(expected, ends), gaps)
	}
	m.VsyncPeriodMs = math.Round(period*1000) / 1000
	m.RefreshHz = round1(1000 / period)
	m.PresentGaps = bucketGaps(gaps, period)
	times := presentTimes(presents, func(p present) int64 { return p.at })
	m.RestFrames, m.RestRunMs = restReading(times)
	if m.FrameTimeline && len(times) > 1 {
		d := SummarizePresents(times, period)
		m.display = &d
	}

	// Per-frame thread work.
	slices := buildSlices(t.prints, m.PID)
	rt := renderThread(slices)
	mainByID := map[int64]float64{}
	rtByID := map[int64]float64{}
	var doFrames, rtDraws []float64
	eglSwaps := 0
	layerCounts := map[string]int{}
	var drawings []slice
	for _, sl := range slices {
		d := nsToMs(sl.end - sl.start)
		switch {
		case sl.tid == m.PID && strings.HasPrefix(sl.name, doFramePrefix):
			if id, ok := trailingID(sl.name); ok {
				mainByID[id] = math.Max(mainByID[id], d)
			} else if sl.depth == 0 {
				doFrames = append(doFrames, d)
			}
		case sl.tid == m.PID && strings.HasPrefix(sl.name, eglSwapPrefix):
			eglSwaps++
		case rt != 0 && sl.tid == rt && strings.HasPrefix(sl.name, drawFramePrefix):
			if id, ok := trailingID(sl.name); ok {
				rtByID[id] += d
			}
		case rt != 0 && sl.tid == rt && strings.HasPrefix(sl.name, rtDrawingPrefix):
			rtDraws = append(rtDraws, d)
			drawings = append(drawings, sl)
		case rt != 0 && sl.tid == rt && strings.HasPrefix(sl.name, drawLayerPrefix):
			layerCounts[sl.name]++
		}
	}
	for _, d := range mainByID {
		doFrames = append(doFrames, d)
	}
	m.MainDoFrameMs = durationStats(doFrames)
	m.RTDrawMs = durationStats(rtDraws)
	// A screen at rest presents nothing, which is a reading of zero, not
	// missing evidence, when the trace proves it: FrameTimeline recorded
	// (SurfaceFlinger display frames), the app's main thread traced its
	// frames (atrace reaches the app) and nothing in the app drew (no
	// RenderThread Drawing, no main-thread GL swap; a DrawFrames that only
	// synced, as on an S20 at rest, queues no buffer). Any app draw without
	// its own surface frames stays unread.
	idle := len(starts) == 0 && displayFrames > 0 && len(doFrames) > 0 && len(rtDraws) == 0 && eglSwaps == 0
	if idle {
		m.FrameTimeline = true
	}
	if !m.FrameTimeline {
		diags = append(diags, warn(DiagNoFrameTimeline, fmt.Sprintf("no FrameTimeline surface frame for pid %d", m.PID),
			`add data_sources { config { name: "android.surfaceflinger.frametimeline" } } to the trace config`))
	}
	if rt == 0 && !idle {
		diags = append(diags, warn(DiagNoRenderThread, "no app thread traced a DrawFrame(s) slice", "record with atrace_categories gfx and view"))
	}
	if len(doFrames) > 0 {
		m.MainEglSwapsPerFrame = math.Round(100*float64(eglSwaps)/float64(len(doFrames))) / 100
	}
	for name, n := range layerCounts {
		m.LayerDraws = append(m.LayerDraws, LayerDraw{Name: name, Count: n})
	}
	sort.Slice(m.LayerDraws, func(i, j int) bool {
		if m.LayerDraws[i].Count != m.LayerDraws[j].Count {
			return m.LayerDraws[i].Count > m.LayerDraws[j].Count
		}
		return m.LayerDraws[i].Name < m.LayerDraws[j].Name
	})

	// Drops (drops.ts): a present gap over DropVsyncs periods, blamed on
	// the thread that ran over a period on that frame or the one before.
	for i := 1; i < len(presents); i++ {
		if nsToMs(presents[i].at-presents[i-1].at) < DropVsyncs*period {
			continue
		}
		m.Drops.Total++
		main := math.Max(mainByID[presents[i].token], mainByID[presents[i-1].token])
		render := math.Max(rtByID[presents[i].token], rtByID[presents[i-1].token])
		switch {
		case main > period && render > period:
			m.Drops.Both++
		case main > period:
			m.Drops.Main++
		case render > period:
			m.Drops.RT++
		default:
			m.Drops.Neither++
		}
	}
	m.RTDrawByRect = byRect(drawings)
	m.RTCpu = placeOnCPUs(drawings, rt, t.switches, t.freqs)
	return m, diags, nil
}

// pickLayer keeps the layer naming want, else the one with the most frames.
func pickLayer(layers map[string]int, want string) string {
	best, bestN := "", -1
	for name, n := range layers {
		if want != "" && !strings.Contains(name, want) {
			continue
		}
		if n > bestN || (n == bestN && name < best) {
			best, bestN = name, n
		}
	}
	return best
}

// expectedPeriods are SurfaceFlinger's expected display frame durations: one
// vsync period each (8.333 ms on a 120 Hz S20).
func expectedPeriods(expected, ends map[int64]int64) []float64 {
	var out []float64
	for cookie, start := range expected {
		if end, ok := ends[cookie]; ok && end > start {
			out = append(out, nsToMs(end-start))
		}
	}
	return out
}

// derivePeriod is the median expected display frame, else the fastest
// common present cadence (the 10th percentile of gaps under RestGapMs),
// else 60 Hz. Presents alone read fast: their ends jitter under a period.
func derivePeriod(expected, gaps []float64) (float64, string) {
	if len(expected) > 0 {
		return quantileOr0(expected, 0.5), "expected-display"
	}
	var short []float64
	for _, g := range gaps {
		if g > 0 && g <= RestGapMs {
			short = append(short, g)
		}
	}
	if len(short) < AnimatingMinIntervals {
		return 1000.0 / 60, "default-60hz"
	}
	return quantileOr0(short, 0.1), "presents"
}

func bucketGaps(gaps []float64, period float64) PresentGaps {
	var g PresentGaps
	for _, gap := range gaps {
		switch r := gap / period; {
		case r < 1.5:
			g.OneVsync++
		case r < 2.5:
			g.TwoVsync++
		case r < 4.5:
			g.ThreeToFour++
		default:
			g.Longer++
		}
	}
	return g
}

func presentTimes[T any](items []T, at func(T) int64) []float64 {
	out := make([]float64, len(items))
	for i, it := range items {
		out[i] = nsToMs(at(it))
	}
	return out
}

// restReading: the frames presented after the first gap longer than
// RestGapMs (the opening run settled), and the longest run of presents
// without such a gap, first to last present.
func restReading(times []float64) (int, float64) {
	if len(times) == 0 {
		return 0, 0
	}
	restFrames, longest := 0, 0.0
	settled := false
	runStart := times[0]
	for i := 1; i < len(times); i++ {
		if times[i]-times[i-1] > RestGapMs {
			longest = math.Max(longest, times[i-1]-runStart)
			runStart = times[i]
			settled = true
		}
		if settled {
			restFrames++
		}
	}
	longest = math.Max(longest, times[len(times)-1]-runStart)
	return restFrames, round1(longest)
}

// renderThread is the app thread with the most DrawFrame(s) slices.
func renderThread(slices []slice) int {
	counts := map[int]int{}
	for _, sl := range slices {
		if strings.HasPrefix(sl.name, drawFramePrefix) {
			counts[sl.tid]++
		}
	}
	best := 0
	for tid, n := range counts {
		if best == 0 || n > counts[best] || (n == counts[best] && tid < best) {
			best = tid
		}
	}
	return best
}

func durationStats(values []float64) DurationStats {
	s := DurationStats{Count: len(values), P50: pctMs(values, 50), P90: pctMs(values, 90), P99: pctMs(values, 99)}
	if len(values) > 0 {
		s.Avg = ms(sum(values) / float64(len(values)))
	}
	return s
}

type cpuRun struct {
	start, end int64
	cpu        int
}

// placeOnCPUs puts each RenderThread drawing on the CPU the thread ran on
// at its start, at that CPU's last cpu_frequency (0 MHz when the CPU reported
// none), grouped by CPU and clock.
func placeOnCPUs(drawings []slice, rt int, switches []schedSwitch, freqs []cpuFreq) []RTCpu {
	out := []RTCpu{}
	if rt == 0 || len(drawings) == 0 || len(switches) == 0 {
		return out
	}
	var runs []cpuRun
	open := map[int]int64{} // cpu -> since, while rt runs there
	for _, sw := range switches {
		if since, ok := open[sw.cpu]; ok && sw.prevPid == rt {
			runs = append(runs, cpuRun{start: since, end: sw.ts, cpu: sw.cpu})
			delete(open, sw.cpu)
		}
		if sw.nextPid == rt {
			open[sw.cpu] = sw.ts
		}
	}
	byCPU := map[int][]cpuFreq{}
	for _, f := range freqs {
		byCPU[f.cpu] = append(byCPU[f.cpu], f)
	}
	type key struct {
		cpu int
		mhz int64
	}
	acc := map[key]*RTCpu{}
	durs := map[key]float64{}
	for _, d := range drawings {
		// One thread runs on one CPU at a time, so its runs, closed in
		// end order, are also in start order.
		cpu := -1
		if i := sort.Search(len(runs), func(i int) bool { return runs[i].end > d.start }); i < len(runs) && runs[i].start <= d.start {
			cpu = runs[i].cpu
		}
		if cpu < 0 {
			continue
		}
		k := key{cpu: cpu, mhz: freqAt(byCPU, cpu, d.start) / 1000}
		if acc[k] == nil {
			acc[k] = &RTCpu{CPU: k.cpu, MHz: k.mhz}
		}
		acc[k].Frames++
		durs[k] += nsToMs(d.end - d.start)
	}
	for k, a := range acc {
		a.AvgDrawMs = math.Round(100*durs[k]/float64(a.Frames)) / 100
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Frames != out[j].Frames {
			return out[i].Frames > out[j].Frames
		}
		if out[i].CPU != out[j].CPU {
			return out[i].CPU < out[j].CPU
		}
		return out[i].MHz < out[j].MHz
	})
	return out
}

func freqAt(byCPU map[int][]cpuFreq, cpu int, ts int64) int64 {
	fs := byCPU[cpu]
	i := sort.Search(len(fs), func(i int) bool { return fs[i].ts > ts })
	if i == 0 {
		return 0
	}
	return fs[i-1].khz
}

// byRect groups the Drawing slices by their damage rect, heaviest total
// first, the top eight.
func byRect(drawings []slice) []RectDraw {
	type acc struct {
		n  int
		ms float64
	}
	m := map[string]*acc{}
	for _, d := range drawings {
		rect := strings.Join(strings.Fields(strings.TrimPrefix(d.name, rtDrawingPrefix)), " ")
		if m[rect] == nil {
			m[rect] = &acc{}
		}
		m[rect].n++
		m[rect].ms += nsToMs(d.end - d.start)
	}
	out := make([]RectDraw, 0, len(m))
	for rect, a := range m {
		out = append(out, RectDraw{Rect: rect, Count: a.n, AvgMs: math.Round(100*a.ms/float64(a.n)) / 100})
	}
	sort.Slice(out, func(i, j int) bool {
		ti, tj := out[i].AvgMs*float64(out[i].Count), out[j].AvgMs*float64(out[j].Count)
		if ti != tj {
			return ti > tj
		}
		return out[i].Rect < out[j].Rect
	})
	if len(out) > 8 {
		out = out[:8]
	}
	return out
}
