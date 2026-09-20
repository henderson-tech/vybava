package claudeguards

import (
	"strings"
	"testing"
)

func screenshotInput(tool, filename, filePath string) *HookInput {
	in := &HookInput{ToolName: tool, CWD: "/repo"}
	in.ToolInput.Filename = filename
	in.ToolInput.FilePathCamel = filePath
	return in
}

func TestScreenshotDirRefusesDevtoolsPathOutsideVitrinkaMCP(t *testing.T) {
	cases := map[string]bool{ // filePath → allowed
		"home.png":                       false, // the regression: lands in the repo root
		"shots/home.png":                 false,
		"/repo/home.png":                 false,
		".vitrinka/mcp/home.png":         true,
		"./.vitrinka/mcp/a/b.png":        true,
		"/repo/.vitrinka/mcp/x.png":      true,
		"/elsewhere/.vitrinka/mcp/x.png": false, // another repo's directory
		"":                               true,  // inline image, no file
	}
	for path, allowed := range cases {
		d := screenshotDir(screenshotInput(toolDevtoolsScreenshot, "", path))
		if (d == nil) != allowed {
			t.Errorf("filePath %q: allowed=%v, want %v", path, d == nil, allowed)
		}
		if d != nil && (d.Rule != "browser:screenshot-dir" || !strings.Contains(d.Text(), `filePath: ".vitrinka/mcp/`)) {
			t.Errorf("filePath %q: denial must name the sanctioned path:\n%s", path, d.Text())
		}
	}
}

func TestScreenshotDirPlaywrightRelativeNamesAreTheWrappersOutputDir(t *testing.T) {
	// The onyx wrapper pins --output-dir to .vitrinka/mcp, so a relative
	// filename already lands there; only an absolute path can escape.
	if d := screenshotDir(screenshotInput(toolPlaywrightScreenshot, "home.png", "")); d != nil {
		t.Errorf("a relative playwright filename must pass:\n%s", d.Text())
	}
	if d := screenshotDir(screenshotInput(toolPlaywrightScreenshot, "/repo/home.png", "")); d == nil {
		t.Error("an absolute playwright filename outside .vitrinka/mcp must be refused")
	}
	if d := screenshotDir(screenshotInput("mcp__playwright__browser_navigate", "", "")); d != nil {
		t.Error("non-screenshot browser tools are not this rule's business")
	}
}
