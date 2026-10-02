package fleet

import (
	"context"
	"path/filepath"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/plugingc"
	"github.com/henderson-tech/vybava/internal/runx"
)

// Liveness is the verdict on whether a session's process is still the one
// that registered it.
type Liveness string

const (
	LivenessAlive Liveness = "alive"
	// LivenessGone is a PID with no process behind it.
	LivenessGone Liveness = "gone"
	// LivenessForeign is a PID held by something that cannot be Claude Code.
	LivenessForeign Liveness = "foreign"
	// LivenessRecycled is a PID held by a process that started after the
	// session's own process did, so it is not the session.
	LivenessRecycled Liveness = "recycled"
	// LivenessUnknown is undecidable: no process table, or another machine's
	// pidDomain. (A procStart that does not parse only skips the recycle
	// check, leaving the session alive.)
	LivenessUnknown Liveness = "unknown"
)

// Dead reports a proven verdict. Unknown is never dead: a session wrongly
// called dead invites a second process onto its transcript.
func (l Liveness) Dead() bool {
	return l == LivenessGone || l == LivenessForeign || l == LivenessRecycled
}

// startGrace absorbs the second-resolution and clock jitter between the
// registry's procStart and ps's lstart for the same process.
const startGrace = time.Minute

// processView is the live table, and whether it could be read at all: a
// failed ps must make every verdict unknown, never every session dead.
type processView struct {
	table plugingc.ProcessTable
	known bool
}

func (env Env) processView(ctx context.Context) (processView, []runx.Diagnostic) {
	lister := env.Processes
	if lister == nil {
		lister = plugingc.PSProcesses
	}
	table, err := lister(ctx)
	if err != nil {
		return processView{}, []runx.Diagnostic{warning(DiagLivenessUnavailable, "process table unreadable, no session is judged dead: "+err.Error())}
	}
	return processView{table: table, known: true}, nil
}

// classify mirrors plugingc's proven-dead rule (internal/plugingc/procs.go,
// processView.classify): kill(pid, 0) succeeding proves nothing because
// macOS recycles PIDs, so a live PID counts as the session only when its
// process could be Claude Code and did not start after the session's own
// recorded start. Every undecidable case is alive or unknown, never dead.
func (v processView) classify(pid int, procStart, domain, goos string, loc *time.Location) Liveness {
	if !v.known || domain != "" && domain != goos {
		return LivenessUnknown
	}
	process, running := v.table[pid]
	if !running {
		return LivenessGone
	}
	if !couldBeClaude(process.Command) {
		return LivenessForeign
	}
	if latest, ok := latestProcStart(procStart, loc); ok && !process.Started.IsZero() && process.Started.After(latest.Add(startGrace)) {
		return LivenessRecycled
	}
	return LivenessAlive
}

// couldBeClaude answers generously, as plugingc's couldHoldPlugins does:
// Claude Code runs as `claude` here but has shipped inside JS runtimes, and
// calling one of those foreign would mark a live session dead.
func couldBeClaude(command string) bool {
	base := strings.ToLower(filepath.Base(command))
	if strings.Contains(base, "claude") {
		return true
	}
	switch base {
	case "node", "bun", "deno", "npx", "bunx":
		return true
	}
	return false
}

// latestProcStart reads the registry's procStart ("Fri Oct  2 06:53:54
// 2026"), which carries no zone: Claude Code 2.1.28x writes it in UTC while
// ps reports lstart in local time, so on a Mac at UTC+2 a live session's own
// process "started" two hours after its record. The stamp is read both ways
// and the LATER instant is the bound — a process is recycled only if it
// started after every reading, so the zone can only ever make a verdict
// alive, never dead.
func latestProcStart(value string, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.Local
	}
	stamp := strings.Join(strings.Fields(value), " ")
	utc, errUTC := time.ParseInLocation(procStartLayout, stamp, time.UTC)
	local, errLocal := time.ParseInLocation(procStartLayout, stamp, loc)
	if errUTC != nil || errLocal != nil {
		return time.Time{}, false
	}
	if local.After(utc) {
		return local, true
	}
	return utc, true
}

const procStartLayout = "Mon Jan 2 15:04:05 2006"
