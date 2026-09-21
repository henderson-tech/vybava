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
		"xcodebuild -scheme WebDriverAgentRunner build-for-testing",
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
