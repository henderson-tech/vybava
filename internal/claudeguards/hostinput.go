package claudeguards

import "regexp"

// ---------------------------------------------------------------------------
// guardHostInput — host-level mouse/keyboard automation is banned (ruled
// 2026-09-09, re-affirmed 2026-09-10): cliclick, AppleScript `System Events`
// clicks/keystrokes and `tell application "Simulator" to activate` steal the
// cursor and focus of the Mac Lukáš is working on, and they do not even
// register inside the Simulator window. iOS simulator UI is driven through
// the repo's Appium/XCUITest support instead (connectViaXcuitest, the
// appium/adhoc drivers, appium/support/ios-alerts.ts).
// ---------------------------------------------------------------------------

// Both commands are identified through commandChainHas (see hostInputMatch), so
// a privilege or arch wrapper in front of them still matches while a quoted
// mention does not. This regex only qualifies WHAT the osascript does.
var reOsascriptInput = regexp.MustCompile(
	`osascript[^;&|]*(System Events|tell application "Simulator" to activate|keystroke|click at)`)

const hostInputMsg = `Host-level input automation (cliclick, AppleScript System Events clicks/keystrokes,
activating the Simulator window) is banned — it hijacks the cursor and focus of the
Mac the user is working on, and it does not register inside the Simulator anyway.

Drive the iOS simulator through Appium/XCUITest instead:
  • open/attach the Expo dev client:  connectViaXcuitest / waitForPersonaReady
                                       (scripts/worktree/connect-dev-client.ts)
  • SpringBoard "Otevřít v aplikaci":   appium/support/ios-alerts.ts
  • ad-hoc taps/walkthroughs/dumps:     appium/adhoc/ (INDEX.md, lib/driver.ts)`

// hostInputMatch reports whether a command drives host-level input. Pure —
// unit-testable. Matching runs per segment, skipping segments that merely
// mention text, so a quoted mention (`grep -rn cliclick`, an echoed warning, a
// commit message) cannot fire it, and a leading `FOO=1` cannot disarm it:
// commandWord sees through environment assignments.
func hostInputMatch(cmd string) bool {
	for _, seg := range segments(cmd) {
		if textOnly(seg) {
			continue
		}
		if commandChainHas(seg, "cliclick") {
			return true
		}
		if commandChainHas(seg, "osascript") && reOsascriptInput.MatchString(seg) {
			return true
		}
	}
	return false
}

func guardHostInput(in *HookInput) *Denial {
	if hostInputMatch(in.ToolInput.Command) {
		// No escape hatch: this action is both harmful and useless — it steals
		// the user's cursor AND does not register in the Simulator. The message
		// used to advertise CLAUDE_ALLOW_DANGEROUS=1, which guardHostInput
		// never honoured; an agent that took the offer was denied twice.
		return deny("simulator:host-input", hostInputMsg, "")
	}
	return nil
}
