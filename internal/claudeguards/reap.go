package claudeguards

// Orphan reaper — SessionEnd hook, and SessionStart through `weather --reap`.
//
// A `timeout N bunx tsx <probe>` that opens an Appium session leaves its
// xcodebuild (WebDriverAgent's `test-without-building`) and the Appium server
// behind when the timeout kills the script: on 2026-09-19 two orphaned
// xcodebuilds were 2 h and 19 h old and an Appium log held 18.5 MB of
// create/delete session pairs. Nothing owned them any more. This sweep kills
// the ones whose claude/codex ancestor is gone and that have lived past ten
// minutes; a process with a live owning session is never touched. A
// WebDriverAgent runner is judged by its driver instead (wdaDriven): on a
// simulator it is a child of launchd_sim, so no session is ever its ancestor.

import (
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"time"
)

const reapMinAge = 10 * 60

// The kinds reapKind names.
const (
	reapWDA        = "WebDriverAgent"
	reapXcodebuild = "xcodebuild"
	reapAppium     = "appium"
)

// reapKind names a process the reaper knows, or "" for everything else. The
// executable decides, never a substring of the whole argv: `tail -f
// appium.log`, an editor on WebDriverAgentRunner.md or a script that merely
// names an appium path must not qualify.
func reapKind(p machineProc) string {
	b := p.base()
	switch {
	case b == "WebDriverAgentRunner-Runner" || strings.Contains(p.exe(), "WebDriverAgentRunner-Runner.app/"):
		return reapWDA
	case b == "xcodebuild" && wdaXcodebuild(p.args):
		return reapXcodebuild
	case b == "appium" || ((b == "node" || b == "bun") && appiumEntry(p.args)):
		return reapAppium
	}
	return ""
}

const udidPattern = `[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}`

var (
	// simDevice finds the simulator a process runs in: everything inside one
	// lives under CoreSimulator's `Devices/<UDID>/data/`.
	simDevice = regexp.MustCompile(`/(` + udidPattern + `)/data/`)
	// destinationValue is xcodebuild's `-destination` value up to the next
	// flag; ps joins `platform=iOS Simulator,id=<UDID>` with its space.
	destinationValue = regexp.MustCompile(`(?:^| )-destination ((?:[^ ]| [^-])+)`)
	destinationID    = regexp.MustCompile(`(?:^|[ ,])id=(` + udidPattern + `)(?:$|[ ,])`)
)

// simulatorUDID is the simulator a process runs in, or "".
func simulatorUDID(args string) string {
	if m := simDevice.FindStringSubmatch(args); m != nil {
		return strings.ToUpper(m[1])
	}
	return ""
}

// destinationUDID is the simulator an xcodebuild targets through
// `-destination …id=<UDID>`, or "".
func destinationUDID(args string) string {
	v := destinationValue.FindStringSubmatch(args)
	if v == nil {
		return ""
	}
	if m := destinationID.FindStringSubmatch(v[1]); m != nil {
		return strings.ToUpper(m[1])
	}
	return ""
}

// wdaDriven reports whether a WebDriverAgent runner still has a driver this
// sweep keeps. On a simulator the runner is a child of the simulator's
// launchd_sim, never of the xcodebuild or Appium server driving it, so the
// session-ancestor rule alone reaped every live XCUITest session older than
// ten minutes whenever any other session started or ended (2026-09-24, twice
// in one Appium proof run). The first rule that finds a driver decides:
//
//  1. an xcodebuild, Appium or in-process driver (xcuitestHost) ancestor,
//     where the tree does tie the runner to its driver;
//  2. the WebDriverAgent xcodebuilds whose -destination names the runner's
//     simulator: any one kept holds it, all of them reaped take it along;
//  3. otherwise any kept Appium server or any in-process driver. A
//     preinstalled runner is launched through simctl with no xcodebuild at
//     all (appium-mcp always does this) and the table cannot say which
//     driver holds it, so an undecidable runner is held.
//
// orphaned is the sweep's own verdict on a driver, so a runner follows it; an
// in-process driver is never a victim, so it holds for as long as it lives.
func wdaDriven(runner machineProc, table []machineProc, byPID map[int]machineProc, orphaned func(machineProc) bool) bool {
	for cur, hops := runner.ppid, 0; cur > 1 && hops < 12; hops++ {
		p, ok := byPID[cur]
		if !ok {
			break
		}
		if k := reapKind(p); k == reapXcodebuild || k == reapAppium {
			return !orphaned(p)
		}
		if xcuitestHost(p) {
			return true
		}
		cur = p.ppid
	}
	if udid := simulatorUDID(runner.args); udid != "" {
		named := false
		for _, p := range table {
			if reapKind(p) != reapXcodebuild || destinationUDID(p.args) != udid {
				continue
			}
			if !orphaned(p) {
				return true
			}
			named = true
		}
		if named {
			// Only orphaned xcodebuilds name this simulator. An in-process
			// host (appium-mcp) launches its runner through simctl, never an
			// xcodebuild, so a stale orphan naming the same simulator must not
			// outrank a live host driving it now.
			for _, p := range table {
				if xcuitestHost(p) {
					return true
				}
			}
			return false
		}
	}
	for _, p := range table {
		if xcuitestHost(p) || (reapKind(p) == reapAppium && !orphaned(p)) {
			return true
		}
	}
	return false
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
	"--preload": true, "--conditions": true, "-C": true, "--cwd": true, "--define": true, "-d": true,
}

// nodeScript is the script a node/bun process runs: its first argument that
// is neither a flag nor a flag's value, or "".
func nodeScript(args string) string {
	fields := strings.Fields(args)
	for i := 1; i < len(fields); i++ {
		a := fields[i]
		if strings.HasPrefix(a, "-") {
			if nodeValueFlags[a] {
				i++
			}
			continue
		}
		return a
	}
	return ""
}

// appiumEntry reports whether a node/bun process runs the appium server
// entry point, by path: `<...>/.bin/appium` or the package's `main.js`.
func appiumEntry(args string) bool {
	s := nodeScript(args)
	return strings.HasSuffix(s, "/.bin/appium") || strings.HasSuffix(s, "/appium/build/lib/main.js")
}

// xcuitestHost reports a process that hosts XCUITestDriver in-process rather
// than behind an Appium server: appium-mcp (the Codex sessions' driver, run
// as `node <...>/appium-mcp/dist/index.js` or through its `appium-mcp` bin)
// launches its cached runner through simctl and drives it by
// webDriverAgentUrl. It is a WebDriverAgent driver, never a reap kind: an MCP
// server lives and dies with its client, which need not be a claude/codex
// session.
func xcuitestHost(p machineProc) bool {
	if b := p.base(); b != "node" && b != "bun" {
		return b == "appium-mcp"
	}
	s := nodeScript(p.args)
	return path.Base(s) == "appium-mcp" || strings.HasSuffix(s, "/appium-mcp/dist/index.js")
}

// selectReapVictims picks the orphans: a known kind, older than reapMinAge,
// with no live claude/codex ancestor in the table, and for a WebDriverAgent
// runner no driver the sweep keeps.
func selectReapVictims(table []machineProc) []machineProc {
	byPID := make(map[int]machineProc, len(table))
	for _, p := range table {
		byPID[p.pid] = p
	}
	orphaned := func(p machineProc) bool {
		sec, ok := etimeSeconds(p.etime)
		return ok && sec > reapMinAge && !ownedBySession(p.pid, byPID)
	}
	var out []machineProc
	for _, p := range table {
		k := reapKind(p)
		if k == "" || !orphaned(p) {
			continue
		}
		if k == reapWDA && wdaDriven(p, table, byPID, orphaned) {
			continue
		}
		out = append(out, p)
	}
	return out
}

// Reap is the SessionEnd hook entry point. Never blocks the session; exit
// stays 0.
func Reap(stderr io.Writer) {
	reapTable(machineProcTable(), stderr)
}

// reapTable sweeps an already-read table — SessionStart's `weather --reap`
// hands over the one it counted.
func reapTable(table []machineProc, stderr io.Writer) {
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
