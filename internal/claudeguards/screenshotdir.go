package claudeguards

import (
	"fmt"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// browser:screenshot-dir — every browser-MCP screenshot lands in .vitrinka/mcp.
//
// The two browser MCPs write files differently. Playwright resolves a relative
// `filename` against its --output-dir, which the onyx wrapper pins to
// .vitrinka/mcp (onyx deploy/install-mcp-browsers.sh). chrome-devtools has no
// such knob: `filePath` is resolved against the cwd, so a bare "home.png" lands
// in the repo root — three of those sat untracked in voke-platform on
// 2026-09-19. The directory is ignored once, globally (~/.config/git/ignore),
// and is the single place a session's raw captures accumulate; the curated
// copy is whatever vitrinka publishes.
// ---------------------------------------------------------------------------

// ScreenshotDir is the one directory, relative to the cwd, that browser-MCP
// screenshots may be written to.
const ScreenshotDir = ".vitrinka/mcp"

const (
	toolPlaywrightScreenshot = "mcp__playwright__browser_take_screenshot"
	toolDevtoolsScreenshot   = "mcp__plugin_chrome-devtools-mcp_chrome-devtools__take_screenshot"
)

// screenshotDir refuses a screenshot call whose target file is outside
// ScreenshotDir. A call with no path (inline image) is always fine.
func screenshotDir(in *HookInput) *Denial {
	var path, param string
	switch in.ToolName {
	case toolPlaywrightScreenshot:
		path, param = in.ToolInput.Filename, "filename"
		// Relative names are resolved by the wrapper's --output-dir, which is
		// already ScreenshotDir; only an absolute path can escape it.
		if path == "" || !filepath.IsAbs(path) {
			return nil
		}
	case toolDevtoolsScreenshot:
		path, param = in.ToolInput.FilePathCamel, "filePath"
		if path == "" {
			return nil
		}
	default:
		return nil
	}
	if insideScreenshotDir(path, in.CWD) {
		return nil
	}
	return deny("browser:screenshot-dir",
		fmt.Sprintf(`%s would write %s outside %s/. Browser-MCP screenshots have one
home per repo — that directory is ignored globally and never committed; a
bare filename lands in the repo root as an untracked file.`, in.ToolName, path, ScreenshotDir),
		fmt.Sprintf(`Write it there instead:
  %s: %q
or omit %s to get the image inline without a file.`, param, filepath.Join(ScreenshotDir, filepath.Base(path)), param))
}

// insideScreenshotDir reports whether path (relative to cwd, or absolute) is
// ScreenshotDir itself or below it. A cwd of "" treats absolute paths as
// outside — the hook always carries one, so that case never allows by accident.
func insideScreenshotDir(path, cwd string) bool {
	if filepath.IsAbs(path) {
		if cwd == "" {
			return false
		}
		rel, err := filepath.Rel(cwd, path)
		if err != nil {
			return false
		}
		path = rel
	}
	clean := filepath.Clean(path)
	return clean == ScreenshotDir || strings.HasPrefix(clean, ScreenshotDir+string(filepath.Separator))
}
