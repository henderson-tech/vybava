// Package memo is the append-only memory ledger: LEDGER.md holds every row
// ever written, MEMORY.md is rendered from it (pinned, then usage score,
// then newest), usage.jsonl records the citations a session actually made.
// Spec: docs/qna/2026-09-20-memo-ledger.md; reference: docs/memo.md.
package memo

// Diag is the one structured failure a memo verb returns. Codes are the
// CLOSED enum below; Fix is the exact command that resolves it.
type Diag struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Detail   string `json:"detail"`
	Fix      string `json:"fix,omitempty"`
	Line     int    `json:"line,omitempty"`
}

func (d *Diag) Error() string { return d.Code + ": " + d.Detail }

// The closed diagnostic vocabulary. Every code names when it fires; the fix
// is carried on the instance because it embeds the offending argument.
const (
	// DiagUsage: the invocation is malformed (unknown verb, missing argument,
	// unparsable flag value). Fix carries the corrected invocation.
	DiagUsage = "USAGE"
	// DiagHomeNotFound: no ledger home could be resolved for the cwd, alias
	// or path given. Fix: `memo homes --json` or `--home <path>`.
	DiagHomeNotFound = "HOME_NOT_FOUND"
	// DiagLedgerInvalid: LEDGER.md (or usage.jsonl) on disk does not parse.
	// Fix: repair the named line by hand, then `memorylint check <home>`.
	DiagLedgerInvalid = "LEDGER_INVALID"
	// DiagRowSyntax: the `<type>/<topic>[!]` head or the sentence is not in
	// the row grammar (unknown type, bad topic, reserved token).
	DiagRowSyntax = "ROW_SYNTAX"
	// DiagRowLongDash: the sentence carries an em or en dash.
	DiagRowLongDash = "ROW_LONG_DASH"
	// DiagRowTwoSentences: the sentence holds more than one sentence.
	DiagRowTwoSentences = "ROW_TWO_SENTENCES"
	// DiagRowNoPeriod: the sentence does not end with a period.
	DiagRowNoPeriod = "ROW_NO_PERIOD"
	// DiagRowTooLong: the sentence exceeds 200 characters (160 warns).
	DiagRowTooLong = "ROW_TOO_LONG"
	// DiagRowTypeHome: the row type does not belong in the resolved home
	// (user/feedback are personal, project/reference are team).
	DiagRowTypeHome = "ROW_TYPE_HOME"
	// DiagRowLinkInvalid: a `->` link is not one of the accepted forms.
	DiagRowLinkInvalid = "ROW_LINK_INVALID"
	// DiagTargetUnknown: the supersede/retire target id is not in the home.
	DiagTargetUnknown = "TARGET_UNKNOWN"
	// DiagTargetClosed: the supersede/retire target is already superseded or
	// retired; point at the row that replaced it.
	DiagTargetClosed = "TARGET_CLOSED"
	// DiagRefSyntax: a row reference is not `45`, `#45`, `alias#45` or a
	// `[[...LEDGER#^m45]]` wikilink.
	DiagRefSyntax = "REF_SYNTAX"
	// DiagRefUnknown: the referenced row does not exist in the home.
	DiagRefUnknown = "REF_UNKNOWN"
	// DiagRefAmbiguous: a bare id exists in more than one session home.
	DiagRefAmbiguous = "REF_AMBIGUOUS"
	// DiagRenderDrift: MEMORY.md on disk differs from `memo render` output.
	DiagRenderDrift = "RENDER_DRIFT"
	// DiagImportInvalid: an import file line is not in the id-less grammar.
	DiagImportInvalid = "IMPORT_INVALID"
	// DiagRegistryInvalid: homes.json does not parse or carries unknown
	// fields.
	DiagRegistryInvalid = "REGISTRY_INVALID"
	// DiagHookRefused: a PreToolUse payload would rewrite a ledger file by
	// hand; the fix names the memo verb that owns the write.
	DiagHookRefused = "HOOK_REFUSED"
	// DiagSnapshotTeamOwned: snapshot/log/restore on a team home, whose
	// repository already versions it. Informational, exit 0.
	DiagSnapshotTeamOwned = "SNAPSHOT_TEAM_OWNED"
	// DiagSnapshotClean: nothing changed since the last snapshot. Informational.
	DiagSnapshotClean = "SNAPSHOT_CLEAN"
	// DiagRowLong: warning, the sentence is over 160 characters.
	DiagRowLong = "ROW_LONG"
)

func errorDiag(code, detail, fix string) *Diag {
	return &Diag{Code: code, Severity: "error", Detail: detail, Fix: fix}
}
