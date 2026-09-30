package framestats

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/henderson-tech/vybava/internal/runx"
)

// TextureUpload is HWUI's per-upload slice name prefix; it is always counted.
const TextureUpload = "Texture upload"

const (
	doFramePrefix   = "Choreographer#doFrame"
	drawFramePrefix = "DrawFrame" // "DrawFrames <vsyncId>" on Android 12+
	inputSliceName  = "input"     // Choreographer's CALLBACK_INPUT section
	artPausePrefix  = "Mutator threads suspended for "
)

// TraceOptions tunes ReadTrace.
type TraceOptions struct {
	Package string
	// PID overrides the package lookup when > 0.
	PID int
	// Counts are slice-name substrings counted per frame, beside TextureUpload.
	Counts []string
	// Frames adds one TraceFrame per UI frame.
	Frames bool
}

// FrameStats summarises one thread's per-frame slice durations.
type FrameStats struct {
	Count      int `json:"count"`
	DragFrames int `json:"dragFrames"`
	P50Ms      Ms  `json:"p50Ms"`
	P90Ms      Ms  `json:"p90Ms"`
	P99Ms      Ms  `json:"p99Ms"`
	MaxMs      Ms  `json:"maxMs"`
	DragP50Ms  Ms  `json:"dragP50Ms"`
	DragP90Ms  Ms  `json:"dragP90Ms"`
	DragMaxMs  Ms  `json:"dragMaxMs"`
}

// SliceCount is one --count pattern's tally. A slice is "in a frame" when
// it nests in the frame's doFrame (main thread) or DrawFrames (RenderThread).
type SliceCount struct {
	Match           string `json:"match"`
	Total           int    `json:"total"`
	TotalMs         Ms     `json:"totalMs"`
	InFrames        int    `json:"inFrames"`
	InDragFrames    int    `json:"inDragFrames"`
	DragFramesWith  int    `json:"dragFramesWith"`
	PerDragFrameP50 Ms     `json:"perDragFrameP50"`
	PerDragFrameMax int    `json:"perDragFrameMax"`
}

// LargeUploadPixels splits HWUI texture uploads: glyph and icon uploads stay
// under 256 x 256 px, a rasterized clip mask or a layer is card-sized.
const LargeUploadPixels = 256 * 256

// UploadStats is the size-aware view of the TextureUpload slices in UI
// frames (the "WxH" at the end of each slice name).
type UploadStats struct {
	LargeMinPixels          int `json:"largeMinPixels"`
	Large                   int `json:"large"`
	LargeInDragFrames       int `json:"largeInDragFrames"`
	LargePerDragFrameP50    Ms  `json:"largePerDragFrameP50"`
	LargePerDragFrameMax    int `json:"largePerDragFrameMax"`
	UploadMsPerDragFrameP50 Ms  `json:"uploadMsPerDragFrameP50"`
}

// uploadPixels reads "Texture upload(207867) 1080x2016" as 1080*2016; 0
// when the name carries no size.
func uploadPixels(name string) int {
	sp := strings.LastIndexByte(name, ' ')
	w, h, ok := strings.Cut(name[sp+1:], "x")
	if !ok {
		return 0
	}
	wi, err1 := strconv.Atoi(w)
	hi, err2 := strconv.Atoi(h)
	if err1 != nil || err2 != nil {
		return 0
	}
	return wi * hi
}

// PauseCause is one ART suspend-all cause (GC, "Start" for a sampling
// profiler, ...).
type PauseCause struct {
	Cause   string `json:"cause"`
	Count   int    `json:"count"`
	TotalMs Ms     `json:"totalMs"`
	MaxMs   Ms     `json:"maxMs"`
}

// ARTPauses summarises the app's stop-the-world pauses ("Mutator threads
// suspended for <cause>").
type ARTPauses struct {
	Count            int          `json:"count"`
	TotalMs          Ms           `json:"totalMs"`
	MaxMs            Ms           `json:"maxMs"`
	DuringDragFrames int          `json:"duringDragFrames"`
	ByCause          []PauseCause `json:"byCause"`
}

// FrameTimeline summarises SurfaceFlinger's verdict on the app's frames.
type FrameTimeline struct {
	SurfaceFrames            int            `json:"surfaceFrames"`
	DragSurfaceFrames        int            `json:"dragSurfaceFrames"`
	PresentTypes             map[string]int `json:"presentTypes"`
	JankTypes                map[string]int `json:"jankTypes"`
	JankyFrames              int            `json:"jankyFrames"`
	DragJankyFrames          int            `json:"dragJankyFrames"`
	PresentIntervalP50Ms     Ms             `json:"presentIntervalP50Ms"`
	PresentIntervalP90Ms     Ms             `json:"presentIntervalP90Ms"`
	DragPresentIntervalP50Ms Ms             `json:"dragPresentIntervalP50Ms"`
	DragPresentIntervalP90Ms Ms             `json:"dragPresentIntervalP90Ms"`
}

// TraceFrame is one UI frame, for --frames.
type TraceFrame struct {
	// VsyncID is nil when the doFrame carries no id (NO_VSYNC_IDS).
	VsyncID  *int64         `json:"vsyncId"`
	StartMs  float64        `json:"startMs"`
	UIMs     Ms             `json:"uiMs"`
	RenderMs Ms             `json:"renderMs"`
	Drag     bool           `json:"drag"`
	Counts   map[string]int `json:"counts"`
}

// TraceSummary is `framestats perfetto`'s data.
type TraceSummary struct {
	Package         string        `json:"package"`
	PID             int           `json:"pid"`
	PIDSource       string        `json:"pidSource"`
	RenderThreadTID int           `json:"renderThreadTid"`
	SpanMs          Ms            `json:"spanMs"`
	UIFrames        FrameStats    `json:"uiFrames"`
	RenderFrames    FrameStats    `json:"renderFrames"`
	Counts          []SliceCount  `json:"counts"`
	Uploads         UploadStats   `json:"uploads"`
	ARTPauses       ARTPauses     `json:"artPauses"`
	FrameTimeline   FrameTimeline `json:"frameTimeline"`
	Frames          []TraceFrame  `json:"frames,omitempty"`
}

// ReadTraceFile reads a Perfetto protobuf trace from disk.
func ReadTraceFile(path string, opts TraceOptions) (TraceSummary, []runx.Diagnostic, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return TraceSummary{}, nil, diag(DiagFileUnreadable, fmt.Sprintf("cannot read %s: %v", path, err),
			"framestats perfetto <trace.pftrace> --package <pkg> --json")
	}
	return ReadTrace(raw, opts)
}

// frame is one UI frame keyed by vsync id, with its spans on both threads.
type frame struct {
	id int64
	// hasID is false for an id-less doFrame (Android < 12, see NO_VSYNC_IDS).
	hasID    bool
	ui       *slice
	renders  []slice
	drag     bool
	counts   map[string]int
	renderMs float64
	// uploadMs and largeUploads cover the frame's TextureUpload slices.
	uploadMs     float64
	largeUploads int
}

// ReadTrace walks the trace once, rebuilds the app's atrace slices and
// measures them per UI frame (vsync id). Warnings come back as diagnostics
// beside a usable summary; only an unusable input is an error.
func ReadTrace(raw []byte, opts TraceOptions) (TraceSummary, []runx.Diagnostic, error) {
	t, err := decodeTrace(raw)
	if t.packets == 0 {
		return TraceSummary{}, nil, diag(DiagNotATrace, "no TracePacket decoded; pass the binary .pftrace perfetto wrote, not a text or JSON export",
			"adb pull /data/misc/perfetto-traces/<name>.pftrace")
	}
	if err != nil {
		return TraceSummary{}, nil, diag(DiagTraceIncomplete,
			fmt.Sprintf("the protobuf stream breaks (%v): the file was pulled while perfetto was still writing, or is corrupt, so later frames are missing and nothing is measured", err),
			"adb pull /data/misc/perfetto-traces/<name>.pftrace")
	}
	s := TraceSummary{Package: opts.Package, PID: opts.PID, PIDSource: "flag"}
	if s.PID <= 0 {
		s.PID, s.PIDSource = resolvePID(t, opts.Package)
	}
	if s.PID <= 0 {
		return s, nil, diag(DiagPackageNotInTrace,
			fmt.Sprintf("no process_tree entry, FrameTimeline layer or main-thread comm names %q", opts.Package),
			fmt.Sprintf("framestats perfetto <trace.pftrace> --package %s --pid <pid from adb shell pidof %s> --json", opts.Package, opts.Package))
	}
	var diags []runx.Diagnostic
	slices := buildSlices(t.prints, s.PID)
	patterns := append([]string{TextureUpload}, opts.Counts...)
	patterns = dedupe(patterns)

	// UI frames: the main thread's outermost doFrame slices.
	byID := map[int64]*frame{}
	var order []*frame
	var inputs []slice
	var noIDs []*slice
	for i := range slices {
		sl := &slices[i]
		if sl.tid != s.PID {
			continue
		}
		if sl.name == inputSliceName {
			inputs = append(inputs, *sl)
			continue
		}
		if !strings.HasPrefix(sl.name, doFramePrefix) {
			continue
		}
		id, ok := trailingID(sl.name)
		if !ok {
			noIDs = append(noIDs, sl)
			continue
		}
		if f, dup := byID[id]; dup {
			if f.ui == nil || sl.end-sl.start > f.ui.end-f.ui.start {
				f.ui = sl
			}
			continue
		}
		f := &frame{id: id, hasID: true, ui: sl, counts: map[string]int{}}
		byID[id] = f
		order = append(order, f)
	}
	// vsyncIDs: the frames carry ids, so RenderThread work, per-frame slice
	// counts and FrameTimeline frames can be tied to them.
	vsyncIDs := len(order) > 0
	if !vsyncIDs {
		if len(noIDs) > 0 {
			// Android < 12: durations and drag frames need only time, so the
			// outermost id-less doFrames are still the UI frames.
			sort.SliceStable(noIDs, func(i, j int) bool {
				if noIDs[i].start != noIDs[j].start {
					return noIDs[i].start < noIDs[j].start
				}
				return noIDs[i].end > noIDs[j].end
			})
			for _, sl := range noIDs {
				if n := len(order); n > 0 && sl.end <= order[n-1].ui.end {
					continue // nested in the previous doFrame
				}
				order = append(order, &frame{ui: sl, counts: map[string]int{}})
			}
			diags = append(diags, warn(DiagNoVsyncIDs, fmt.Sprintf("%d doFrame slices carry no vsync id (Android < 12); UI durations are measured, RenderThread, per-frame count and FrameTimeline attribution need Android 12+", len(noIDs)), ""))
		} else {
			diags = append(diags, warn(DiagNoAppFrames, fmt.Sprintf("pid %d traced no Choreographer#doFrame slice", s.PID),
				"record with atrace_categories gfx, view and input plus atrace_apps <pkg>"))
		}
	}
	sort.Slice(order, func(i, j int) bool { return order[i].ui.start < order[j].ui.start })
	uiSpans := make([]span, len(order))
	for i, f := range order {
		uiSpans[i] = span{start: f.ui.start, end: f.ui.end, frame: f}
	}
	for _, in := range inputs {
		if f := enclosing(uiSpans, in.start, in.end); f != nil {
			f.drag = true
		}
	}

	// RenderThread: the app thread with the most DrawFrame(s) slices.
	drawCounts := map[int]int{}
	for _, sl := range slices {
		if strings.HasPrefix(sl.name, drawFramePrefix) {
			drawCounts[sl.tid]++
		}
	}
	for tid, n := range drawCounts {
		if s.RenderThreadTID == 0 || n > drawCounts[s.RenderThreadTID] || (n == drawCounts[s.RenderThreadTID] && tid < s.RenderThreadTID) {
			s.RenderThreadTID = tid
		}
	}
	var rtSpans []span
	if s.RenderThreadTID == 0 {
		diags = append(diags, warn(DiagNoRenderThread, "no app thread traced a DrawFrame(s) slice", "record with atrace_categories gfx and view"))
	}
	for i := range slices {
		sl := &slices[i]
		if sl.tid != s.RenderThreadTID || s.RenderThreadTID == 0 || !strings.HasPrefix(sl.name, drawFramePrefix) {
			continue
		}
		id, ok := trailingID(sl.name)
		f := byID[id]
		if ok && f != nil {
			f.renders = append(f.renders, *sl)
			f.renderMs += nsToMs(sl.end - sl.start)
		}
		rtSpans = append(rtSpans, span{start: sl.start, end: sl.end, frame: f})
	}

	// Pattern counts, per frame by nesting, and ART pauses.
	totals := make([]int, len(patterns))
	totalMs := make([]float64, len(patterns))
	inFrames := make([]int, len(patterns))
	causes := map[string]*pauseAcc{}
	var pauseMs []float64
	dragSpans := spansWhere(uiSpans, func(f *frame) bool { return f.drag })
	for _, sl := range slices {
		for k, p := range patterns {
			if !strings.Contains(sl.name, p) {
				continue
			}
			totals[k]++
			totalMs[k] += nsToMs(sl.end - sl.start)
			var f *frame
			switch {
			case !vsyncIDs:
				// Without ids the RenderThread half cannot be attributed, so
				// neither half is: a per-frame count of 0 would pass a budget.
			case sl.tid == s.PID:
				f = enclosing(uiSpans, sl.start, sl.end)
			case sl.tid == s.RenderThreadTID:
				f = enclosing(rtSpans, sl.start, sl.end)
			}
			if f != nil {
				inFrames[k]++
				f.counts[p]++
				if p == TextureUpload {
					f.uploadMs += nsToMs(sl.end - sl.start)
					if uploadPixels(sl.name) >= LargeUploadPixels {
						f.largeUploads++
					}
				}
			}
		}
		if strings.HasPrefix(sl.name, artPausePrefix) {
			d := nsToMs(sl.end - sl.start)
			pauseMs = append(pauseMs, d)
			cause := strings.TrimPrefix(sl.name, artPausePrefix)
			acc := causes[cause]
			if acc == nil {
				acc = &pauseAcc{}
				causes[cause] = acc
			}
			acc.values = append(acc.values, d)
			if overlaps(dragSpans, sl.start, sl.end) {
				s.ARTPauses.DuringDragFrames++
			}
		}
	}
	s.ARTPauses.Count = len(pauseMs)
	s.ARTPauses.TotalMs = ms(sum(pauseMs))
	s.ARTPauses.MaxMs = pctMs(pauseMs, 100)
	s.ARTPauses.ByCause = []PauseCause{}
	for cause, acc := range causes {
		s.ARTPauses.ByCause = append(s.ARTPauses.ByCause, PauseCause{Cause: cause, Count: len(acc.values), TotalMs: ms(sum(acc.values)), MaxMs: pctMs(acc.values, 100)})
	}
	sort.Slice(s.ARTPauses.ByCause, func(i, j int) bool {
		a, b := s.ARTPauses.ByCause[i], s.ARTPauses.ByCause[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Cause < b.Cause
	})

	// Frame durations and the per-drag-frame count distribution.
	var ui, uiDrag, rt, rtDrag []float64
	for _, sp := range rtSpans {
		rt = append(rt, nsToMs(sp.end-sp.start))
	}
	dragFrames := 0
	perDrag := make([][]float64, len(patterns))
	var largePerDrag, uploadMsPerDrag []float64
	s.Uploads.LargeMinPixels = LargeUploadPixels
	for _, f := range order {
		d := nsToMs(f.ui.end - f.ui.start)
		ui = append(ui, d)
		if !f.drag {
			continue
		}
		dragFrames++
		uiDrag = append(uiDrag, d)
		if !f.hasID {
			continue // its per-frame counts were never attributed
		}
		if len(f.renders) > 0 {
			rtDrag = append(rtDrag, f.renderMs)
		}
		for k, p := range patterns {
			perDrag[k] = append(perDrag[k], float64(f.counts[p]))
		}
		largePerDrag = append(largePerDrag, float64(f.largeUploads))
		uploadMsPerDrag = append(uploadMsPerDrag, f.uploadMs)
		s.Uploads.LargeInDragFrames += f.largeUploads
		if f.largeUploads > s.Uploads.LargePerDragFrameMax {
			s.Uploads.LargePerDragFrameMax = f.largeUploads
		}
	}
	for _, f := range order {
		s.Uploads.Large += f.largeUploads
	}
	s.Uploads.LargePerDragFrameP50 = pctMs(largePerDrag, 50)
	s.Uploads.UploadMsPerDragFrameP50 = pctMs(uploadMsPerDrag, 50)
	s.UIFrames = frameStats(ui, uiDrag, dragFrames)
	// Every DrawFrames counts (an off-window draw such as a replay encoder's
	// `DrawFrames -1` is RenderThread time too); drag ones are matched by id.
	s.RenderFrames = frameStats(rt, rtDrag, len(rtDrag))
	s.Counts = make([]SliceCount, len(patterns))
	for k, p := range patterns {
		c := SliceCount{Match: p, Total: totals[k], InFrames: inFrames[k], PerDragFrameP50: pctMs(perDrag[k], 50)}
		if totals[k] > 0 {
			c.TotalMs = ms(totalMs[k])
		}
		for _, n := range perDrag[k] {
			c.InDragFrames += int(n)
			if n > 0 {
				c.DragFramesWith++
			}
			if int(n) > c.PerDragFrameMax {
				c.PerDragFrameMax = int(n)
			}
		}
		s.Counts[k] = c
	}
	if len(order) > 0 {
		last := order[len(order)-1].ui.end
		for _, f := range order {
			if f.ui.end > last {
				last = f.ui.end
			}
		}
		s.SpanMs = ms(nsToMs(last - order[0].ui.start))
	}

	var ftDiag *runx.Diagnostic
	s.FrameTimeline, ftDiag = summarizeTimeline(t.timeline, s.PID, byID)
	if ftDiag != nil {
		diags = append(diags, *ftDiag)
	}

	if opts.Frames && len(order) > 0 {
		t0 := order[0].ui.start
		s.Frames = make([]TraceFrame, len(order))
		for i, f := range order {
			tf := TraceFrame{StartMs: nsRound(f.ui.start - t0), UIMs: ms(nsToMs(f.ui.end - f.ui.start)), Drag: f.drag, Counts: f.counts}
			if f.hasID {
				tf.VsyncID = &f.id
			}
			if len(f.renders) > 0 {
				tf.RenderMs = ms(f.renderMs)
			}
			s.Frames[i] = tf
		}
	}
	return s, diags, nil
}

type pauseAcc struct{ values []float64 }

// span is a time range owned by a frame (nil for an unmatched DrawFrames).
type span struct {
	start, end int64
	frame      *frame
}

// enclosing is the frame of the last span starting at or before start that
// also ends at or after end; spans are sorted by start and do not nest.
func enclosing(spans []span, start, end int64) *frame {
	i := sort.Search(len(spans), func(i int) bool { return spans[i].start > start }) - 1
	if i < 0 || spans[i].end < end {
		return nil
	}
	return spans[i].frame
}

// overlaps reports whether [start, end] intersects any span. Spans are
// sorted and disjoint, so only the last one starting at or before end can.
func overlaps(spans []span, start, end int64) bool {
	i := sort.Search(len(spans), func(i int) bool { return spans[i].start > end }) - 1
	return i >= 0 && spans[i].end >= start
}

func spansWhere(spans []span, keep func(*frame) bool) []span {
	var out []span
	for _, sp := range spans {
		if sp.frame != nil && keep(sp.frame) {
			out = append(out, sp)
		}
	}
	return out
}

func frameStats(all, drag []float64, dragFrames int) FrameStats {
	return FrameStats{
		Count: len(all), DragFrames: dragFrames,
		P50Ms: pctMs(all, 50), P90Ms: pctMs(all, 90), P99Ms: pctMs(all, 99), MaxMs: pctMs(all, 100),
		DragP50Ms: pctMs(drag, 50), DragP90Ms: pctMs(drag, 90), DragMaxMs: pctMs(drag, 100),
	}
}

// trailingID parses the vsync id after the last space ("DrawFrames 31017048").
func trailingID(name string) (int64, bool) {
	sp := strings.LastIndexByte(name, ' ')
	if sp < 0 {
		return 0, false
	}
	id, err := strconv.ParseInt(name[sp+1:], 10, 64)
	return id, err == nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// sum is the total of values, NaN (null through ms) when there are none.
func sum(values []float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	total := 0.0
	for _, v := range values {
		total += v
	}
	return total
}

func nsRound(ns int64) float64 {
	v := ms(nsToMs(ns))
	if v == nil {
		return 0
	}
	return *v
}

// Present and jank vocabularies of perfetto's FrameTimelineEvent.
var presentTypeNames = map[int]string{0: "UNSPECIFIED", 1: "ON_TIME", 2: "LATE", 3: "EARLY", 4: "DROPPED", 5: "UNKNOWN"}

var jankBits = []struct {
	bit  int
	name string
}{
	{1, "NONE"}, {2, "SF_SCHEDULING"}, {4, "PREDICTION_ERROR"}, {8, "DISPLAY_HAL"},
	{16, "SF_CPU_DEADLINE_MISSED"}, {32, "SF_GPU_DEADLINE_MISSED"}, {64, "APP_DEADLINE_MISSED"},
	{128, "BUFFER_STUFFING"}, {256, "UNKNOWN"}, {512, "SF_STUFFING"}, {1024, "DROPPED"},
}

// summarizeTimeline reads the app's actual surface frames: present and jank
// types, and the present-to-present interval, a present being the end of
// the actual display frame the surface frame landed in.
func summarizeTimeline(events []timelineEvent, pid int, byID map[int64]*frame) (FrameTimeline, *runx.Diagnostic) {
	ft := FrameTimeline{PresentTypes: map[string]int{}, JankTypes: map[string]int{}}
	starts := map[int64]timelineEvent{}
	displayEnd := map[int64]int64{}
	var surfaces []timelineEvent
	for _, e := range events {
		switch e.kind {
		case tlActualDisplay:
			starts[e.cookie] = e
		case tlActualSurface:
			if e.pid == pid {
				surfaces = append(surfaces, e)
			}
		case tlFrameEnd:
			if start, ok := starts[e.cookie]; ok && start.kind == tlActualDisplay {
				displayEnd[start.token] = e.ts
			}
		}
	}
	if len(surfaces) == 0 {
		d := warn(DiagNoFrameTimeline, fmt.Sprintf("no FrameTimeline surface frame for pid %d", pid),
			`add data_sources { config { name: "android.surfaceflinger.frametimeline" } } to the trace config`)
		return ft, &d
	}
	type present struct {
		at   int64
		drag bool
	}
	presentAt := map[int64]present{}
	for _, e := range surfaces {
		ft.SurfaceFrames++
		f := byID[e.token]
		drag := f != nil && f.drag
		if drag {
			ft.DragSurfaceFrames++
		}
		name, ok := presentTypeNames[e.presentType]
		if !ok {
			name = "OTHER"
		}
		ft.PresentTypes[name]++
		janky := false
		if e.jankType == 0 {
			ft.JankTypes["UNSPECIFIED"]++
		}
		for _, jb := range jankBits {
			if e.jankType&jb.bit != 0 {
				ft.JankTypes[jb.name]++
				if jb.bit != 1 {
					janky = true
				}
			}
		}
		if janky {
			ft.JankyFrames++
			if drag {
				ft.DragJankyFrames++
			}
		}
		if e.presentType == 4 {
			continue
		}
		if at, ok := displayEnd[e.displayToken]; ok {
			p := presentAt[e.displayToken]
			presentAt[e.displayToken] = present{at: at, drag: p.drag || drag}
		}
	}
	presents := make([]present, 0, len(presentAt))
	for _, p := range presentAt {
		presents = append(presents, p)
	}
	sort.Slice(presents, func(i, j int) bool { return presents[i].at < presents[j].at })
	var all, drag []float64
	for i := 1; i < len(presents); i++ {
		d := nsToMs(presents[i].at - presents[i-1].at)
		if d <= 0 || d > cadenceGapMs {
			continue
		}
		all = append(all, d)
		if presents[i].drag {
			drag = append(drag, d)
		}
	}
	ft.PresentIntervalP50Ms, ft.PresentIntervalP90Ms = pctMs(all, 50), pctMs(all, 90)
	ft.DragPresentIntervalP50Ms, ft.DragPresentIntervalP90Ms = pctMs(drag, 50), pctMs(drag, 90)
	return ft, nil
}
