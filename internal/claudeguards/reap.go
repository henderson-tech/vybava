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
	"path"
	"strings"
	"time"
)

const reapMinAge = 10 * 60

// reapKind names a process the reaper knows, or "" for everything else. The
// executable decides, never a substring of the whole argv: `tail -f
// appium.log`, an editor on WebDriverAgentRunner.md or a script that merely
// names an appium path must not qualify.
func reapKind(p machineProc) string {
	b := p.base()
	switch {
	case b == "WebDriverAgentRunner-Runner" || strings.Contains(p.exe(), "WebDriverAgentRunner-Runner.app/"):
		return "WebDriverAgent"
	case b == "xcodebuild" && wdaXcodebuild(p.args):
		return "xcodebuild"
	case b == "appium" || ((b == "node" || b == "bun") && appiumEntry(p.args)):
		return "appium"
	}
	return ""
}

// wdaXcodebuild reports an xcodebuild that runs WebDriverAgent's test bundle:
// the `test-without-building` action with the WebDriverAgent project, the
// WebDriverAgentRunner scheme or its xctestrun as the value of that flag — a
// user's own test run that merely mentions WebDriverAgent elsewhere does not
// qualify.
func wdaXcodebuild(args string) bool {
	fields := strings.Fields(args)
	action, wda := false, false
	for i := 1; i < len(fields); i++ {
		switch fields[i] {
		case "test-without-building":
			action = true
		case "-project", "-workspace", "-scheme", "-xctestrun":
			if i+1 < len(fields) && strings.HasPrefix(path.Base(fields[i+1]), "WebDriverAgent") {
				wda = true
			}
			i++
		}
	}
	return action && wda
}

// nodeValueFlags take the next token as their value, so that token is never
// the script (`node -r appium ./build.js` runs build.js).
var nodeValueFlags = map[string]bool{
	"-r": true, "--require": true, "--import": true, "--loader": true, "--experimental-loader": true,
	"-e": true, "--eval": true, "-p": true, "--print": true, "--input-type": true, "--env-file": true,
	"--preload": true, "--conditions": true, "-C": true, "--define": true, "-d": true,
}

// appiumEntry reports whether the script a node/bun process runs — its first
// argument that is neither a flag nor a flag's value — is the appium server
// entry point, by path: `<...>/.bin/appium` or the package's `main.js`.
func appiumEntry(args string) bool {
	fields := strings.Fields(args)
	for i := 1; i < len(fields); i++ {
		a := fields[i]
		if strings.HasPrefix(a, "-") {
			if nodeValueFlags[a] {
				i++
			}
			continue
		}
		return strings.HasSuffix(a, "/.bin/appium") || strings.HasSuffix(a, "/appium/build/lib/main.js")
	}
	return false
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
