package perflab

import (
	"errors"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/devlab"
	"github.com/henderson-tech/vybava/internal/framestats"
	"github.com/henderson-tech/vybava/internal/perflab/analysis"
	"github.com/henderson-tech/vybava/internal/perflab/buildindex"
	"github.com/henderson-tech/vybava/internal/perflab/doctor"
	"github.com/henderson-tech/vybava/internal/perflab/netfwd"
	"github.com/henderson-tech/vybava/internal/perflab/wda"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/xctrace"
)

// perflab's CLOSED diagnostic enum is the union of its packages' codes (each
// documented where it fires) and the codes the verb layer raises itself,
// below. Every code has a row in docs/perflab.md "Diagnostics"; a test holds
// the doc, this list and the packages' diag files together.
const (
	// DiagUsage: a missing or malformed argument or flag; fix is the
	// corrected invocation.
	DiagUsage = "USAGE"
	// DiagConfigMissing: the project has no vybava.config.ts, or it has no
	// perflab section; fix is `perflab adapter check --json` after adding it.
	DiagConfigMissing = "CONFIG_MISSING"
	// DiagConfigInvalid: the perflab section, or the adapter's scenario
	// rows, carry an unknown key, an unknown {token} or a missing field.
	DiagConfigInvalid = "CONFIG_INVALID"
	// DiagAdapterCommandFailed: an adapter command (profile env, scenarios,
	// api.origin, a hook) exited non-zero or printed nothing usable; the fix
	// is the resolved command to run by hand.
	DiagAdapterCommandFailed = "ADAPTER_COMMAND_FAILED"
	// DiagScenarioUnknown: a scenario name the adapter's rows do not list;
	// the detail lists the names.
	DiagScenarioUnknown = "SCENARIO_UNKNOWN"
	// DiagRunnerFailed: the project's scenario runner exited non-zero; its
	// exit is data.runner.exit, never perflab's own. Fix: read run.log, then
	// `perflab run … --resume <runDir>`.
	DiagRunnerFailed = "RUNNER_FAILED"
	// DiagRunTimeout: a case passed its hard limit (windows + 20 min) or the
	// run its --max; the runner's process group was stopped.
	DiagRunTimeout = "RUN_TIMEOUT"
	// DiagEvidenceMissing: the runner finished a scenario but left no trace,
	// frames sidecar or pftrace for it under the case dir.
	DiagEvidenceMissing = "EVIDENCE_MISSING"
	// DiagWDARebuilding: an `xcodebuild build-for-testing` appeared under the
	// runner: the prebuilt WDA was ignored (check runner.unset and the WDA
	// env the adapter maps).
	DiagWDARebuilding = "WDA_REBUILDING"
	// DiagWDAStalled: WebDriverAgent did not report started in the Appium
	// server log within 180 s of its xcodebuild (retried once).
	DiagWDAStalled = "WDA_STALLED"
	// DiagXctraceAttachFailed: xctrace could not attach to the app
	// ("Cannot find process", exit 21) after its retries (retried once).
	DiagXctraceAttachFailed = "XCTRACE_ATTACH_FAILED"
	// DiagXctraceNotReady: xctrace never printed its ready line within 60 s.
	DiagXctraceNotReady = "XCTRACE_NOT_READY"
	// DiagAppCrashed: a crash report or an ErrorBoundary line appeared in a
	// measured window; fix is `perflab crashes --device … --since …`.
	DiagAppCrashed = "APP_CRASHED"
	// DiagADBDisconnected: the Android device left adb mid-run (USB power);
	// retried once.
	DiagADBDisconnected = "ADB_DISCONNECTED"
	// DiagHazardNew: `hazards --gate` found a render-cost site the baseline
	// does not list; review it, then `perflab hazards --write-baseline`.
	DiagHazardNew = "HAZARD_NEW"
	// DiagInfraError: an unstructured failure (exit 1); detail is the
	// sanitized error.
	DiagInfraError = runx.DiagInfraError
)

// Own lists the codes the verb layer raises itself.
var Own = []string{
	DiagUsage, DiagConfigMissing, DiagConfigInvalid, DiagAdapterCommandFailed, DiagScenarioUnknown,
	DiagRunnerFailed, DiagRunTimeout, DiagEvidenceMissing, DiagWDARebuilding, DiagWDAStalled,
	DiagXctraceAttachFailed, DiagXctraceNotReady, DiagAppCrashed, DiagADBDisconnected,
	DiagHazardNew, DiagInfraError,
}

// packageCodes are the codes perflab's packages raise; the ones without a
// Codes list are named by their constants so a rename breaks the build.
var packageCodes = [][]string{
	devlab.Codes, doctor.Codes, netfwd.Codes, wda.Codes,
	{
		buildindex.DiagFingerprintFailed, buildindex.DiagBuildNotFound, buildindex.DiagBuildArtifactMissing,
		buildindex.DiagBuildInProgress, buildindex.DiagHostBusyBuilding, buildindex.DiagBuildFailed,
		buildindex.DiagBuildStalled, buildindex.DiagFingerprintMismatch, buildindex.DiagBundleNotHBC,
		buildindex.DiagBundleAssetsChanged, buildindex.DiagSigningIdentityMissing, buildindex.DiagResignFailed,
		buildindex.DiagVerifyFailed, buildindex.DiagInstallFailed, buildindex.DiagInstallTransport,
		buildindex.DiagPackageProtected, buildindex.DiagDeviceStateChanged, buildindex.DiagDeviceLocked,
		buildindex.DiagAppNotInstalled, buildindex.DiagToolMissing, buildindex.DiagAdapterCommandFailed,
		buildindex.DiagNotProductionEquivalent,
	},
	{
		analysis.DiagUsage, analysis.DiagFileUnreadable, analysis.DiagConfigInvalid, analysis.DiagTooFewRuns,
		analysis.DiagNoisy, analysis.DiagConfounded, analysis.DiagThermalHot, analysis.DiagMemoryPressure,
		analysis.DiagOverBudget, analysis.DiagNothingMeasured, analysis.DiagNotProductionEquivalent,
	},
	{
		xctrace.DiagTraceUnreadable, xctrace.DiagTraceRunErrors, xctrace.DiagHitchTableMissing,
		xctrace.DiagTraceEmpty, xctrace.DiagProfileMissing, xctrace.DiagStepsUnmapped,
	},
	{
		framestats.DiagNotFramestats, framestats.DiagMissingColumns, framestats.DiagMalformedRows,
		framestats.DiagNoFrames, framestats.DiagNotATrace, framestats.DiagTraceIncomplete,
		framestats.DiagPackageNotInTrace, framestats.DiagNoAppFrames, framestats.DiagNoRenderThread,
		framestats.DiagNoVsyncIDs, framestats.DiagNoFrameTimeline, framestats.DiagCompactSched, framestats.DiagColumnOrderSwapped,
		framestats.DiagCappedFpsSource, framestats.DiagStepsUnmapped, framestats.DiagPresetUnknown,
		framestats.DiagPresetFailed, framestats.DiagToolMissing, framestats.DiagFileUnreadable,
	},
}

// Codes is the whole closed enum, sorted and unique.
var Codes = func() []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range append([][]string{Own}, packageCodes...) {
		for _, c := range list {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	sort.Strings(out)
	return out
}()

func diag(code, detail, fix string) runx.DiagError {
	return runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
}

func warn(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "warning", Detail: detail, Fix: fix}
}

func info(code, detail string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "info", Detail: detail}
}

func errDiag(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}
}

// HasErrors reports whether any diagnostic is an error.
func HasErrors(diags []runx.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == "error" {
			return true
		}
	}
	return false
}

// tokenPlaceholder is what a package below the verb layer writes where the
// lease token goes in a fix: buildindex installs and verifies under a hold
// it never sees the token of.
const tokenPlaceholder = "<token>"

// withToken fills the token into a DiagError's fix, so the `next` it
// becomes is a command the holder can run as printed.
func withToken(err error, token string) error {
	var de runx.DiagError
	if token == "" || !errors.As(err, &de) || !strings.Contains(de.Diag.Fix, tokenPlaceholder) {
		return err
	}
	de.Diag.Fix = strings.ReplaceAll(de.Diag.Fix, tokenPlaceholder, token)
	return de
}

// diagsWithToken is withToken over diagnostic rows.
func diagsWithToken(diags []runx.Diagnostic, token string) []runx.Diagnostic {
	if token == "" {
		return diags
	}
	for i := range diags {
		diags[i].Fix = strings.ReplaceAll(diags[i].Fix, tokenPlaceholder, token)
	}
	return diags
}
