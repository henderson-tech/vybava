package fleet

// The closed diagnostic vocabulary of the fleet applet (runx envelope codes).
const (
	// DiagRegistryMissing: ~/.claude/sessions does not exist (warning).
	DiagRegistryMissing = "REGISTRY_MISSING"
	// DiagRegistryFileSkipped: one registry file lacks the expected shape (warning).
	DiagRegistryFileSkipped = "REGISTRY_FILE_SKIPPED"
	// DiagRegistryShapeUnknown: no registry file has the expected shape (error, exit 2).
	DiagRegistryShapeUnknown = "REGISTRY_SHAPE_UNKNOWN"
	// DiagLivenessUnavailable: the process table could not be read, nothing is judged dead (warning).
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
	// DiagSchemaFormat: `fleet schema` was asked for a format other than TypeScript (error).
	DiagSchemaFormat = "SCHEMA_FORMAT"
)
