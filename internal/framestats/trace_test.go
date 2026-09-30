package framestats

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/henderson-tech/vybava/internal/runx"
)

// A minimal protobuf writer for synthetic traces.

func pbKey(num, wt int) []byte { return binary.AppendUvarint(nil, uint64(num<<3|wt)) }

func pbVarint(num int, v int64) []byte {
	return append(pbKey(num, 0), binary.AppendUvarint(nil, uint64(v))...)
}

func pbBytes(num int, data []byte) []byte {
	out := append(pbKey(num, 2), binary.AppendUvarint(nil, uint64(len(data)))...)
	return append(out, data...)
}

func pbMsg(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

const (
	appPid   = 4242
	renderTi = 4260
	gcTid    = 4250
	otherPid = 999
)

// marker is one atrace print at tsMs on tid.
type marker struct {
	tsMs float64
	tid  int
	buf  string
}

func ns(msValue float64) int64 { return int64(msValue * 1e6) }

func ftracePacket(markers []marker) []byte {
	var events [][]byte
	for _, m := range markers {
		print := pbMsg(pbBytes(2, []byte(m.buf+"\n")))
		events = append(events, pbBytes(2, pbMsg(pbVarint(1, ns(m.tsMs)), pbVarint(2, int64(m.tid)), pbBytes(3, print))))
	}
	bundle := pbMsg(append([][]byte{pbVarint(1, 0)}, events...)...)
	return pbBytes(fTracePacket, pbBytes(fFtraceBundle, bundle))
}

func timelinePacket(tsMs float64, kind timelineKind, fieldsOfEvent ...[]byte) []byte {
	event := pbBytes(int(kind), pbMsg(fieldsOfEvent...))
	return pbBytes(fTracePacket, pbMsg(pbVarint(fPacketTimestamp, ns(tsMs)), pbBytes(fFrameTimeline, event)))
}

func slicePair(tid int, pid int, name string, fromMs, toMs float64) []marker {
	return []marker{
		{fromMs, tid, "B|" + itoa(int64(pid)) + "|" + name},
		{toMs, tid, "E|" + itoa(int64(pid))},
	}
}

// syntheticTrace: frame 100 is idle (one glyph upload), frame 101 is a
// drag frame (an input section, a commitUpdates, one card-sized and one
// glyph upload), a GC pause overlaps frame 101, and an off-window
// `DrawFrames -1` uploads outside every frame. A foreign process traces a
// doFrame and an upload that must not count.
func syntheticTrace() []byte {
	var m []marker
	add := func(ms ...[]marker) {
		for _, x := range ms {
			m = append(m, x...)
		}
	}
	// Nested pairs are listed outer-B, inner pair, outer-E by timestamp; the
	// decoder sorts by time.
	add(
		[]marker{{1, appPid, "B|4242|Choreographer#doFrame 100"}, {9, appPid, "E|4242"}},
		[]marker{{9.5, renderTi, "B|4242|DrawFrames 100"}, {15, renderTi, "E|4242"}},
		slicePair(renderTi, appPid, "Texture upload(1) 24x29", 10, 10.2),

		[]marker{{20, appPid, "B|4242|Choreographer#doFrame 101"}, {28, appPid, "E|4242"}},
		slicePair(appPid, appPid, "input", 20.1, 20.5),
		slicePair(appPid, appPid, "ReanimatedModuleProxy::commitUpdates", 21, 21.4),
		[]marker{{28.5, renderTi, "B|4242|DrawFrames 101"}, {34, renderTi, "E|4242"}},
		slicePair(renderTi, appPid, "Texture upload(2) 1080x2016", 29, 30),
		slicePair(renderTi, appPid, "Texture upload(3) 25x30", 30.5, 30.6),

		slicePair(gcTid, appPid, "Mutator threads suspended for Start", 21.5, 23.5),
		slicePair(gcTid, appPid, "Mutator threads suspended for HeapTrim", 40, 41),

		[]marker{{50, renderTi, "B|4242|DrawFrames -1"}, {55, renderTi, "E|4242"}},
		slicePair(renderTi, appPid, "Texture upload(4) 352x800", 51, 52),

		[]marker{{20, otherPid, "B|999|Choreographer#doFrame 101"}, {30, otherPid, "E|999"}},
		slicePair(otherPid, otherPid, "Texture upload(9) 2000x2000", 21, 22),
	)
	layer := []byte("TX - app.test/app.test.MainActivity#1")
	return pbMsg(
		ftracePacket(m),
		timelinePacket(12, tlActualDisplay, pbVarint(1, 1), pbVarint(2, 500), pbVarint(3, 1000)),
		timelinePacket(17, tlFrameEnd, pbVarint(1, 1)),
		timelinePacket(30, tlActualDisplay, pbVarint(1, 2), pbVarint(2, 501), pbVarint(3, 1000)),
		timelinePacket(38.33, tlFrameEnd, pbVarint(1, 2)),
		timelinePacket(1, tlActualSurface, pbVarint(1, 3), pbVarint(2, 100), pbVarint(3, 500), pbVarint(4, appPid),
			pbBytes(5, layer), pbVarint(6, 1), pbVarint(9, 1)),
		timelinePacket(20, tlActualSurface, pbVarint(1, 4), pbVarint(2, 101), pbVarint(3, 501), pbVarint(4, appPid),
			pbBytes(5, layer), pbVarint(6, 2), pbVarint(9, 64|128)),
	)
}

func readSynthetic(t *testing.T, opts TraceOptions) TraceSummary {
	t.Helper()
	if opts.Package == "" {
		opts.Package = "app.test"
	}
	s, diags, err := ReadTrace(syntheticTrace(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 0 {
		t.Fatalf("unexpected diagnostics: %+v", diags)
	}
	return s
}

func countOf(t *testing.T, s TraceSummary, match string) SliceCount {
	t.Helper()
	for _, c := range s.Counts {
		if c.Match == match {
			return c
		}
	}
	t.Fatalf("no count for %q in %+v", match, s.Counts)
	return SliceCount{}
}

func TestReadTraceAttributesSlicesToDragFrames(t *testing.T) {
	s := readSynthetic(t, TraceOptions{Counts: []string{"commitUpdates"}})
	if s.PID != appPid || s.PIDSource != "frametimeline" || s.RenderThreadTID != renderTi {
		t.Fatalf("pid %d (%s) renderThread %d", s.PID, s.PIDSource, s.RenderThreadTID)
	}
	if s.UIFrames.Count != 2 || s.UIFrames.DragFrames != 1 || val(t, "ui drag p50", s.UIFrames.DragP50Ms) != 8 {
		t.Errorf("uiFrames = %+v", s.UIFrames)
	}
	if s.RenderFrames.Count != 3 || s.RenderFrames.DragFrames != 1 || val(t, "rt drag p50", s.RenderFrames.DragP50Ms) != 5.5 {
		t.Errorf("renderFrames = %+v (the off-window DrawFrames -1 counts, but never as a drag frame)", s.RenderFrames)
	}
	up := countOf(t, s, TextureUpload)
	if up.Total != 4 || up.InFrames != 3 || up.InDragFrames != 2 || up.DragFramesWith != 1 || up.PerDragFrameMax != 2 {
		t.Errorf("texture uploads = %+v (the foreign process's upload must not count)", up)
	}
	if c := countOf(t, s, "commitUpdates"); c.Total != 1 || c.InDragFrames != 1 {
		t.Errorf("commitUpdates = %+v", c)
	}
	if s.Uploads.Large != 1 || s.Uploads.LargeInDragFrames != 1 || s.Uploads.LargePerDragFrameMax != 1 ||
		val(t, "upload ms per drag frame", s.Uploads.UploadMsPerDragFrameP50) != 1.1 {
		t.Errorf("uploads = %+v", s.Uploads)
	}
}

func TestReadTraceSummarisesArtPauses(t *testing.T) {
	p := readSynthetic(t, TraceOptions{}).ARTPauses
	if p.Count != 2 || p.DuringDragFrames != 1 || val(t, "pause max", p.MaxMs) != 2 {
		t.Fatalf("artPauses = %+v", p)
	}
	if len(p.ByCause) != 2 || p.ByCause[0].Cause != "HeapTrim" || p.ByCause[1].Cause != "Start" {
		t.Errorf("byCause = %+v", p.ByCause)
	}
}

func TestReadTraceReadsFrameTimelinePresents(t *testing.T) {
	ft := readSynthetic(t, TraceOptions{}).FrameTimeline
	if ft.SurfaceFrames != 2 || ft.DragSurfaceFrames != 1 || ft.JankyFrames != 1 || ft.DragJankyFrames != 1 {
		t.Fatalf("frameTimeline = %+v", ft)
	}
	if ft.PresentTypes["ON_TIME"] != 1 || ft.PresentTypes["LATE"] != 1 ||
		ft.JankTypes["APP_DEADLINE_MISSED"] != 1 || ft.JankTypes["BUFFER_STUFFING"] != 1 || ft.JankTypes["NONE"] != 1 {
		t.Errorf("types = %+v %+v", ft.PresentTypes, ft.JankTypes)
	}
	if val(t, "present p50", ft.PresentIntervalP50Ms) != 21.33 || val(t, "drag present p50", ft.DragPresentIntervalP50Ms) != 21.33 {
		t.Errorf("present intervals = %v / %v", *ft.PresentIntervalP50Ms, *ft.DragPresentIntervalP50Ms)
	}
}

func TestReadTraceNamesAMissingPackage(t *testing.T) {
	_, _, err := ReadTrace(syntheticTrace(), TraceOptions{Package: "app.absent"})
	var d runx.DiagError
	if !errors.As(err, &d) || d.Diag.Code != DiagPackageNotInTrace || d.Diag.Fix == "" {
		t.Fatalf("err = %v, want %s with a --pid fix", err, DiagPackageNotInTrace)
	}
	if s, _, err := ReadTrace(syntheticTrace(), TraceOptions{Package: "app.absent", PID: appPid}); err != nil || s.PIDSource != "flag" {
		t.Errorf("--pid must override the lookup: %v %+v", err, s.PIDSource)
	}
}

func TestReadTraceRefusesATruncatedTrace(t *testing.T) {
	raw := syntheticTrace()
	_, _, err := ReadTrace(raw[:len(raw)-3], TraceOptions{Package: "app.test"})
	var d runx.DiagError
	if !errors.As(err, &d) || d.Diag.Code != DiagTraceIncomplete || d.Diag.Fix == "" {
		t.Fatalf("err = %v, want %s: a cut trace must not measure its prefix", err, DiagTraceIncomplete)
	}
}

func TestReadTraceReportsDurationTotalsWithoutSamplesAsNull(t *testing.T) {
	raw := pbMsg(ftracePacket(slicePair(appPid, appPid, "Choreographer#doFrame 1", 0, 8)))
	s, _, err := ReadTrace(raw, TraceOptions{PID: appPid, Counts: []string{"absent"}})
	if err != nil {
		t.Fatal(err)
	}
	if p := s.ARTPauses; p.Count != 0 || p.TotalMs != nil || p.MaxMs != nil {
		t.Errorf("artPauses = %+v, want count 0 and null durations", p)
	}
	for _, match := range []string{TextureUpload, "absent"} {
		if c := countOf(t, s, match); c.Total != 0 || c.TotalMs != nil {
			t.Errorf("%s = %+v, want total 0 and totalMs null", match, c)
		}
	}
}

// Android < 12: doFrame and DrawFrame carry no vsync id. UI durations and
// drag frames need only time; RenderThread and per-frame counts need ids.
func TestReadTraceMeasuresUIFramesWithoutVsyncIDs(t *testing.T) {
	var m []marker
	for _, x := range [][]marker{
		slicePair(appPid, appPid, "Choreographer#doFrame", 0, 8),
		slicePair(renderTi, appPid, "DrawFrame", 9, 15),
		{{20, appPid, "B|4242|Choreographer#doFrame"}, {30, appPid, "E|4242"}},
		slicePair(appPid, appPid, "input", 20.1, 20.5),
		slicePair(appPid, appPid, "Choreographer#doFrame - resynced to 5 in 0.1ms", 20.6, 20.7),
		slicePair(appPid, appPid, "ReanimatedModuleProxy::commitUpdates", 21, 21.4),
		{{31, renderTi, "B|4242|DrawFrame"}, {37, renderTi, "E|4242"}},
		slicePair(renderTi, appPid, "Texture upload(2) 1080x2016", 32, 33),
	} {
		m = append(m, x...)
	}
	s, diags, err := ReadTrace(pbMsg(ftracePacket(m)), TraceOptions{PID: appPid, Counts: []string{"commitUpdates"}, Frames: true})
	if err != nil {
		t.Fatal(err)
	}
	warned := false
	for _, d := range diags {
		warned = warned || d.Code == DiagNoVsyncIDs
	}
	if !warned {
		t.Errorf("diagnostics = %+v, want %s", diags, DiagNoVsyncIDs)
	}
	if s.UIFrames.Count != 2 || s.UIFrames.DragFrames != 1 || val(t, "ui p50", s.UIFrames.P50Ms) != 9 || val(t, "ui drag p50", s.UIFrames.DragP50Ms) != 10 {
		t.Errorf("uiFrames = %+v (the nested resynced doFrame is not a frame)", s.UIFrames)
	}
	if s.RenderFrames.Count != 2 || s.RenderFrames.DragFrames != 0 || s.RenderFrames.DragP50Ms != nil {
		t.Errorf("renderFrames = %+v, want both draws counted and no drag attribution", s.RenderFrames)
	}
	for _, match := range []string{TextureUpload, "commitUpdates"} {
		if c := countOf(t, s, match); c.Total != 1 || c.InFrames != 0 || c.PerDragFrameP50 != nil {
			t.Errorf("%s = %+v, want the total but no per-frame attribution", match, c)
		}
	}
	if s.Uploads.LargePerDragFrameP50 != nil || s.Uploads.UploadMsPerDragFrameP50 != nil {
		t.Errorf("uploads = %+v, want null per-drag-frame values", s.Uploads)
	}
	if len(s.Frames) != 2 || s.Frames[0].VsyncID != nil || val(t, "frame 1 ui", s.Frames[1].UIMs) != 10 {
		t.Errorf("frames = %+v, want two rows with a null vsyncId", s.Frames)
	}
}

func TestReadTraceRefusesNonTrace(t *testing.T) {
	_, _, err := ReadTrace([]byte("# tracer: nop\n"), TraceOptions{Package: "app.test"})
	var d runx.DiagError
	if !errors.As(err, &d) || d.Diag.Code != DiagNotATrace {
		t.Fatalf("err = %v, want %s", err, DiagNotATrace)
	}
}
