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
	// DiagNoFrames: the dumps parsed but every row was flagged (or there
	// were none), so no metric has samples. Warning; the fix re-dumps right
	// after the interaction, before the ~120-row ring wraps.
	DiagNoFrames = "NO_FRAMES"
	// DiagNotATrace: `perfetto` read no TracePacket from the file, so it is
	// not a Perfetto protobuf trace (a text or JSON export is not accepted).
	DiagNotATrace = "NOT_A_PERFETTO_TRACE"
	// DiagPackageNotInTrace: no process_tree entry, FrameTimeline layer or
	// sched comm names the package; the fix passes the pid explicitly.
	DiagPackageNotInTrace = "PACKAGE_NOT_IN_TRACE"
	// DiagNoAppFrames: the app's main thread traced no Choreographer#doFrame
	// slice; the trace config lacks the `view` and `gfx` atrace categories
	// or the app did not draw. Warning.
	DiagNoAppFrames = "NO_APP_FRAMES"
	// DiagNoRenderThread: no app thread traced a DrawFrame(s) slice, so the
	// RenderThread metrics and the per-frame RenderThread counts are empty.
	// Warning.
	DiagNoRenderThread = "NO_RENDER_THREAD"
	// DiagNoVsyncIDs: doFrame slices carry no vsync id (Android < 12), so
	// RenderThread work and FrameTimeline frames cannot be tied to a UI
	// frame and drag attribution is empty. Warning.
	DiagNoVsyncIDs = "NO_VSYNC_IDS"
	// DiagNoFrameTimeline: the trace has no FrameTimeline packets for the
	// app; the config lacks the android.surfaceflinger.frametimeline data
	// source. Warning.
	DiagNoFrameTimeline = "NO_FRAME_TIMELINE"
)

func diag(code, detail, fix string) runx.DiagError {
	return runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
}

func warn(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "warning", Detail: detail, Fix: fix}
}
