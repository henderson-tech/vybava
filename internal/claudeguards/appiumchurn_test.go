package claudeguards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const churnScript = `import { remote } from 'webdriverio';
const driver = await remote({ capabilities: {} });
await driver.saveScreenshot('/tmp/shot.png');
await driver.deleteSession();
`

const requireChurnScript = `const { remote } = require('webdriverio');
(async () => { const d = await remote({}); await d.deleteSession(); })();
`

// churnFixture writes the same session-churning script at every path and
// returns the repo root.
func churnFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, src := range files {
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestAppiumChurnPatterns(t *testing.T) {
	root := churnFixture(t, map[string]string{
		"appium/adhoc/__whereami-shot.ts": churnScript,
		"scripts/probe.js":                requireChurnScript,
		"appium/adhoc/lib/driver.ts":      churnScript,
		"appium/support/session.ts":       churnScript,
		"appium/specs/login.ts":           churnScript,
		"appium/flows/home.spec.ts":       churnScript,
		"e2e/fixtures/wdio.ts":            churnScript,
		"tools/probes/keep.ts":            churnScript,
		"appium/adhoc/tree.ts":            "import { remote } from 'webdriverio';\nexport const open = () => remote({});\n",
		"appium/adhoc/noop.ts":            "console.log('no session here'); deleteSession();\n",
	})
	block := []string{
		"bunx tsx appium/adhoc/__whereami-shot.ts",
		"tsx appium/adhoc/__whereami-shot.ts",
		"bun appium/adhoc/__whereami-shot.ts",
		"bun run appium/adhoc/__whereami-shot.ts",
		"node scripts/probe.js",
		"npx tsx appium/adhoc/__whereami-shot.ts",
		"timeout 90 bunx tsx appium/adhoc/__whereami-shot.ts",
		"bunx tsx " + filepath.Join(root, "appium/adhoc/__whereami-shot.ts"),
		"UDID=abc bunx tsx appium/adhoc/__whereami-shot.ts --persona customer",
		"sleep 15; bunx tsx appium/adhoc/__whereami-shot.ts",
		"bunx tsx appium/adhoc/__whereami-shot.ts && open /tmp/shot.png",
	}
	pass := []string{
		"bunx tsx appium/adhoc/lib/driver.ts",
		"bunx tsx appium/support/session.ts",
		"bunx tsx appium/specs/login.ts",
		"bunx tsx appium/flows/home.spec.ts",
		"bunx tsx e2e/fixtures/wdio.ts",
		"bunx tsx appium/adhoc/tree.ts",
		"bunx tsx appium/adhoc/noop.ts",
		"bunx tsx appium/adhoc/missing.ts",
		"bun run test",
		"bun test appium/adhoc/__whereami-shot.ts",
		"cat appium/adhoc/__whereami-shot.ts",
		"echo bunx tsx appium/adhoc/__whereami-shot.ts",
		`git commit -m "remove bunx tsx appium/adhoc/__whereami-shot.ts"`,
		"xcrun simctl io booted screenshot /tmp/shot.png",
	}
	for _, c := range block {
		if got := appiumChurnMatch(c, root, nil); got == "" {
			t.Errorf("should block %q", c)
		}
	}
	for _, c := range pass {
		if got := appiumChurnMatch(c, root, nil); got != "" {
			t.Errorf("should pass %q (matched %s)", c, got)
		}
	}
	// guards.appiumSessionDirs extends the allowlist with the same shapes.
	if got := appiumChurnMatch("bunx tsx tools/probes/keep.ts", root, []string{"tools/probes/"}); got != "" {
		t.Errorf("configured subtree should pass, matched %s", got)
	}
	if got := appiumChurnMatch("bunx tsx tools/probes/keep.ts", root, []string{"tools/**/*.ts"}); got != "" {
		t.Errorf("configured glob should pass, matched %s", got)
	}
}

func TestAppiumChurnDenialText(t *testing.T) {
	root := churnFixture(t, map[string]string{"appium/adhoc/__whereami-shot.ts": churnScript})
	in := &HookInput{CWD: root}
	in.ToolInput.Command = "bunx tsx appium/adhoc/__whereami-shot.ts"
	d := guardAppiumChurn(in)
	if d == nil || d.Rule != "simulator:appium-session-churn" {
		t.Fatalf("denial: %v", d)
	}
	for _, want := range []string{
		"appium/adhoc/__whereami-shot.ts", "xcodebuild", "xcrun simctl io <udid> screenshot",
		"appium/adhoc/lib/driver.ts", "guards.appiumSessionDirs",
	} {
		if !strings.Contains(d.Text(), want) {
			t.Errorf("stderr must carry %q:\n%s", want, d.Text())
		}
	}
}

// The per-repo config reaches the rule through the same loader as noRead.
func TestAppiumChurnConfigDirs(t *testing.T) {
	root := churnFixture(t, map[string]string{"tools/probes/keep.ts": churnScript})
	in := &HookInput{CWD: root}
	in.ToolInput.Command = "bunx tsx tools/probes/keep.ts"
	if d := guardAppiumChurn(in); d == nil {
		t.Fatal("unconfigured tree must block")
	}
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(`{"guards":{"appiumSessionDirs":["tools/probes/"]}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := guardAppiumChurn(&HookInput{CWD: root, ToolInput: in.ToolInput}); d != nil {
		t.Fatalf("configured tree must pass: %v", d)
	}
}
