package claudeguards

// Orphan reaper — SessionStart/SessionEnd hook.
//
// A `timeout N bunx tsx <probe>` that opens an Appium session leaves its
// xcodebuild (WebDriverAgent's `test-without-building`) and the Appium server
// behind when the timeout kills the script: on 2026-09-19 two orphaned
// xcodebuilds were 2 h and 19 h old and an Appium log held 18.5 MB of
// create/delete session pairs. Nothing owned them any more. This sweep kills
// the ones whose claude/codex ancestor is gone and that have lived past ten
// minutes; a process with a live owning session is never touched.

import (
	"fmt"
	"io"
	"strings"
	"time"
)

const reapMinAge = 10 * 60

// reapKind names a process the reaper knows, or "" for everything else.
func reapKind(p machineProc) string {
	b := p.base()
	switch {
	case strings.Contains(p.args, "WebDriverAgentRunner"):
		return "WebDriverAgent"
	case b == "xcodebuild" && strings.Contains(p.args, "test-without-building"):
		return "xcodebuild"
	case b == "appium" || ((b == "node" || b == "bun") && strings.Contains(p.args, "appium")):
		return "appium"
	}
	return ""
}

// selectReapVictims picks the orphans: a known kind, older than reapMinAge,
// with no live claude/codex ancestor in the table.
func selectReapVictims(table []machineProc) []machineProc {
	byPID := make(map[int]machineProc, len(table))
	for _, p := range table {
		byPID[p.pid] = p
	}
	var out []machineProc
	for _, p := range table {
		if reapKind(p) == "" {
			continue
		}
		if sec, ok := etimeSeconds(p.etime); !ok || sec <= reapMinAge {
			continue
		}
		if ownedBySession(p.pid, byPID) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Reap is the hook entry point. Never blocks the session; exit stays 0.
func Reap(stderr io.Writer) {
	table := machineProcTable()
	victims := selectReapVictims(table)
	if len(victims) == 0 {
		return
	}
	var pids []int
	for _, v := range victims {
		pids = append(pids, v.pid)
		pids = append(pids, descendants(v.pid)...)
	}
	for _, pid := range pids {
		terminate(pid)
	}
	time.Sleep(2 * time.Second)
	for _, pid := range pids {
		if pidAlive(pid) {
			kill(pid)
		}
	}
	for _, v := range victims {
		fmt.Fprintf(stderr, "claude-guards: reaped %s pid %d (%s, no owning session)\n", reapKind(v), v.pid, v.etime)
	}
}
