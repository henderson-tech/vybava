package claudeguards

// Device lease release: SessionEnd hook, plus the stale sweep SessionStart's
// `weather --reap` runs.
//
// perflab leases a physical phone to one token holder (internal/devlab). A
// session that ends while holding one would keep the phone for the rest of
// the TTL (up to 8 h), with its forwarders and recorders still running. The
// ending session knows it is ending; nobody else does. So it releases its
// OWN leases: those whose owner names this session id, and the process
// groups those leases recorded. Never a peer's lease, never a sweep by
// pattern, and it fails open: no state dir, a held lock, a bad file are each
// one stderr line and a normal session end.
//
// Subagents share the session id, so this runs once, when the session that
// spawned them ends: the right moment, since a subagent's lease belongs to
// the session's work.

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/henderson-tech/vybava/internal/devlab"
)

// DeviceLeaseRelease releases the ending session's device leases. The
// session id comes from the hook payload, else CLAUDE_CODE_SESSION_ID.
func DeviceLeaseRelease(in *HookInput, stderr io.Writer) {
	id := ""
	if in != nil {
		id = strings.TrimSpace(in.SessionID)
	}
	if id == "" {
		id = strings.TrimSpace(os.Getenv("CLAUDE_CODE_SESSION_ID"))
	}
	if id == "" {
		return
	}
	lab, err := devlab.Open()
	if err != nil {
		fmt.Fprintf(stderr, "claude-guards device-lease-release: %v\n", err)
		return
	}
	lab.ReleaseSession(id, stderr)
}

// reapDeviceLeases releases every stale lease (expired, holder gone) at
// session start; quiet when there is none.
func reapDeviceLeases(stderr io.Writer) {
	lab, err := devlab.Open()
	if err != nil {
		fmt.Fprintf(stderr, "claude-guards weather --reap: device leases: %v\n", err)
		return
	}
	lab.ReapStale(stderr)
}
