package framestats

import "github.com/henderson-tech/vybava/internal/runx"

// The CLOSED diagnostic-code enum for the framestats applet. Adding a code
// means a doc comment here stating when it fires and what the fix is.
const (
	// DiagUsage: a verb was called without its required argument or flag;
	// the fix is the corrected invocation.
	DiagUsage = "USAGE"
	// DiagFileUnreadable: an input file cannot be opened or read.
	DiagFileUnreadable = "FILE_UNREADABLE"
	// DiagNotFramestats: `parse` found no ---PROFILEDATA--- block, so the
	// input is not a `dumpsys gfxinfo <pkg> framestats` dump.
	DiagNotFramestats = "NOT_FRAMESTATS"
	// DiagMissingColumns: a PROFILEDATA header lacks a column every frame
	// measurement reads (Flags, IntendedVsync, SwapBuffers, FrameCompleted),
	// so the dump is cut or not framestats; the fix dumps again.
	DiagMissingColumns = "MISSING_COLUMNS"
	// DiagMalformedRows: some rows were cut short, carried a non-integer or
	// completed before their vsync; they are counted in rowsMalformed and
	// never measured. Warning; the fix dumps again.
	DiagMalformedRows = "MALFORMED_ROWS"
	// DiagNoFrames: the dumps parsed but every row was flagged or malformed
	// (or there were none), so no metric has samples. Warning; the fix
	// re-dumps right after the interaction, before the ~120-row ring wraps.
	DiagNoFrames = "NO_FRAMES"
	// DiagNotATrace: `perfetto` read no TracePacket from the file, so it is
	// not a Perfetto protobuf trace (a text or JSON export is not accepted).
	DiagNotATrace = "NOT_A_PERFETTO_TRACE"
	// DiagTraceIncomplete: the protobuf stream breaks after at least one
	// packet (a file pulled while perfetto was still writing, or corrupt),
	// so later frames are missing and no metric is reported; the fix pulls
	// the trace again once perfetto has stopped.
	DiagTraceIncomplete = "TRACE_INCOMPLETE"
	// DiagPackageNotInTrace: no process_tree entry, FrameTimeline layer or
	// sched comm names the package; the fix passes the pid explicitly.
	DiagPackageNotInTrace = "PACKAGE_NOT_IN_TRACE"
	// DiagNoAppFrames: the app's main thread traced no Choreographer#doFrame
	// slice; the trace config lacks the `view` and `gfx` atrace categories
	// or the app did not draw. Also a drag or fling probe whose gestures
	// presented no frame (nothing scrolled under them). Warning.
	DiagNoAppFrames = "NO_APP_FRAMES"
	// DiagNoRenderThread: no app thread traced a DrawFrame(s) slice, so the
	// RenderThread metrics and the per-frame RenderThread counts are empty.
	// Warning.
	DiagNoRenderThread = "NO_RENDER_THREAD"
	// DiagNoVsyncIDs: doFrame slices carry no vsync id (Android < 12). UI
	// durations and drag frames are still measured (they only need time),
	// but RenderThread work, per-frame slice counts and FrameTimeline frames
	// cannot be tied to a UI frame, so those stay empty. Warning.
	DiagNoVsyncIDs = "NO_VSYNC_IDS"
	// DiagNoFrameTimeline: the trace has no FrameTimeline packets for the
	// app; the config lacks the android.surfaceflinger.frametimeline data
	// source. Warning.
	DiagNoFrameTimeline = "NO_FRAME_TIMELINE"
	// DiagCompactSched: the trace's sched switches came compact (Android
	// 16's traced default), which this reader does not decode, so the
	// RenderThread's CPU and clock placement (rtCpu) is unread. Warning; the
	// fix records with ftrace_config compact_sched { enabled: false }.
	DiagCompactSched = "COMPACT_SCHED"
)

func diag(code, detail, fix string) runx.DiagError {
	return runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
}

func warn(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "warning", Detail: detail, Fix: fix}
}
