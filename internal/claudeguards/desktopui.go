package claudeguards

import (
	"fmt"
	"strings"

	"github.com/henderson-tech/vybava/internal/shellseg"
)

// ---------------------------------------------------------------------------
// guardDesktopUITests — machine:desktop-ui-tests. A macOS UI test bundle run on
// the Mac the user works on: XCUITest synthesizes real mouse and keyboard
// events, and testmanagerd attaches an automation session to whatever app is
// in front. On 2026-09-27 a background `xcodebuild … build test` of
// SwitcherooBar ran its UI bundle, and the session's teardown crashed Warp
// (SIGSEGV in XCTAutomationSupport) together with every Claude session inside
// it. simulator:host-input bans the same thing done with cliclick; this is
// the xcodebuild/tuist door. Simulator and device destinations pass: they do
// not drive the host's cursor.
// ---------------------------------------------------------------------------

var desktopUITestRunners = map[string]bool{"xcodebuild": true, "tuist": true, "xcrun": true}

// xcodebuild options whose value is the next token, so a scheme named "test"
// is never read as the test action.
var xcodebuildValueFlags = map[string]bool{
	"-scheme": true, "-workspace": true, "-project": true, "-target": true,
	"-destination": true, "-configuration": true, "-sdk": true, "-arch": true,
	"-derivedDataPath": true, "-resultBundlePath": true, "-testPlan": true,
	"-xctestrun": true, "-xcconfig": true, "-testProductsPath": true, "-toolchain": true,
	"-only-testing": true, "-skip-testing": true,
}

// desktopUITestMatch returns the first local xcodebuild/tuist test run in cmd
// that targets this Mac without narrowing itself to non-UI bundles, or "".
// Pure — unit-testable.
func desktopUITestMatch(cmd string) string {
	for _, seg := range shellseg.LocalSegments(cmd) {
		if textOnly(seg) {
			continue
		}
		argv := chainCommand(seg, desktopUITestRunners)
		if argv == nil {
			continue
		}
		if argv[0] == "xcrun" {
			for i, t := range argv {
				if t == "xcodebuild" {
					argv = argv[i:]
					break
				}
			}
		}
		hit := false
		switch argv[0] {
		case "xcodebuild":
			hit = xcodebuildDesktopUI(argv[1:])
		case "tuist":
			hit = tuistDesktopUI(argv[1:])
		}
		if hit {
			return strings.Join(argv, " ")
		}
	}
	return ""
}

func xcodebuildDesktopUI(args []string) bool {
	test := false
	var dests, sdks, only, skip []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "-only-testing:"):
			only = append(only, strings.TrimPrefix(a, "-only-testing:"))
		case strings.HasPrefix(a, "-skip-testing:"):
			skip = append(skip, strings.TrimPrefix(a, "-skip-testing:"))
		case xcodebuildValueFlags[a] && i+1 < len(args):
			i++
			switch a {
			case "-destination":
				dests = append(dests, args[i])
			case "-sdk":
				sdks = append(sdks, args[i])
			case "-only-testing":
				only = append(only, args[i])
			case "-skip-testing":
				skip = append(skip, args[i])
			}
		case a == "test" || a == "test-without-building":
			test = true
		}
	}
	return test && xcodebuildOnMac(dests, sdks) && !narrowedToUnit(only, skip)
}

// xcodebuildOnMac: a macOS or Mac Catalyst destination, or no destination and
// no non-macOS SDK (xcodebuild then tests on this Mac).
func xcodebuildOnMac(dests, sdks []string) bool {
	if len(dests) == 0 {
		for _, s := range sdks {
			if !strings.HasPrefix(strings.ToLower(s), "macosx") {
				return false
			}
		}
		return true
	}
	for _, d := range dests {
		l := strings.ToLower(d)
		if strings.Contains(l, "platform=macos") || strings.Contains(l, "platform=os x") || strings.Contains(l, "variant=mac catalyst") {
			return true
		}
	}
	return false
}

func tuistDesktopUI(args []string) bool {
	if len(args) == 0 || args[0] != "test" {
		return false
	}
	skipUI, device := false, false
	var platforms, targets, skip []string
	// values collects an option's values: --opt=v, or every token up to the next option.
	values := func(i int, a, opt string, into *[]string) (int, bool) {
		if v, ok := strings.CutPrefix(a, opt+"="); ok {
			*into = append(*into, v)
			return i, true
		}
		if a != opt {
			return i, false
		}
		for i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
			*into = append(*into, args[i])
		}
		return i, true
	}
	for i := 1; i < len(args); i++ {
		a := args[i]
		var ok bool
		switch {
		case a == "--skip-ui-tests":
			skipUI = true
		case a == "--no-skip-ui-tests":
			skipUI = false
		case a == "-d" || a == "--device" || strings.HasPrefix(a, "--device="):
			device = true
		default:
			if i, ok = values(i, a, "--platform", &platforms); ok {
				continue
			}
			if i, ok = values(i, a, "--test-targets", &targets); ok {
				continue
			}
			i, _ = values(i, a, "--skip-test-targets", &skip)
		}
	}
	onMac := len(platforms) == 0 && !device
	for _, p := range platforms {
		onMac = onMac || strings.EqualFold(p, "macos")
	}
	return onMac && !skipUI && !narrowedToUnit(targets, skip)
}

// narrowedToUnit: a UI bundle is skipped, or every selected bundle is a
// non-UI one. UI bundles are recognised by Xcode's `…UITests` naming.
func narrowedToUnit(only, skip []string) bool {
	for _, s := range skip {
		if namesUIBundle(s) {
			return true
		}
	}
	if len(only) == 0 {
		return false
	}
	for _, o := range only {
		if namesUIBundle(o) {
			return false
		}
	}
	return true
}

func namesUIBundle(id string) bool {
	bundle, _, _ := strings.Cut(id, "/")
	return strings.Contains(strings.ToLower(bundle), "uitests")
}

const desktopUITestsMsg = `%s

can run a UI test bundle on this Mac. XCUITest drives the real cursor and
keyboard focus of the Mac the user is working on, and its automation session
attaches to whatever app is in front: on 2026-09-27 a SwitcherooBar UI test
run's teardown crashed Warp (SIGSEGV in XCTAutomationSupport) and every Claude
session running in it.

Run the unit bundle only:
    xcodebuild … test -only-testing:<UnitTestTarget>
    xcodebuild … test -skip-testing:<UITestTarget>
    tuist test --skip-ui-tests
UI tests run when the user has said the Mac is free for them.`

const desktopUITestsEscape = "The user has said the Mac is free for a UI test run: CLAUDE_GUARDS_ALLOW_DESKTOP_UI=1 <command>"

func guardDesktopUITests(in *HookInput) *Denial {
	cmd := in.ToolInput.Command
	if cmd == "" || escapeHatch(cmd, "CLAUDE_GUARDS_ALLOW_DESKTOP_UI") {
		return nil
	}
	if argv := desktopUITestMatch(cmd); argv != "" {
		return deny("machine:desktop-ui-tests", fmt.Sprintf(desktopUITestsMsg, argv), desktopUITestsEscape)
	}
	return nil
}
