package readiness

import "github.com/henderson-tech/vybava/internal/runx"

// The CLOSED diagnostic-code enum for the readiness applet. Adding a code
// means a doc comment here stating when it fires and what the fix is.
const (
	// DiagConfigMissing: no vybava.config.ts, or it has no `readiness`
	// section — the fix is to add one (docs/readiness.md has the shape).
	DiagConfigMissing = "CONFIG_MISSING"
	// DiagConfigInvalid: the section does not decode or does not validate;
	// the detail lists every problem.
	DiagConfigInvalid = "CONFIG_INVALID"
	// DiagRepoMissing: a repo's path is not a git checkout.
	DiagRepoMissing = "REPO_MISSING"
	// DiagRefMissing: the integration branch or the production branch/tag
	// cannot be resolved on the remote.
	DiagRefMissing = "REF_MISSING"
	// DiagFetchFailed: `git fetch` failed; the range may be stale (warning).
	DiagFetchFailed = "FETCH_FAILED"
	// DiagDeviceBuildMissing: devices.build is release but a simulator or
	// emulator has no build command — phase 3 plumbing must add one (warning).
	DiagDeviceBuildMissing = "DEVICE_BUILD_MISSING"
	// DiagSimCapBelowDevices: devices.concurrent exceeds the repo's
	// guards.simCap, so claude-guards refuses the boots the runner plans.
	DiagSimCapBelowDevices = "SIM_CAP_BELOW_DEVICES"
	// DiagPlumbingPending: the adapter lists known-missing plumbing (info).
	DiagPlumbingPending = "PLUMBING_PENDING"
	// DiagDirRequired: init has no --dir and the section names no exports folder.
	DiagDirRequired = "DIR_REQUIRED"
	// DiagRunMissing: the run directory has no run.json — the fix is init.
	DiagRunMissing = "RUN_MISSING"
	// DiagRunInvalid: run.json, lanes.json or inventory.json does not parse,
	// or a lane names a feature/journey the inventory does not have.
	DiagRunInvalid = "RUN_INVALID"
	// DiagAuthorityMissing: run.json has no merge authority or finish line
	// yet — ask the phase-0 questions and record the answers first.
	DiagAuthorityMissing = "AUTHORITY_MISSING"
	// DiagEpicMissing: every rendered file links the epic; run.json has none yet.
	DiagEpicMissing = "EPIC_MISSING"
	// DiagLaneTasksPending: a lane has no story or QA task yet, so its
	// brief is not rendered (info).
	DiagLaneTasksPending = "LANE_TASKS_PENDING"
	// DiagPayloadDiffers: a copied script in the run directory differs from
	// the one this binary ships — kept, because runs adapt them (info).
	DiagPayloadDiffers = "PAYLOAD_DIFFERS"
	// DiagArgsRefreshed: inventory-args.json differed from what run.json and
	// the adapter derive; init re-derived every field but clusters (info).
	DiagArgsRefreshed = "ARGS_REFRESHED"
	// DiagRenderDrift: a rendered file differs from what its inputs produce;
	// the fix is `readiness render`.
	DiagRenderDrift = "RENDER_DRIFT"
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
