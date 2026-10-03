package fleet

// The closed diagnostic vocabulary of the fleet applet (runx envelope codes).
const (
	// DiagRegistryMissing: ~/.claude/sessions does not exist (warning).
	DiagRegistryMissing = "REGISTRY_MISSING"
	// DiagRegistryFileSkipped: one registry file lacks the expected shape (warning).
	DiagRegistryFileSkipped = "REGISTRY_FILE_SKIPPED"
	// DiagRegistryShapeUnknown: no registry file has the expected shape (error, exit 2).
	DiagRegistryShapeUnknown = "REGISTRY_SHAPE_UNKNOWN"
	// DiagLivenessUnavailable: the process table could not be read, nothing is judged dead (warning on reads; an action refuses — error).
	DiagLivenessUnavailable = "LIVENESS_UNAVAILABLE"
	// DiagCodexUnavailable: no Codex rows could be read (warning).
	DiagCodexUnavailable = "CODEX_UNAVAILABLE"
	// DiagCodexPartial: Codex rows were read with a gap (warning).
	DiagCodexPartial = "CODEX_PARTIAL"
	// DiagLedgerUnreadable: a ledger file could not be read (warning on reads, error on record/show).
	DiagLedgerUnreadable = "LEDGER_UNREADABLE"
	// DiagLedgerEventInvalid: the event on stdin is not a ledger event (error).
	DiagLedgerEventInvalid = "LEDGER_EVENT_INVALID"
	// DiagLedgerBusy: another writer held the session's ledger lock too long (error).
	DiagLedgerBusy = "LEDGER_BUSY"
	// DiagLedgerUnproven: ledgers with open jobs whose owner cannot be proven dead (info).
	DiagLedgerUnproven = "LEDGER_UNPROVEN"
	// DiagSessionInvalid: --session is missing or not a session id (error).
	DiagSessionInvalid = "SESSION_INVALID"
	// DiagSchemaFormat: `fleet schema` was asked for neither --ts nor --swift (error).
	DiagSchemaFormat = "SCHEMA_FORMAT"

	// DiagCmuxUnavailable: cmux is not running, refuses this client, or is too old (error on actions, warning on publish).
	DiagCmuxUnavailable = "CMUX_UNAVAILABLE"
	// DiagSessionNotFound: no registry entry carries the session id (error).
	DiagSessionNotFound = "SESSION_NOT_FOUND"
	// DiagSessionGone: the session's process is proven gone (error).
	DiagSessionGone = "SESSION_GONE"
	// DiagNotInCmux: the session's process runs outside any cmux surface (error).
	DiagNotInCmux = "NOT_IN_CMUX"
	// DiagReplyInvalid: neither or both of text and an option, or text over the cap (error).
	DiagReplyInvalid = "REPLY_INVALID"
	// DiagReplyRefused: the target cannot take a reply — busy, at its shell, or a Codex process (error).
	DiagReplyRefused = "REPLY_REFUSED"
	// DiagDialogOpen: text was refused because a dialog is open (error).
	DiagDialogOpen = "DIALOG_OPEN"
	// DiagNoDialog: an option was refused because no dialog is open any more (error).
	DiagNoDialog = "NO_DIALOG"
	// DiagDialogChanged: the open dialog is not the one the option was chosen from (error).
	DiagDialogChanged = "DIALOG_CHANGED"
	// DiagOptionInvalid: the key is not an answer of the open dialog, or the dialog takes more than one key (error).
	DiagOptionInvalid = "OPTION_INVALID"
	// DiagNotSubmitted: the text reached the prompt but the submit key failed; never re-sent (error).
	DiagNotSubmitted = "NOT_SUBMITTED"
	// DiagSnapshotUnwritable: snapshot.json could not be written (error).
	DiagSnapshotUnwritable = "SNAPSHOT_UNWRITABLE"
)
