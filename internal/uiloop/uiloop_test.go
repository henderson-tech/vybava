package uiloop

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/runx"
)

func testConfig() Config {
	return Config{
		Dir: "tests/ui-loop", Out: ".ui-loop", AppMap: "docs/app-map.md", Runner: "pnpm exec playwright test",
		Areas: []string{"tasks", "admin"},
		Apps: map[string]App{
			"portal": {BaseURL: "http://127.0.0.1:5500", Viewports: []string{"phone", "tablet", "desktop"}, Themes: []string{"light", "dark"}},
		},
		Vitrinka: Vitrinka{Project: "powerflow", BoardPrefix: "ui-polish"},
	}
}

func newTool(t *testing.T, cfg Config) *Tool {
	t.Helper()
	root := t.TempDir()
	tool, err := New(root, filepath.Join(root, "vybava.config.json"), cfg, "1.2.3", nil)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func diagCode(err error) string {
	var de runx.DiagError
	if errors.As(err, &de) {
		return de.Diag.Code
	}
	return ""
}

func TestConfigRejectsUnknownKeysAndReportsEveryProblem(t *testing.T) {
	root := t.TempDir()
	doc := `{"uiLoop": {"dir": "tests/ui-loop", "out": ".ui-loop", "appMap": "docs/app-map.md", "runner": "x",
		"areas": ["tasks"], "apps": {"portal": {"baseUrl": "http://x", "viewports": ["phone"], "themes": ["dark"]}},
		"vitrinka": {"project": "p", "boardPrefix": "b"}, "outDir": "typo"}}`
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(root, "test", nil)
	if diagCode(err) != DiagConfigInvalid || !strings.Contains(err.Error(), `unknown field "outDir"`) {
		t.Fatalf("an unknown key must be CONFIG_INVALID naming it, got %v", err)
	}

	bad := testConfig()
	bad.Dir = "../elsewhere"
	bad.Areas = []string{"Tasks", "Tasks"}
	bad.Apps = map[string]App{"portal": {BaseURL: "ftp://x", Env: "lower", Viewports: []string{"watch"}, Themes: []string{"sepia"}}}
	bad.Lint.Off = []string{"no-such-rule"}
	bad.Lint.Allow = map[string][]string{"grid": {"ui-button", " "}, "truncated": {".x"}, "nope": {".y"}, "contrast": {}}
	problems := strings.Join(bad.Validate(), "\n")
	for _, want := range []string{"dir \"../elsewhere\"", "not kebab-case", "listed twice", "not an http(s) URL", "not an env var",
		"unknown viewport \"watch\"", "\"sepia\" is not light or dark", "unknown rule \"no-such-rule\"",
		"lint.allow.grid holds an empty selector", "\"truncated\" is informational already", "lint.allow: unknown rule \"nope\"", "lint.allow.contrast must list at least one selector"} {
		if !strings.Contains(problems, want) {
			t.Errorf("Validate misses %q in:\n%s", want, problems)
		}
	}

	c := testConfig().WithDefaults()
	if c.Lint.Grid != 4 || c.Lint.TouchTarget != 44 || c.TSRunner != "npx --yes tsx" {
		t.Errorf("defaults: %+v %+v %q", c.Lint, c.Publish, c.TSRunner)
	}
	c.Runner, c.TSRunner = "bunx playwright test", ""
	if got := c.TSRunnerOrDefault(); got != "bun" {
		t.Errorf("a bun repo's tsRunner defaults to bun, got %q", got)
	}
}

// TestHarnessTablesMatchGo keeps the two hand-mirrored tables equal: the
// built-in viewports and the lint rule ids.
func TestHarnessTablesMatchGo(t *testing.T) {
	vp, err := harnessFS.ReadFile("harness/viewports.ts")
	if err != nil {
		t.Fatal(err)
	}
	line := regexp.MustCompile(`(?m)^\s+'?([a-z0-9-]+)'?: \{ width: (\d+), height: (\d+)(, mobile: true)?(?:, insets: \{ top: (\d+), right: (\d+), bottom: (\d+), left: (\d+) \})? \},$`)
	ts := map[string]Viewport{}
	for _, m := range line.FindAllStringSubmatch(string(vp), -1) {
		n := func(s string) int { v, _ := strconv.Atoi(s); return v }
		v := Viewport{Width: n(m[2]), Height: n(m[3]), Mobile: m[4] != ""}
		if m[5] != "" {
			v.Insets = &Insets{Top: n(m[5]), Right: n(m[6]), Bottom: n(m[7]), Left: n(m[8])}
		}
		ts[m[1]] = v
	}
	a, _ := json.Marshal(ts)
	b, _ := json.Marshal(BuiltinViewports)
	if string(a) != string(b) {
		t.Errorf("BUILTIN_VIEWPORTS (viewports.ts) and BuiltinViewports (viewports.go) differ:\nts %s\ngo %s", a, b)
	}

	lint, err := harnessFS.ReadFile("harness/lint.ts")
	if err != nil {
		t.Fatal(err)
	}
	block := string(lint)[strings.Index(string(lint), "export const LINT_RULES = {"):]
	block = block[:strings.Index(block, "} as const;")]
	var rules, infoRules []string
	for _, m := range regexp.MustCompile(`(?m)^\s+'?([a-z-]+)'?: '(defect|info)',$`).FindAllStringSubmatch(block, -1) {
		rules = append(rules, m[1])
		if m[2] == "info" {
			infoRules = append(infoRules, m[1])
		}
	}
	if !slices.Equal(infoRules, LintInfoRules) {
		t.Errorf("LINT_RULES info rules %v != LintInfoRules (config.go) %v", infoRules, LintInfoRules)
	}
	if !slices.Equal(rules, LintRules) {
		t.Errorf("LINT_RULES (lint.ts) %v != LintRules (config.go) %v", rules, LintRules)
	}
}

func TestSyncWritesStampAndCheckSeesEveryDriftKind(t *testing.T) {
	tool := newTool(t, testConfig())
	vendor := tool.VendorDir()
	res, err := tool.Sync(false)
	if err != nil {
		t.Fatal(err)
	}
	if rep := res.Data.(VendorReport); !rep.Clean || len(rep.Written) == 0 || !slices.Contains(rep.Written, "capture.ts") {
		t.Fatalf("first sync writes the harness: %+v", rep)
	}
	stamp, err := readStamp(vendor)
	if err != nil || stamp == nil || stamp.Vybava != "1.2.3" || len(stamp.Files["manifest.ts"]) != 64 {
		t.Fatalf("stamp: %+v %v", stamp, err)
	}
	if rep, _ := tool.Vendor(); !rep.Clean {
		t.Fatalf("freshly synced vendor is clean: %+v", rep)
	}

	// A newer binary with an identical harness keeps the stamp (no churn).
	tool.Version = "1.2.4"
	if _, err := tool.Sync(false); err != nil {
		t.Fatal(err)
	}
	if s, _ := readStamp(vendor); s.Vybava != "1.2.3" {
		t.Errorf("an unchanged harness must not rewrite the stamp, got %s", s.Vybava)
	}
	// So check names the stamp's release (syncedBy, omitted when empty) only
	// when it explains a drift.
	syncedBy := func() string {
		t.Helper()
		res, err := tool.Check(true)
		if err != nil {
			t.Fatal(err)
		}
		return res.Data.(*CheckData).Vendor.Synced
	}
	if s := syncedBy(); s != "" {
		t.Errorf("a clean vendor reports no syncedBy, got %q", s)
	}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(vendor, "lint.ts"), []byte("// edited in the repo\n"), 0o644))
	must(os.Remove(filepath.Join(vendor, "states.ts")))
	must(os.WriteFile(filepath.Join(vendor, "old.ts"), []byte("x"), 0o644))
	// "outdated": the file is what the stamp recorded, but this binary ships other bytes.
	stamp.Files["report.ts"] = sum([]byte("older harness\n"))
	b, _ := json.Marshal(stamp)
	must(os.WriteFile(filepath.Join(vendor, StampFile), b, 0o644))
	must(os.WriteFile(filepath.Join(vendor, "report.ts"), []byte("older harness\n"), 0o644))

	rep, err := tool.Vendor()
	must(err)
	states := map[string]string{}
	for _, f := range rep.Files {
		states[f.File] = f.State
	}
	want := map[string]string{"lint.ts": "edited", "states.ts": "missing", "old.ts": "extra", "report.ts": "outdated", "capture.ts": "ok"}
	for file, state := range want {
		if states[file] != state {
			t.Errorf("%s: state %q, want %q", file, states[file], state)
		}
	}
	if rep.Clean {
		t.Error("a drifted vendor is not clean")
	}
	check, err := tool.Check(true)
	must(err)
	var codes []string
	for _, d := range check.Diagnostics {
		codes = append(codes, d.Code)
	}
	if !slices.Contains(codes, DiagVendorDrift) || !slices.Contains(codes, DiagVendorEdited) || !slices.Contains(codes, DiagProjectMissing) {
		t.Errorf("check diagnostics %v", codes)
	}
	if s := syncedBy(); s != "1.2.3" {
		t.Errorf("a drifted vendor names the release that synced it, got %q", s)
	}

	if _, err := tool.Sync(false); diagCode(err) != DiagVendorEdited {
		t.Fatalf("sync must refuse to overwrite an edited file without --force, got %v", err)
	}
	res, err = tool.Sync(true)
	must(err)
	if rep := res.Data.(VendorReport); !rep.Clean || !slices.Contains(rep.Removed, "old.ts") {
		t.Errorf("forced sync restores everything and removes extras: %+v", rep)
	}
	if rep, _ := tool.Vendor(); !rep.Clean {
		t.Errorf("vendor after forced sync: %+v", rep)
	}
}

func TestInitNeverImportsAnExampleItDidNotCreate(t *testing.T) {
	fresh := newTool(t, testConfig())
	if _, err := fresh.Init(); err != nil {
		t.Fatal(err)
	}
	project, _ := os.ReadFile(filepath.Join(fresh.Root, "tests/ui-loop/project.ts"))
	example, _ := os.ReadFile(filepath.Join(fresh.Root, "tests/ui-loop/screens/example.ts"))
	if !strings.Contains(string(project), "from './screens/example'") || !strings.Contains(string(example), "area: 'tasks'") {
		t.Errorf("a fresh init scaffolds and imports the example:\n%s", project)
	}
	ignore, _ := os.ReadFile(filepath.Join(fresh.Root, ".gitignore"))
	if string(ignore) != "/.ui-loop/\n" {
		t.Errorf(".gitignore: %q", ignore)
	}

	adopter := newTool(t, testConfig())
	screens := filepath.Join(adopter.Root, "tests/ui-loop/screens")
	if err := os.MkdirAll(screens, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(screens, "tasks.ts"), []byte("export const screens = [];\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := adopter.Init(); err != nil {
		t.Fatal(err)
	}
	project, _ = os.ReadFile(filepath.Join(adopter.Root, "tests/ui-loop/project.ts"))
	if strings.Contains(string(project), "example") || !strings.Contains(string(project), "screens: [],") {
		t.Errorf("with screens already present, project.ts imports nothing it lacks:\n%s", project)
	}
	if _, err := os.Stat(filepath.Join(screens, "example.ts")); err == nil {
		t.Error("no example.ts beside existing screens")
	}
}

// The spec states the lint the reviewers judge by; check warns once a line
// init writes for a knob is gone (here: the config moved touchTarget to 40).
func TestCheckWarnsWhenTheSpecNoLongerStatesALintValue(t *testing.T) {
	cfg := testConfig()
	cfg.Spec = "docs/ui-spec.md"
	tool := newTool(t, cfg)
	if _, err := tool.Init(); err != nil {
		t.Fatal(err)
	}
	drift := func() []runxDiagnostic {
		t.Helper()
		res, err := tool.Check(true)
		if err != nil {
			t.Fatal(err)
		}
		var out []runxDiagnostic
		for _, d := range res.Diagnostics {
			if d.Code == DiagSpecLintDrift {
				out = append(out, d)
			}
		}
		return out
	}
	if d := drift(); len(d) != 0 {
		t.Fatalf("the scaffolded spec states both values: %+v", d)
	}
	tool.Config.Lint.TouchTarget = 40
	d := drift()
	if len(d) != 1 || d[0].Severity != "warning" || !strings.Contains(d[0].Detail, "uiLoop.lint.touchTarget 40") ||
		!strings.Contains(d[0].Detail, "Touch targets are at least 40×40px on coarse pointers.") {
		t.Fatalf("a spec without the touch-target line warns naming it and the value: %+v", d)
	}
	writeFile(t, filepath.Join(tool.Root, "docs/ui-spec.md"), "# Spec\n\n- Touch targets are at least 40×40px\n  on coarse pointers.\n- Spacing, control heights and icon sizes sit on the 4px grid.\n")
	if d := drift(); len(d) != 0 {
		t.Errorf("a spec holding both lines (one rewrapped) is silent: %+v", d)
	}
}
