// Package wda is perflab's prebuilt WebDriverAgent index. A physical-iPhone
// Appium session that builds WDA itself spends minutes in xcodebuild before
// every run (and on iOS 26 `usePreinstalledWDA` aborts in XCTest), so WDA is
// built ONCE per (Xcode build, xcuitest driver, WebDriverAgent version,
// team, runner bundle id) with `xcodebuild build-for-testing -destination
// generic/platform=iOS` and handed to the runner as a derived-data path
// (Appium `usePrebuiltWDA` + `derivedDataPath`).
//
// Layout under the perflab cache dir:
//
//	wda/<key>/            the derived data (Build/Products/*.xctestrun, the Runner app)
//	wda/<key>/perflab-wda.json   the manifest
//	wda/logs/<key>-<unix>.log    every build's full xcodebuild log
//
// A key directory is renamed into place only after a successful build, so a
// present key is always a complete build. Appium writes per-device
// xctestrun copies into it later; that is the only mutation.
package wda

import "github.com/henderson-tech/vybava/internal/runx"

// The CLOSED diagnostic codes this package raises: the WDA rows of perflab's
// enum (design section 9), same names, so the verb layer re-exports them.
const (
	// DiagUsage: a required input is missing (no team, no bundle id, an
	// import dir that is not WDA derived data); fix is the corrected call.
	DiagUsage = "USAGE"
	// DiagWDAMissing: no prebuilt WDA for this Mac's Xcode build, driver,
	// team and bundle id (or an import was built by another Xcode); the fix
	// is `perflab wda build`.
	DiagWDAMissing = "WDA_MISSING"
	// DiagAppiumDriverMissing: the adapter's APPIUM_HOME has no xcuitest
	// driver (or its WebDriverAgent project); the fix installs it there.
	DiagAppiumDriverMissing = "APPIUM_DRIVER_MISSING"
	// DiagToolMissing: xcodebuild, plutil, codesign or ditto is missing.
	DiagToolMissing = "TOOL_MISSING"
	// DiagPipeCapacityLow: a fresh kernel pipe buffers under 16 KiB, so an
	// xcodebuild would hang at "Planning build"; the build is refused.
	DiagPipeCapacityLow = "PIPE_CAPACITY_LOW"
	// DiagBuildFailed: xcodebuild exited non-zero or left no xctestrun; the
	// detail names the log (and the signing line when signing failed).
	DiagBuildFailed = "BUILD_FAILED"
	// DiagBuildStalled: xcodebuild printed nothing for the stall window;
	// the fix names the log and the pipe probe.
	DiagBuildStalled = "BUILD_STALLED"
	// DiagHostBusyBuilding: the Mac-wide build lock is held; raised by the
	// lock hook the verb layer wires (buildindex owns the lock).
	DiagHostBusyBuilding = "HOST_BUSY_BUILDING"
)

// Codes lists this package's slice of the closed enum (docs and tests walk
// it).
var Codes = []string{
	DiagUsage, DiagWDAMissing, DiagAppiumDriverMissing, DiagToolMissing,
	DiagPipeCapacityLow, DiagBuildFailed, DiagBuildStalled, DiagHostBusyBuilding,
}

func diag(code, detail, fix string) runx.DiagError {
	return runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
}
