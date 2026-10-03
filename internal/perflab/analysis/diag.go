package analysis

import "github.com/henderson-tech/vybava/internal/runx"

// The diagnostic codes analyze, compare and report raise themselves. The
// readers' codes (internal/xctrace, internal/framestats) pass through under
// their own names. perflab's closed enum (design section 9) lists all of
// them; adding one means a doc comment here stating when it fires and what
// the fix is.
const (
	// DiagUsage: a verb was called without what it needs (no input, an
	// input of no known kind, two sides that are the same); the fix is the
	// corrected invocation.
	DiagUsage = "USAGE"
	// DiagFileUnreadable: an input or a run dir's perflab.run.json cannot be
	// read or parsed.
	DiagFileUnreadable = "FILE_UNREADABLE"
	// DiagConfigInvalid: a scenario budget names a key outside the closed
	// budget vocabulary (or a non-number); the fix names the section.
	DiagConfigInvalid = "CONFIG_INVALID"
	// DiagTooFewRuns: a compare side has fewer runs than --min-runs (2 for
	// device runs). Warning; the fix re-runs alternating the variants.
	DiagTooFewRuns = "TOO_FEW_RUNS"
	// DiagNoisy: a side's runs disagree past the noise rule (relative MAD
	// > 0.25, p90/p50 > 1.5 or half drift > 0.25). Warning.
	DiagNoisy = "NOISY"
	// DiagConfounded: the sides differ in a field that changes the numbers
	// by itself (native key, public env, device, input source, production
	// equivalence). Error unless --allow-confound <field>, then a warning.
	DiagConfounded = "CONFOUNDED"
	// DiagThermalHot: a record ran with the device at a raised thermal
	// status (>= ThermalWarnStatus). Warning; cool the device and re-run.
	DiagThermalHot = "THERMAL_HOT"
	// DiagMemoryPressure: a record ran with more than SwapWarnMb of swap in
	// use. Warning; `adb shell am kill-all` and re-run.
	DiagMemoryPressure = "MEMORY_PRESSURE"
	// DiagOverBudget: report --gate found a row over its scenario budget;
	// the detail lists the offending rows.
	DiagOverBudget = "OVER_BUDGET"
	// DiagNothingMeasured: report --gate found no measured record (every
	// attempt failed, or no run dir was given).
	DiagNothingMeasured = "NOTHING_MEASURED"
	// DiagNotProductionEquivalent: the record ran a bundle-swapped variant
	// (EXUpdatesEnabled NO). Info; the fix builds the as-shipped binary for
	// the confirmation run.
	DiagNotProductionEquivalent = "NOT_PRODUCTION_EQUIVALENT"
)

func diag(code, detail, fix string) runx.DiagError {
	return runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
}

func warn(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "warning", Detail: detail, Fix: fix}
}

func info(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "info", Detail: detail, Fix: fix}
}

// HasErrors reports whether any diagnostic is an error (exit 2).
func HasErrors(diags []runx.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == "error" {
			return true
		}
	}
	return false
}
