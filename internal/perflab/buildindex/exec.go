package buildindex

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
)

// The external-process seam is perflab's one runner (internal/perflab/
// hostexec): each child in its own process group, a hard timeout and a
// no-output stall watchdog that stop exactly that group.
type (
	Cmd      = hostexec.Cmd
	Result   = hostexec.Result
	Runner   = hostexec.Runner
	Progress = hostexec.Progress
)

// phase records a phase change on a possibly nil Progress.
func phase(p *Progress, name string) {
	if p != nil {
		p.Phase(name)
	}
}

// heartbeat prints `still phase=<p>` every interval until stop is called.
func heartbeat(p *Progress, every time.Duration) (stop func()) {
	if p == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go p.Heartbeat(ctx, every, nil)
	return cancel
}

// toolInstall is the install line TOOL_MISSING names for each external tool.
var toolInstall = map[string]string{
	"xcrun":      "xcode-select --install (and install Xcode from the App Store)",
	"xcodebuild": "install Xcode, then sudo xcode-select -s /Applications/Xcode.app",
	"codesign":   "xcode-select --install",
	"plutil":     "xcode-select --install",
	"ditto":      "ditto ships with macOS; run on a Mac",
	"security":   "security ships with macOS; run on a Mac",
	"adb":        "brew install --cask android-platform-tools",
	"aapt2":      "sdkmanager 'build-tools;36.0.0'",
	"zipalign":   "sdkmanager 'build-tools;36.0.0'",
	"apksigner":  "sdkmanager 'build-tools;36.0.0'",
	"keytool":    "brew install openjdk@21",
	"java":       "brew install openjdk@21",
	"bunx":       "brew install oven-sh/bun/bun",
	"bun":        "brew install oven-sh/bun/bun",
	"git":        "xcode-select --install",
	"node":       "brew install node",
}

// run is Runner.Run with a start failure mapped to TOOL_MISSING.
func run(ctx context.Context, r Runner, c Cmd) (Result, error) {
	res, err := r.Run(ctx, c)
	if err != nil {
		tool := baseName(c.Argv[0])
		if hostexec.NotFound(err) {
			fix := toolInstall[tool]
			if fix == "" {
				fix = "install " + tool + " and put it on PATH"
			}
			return res, diag(DiagToolMissing, fmt.Sprintf("%s is not installed or not on PATH", tool), fix)
		}
		return res, fmt.Errorf("start %s: %w", tool, err)
	}
	return res, nil
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// lastLines is the trimmed tail of a command's output for a diagnostic.
func lastLines(b []byte, n int) string {
	lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.TrimSpace(strings.Join(lines, " | "))
}

// envNames is the sorted, de-duplicated name list of KEY=VALUE entries.
func envNames(env []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
