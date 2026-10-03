package devlab

import (
	"fmt"

	"github.com/henderson-tech/vybava/internal/runx"
)

// The device ledger's slice of perflab's CLOSED diagnostic enum
// (docs/perflab.md "Diagnostics"). perflab's own enum lists these names
// verbatim; adding one means a doc comment here and a row in the doc.
const (
	// DiagUsage: a missing or malformed argument; fix is the corrected call.
	DiagUsage = "USAGE"
	// DiagDeviceUnknown: the handle is not a ledger id or a registered
	// alias (hardware UDID, CoreDevice id, adb serial). Marketing names are
	// never accepted: two pairs of iPhones share a name on one Mac.
	DiagDeviceUnknown = "DEVICE_UNKNOWN"
	// DiagDeviceAmbiguous: two live devices share a name (scan, warning) or
	// an add matches several live rows.
	DiagDeviceAmbiguous = "DEVICE_AMBIGUOUS"
	// DiagDeviceExists: an add would rebind an id or an alias another row
	// already owns.
	DiagDeviceExists = "DEVICE_EXISTS"
	// DiagDeviceLeased: another live lease holds the device; the detail
	// names the holder and how long it has held. There is no steal.
	DiagDeviceLeased = "DEVICE_LEASED"
	// DiagLeaseRequired: a device-touching verb was called without --lease.
	DiagLeaseRequired = "LEASE_REQUIRED"
	// DiagLeaseInvalid: the token is wrong, the device is not leased, or the
	// lease expired (renew it).
	DiagLeaseInvalid = "LEASE_INVALID"
	// DiagLeaseStaleReclaimed: acquire took over an expired lease whose
	// holder process is dead (info).
	DiagLeaseStaleReclaimed = "LEASE_STALE_RECLAIMED"
	// DiagLedgerLocked: the ledger or a lease file lock stayed held past the
	// lock wait; the detail names the path and the holder pid.
	DiagLedgerLocked = "LEDGER_LOCKED"
	// DiagDeviceBusy: another verb holds the device lock; the detail names
	// the verb, pid and elapsed time.
	DiagDeviceBusy = "DEVICE_BUSY"
	// DiagDeviceStateChanged: the installed artifact differs from the
	// lease's lastInstalled (a change made outside perflab).
	DiagDeviceStateChanged = "DEVICE_STATE_CHANGED"
	// DiagPackageProtected: a passthrough would uninstall or clear a package
	// the device protects (a personal phone's production app).
	DiagPackageProtected = "PACKAGE_PROTECTED"
	// DiagDeviceOffline: not attached or not reachable now.
	DiagDeviceOffline = "DEVICE_OFFLINE"
	// DiagDeviceUnpaired: the phone does not trust this Mac (iOS) or the
	// USB debugging prompt was not accepted (adb unauthorized).
	DiagDeviceUnpaired = "DEVICE_UNPAIRED"
	// DiagDeviceLocked: the screen is locked or asleep.
	DiagDeviceLocked = "DEVICE_LOCKED"
	// DiagDeveloperModeOff: iOS developerModeStatus is not enabled.
	DiagDeveloperModeOff = "DEVELOPER_MODE_OFF"
	// DiagTunnelRegistryDown: the RemoteXPC tunnel registry did not answer
	// OK (iOS 17+ physical automation needs it).
	DiagTunnelRegistryDown = "TUNNEL_REGISTRY_DOWN"
	// DiagTunnelMissing: the registry answers but lists no tunnel for the
	// device's hardware UDID.
	DiagTunnelMissing = "TUNNEL_MISSING"
	// DiagRefreshRateMismatch: the measured vsync differs from expectHz.
	DiagRefreshRateMismatch = "REFRESH_RATE_MISMATCH"
	// DiagThermalHot: Android thermal status moderate (warning) or worse.
	DiagThermalHot = "THERMAL_HOT"
	// DiagLowBattery: battery under 20 % (error unplugged, warning charging).
	DiagLowBattery = "LOW_BATTERY"
	// DiagMemoryPressure: MemAvailable under 10 % of MemTotal.
	DiagMemoryPressure = "MEMORY_PRESSURE"
	// DiagAutoLockOn: the screen will sleep mid-run (no stay-on, short
	// timeout).
	DiagAutoLockOn = "AUTO_LOCK_ON"
	// DiagDeviceToolingUnsupported: the step needs tooling the device's OS
	// lacks (iOS < 17 for devicectl-only steps, an iOS screencap without a
	// tunnel-capable tool).
	DiagDeviceToolingUnsupported = "DEVICE_TOOLING_UNSUPPORTED"
	// DiagHumanCheck: a setting the lab cannot read; the detail is the
	// checklist line for the human.
	DiagHumanCheck = "HUMAN_CHECK"
	// DiagToolMissing: xcrun, adb or ios (go-ios) is not on PATH.
	DiagToolMissing = "TOOL_MISSING"
	// DiagDeviceCommandFailed: a passthrough (device shell, screencap, pull)
	// ran and exited non-zero; data.exit carries its code, the fix is the
	// resolved command. perflab never propagates a wrapped exit code.
	DiagDeviceCommandFailed = "DEVICE_COMMAND_FAILED"
)

// Codes lists this package's slice of the closed enum (docs and tests walk
// it).
var Codes = []string{
	DiagUsage, DiagDeviceUnknown, DiagDeviceAmbiguous, DiagDeviceExists,
	DiagDeviceLeased, DiagLeaseRequired, DiagLeaseInvalid, DiagLeaseStaleReclaimed,
	DiagLedgerLocked, DiagDeviceBusy, DiagDeviceStateChanged, DiagPackageProtected,
	DiagDeviceOffline, DiagDeviceUnpaired, DiagDeviceLocked, DiagDeveloperModeOff,
	DiagTunnelRegistryDown, DiagTunnelMissing, DiagRefreshRateMismatch, DiagThermalHot,
	DiagLowBattery, DiagMemoryPressure, DiagAutoLockOn, DiagDeviceToolingUnsupported,
	DiagHumanCheck, DiagToolMissing, DiagDeviceCommandFailed,
}

func diag(code, detail, fix string) runx.DiagError {
	return runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
}

func errorRow(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}
}

func warnRow(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "warning", Detail: detail, Fix: fix}
}

func infoRow(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "info", Detail: detail, Fix: fix}
}

func usage(detail, fix string) runx.DiagError { return diag(DiagUsage, detail, fix) }

func unknownDevice(handle string) runx.DiagError {
	return diag(DiagDeviceUnknown,
		fmt.Sprintf("%q is not a ledger id or a registered UDID, CoreDevice id or adb serial (marketing names are never accepted)", handle),
		"perflab device scan --json")
}
