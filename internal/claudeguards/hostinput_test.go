package claudeguards

import "testing"

func TestHostInputPatterns(t *testing.T) {
	block := []string{
		"cliclick c:100,200",
		"sleep 1; cliclick c:1,2",
		"sudo cliclick c:1,2",
		`arch -x86_64 osascript -e 'tell application "System Events" to keystroke "a"'`,
		`osascript -e 'tell application "Simulator" to activate'`,
		`osascript -e 'tell application "System Events" to click at {1, 2}'`,
		`osascript -e 'tell application "System Events" to keystroke "a"'`,
	}
	pass := []string{
		"xcrun simctl list devices booted",
		"bunx tsx appium/adhoc/__dump-tree.ts",
		"echo cliclick-is-banned",
		`osascript -e 'display notification "done"'`,
		"grep -rn cliclick scripts/",
		`rg -n 'osascript.*System Events' skills/`,
		`rg -e osascript -e 'System Events' skills/`,
		"git commit -m \"guards: block host input (cliclick)\"",
	}
	for _, c := range block {
		if !hostInputMatch(c) {
			t.Errorf("should block %q", c)
		}
	}
	for _, c := range pass {
		if hostInputMatch(c) {
			t.Errorf("should pass %q", c)
		}
	}
}

// A quoted mention can never execute (B2); a leading environment assignment
// must not walk past the rule (F1).
func TestHostInputSegmentScoped(t *testing.T) {
	if !hostInputMatch("FOO=1 cliclick c:1,2") {
		t.Error("an environment assignment must not disarm the cliclick ban")
	}
	if hostInputMatch(`git commit -m "guards: ban osascript System Events keystroke"`) {
		t.Error("a commit message mentioning the rule must not fire it")
	}
}
