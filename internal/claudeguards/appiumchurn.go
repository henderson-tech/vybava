package claudeguards

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// ---------------------------------------------------------------------------
// simulator:appium-session-churn - an ad-hoc script that opens a webdriverio
// remote() session, looks once and deletes it again. Every fresh XCUITest
// session relaunches WebDriverAgent through xcodebuild: 5-10 s on every core
// and 1-2 GB, per run. In a look-think-tap loop that is a machine-wide burst
// every ~15 s; appium/adhoc/__whereami-shot.ts did exactly this for 4 hours on
// 2026-09-19, and a `timeout` wrapper around it only orphaned the xcodebuild
// while the loop retried. The repo's session factory (one session kept open
// for the task) and `xcrun simctl io … screenshot` are the sanctioned forms;
// the support and spec directories that already reuse sessions are allowed.
// ---------------------------------------------------------------------------

// scriptRunners are the words that execute a TS/JS file given as an argument.
var scriptRunners = map[string]bool{
	"bun": true, "bunx": true, "npx": true, "node": true, "tsx": true, "ts-node": true, "deno": true,
}

// scriptExts are the file suffixes a runner argument must carry to be read as
// a script; `bun run test` names a package script, not a file.
var scriptExts = []string{".ts", ".tsx", ".mts", ".cts", ".js", ".mjs", ".cjs"}

// defaultAppiumSessionDirs are the trees whose scripts are expected to hold a
// webdriverio session: the shared support and driver libraries, the specs,
// and the web e2e tree. A trailing slash means "anywhere under"; other entries
// are MatchNoRead globs. Both match at any directory depth, so a worktree or
// a nested package needs no configuration.
var defaultAppiumSessionDirs = []string{
	"appium/support/", "appium/adhoc/lib/", "appium/specs/", "appium/**/*.spec.ts", "e2e/",
}

// maxScriptBytes bounds the read; a script that opens a session is small, and
// anything larger is a bundle this rule was never meant to inspect.
const maxScriptBytes = 512 << 10

var (
	reWdioRemoteImport = regexp.MustCompile(`import\s*(?:type\s*)?\{[^}]*\bremote\b[^}]*\}\s*from\s*['"]webdriverio['"]`)
	reWdioRequire      = regexp.MustCompile(`require\(\s*['"]webdriverio['"]\s*\)`)
	reDeleteSession    = regexp.MustCompile(`\bdeleteSession\s*\(`)
)

// scriptArg returns the TS/JS file a runner invocation executes, or "".
func scriptArg(argv []string) string {
	i := 1
	switch argv[0] {
	case "bunx", "npx":
		// bunx tsx <file> / npx ts-node <file>: the tool comes first.
		for i < len(argv) && strings.HasPrefix(argv[i], "-") {
			i++
		}
		if i >= len(argv) || !scriptRunners[argv[i]] {
			return ""
		}
		i++
	case "bun", "deno":
		if i < len(argv) && argv[i] == "run" {
			i++
		}
	}
	for ; i < len(argv); i++ {
		a := argv[i]
		if strings.HasPrefix(a, "-") {
			continue
		}
		for _, ext := range scriptExts {
			if strings.HasSuffix(a, ext) {
				return a
			}
		}
		return ""
	}
	return ""
}

// inAppiumSessionDir reports whether abs sits under one of the allowlisted
// trees, matching each pattern against every suffix of the path so the list
// stays repo-relative without a repo root lookup.
func inAppiumSessionDir(abs string, dirs []string) bool {
	parts := strings.Split(filepath.ToSlash(abs), "/")
	for _, pattern := range dirs {
		for i := range parts {
			rel := strings.Join(parts[i:], "/")
			if strings.HasSuffix(pattern, "/") {
				if strings.HasPrefix(rel, pattern) {
					return true
				}
				continue
			}
			if MatchNoRead(pattern, rel) {
				return true
			}
		}
	}
	return false
}

// churnsAppiumSession reports whether the script at abs opens a webdriverio
// session and deletes it in the same file. An unreadable file is not a match.
func churnsAppiumSession(abs string) bool {
	f, err := os.Open(abs)
	if err != nil {
		return false
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return false
	}
	src, err := io.ReadAll(io.LimitReader(f, maxScriptBytes))
	if err != nil {
		return false
	}
	return (reWdioRemoteImport.Match(src) || reWdioRequire.Match(src)) && reDeleteSession.Match(src)
}

// appiumChurnMatch returns the offending script path, or "". dirs extends the
// default allowlist.
func appiumChurnMatch(cmd, cwd string, dirs []string) string {
	allow := append(append([]string{}, defaultAppiumSessionDirs...), dirs...)
	for _, seg := range segments(cmd) {
		if textOnly(seg) {
			continue
		}
		argv := chainCommand(seg, scriptRunners)
		if argv == nil {
			continue
		}
		script := scriptArg(argv)
		if script == "" {
			continue
		}
		abs := resolvePath(script, cwd)
		if inAppiumSessionDir(abs, allow) {
			continue
		}
		if churnsAppiumSession(abs) {
			return script
		}
	}
	return ""
}

const appiumChurnMsg = `%s opens a fresh webdriverio remote() session and deletes it before it exits.
Every new XCUITest session relaunches WebDriverAgent through xcodebuild: 5-10 s
on every core and 1-2 GB of RAM, per run. In a look-think-tap loop that is a
machine-wide burst every ~15 s (the appium/adhoc/__whereami-shot.ts storm ran
4 h on 2026-09-19), and a 'timeout' wrapper only orphans the xcodebuild while
the loop retries.

A screenshot needs no Appium at all:   xcrun simctl io <udid> screenshot <file>
The a11y tree goes through ONE session kept open for the whole task, via the
repo's session factory (FixIt: appium/adhoc/lib/driver.ts 'Driver',
appium/adhoc/lib/persona-session.ts) - never a remote() per look.
Once the script reuses a session, move it under an allowlisted tree
(appium/support/, appium/adhoc/lib/, appium/specs/, e2e/, or
guards.appiumSessionDirs in vybava.config.ts).`

func guardAppiumChurn(in *HookInput) *Denial {
	script := appiumChurnMatch(in.ToolInput.Command, in.CWD, in.guards().AppiumSessionDirs)
	if script == "" {
		return nil
	}
	// No escape hatch: the alternatives above ARE the task, cheaper.
	return deny("simulator:appium-session-churn", fmt.Sprintf(appiumChurnMsg, script), "")
}
