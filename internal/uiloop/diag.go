package uiloop

import "github.com/henderson-tech/vybava/internal/runx"

// The CLOSED diagnostic-code enum of the ui-loop applet. Adding a code means a
// doc comment here stating when it fires and what the fix is.
const (
	// DiagConfigMissing: no vybava.config.ts, or it has no `uiLoop` section.
	DiagConfigMissing = "CONFIG_MISSING"
	// DiagConfigInvalid: the section does not decode or does not validate;
	// the detail lists every problem.
	DiagConfigInvalid = "CONFIG_INVALID"
	// DiagProjectMissing: <dir>/project.ts does not exist — run init.
	DiagProjectMissing = "PROJECT_MISSING"
	// DiagVendorDrift: <dir>/vendor differs from the harness this binary
	// ships (missing, outdated or locally edited files) — run sync.
	DiagVendorDrift = "VENDOR_DRIFT"
	// DiagVendorEdited: a vendored file was edited in the repo; sync would
	// overwrite the edit — move it into Výbava first (warning on check).
	DiagVendorEdited = "VENDOR_EDITED"
	// DiagManifestInvalid: check.ts found manifest problems; the detail lists them.
	DiagManifestInvalid = "MANIFEST_INVALID"
	// DiagAppMapStale: the committed app map differs from the manifest — run map.
	DiagAppMapStale = "APP_MAP_STALE"
	// DiagTSRunnerFailed: the repo's tsRunner could not run a harness script
	// (not installed, a syntax error in project.ts) — the detail has its stderr.
	DiagTSRunnerFailed = "TS_RUNNER_FAILED"
	// DiagSelectionInvalid: --app/--viewports/--themes/--pass name something
	// the config or the out dir does not have.
	DiagSelectionInvalid = "SELECTION_INVALID"
	// DiagPassMissing: the pass directory (or its shots) does not exist.
	DiagPassMissing = "PASS_MISSING"
	// DiagRunFailed: the capture command exited non-zero (harness errors;
	// recipe failures are results, not this) — see its output.
	DiagRunFailed = "RUN_FAILED"
	// DiagShotsNotOk: the pass holds shots whose status is not ok (info).
	DiagShotsNotOk = "SHOTS_NOT_OK"
	// DiagFileTooLarge: a single capture exceeds publish.maxBytes and gets a
	// set of its own (warning).
	DiagFileTooLarge = "FILE_TOO_LARGE"
	// DiagPublishFailed: a set could not be pushed even after retries and
	// halving; its row in publish/index.json says why.
	DiagPublishFailed = "PUBLISH_FAILED"
	// DiagVitrinkaMissing: the vitrinka CLI is not on PATH.
	DiagVitrinkaMissing = "VITRINKA_MISSING"
	// DiagBacklogInvalid: the review backlog JSON does not decode or validate.
	DiagBacklogInvalid = "BACKLOG_INVALID"
	// DiagNoPrevious: no previous pass to compute a delta against (info).
	DiagNoPrevious = "NO_PREVIOUS"
	// DiagDevboxSync: a devbox.yaml next to the repo syncs it one-way without
	// ignoring the capture's output under <out>, so a sync mid-run deletes
	// shots, sessions and params the box wrote (warning on check) — add the
	// missing sync_ignores.
	DiagDevboxSync = "DEVBOX_SYNC"
	// DiagFollowSource: publish --follow has no box to fetch from — pass
	// --from, or set publish.from in the config.
	DiagFollowSource = "FOLLOW_SOURCE"
	// DiagFetchFailed: publish --follow's rsync from the box failed on
	// followFetchTries ticks in a row — see its stderr.
	DiagFetchFailed = "FETCH_FAILED"
	// DiagRunUnfinished: publish --follow stopped without the run's done.json
	// after 3 × --until-idle without a new shot (the capture died before its
	// teardown); finish the pass with run --resume and follow again (warning).
	DiagRunUnfinished = "RUN_UNFINISHED"
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

// errDiag is an error-severity diagnostic a verb reports alongside its data
// (several can fire at once, so they travel in Result, not as the error).
func errDiag(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}
}

type runxDiagnostic = runx.Diagnostic

// SelectionError is a SELECTION_INVALID refusal for the CLI's flag checks.
func SelectionError(detail, fix string) error { return diag(DiagSelectionInvalid, detail, fix) }
