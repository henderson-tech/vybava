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
