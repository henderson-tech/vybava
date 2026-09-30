package polishkit

import "github.com/henderson-tech/vybava/internal/runx"

// The CLOSED diagnostic-code enum of the polish-kit applet. Adding a code
// means a doc comment here stating when it fires and what the fix is, and a
// row in docs/polish-kit.md.
const (
	// DiagNoConfigSection: no vybava.config.ts, no `polish` section, or one
	// that does not decode or validate; the fix is the exact block to add.
	DiagNoConfigSection = "no-config-section"
	// DiagNoChanges: the diff against the base is empty and nothing else
	// (cwd, --target) names a target; the fix is `plan --target <t>`.
	DiagNoChanges = "no-changes"
	// DiagUnknownTarget: --target names something other than app, ui, api.
	DiagUnknownTarget = "unknown-target"
	// DiagUnknownLane: a lane id the config does not declare.
	DiagUnknownLane = "unknown-lane"
	// DiagUnknownScreen: a screen id the config does not declare.
	DiagUnknownScreen = "unknown-screen"
	// DiagLaneMissing: the runtime exists but no simulator/AVD of the lane's
	// deviceType does; the fix is the exact create command.
	DiagLaneMissing = "lane-missing"
	// DiagRuntimeMissing: no installed iOS runtime matches the lane's
	// runtime prefix; the fix names the Xcode Components download.
	DiagRuntimeMissing = "runtime-missing"
	// DiagDeviceUnavailable: the device (or URL) exists but is not reachable
	// now: unpaired or disconnected phone, offline/unauthorized adb device,
	// an emulator that is not running, a URL that does not answer.
	DiagDeviceUnavailable = "device-unavailable"
	// DiagLaneUnsupported: the verb cannot drive this lane kind (set on an
	// ios-device, shoot on ios-device or browser); the detail names the
	// alternative and where its files go.
	DiagLaneUnsupported = "lane-unsupported"
	// DiagToolMissing: xcrun, adb or emulator is not on PATH; the fix names
	// the install.
	DiagToolMissing = "tool-missing"
	// DiagShotRequired: `cell <id> fail` without --shot and without a shot
	// already on the cell, or --shot names a file that does not exist.
	DiagShotRequired = "shot-required"
	// DiagPassMissing: no pass directory (or not the one asked for) under
	// <out>; the fix is `run init`.
	DiagPassMissing = "pass-missing"
	// DiagCellUnknown: the cell id is not in the pass's run.json.
	DiagCellUnknown = "cell-unknown"
	// DiagRunVersion: run.json was written by another RUN_VERSION; the fix
	// is `run init --force` on that pass.
	DiagRunVersion = "run-version"
	// DiagUsage: a flag, argument or verb the applet does not accept; the
	// fix is the corrected invocation.
	DiagUsage = "usage"
	// DiagLedgerLocked: the pass's run.lock stayed held by another
	// polish-kit command for the whole LockWait; the fix is to wait for it,
	// or delete the lock file when no polish-kit process is running.
	DiagLedgerLocked = "ledger-locked"
)

// Codes lists the closed enum (docs and tests walk it).
var Codes = []string{
	DiagNoConfigSection, DiagNoChanges, DiagUnknownTarget, DiagUnknownLane, DiagUnknownScreen,
	DiagLaneMissing, DiagRuntimeMissing, DiagDeviceUnavailable, DiagLaneUnsupported, DiagToolMissing,
	DiagShotRequired, DiagPassMissing, DiagCellUnknown, DiagRunVersion, DiagUsage, DiagLedgerLocked,
}

func diag(code, detail, fix string) runx.DiagError {
	return runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
}

// errDiag is an error-severity diagnostic a verb reports alongside its data
// (several can fire at once, so they travel in Result, not as the error).
func errDiag(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}
}

func warn(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "warning", Detail: detail, Fix: fix}
}

func info(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "info", Detail: detail, Fix: fix}
}
