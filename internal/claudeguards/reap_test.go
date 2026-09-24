package claudeguards

import "testing"

func TestSelectReapVictims(t *testing.T) {
	got := map[int]string{}
	for _, v := range selectReapVictims(fakeTable) {
		got[v.pid] = reapKind(v)
	}
	// 400 (orphan xcodebuild, 2 h), 401 (orphan appium, 19 h), 402 (WDA
	// runner under the orphan) are reaped; 201 has a live codex owner and is
	// 5 min old, 501 has a live claude owner.
	want := map[int]string{400: "xcodebuild", 401: "appium", 402: "WebDriverAgent"}
	if len(got) != len(want) {
		t.Fatalf("victims %v want %v", got, want)
	}
	for pid, kind := range want {
		if got[pid] != kind {
			t.Errorf("pid %d: %q want %q", pid, got[pid], kind)
		}
	}
	// The same orphan under ten minutes is left alone: a fresh probe may
	// still be starting up.
	young := []machineProc{{pid: 9, ppid: 1, etime: "05:00", args: "xcodebuild test-without-building -scheme WebDriverAgentRunner"}}
	if v := selectReapVictims(young); len(v) != 0 {
		t.Errorf("young orphan reaped: %v", v)
	}
}

// The executable decides the kind: a process that merely mentions appium or
// WebDriverAgent in its arguments is not a victim, however old and unowned.
func TestReapKindNeedsTheExecutable(t *testing.T) {
	for _, args := range []string{
		"tail -f /private/tmp/appium-14006.log",
		"vim WebDriverAgentRunner.md",
		"grep -r appium /w/appium",
		"node /w/scripts/report.js --input appium/results.json",
		"xcodebuild test-without-building -scheme MyAppTests",
		"xcodebuild test-without-building -scheme MyAppTests -resultBundlePath /tmp/WebDriverAgent-compare",
		"xcodebuild -scheme WebDriverAgentRunner build-for-testing",
		"node -r appium ./build.js",
		"node --experimental-loader appium/x main.js",
		"node appium",
		// An in-process driver host holds runners but is never reaped.
		"node /Users/x/.codex/appium-mcp/node_modules/appium-mcp/dist/index.js",
	} {
		if k := reapKind(machineProc{pid: 1, args: args}); k != "" {
			t.Errorf("%q classified as %q", args, k)
		}
	}
	for args, want := range map[string]string{
		"node /w/node_modules/.bin/appium --port 14006":                                    "appium",
		"bun /w/node_modules/appium/build/lib/main.js":                                     "appium",
		"appium --port 4723":                                                               "appium",
		"/Users/x/Library/Developer/Xcode/DerivedData/WebDriverAgentRunner-Runner":         "WebDriverAgent",
		"/Users/x/DerivedData/WebDriverAgentRunner-Runner.app/WebDriverAgentRunner-Runner": "WebDriverAgent",
		"xcodebuild test-without-building -project WebDriverAgent.xcodeproj":               "xcodebuild",
	} {
		if k := reapKind(machineProc{pid: 1, args: args}); k != want {
			t.Errorf("%q classified as %q want %q", args, k, want)
		}
	}
}

// On a simulator the WebDriverAgent runner is a child of that simulator's
// launchd_sim, never of the xcodebuild or Appium server driving it, so no
// session is ever its ancestor. These helpers build that real shape.
const (
	liveSim     = "5F1B2C3D-0000-4000-8000-00000000A001"
	orphanSim   = "5F1B2C3D-0000-4000-8000-00000000A002"
	preinstSim  = "5F1B2C3D-0000-4000-8000-00000000A003"
	strandedSim = "5F1B2C3D-0000-4000-8000-00000000A004"
	mcpSim      = "5F1B2C3D-0000-4000-8000-00000000A005"
)

func launchdSim(pid int, udid string) machineProc {
	return machineProc{pid: pid, ppid: 1, etime: "03:00:00", tty: "??",
		args: "/Library/Developer/PrivateFrameworks/CoreSimulator.framework/Resources/bin/launchd_sim /Users/x/Library/Developer/CoreSimulator/Devices/" + udid + "/data/var/run/launchd_bootstrap.plist"}
}

func simRunner(pid, ppid int, udid string) machineProc {
	return machineProc{pid: pid, ppid: ppid, etime: "45:00", tty: "??",
		args: "/Users/x/Library/Developer/CoreSimulator/Devices/" + udid + "/data/Containers/Bundle/Application/0C1D2E3F-0000-4000-8000-000000000001/WebDriverAgentRunner-Runner.app/WebDriverAgentRunner-Runner"}
}

func wdaXcodebuildProc(pid, ppid int, etime, destination string) machineProc {
	return machineProc{pid: pid, ppid: ppid, etime: etime, tty: "??",
		args: "xcodebuild build-for-testing test-without-building -project /w/WebDriverAgent.xcodeproj -scheme WebDriverAgentRunner -destination " + destination + " IPHONEOS_DEPLOYMENT_TARGET=26.0"}
}

// liveLane is a long Appium lane: a claude session's Appium server runs the
// xcodebuild that drives liveSim's runner, and a preinstalled runner on
// preinstSim, launched through simctl with no xcodebuild at all.
var liveLane = []machineProc{
	{pid: 100, ppid: 1, etime: "05:00:00", tty: "ttys005", args: "claude"},
	{pid: 110, ppid: 100, etime: "50:00", tty: "ttys005", args: "node /w/node_modules/.bin/appium --port 4723"},
	wdaXcodebuildProc(111, 110, "49:00", "id="+liveSim),
	launchdSim(300, liveSim),
	simRunner(301, 300, liveSim),
	launchdSim(320, preinstSim),
	simRunner(321, 320, preinstSim),
}

// Another session's SessionStart must not take a live XCUITest session down.
func TestReapKeepsLiveWebDriverAgent(t *testing.T) {
	if v := selectReapVictims(liveLane); len(v) != 0 {
		t.Fatalf("live lane reaped: %v", v)
	}
}

// codexMCPLane is appium-mcp under a codex session, as it ran on 2026-09-24:
// the MCP server hosts XCUITestDriver in its own node process and launches
// its cached runner through simctl, so the table holds no xcodebuild and no
// standalone Appium server at all.
var codexMCPLane = []machineProc{
	{pid: 200, ppid: 1, etime: "02:40:00", tty: "??", args: "node /w/codex-plugin-cc/plugins/codex/scripts/app-server-broker.mjs serve"},
	{pid: 210, ppid: 200, etime: "02:30:00", tty: "??", args: "codex app-server"},
	{pid: 220, ppid: 210, etime: "02:30:00", tty: "??", args: "node /Users/x/.codex/appium-mcp/node_modules/appium-mcp/dist/index.js"},
	launchdSim(340, mcpSim),
	simRunner(341, 340, mcpSim),
}

// An in-process driver holds its runner. The sweep never takes the host, so
// it holds with no session above it too (an editor's MCP client, npx).
func TestReapKeepsInProcessDriverRunner(t *testing.T) {
	if v := selectReapVictims(codexMCPLane); len(v) != 0 {
		t.Fatalf("appium-mcp lane reaped: %v", v)
	}
	unowned := []machineProc{
		{pid: 220, ppid: 1, etime: "02:30:00", tty: "??", args: "node /Users/x/.npm/_npx/0a1b2c/node_modules/.bin/appium-mcp"},
		launchdSim(340, mcpSim),
		simRunner(341, 340, mcpSim),
	}
	if v := selectReapVictims(unowned); len(v) != 0 {
		t.Fatalf("runner of an unowned appium-mcp reaped: %v", v)
	}
}

// A stale orphaned xcodebuild naming the simulator never outranks a live
// in-process host: the orphan goes, the runner the host drives stays.
func TestReapKeepsInProcessRunnerDespiteStaleXcodebuild(t *testing.T) {
	table := []machineProc{
		{pid: 220, ppid: 1, etime: "02:30:00", tty: "??", args: "node /Users/x/.npm/_npx/0a1b2c/node_modules/.bin/appium-mcp"},
		wdaXcodebuildProc(400, 1, "02:10:00", "platform=iOS Simulator,id="+mcpSim),
		launchdSim(340, mcpSim),
		simRunner(341, 340, mcpSim),
	}
	got := map[int]string{}
	for _, v := range selectReapVictims(table) {
		got[v.pid] = reapKind(v)
	}
	if len(got) != 1 || got[400] != "xcodebuild" {
		t.Fatalf("victims %v want only the orphaned xcodebuild 400", got)
	}
}

// The host is recognised by the script node/bun runs, never by a mention.
func TestXCUITestHostNeedsTheEntry(t *testing.T) {
	for args, want := range map[string]bool{
		"node /Users/x/.codex/appium-mcp/node_modules/appium-mcp/dist/index.js": true,
		"node /Users/x/.npm/_npx/0a1b2c/node_modules/.bin/appium-mcp":           true,
		"node /Users/x/.nvm/versions/node/v24.17.0/bin/appium-mcp":              true,
		"bun /w/node_modules/appium-mcp/dist/index.js":                          true,
		"bun --cwd /w /w/node_modules/appium-mcp/dist/index.js":                 true,
		"tail -f /Users/x/.codex/appium-mcp/artifacts/appium-mcp.log":           false,
		"node /w/scripts/report.js /w/node_modules/appium-mcp/dist/index.js":    false,
		"node -r /w/node_modules/appium-mcp/dist/index.js build.js":             false,
		"npm exec appium-mcp": false,
	} {
		if got := xcuitestHost(machineProc{pid: 1, args: args}); got != want {
			t.Errorf("%q: %v want %v", args, got, want)
		}
	}
}

// A runner whose driver is gone is still an orphan: the xcodebuild naming its
// simulator lost its Appium server, or nothing drives it at all.
func TestReapTakesOrphanWebDriverAgent(t *testing.T) {
	table := append(append([]machineProc{}, liveLane...),
		wdaXcodebuildProc(400, 1, "02:10:00", "platform=iOS Simulator,id="+orphanSim),
		launchdSim(310, orphanSim),
		simRunner(311, 310, orphanSim),
	)
	got := map[int]string{}
	for _, v := range selectReapVictims(table) {
		got[v.pid] = reapKind(v)
	}
	// The live Appium elsewhere does not hold 311: the xcodebuild naming its
	// simulator decides, and that one is orphaned.
	want := map[int]string{400: "xcodebuild", 311: "WebDriverAgent"}
	if len(got) != len(want) {
		t.Fatalf("victims %v want %v", got, want)
	}
	for pid, kind := range want {
		if got[pid] != kind {
			t.Errorf("pid %d: %q want %q", pid, got[pid], kind)
		}
	}
	// With no xcodebuild for its simulator and no Appium server alive,
	// nothing can be driving the runner.
	stranded := []machineProc{launchdSim(330, strandedSim), simRunner(331, 330, strandedSim)}
	if v := selectReapVictims(stranded); len(v) != 1 || v[0].pid != 331 {
		t.Errorf("stranded runner: %v", v)
	}
}
