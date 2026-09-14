package claudeguards

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// /e2e skill guards (previously inline bash in ~/.claude/settings.json):
//   - Bash: block raw `xcrun simctl io … screenshot` and `screencapture` when
//     the session dir is an /e2e workspace (has a .e2e dir) — screenshots must
//     go through `snap`, which downsizes to JPEG.
//   - Read: block reading raw PNGs under .e2e/ — huge, context-hostile.
// ---------------------------------------------------------------------------

// The `xcrun` and `screencapture` words are identified by commandWord (see
// e2eScreenshotMatch), so these only have to recognise the rest of the argv.
var reRawSimShot = regexp.MustCompile(`simctl[[:space:]]+io[[:space:]][^|;&]*screenshot`)

// e2eScreenshotMatch returns "raw-screenshot", "screencapture" or "". Pure —
// unit-testable. Matching runs per segment of the command, skipping segments
// that merely mention text, so a quoted mention (an echoed warning, a commit
// message, `grep -rn screencapture`) cannot fire a rule, and a leading `FOO=1`
// cannot disarm one: commandWord sees through environment assignments.
func e2eScreenshotMatch(cmd string) string {
	// The downsizing exemptions are pipeline-wide: `… screenshot - | sips -Z`
	// puts sips in a later segment, and it still makes the shot cheap.
	exempt := strings.Contains(cmd, "sips -Z") || strings.Contains(cmd, "snap ")
	for _, seg := range segments(cmd) {
		if textOnly(seg) {
			continue
		}
		switch commandWord(seg) {
		case "xcrun":
			if !exempt && reRawSimShot.MatchString(seg) {
				return "raw-screenshot"
			}
		case "screencapture":
			return "screencapture"
		}
	}
	return ""
}

func guardE2EScreenshot(in *HookInput) *Denial {
	dir := in.CWD
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if st, err := os.Stat(filepath.Join(dir, ".e2e")); err != nil || !st.IsDir() {
		return nil
	}
	switch e2eScreenshotMatch(in.ToolInput.Command) {
	case "raw-screenshot":
		return deny("e2e:raw-screenshot", "raw xcrun screenshot — use: source ~/.claude/skills/e2e/references/snap.sh && snap <label>", "")
	case "screencapture":
		return deny("e2e:screencapture", "screencapture is banned by /e2e — use snap", "")
	}
	return nil
}

func containsE2E(p string) bool { return strings.Contains(p, ".e2e") }
func hasPNG(p string) bool      { return strings.HasSuffix(p, ".png") }

func guardE2ERead(in *HookInput) *Denial {
	path := in.ToolInput.FilePath
	if containsE2E(path) && hasPNG(path) {
		return deny("e2e:raw-png-read", "do not Read raw PNG screenshots under .e2e/ — use snap to make a JPEG first", "")
	}
	return nil
}
