// Package doctor is `perflab doctor`: every preflight that cost the
// 2026-10-01 calendar campaign hours, run before a build, a measured run or
// a probe, each failure a closed diagnostic with the exact fix and `next`
// listing the fixes in dependency order (host before device, transport
// before app).
//
// Host: tools, the kernel pipe capacity probe, load, the host build lock,
// disk, the adapter's Appium drivers, the prebuilt WDA, the RemoteXPC
// tunnel registry and the API origin (a parked Devbox is named as such).
// Device: the device probe rows (devlab owns the state readers: lock,
// Developer Mode, Auto-Lock, refresh, thermal, battery, memory and the
// human checklist; doctor wires them through Env.ProbeDevice and never
// re-reads them), then what only a preflight asks: is the iPhone online
// for Instruments (and woken with --wake), is the measured build the one
// installed (profileable, not debuggable, not a dev client), does the last
// WDA start prove UI Automation is on, is the input focus the app, and does
// the phone reach the API through adb reverse and the perflab forward.
//
// Doctor never mutates a device unless --wake is passed with a lease; its
// fixes are perflab verbs (the device passthroughs for raw adb), because
// claude-guards refuses raw adb/devicectl against a leased device.
package doctor

import "github.com/henderson-tech/vybava/internal/runx"

// The CLOSED diagnostic codes doctor itself raises; device-probe rows
// (devlab), forward rows (netfwd) and WDA rows (wda) keep their own
// package's names and pass through unchanged.
const (
	// DiagUsage: --wake without a lease, an unknown --for, a device with
	// no platform; fix is the corrected call.
	DiagUsage = "USAGE"
	// DiagPipeCapacityLow: a fresh kernel pipe buffers under 16 KiB, so
	// xcodebuild (a native build, or WDA's test-without-building) hangs at
	// "Planning build"; the fix closes idle sessions.
	DiagPipeCapacityLow = "PIPE_CAPACITY_LOW"
	// DiagHostLoaded: the 1-min load average is above one per core (load 98
	// starved WDA's start); a run refuses to measure on a loaded host.
	DiagHostLoaded = "HOST_LOADED"
	// DiagHostBusyBuilding: a perflab native build holds the Mac-wide build
	// lock; a run refuses, a second build waits.
	DiagHostBusyBuilding = "HOST_BUSY_BUILDING"
	// DiagDiskLow: under 20 GB free on the perflab cache volume.
	DiagDiskLow = "DISK_LOW"
	// DiagToolMissing: a host tool the purpose needs is missing; the fix is
	// its install line.
	DiagToolMissing = "TOOL_MISSING"
	// DiagJDKWrong: no JDK 21 for Gradle, or the first java on PATH is
	// GraalVM / another major (warning: perflab builds pin JAVA_HOME).
	DiagJDKWrong = "JDK_WRONG"
	// DiagAppiumDriverMissing: the adapter's APPIUM_HOME lacks the
	// platform's driver (xcuitest / uiautomator2).
	DiagAppiumDriverMissing = "APPIUM_DRIVER_MISSING"
	// DiagWDAMissing: no prebuilt WDA for this Xcode, driver, team and
	// bundle id; the fix is `perflab wda build`.
	DiagWDAMissing = "WDA_MISSING"
	// DiagTunnelRegistryDown: the RemoteXPC tunnel registry does not answer
	// OK. An error for an iOS 17+ phone over the local network, a warning
	// for a wired one (the xcuitest driver falls back to usbmux; an iOS
	// `device screencap` still needs the tunnel).
	DiagTunnelRegistryDown = "TUNNEL_REGISTRY_DOWN"
	// DiagTunnelMissing: the registry lists no tunnel (address + rsdPort)
	// for the phone's hardware UDID; same severity rule.
	DiagTunnelMissing = "TUNNEL_MISSING"
	// DiagAPIUnreachable: the API origin does not answer its health path
	// within 8 s from the Mac.
	DiagAPIUnreachable = "API_UNREACHABLE"
	// DiagDevboxParked: the origin command says the Devbox workspace is
	// parked or stopped; the fix brings it up and holds it.
	DiagDevboxParked = "DEVBOX_PARKED"
	// DiagAdapterCommandFailed: the adapter's api.origin command failed for
	// another reason; the fix is the resolved command to run by hand.
	DiagAdapterCommandFailed = "ADAPTER_COMMAND_FAILED"
	// DiagDeviceOffline: Instruments lists the iPhone under "Devices
	// Offline" (its screen sleeps) or does not list it; --wake launches the
	// app and polls for up to 120 s.
	DiagDeviceOffline = "DEVICE_OFFLINE"
	// DiagDeviceToolingUnsupported: iOS < 17 cannot list its processes
	// through CoreDevice, which every devicectl-driven step needs.
	DiagDeviceToolingUnsupported = "DEVICE_TOOLING_UNSUPPORTED"
	// DiagAppNotInstalled: the measured app is not on the phone.
	DiagAppNotInstalled = "APP_NOT_INSTALLED"
	// DiagWrongBinary: the installed app is not the expected variant (its
	// CFBundleVersion stamp differs).
	DiagWrongBinary = "WRONG_BINARY"
	// DiagNotProfileable: the installed APK's manifest lacks
	// <profileable android:shell="true">, so Perfetto cannot attach app
	// tracing and framestats come from an unmeasurable build.
	DiagNotProfileable = "NOT_PROFILEABLE"
	// DiagDebuggableBuild: the installed package is DEBUGGABLE (a debug or
	// dev-client build); its frames are not production frames.
	DiagDebuggableBuild = "DEBUGGABLE_BUILD"
	// DiagInputFocusWrong: the foreground window is not the target package,
	// so adb-injected gestures would land in another app (#t328).
	DiagInputFocusWrong = "INPUT_FOCUS_WRONG"
	// DiagUIAutomationOff: the adapter's Appium server log shows the last
	// WDA start failing with "Timed out while enabling automation mode" at
	// low load: Settings > Developer > Enable UI Automation is off. Under
	// load the same line is a warning (it was load 98 once).
	DiagUIAutomationOff = "UI_AUTOMATION_OFF"
	// DiagHumanCheck: a setting perflab cannot read; the detail is the
	// checklist line for the human (info).
	DiagHumanCheck = "HUMAN_CHECK"
)

// Codes lists doctor's own slice of the closed enum.
var Codes = []string{
	DiagUsage, DiagPipeCapacityLow, DiagHostLoaded, DiagHostBusyBuilding, DiagDiskLow,
	DiagToolMissing, DiagJDKWrong, DiagAppiumDriverMissing, DiagWDAMissing,
	DiagTunnelRegistryDown, DiagTunnelMissing, DiagAPIUnreachable, DiagDevboxParked,
	DiagAdapterCommandFailed, DiagDeviceOffline, DiagDeviceToolingUnsupported,
	DiagAppNotInstalled, DiagWrongBinary, DiagNotProfileable, DiagDebuggableBuild,
	DiagInputFocusWrong, DiagUIAutomationOff, DiagHumanCheck,
}

func usage(detail, fix string) runx.DiagError {
	return runx.DiagError{Diag: runx.Diagnostic{Code: DiagUsage, Severity: "error", Detail: detail, Fix: fix}}
}
