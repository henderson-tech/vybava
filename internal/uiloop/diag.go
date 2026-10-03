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
	// DiagSpecLintDrift: the spec (uiLoop.spec) no longer holds a line init
	// writes for a lint knob, rendered with the config's current value
	// ("Touch targets are at least 40×40px …"), so reviewers judge against
	// one value while the lint measures another (warning on check) — add the
	// line, or set the knob when the spec means another value.
	DiagSpecLintDrift = "SPEC_LINT_DRIFT"
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
	// DiagAppUnreachable: doctor's GET of an app's base URL (its env var's
	// value when set here) got no 2xx/3xx answer within a few seconds, or got
	// a dev server's error page (Vite overlay, `Cannot GET`, a compile error)
	// — start the dev server or fix its build, or point the env var at it.
	DiagAppUnreachable = "APP_UNREACHABLE"
	// DiagRunFailed: the capture command exited non-zero (harness errors;
	// recipe failures are results, not this) — see its output.
	DiagRunFailed = "RUN_FAILED"
	// DiagShotsNotOk: the pass holds shots whose status is not ok (info).
	DiagShotsNotOk = "SHOTS_NOT_OK"
	// DiagSetTooLarge: an area holds more captures than one vitrinka set takes
	// (ingest.MaxSetFiles, 20,000 files); its set is not planned — split the
	// area in the config.
	DiagSetTooLarge = "SET_TOO_LARGE"
	// DiagConfigDeprecated: the config sets a key that is still accepted but
	// ignored (publish.maxFiles, publish.maxBytes) — delete it (warning).
	DiagConfigDeprecated = "CONFIG_DEPRECATED"
	// DiagLegacySets: publish/index.json lists sets an older publish of this
	// pass chunked by viewport × theme; they are kept under `legacy` and never
	// re-adopted — delete their boards when the area sets replace them (info).
	DiagLegacySets = "LEGACY_SETS"
	// DiagPublishFailed: a set could not be pushed even after retries; its row
	// in publish/index.json says why.
	DiagPublishFailed = "PUBLISH_FAILED"
	// DiagPublishRefused: `board capture` refused some files of a set; the rest
	// were adopted and the set pushed. The row's `refused` in
	// publish/index.json names each file and why (warning).
	DiagPublishRefused = "PUBLISH_REFUSED"
	// DiagVitrinkaMissing: the vitrinka CLI is not on PATH.
	DiagVitrinkaMissing = "VITRINKA_MISSING"
	// DiagBacklogInvalid: the review backlog JSON does not decode or validate.
	DiagBacklogInvalid = "BACKLOG_INVALID"
	// DiagNoPrevious: no previous pass to compute a delta against (info).
	DiagNoPrevious = "NO_PREVIOUS"
	// DiagReviewedMissing: the backlog has no `reviewed` list (a review-loop
	// before it named the screens it judged), so a screen with no finding
	// counts as clean even if no reviewer looked at it (warning) — have the
	// review stage write `reviewed`.
	DiagReviewedMissing = "REVIEWED_MISSING"
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
	// DiagReviewInvalid: a <pass>/review/raw/<batch>.json does not decode;
	// merge-review stops — re-run that batch (delete its raw file).
	DiagReviewInvalid = "REVIEW_INVALID"
	// DiagReviewIncomplete: merge-review ran while planned batches have no
	// raw file; the draft covers only the reviewed ones (warning) — finish
	// the review stage first.
	DiagReviewIncomplete = "REVIEW_INCOMPLETE"
	// DiagCheckpointInvalid: a <pass>/fix/**/*.json is not a checkpoint (it
	// does not decode, or has neither key nor status) and is skipped, so its
	// item counts as not finished; or two admitted files checkpoint one key
	// and the one not at fix/<key>.json (else the older) is skipped
	// (warning) — rewrite, delete or archive the file.
	DiagCheckpointInvalid = "CHECKPOINT_INVALID"
	// DiagForeignItems: lanes found open items that name no file inside the
	// repo (an absolute path into another repo); no lane owns them (warning)
	// — checkpoint them blocked with where the fix lands, or repair their files.
	DiagForeignItems = "FOREIGN_ITEMS"
	// DiagCaptureRevisionMissing: a pass's capture.json names a revision this
	// clone does not have (gc'd after its branch went, or the pass was copied
	// from another clone), so its manifest and spec basis is read from the
	// working tree, a rig or spec change since capture stales the pass, and
	// its drift cannot be weighed, so state routes a reshoot (warning) —
	// fetch that revision, or capture a new pass.
	DiagCaptureRevisionMissing = "CAPTURE_REVISION_MISSING"
	// DiagCaptureRunning: `run` found another run's live capture lease (any
	// pass under <out>): one capture runs at a time — wait for it, following
	// `state`, which reports `capture` and routes `wait` meanwhile. doctor
	// reports it as a warning.
	DiagCaptureRunning = "CAPTURE_RUNNING"
	// DiagLeaseHeld: merge-review's synth or publish's publish lease (or a
	// follow's renewal of it) is held by another owner; the detail names the
	// holder and since when — leave the stage to that holder, or remove the
	// lease file once it is gone.
	DiagLeaseHeld = "LEASE_HELD"
	// DiagLeaseStale: doctor found a process lease its holder never released
	// (a crash: its pid is gone, its ttl ran out, or its run finished) or a
	// lease file that does not decode; the next taker replaces it anyway
	// (warning) — remove it.
	DiagLeaseStale = "LEASE_STALE"
	// DiagDigestCache: a pass's .cache/digests.json (the shots' SHA256 by
	// file stamp, never evidence) does not decode, is another version, or
	// could not be written, so the read hashed every file again (warning) —
	// nothing to do unless it recurs; the next read rewrites it.
	DiagDigestCache = "DIGEST_CACHE"
	// DiagPaused: the polish loop is paused (<out>/paused.json), so a verb
	// that starts work (run, batches --claim, lanes) starts nothing; doctor
	// reports it as a warning — `ui-loop resume`.
	DiagPaused = "PAUSED"
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
