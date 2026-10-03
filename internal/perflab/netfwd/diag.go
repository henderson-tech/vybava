// Package netfwd is `perflab net`: how an Android phone reaches an API it
// cannot route to. A phone cannot join the WireGuard mesh a Devbox API
// answers on, so a perf build bakes http://localhost:<devicePort>, `adb
// reverse tcp:P tcp:P` carries the phone's localhost to the Mac, and an
// in-process TCP forwarder on the Mac's two loopbacks (127.0.0.1 AND ::1:
// adb's host side may dial either) carries it on to the adapter's origin.
//
// The forwarder lives exactly as long as the perflab process that started
// it: `net forward` streams until SIGINT (run as a background task of the
// session for a hand test), `run` starts it in-process. It is never
// daemonised, and `net stop` signals only the literal pid its record names,
// after proving that pid still holds the port.
package netfwd

import "github.com/henderson-tech/vybava/internal/runx"

// The CLOSED diagnostic codes this package raises: the network rows of
// perflab's enum (design section 9), same names.
const (
	// DiagUsage: no serial, a device port outside 1..65535, or an origin
	// that is not an http(s) URL; fix is the corrected call.
	DiagUsage = "USAGE"
	// DiagForwardDown: some link of phone -> adb reverse -> Mac loopback ->
	// origin is missing or unhealthy (no reverse, nothing listening, a
	// health check through the forward that fails, a port another process
	// holds); the fix is `perflab net forward` (or the lsof line naming the
	// port's holder).
	DiagForwardDown = "FORWARD_DOWN"
	// DiagAPIUnreachable: the origin itself does not answer its health path
	// from the Mac, so no forward can help; the fix is the adapter's hold
	// (or up) command.
	DiagAPIUnreachable = "API_UNREACHABLE"
	// DiagDeviceOffline: adb does not see the serial (unplugged, offline or
	// unauthorized).
	DiagDeviceOffline = "DEVICE_OFFLINE"
	// DiagToolMissing: adb is not installed.
	DiagToolMissing = "TOOL_MISSING"
)

// Codes lists this package's slice of the closed enum.
var Codes = []string{DiagUsage, DiagForwardDown, DiagAPIUnreachable, DiagDeviceOffline, DiagToolMissing}

func diag(code, detail, fix string) runx.DiagError {
	return runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
}

func row(severity, code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: severity, Detail: detail, Fix: fix}
}
