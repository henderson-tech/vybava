// Package buildindex is perflab's native build index and JS bundle variants:
// the portable native key computed from `@expo/fingerprint --debug`, the
// content-addressed store under the perflab cache dir (builds, bundles,
// variants), the serialised native builds with a no-progress watchdog, the
// Hermes bundle export, the iOS and Android pack (bundle swap + re-sign) and
// the device install with its fencing stamp.
//
// It knows Expo, Hermes, xcodebuild, Gradle, codesign, zipalign, apksigner,
// devicectl and adb, and nothing about any project: every project-specific
// command reaches it already resolved (argv) through Project. Leases, the
// device flock and the ledger are the caller's (internal/devlab); Install
// only returns the stamp the caller records as the lease's lastInstalled.
package buildindex

import "github.com/henderson-tech/vybava/internal/runx"

// The CLOSED diagnostic codes this package raises. They are the build/bundle
// rows of perflab's enum (design section 9) and keep the same names, so the
// perflab verb layer re-exports them unchanged.
const (
	// DiagUsage: a required input is missing or malformed (no platform, an
	// unknown kind, a variant id that is not <key12>-<sha12>); the fix is the
	// corrected invocation.
	DiagUsage = "USAGE"
	// DiagFingerprintFailed: the fingerprint command exited non-zero or its
	// stdout is not `--debug` fingerprint JSON; the fix installs the app's
	// dependencies and re-runs.
	DiagFingerprintFailed = "FINGERPRINT_FAILED"
	// DiagBuildNotFound: no index entry carries the current key; the fix is
	// `perflab build native` for the same platform, profile and kind.
	DiagBuildNotFound = "BUILD_NOT_FOUND"
	// DiagBuildArtifactMissing: the entry's manifest exists but its artifact
	// was deleted; the fix rebuilds (the stale entry is removed first).
	DiagBuildArtifactMissing = "BUILD_ARTIFACT_MISSING"
	// DiagBuildInProgress: another process holds this key's build lock past
	// the wait; the detail names the holder pid and since when.
	DiagBuildInProgress = "BUILD_IN_PROGRESS"
	// DiagHostBusyBuilding: the Mac-wide build lock is held by another
	// perflab build; `run` refuses to measure and a second build waits.
	DiagHostBusyBuilding = "HOST_BUSY_BUILDING"
	// DiagBuildFailed: the build recipe exited non-zero or left no artifact;
	// the fix names the build log.
	DiagBuildFailed = "BUILD_FAILED"
	// DiagBuildStalled: the build printed nothing for the stall window (the
	// 4 h "Planning build" hang); the fix names the log and the pipe probe.
	DiagBuildStalled = "BUILD_STALLED"
	// DiagFingerprintMismatch: pack was asked to put a bundle exported under
	// one native key into a native build of another key; a Hermes bytecode or
	// native module skew would crash or lie.
	DiagFingerprintMismatch = "FINGERPRINT_MISMATCH"
	// DiagBundleNotHBC: the exported bundle does not start with the Hermes
	// bytecode magic; the fix re-exports with --bytecode.
	DiagBundleNotHBC = "BUNDLE_NOT_HBC"
	// DiagBundleAssetsChanged: an Android bundle ships assets the native APK
	// has no resource for; a swap cannot add resources.arsc entries, so the
	// variant must be built natively.
	DiagBundleAssetsChanged = "BUNDLE_ASSETS_CHANGED"
	// DiagSigningIdentityMissing: no valid code-signing identity matches the
	// team (or the original signature); the fix lists the identities.
	DiagSigningIdentityMissing = "SIGNING_IDENTITY_MISSING"
	// DiagResignFailed: codesign or apksigner refused to sign the variant.
	DiagResignFailed = "RESIGN_FAILED"
	// DiagVerifyFailed: the re-signed variant does not verify.
	DiagVerifyFailed = "VERIFY_FAILED"
	// DiagInstallFailed: the device rejected the install.
	DiagInstallFailed = "INSTALL_FAILED"
	// DiagInstallTransport: the install lost the device mid-transfer (USB
	// dropped); the caller retries once.
	DiagInstallTransport = "INSTALL_TRANSPORT"
	// DiagPackageProtected: an install needs an uninstall (version downgrade
	// or another signing key) of a package the device protects; the fix is
	// the adapter's dev package. As a warning: an install replaced a
	// protected app in place (the iOS perf build signs the store bundle id);
	// the fix is the store reinstall when the lab is done.
	DiagPackageProtected = "PACKAGE_PROTECTED"
	// DiagDeviceStateChanged: the installed artifact is not the one perflab
	// installed last (someone installed outside perflab); the fix re-installs.
	DiagDeviceStateChanged = "DEVICE_STATE_CHANGED"
	// DiagDeviceLocked: the post-install launch found the phone locked; the
	// fix is unlocking it (a human step) and launching again.
	DiagDeviceLocked = "DEVICE_LOCKED"
	// DiagAppNotInstalled: the app is not on the device at all.
	DiagAppNotInstalled = "APP_NOT_INSTALLED"
	// DiagToolMissing: a host tool a recipe needs is not installed; the fix
	// is the install line.
	DiagToolMissing = "TOOL_MISSING"
	// DiagAdapterCommandFailed: an adapter command (profile env, bundled
	// build) exited non-zero; the fix is the command to run by hand.
	DiagAdapterCommandFailed = "ADAPTER_COMMAND_FAILED"
	// DiagNotProductionEquivalent: info on every packed variant (its
	// expo-updates is off), naming the as-shipped confirmation build.
	DiagNotProductionEquivalent = "NOT_PRODUCTION_EQUIVALENT"
)

func diag(code, detail, fix string) runx.DiagError {
	return runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
}

// info is a non-failing diagnostic a result carries in its Diagnostics.
func info(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "info", Detail: detail, Fix: fix}
}

type runxDiagError = runx.DiagError
