package xctrace

import "github.com/henderson-tech/vybava/internal/runx"

// The xctrace readers' diagnostic codes. perflab's closed enum
// (internal/perflab) lists them under the same names; adding one means a doc
// comment here stating when it fires and what the fix is.
const (
	// DiagUsage: a flag value is malformed (a --window that is not
	// <from>-<to> seconds); the fix is the corrected invocation.
	DiagUsage = "USAGE"
	// DiagFileUnreadable: a marks, steps or run-log file cannot be read or
	// parsed; the fix names the file.
	DiagFileUnreadable = "FILE_UNREADABLE"
	// DiagToolMissing: `xcrun xctrace` could not be started (no Xcode, no
	// command line tools); the fix installs or selects Xcode.
	DiagToolMissing = "TOOL_MISSING"
	// DiagTraceUnreadable: `xctrace export` itself failed (the 52 KB traces
	// of an aborted recording answer "Document Missing Template Error", exit
	// 10) or printed XML that is not an export; the fix re-records.
	DiagTraceUnreadable = "TRACE_UNREADABLE"
	// DiagTraceRunErrors: `xctrace record` printed `* [Error] ...` lines
	// under "Run issues were detected"; the trace still saves but its tables
	// can come back empty ("Transferred trace file is malformed" read as zero
	// hitches), so the window fails and never reports.
	DiagTraceRunErrors = "TRACE_RUN_ERRORS"
	// DiagHitchTableMissing: the run lists neither `hitches` (iOS 26 devices)
	// nor `hitches-summary` (iOS 18 devices under Xcode 26). The reader fails
	// closed: the stage siblings are never summed in their place. A
	// simulator's "Hitches is not supported" lands here too.
	DiagHitchTableMissing = "HITCH_TABLE_MISSING"
	// DiagTraceEmpty: the TOC has no run or a zero-length one.
	DiagTraceEmpty = "TRACE_EMPTY"
	// DiagProfileMissing: --classify or --stacks was asked of a trace recorded
	// without the Time Profiler. Warning; the hitch metrics still stand.
	DiagProfileMissing = "PROFILE_MISSING"
	// DiagStepsUnmapped: marks or taps fell outside the recording, so some
	// steps carry no rows. Warning; check the marks file and the tap lag.
	DiagStepsUnmapped = "STEPS_UNMAPPED"
)

func diag(code, detail, fix string) runx.DiagError {
	return runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
}

func warn(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "warning", Detail: detail, Fix: fix}
}

// reRecordFix is the one fix for a trace that cannot be measured.
const reRecordFix = "re-record on a physical device with the Animation Hitches and Time Profiler instruments: perflab run <scenario> --device <id> --lease <token>"
