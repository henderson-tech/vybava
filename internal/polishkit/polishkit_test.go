package polishkit

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// fakeExec answers commands by argv prefix and records every call; no test
// ever reaches git, xcrun or adb.
type fakeExec struct {
	calls [][]string
	rules []rule
}

type rule struct {
	prefix string
	out    CmdOut
	fn     func(Cmd) CmdOut
}

func (f *fakeExec) run(_ context.Context, c Cmd) (CmdOut, error) {
	f.calls = append(f.calls, c.Args)
	joined := strings.Join(c.Args, " ")
	for _, r := range f.rules {
		if strings.HasPrefix(joined, r.prefix) {
			if r.fn != nil {
				return r.fn(c), nil
			}
			return r.out, nil
		}
	}
	return CmdOut{}, nil
}

func (f *fakeExec) ran(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

func testConfig() Config {
	return Config{
		Targets: map[Target][]string{
			TargetApp: {"apps/client/**"},
			TargetUI:  {"apps/web/**", "apps/admin-web/**"},
			TargetAPI: {"apps/api/**", "packages/shared/**"},
		},
		Lanes: []Lane{
			{ID: "ios26", Target: TargetApp, Kind: KindIOSSim, Runtime: "26", DeviceType: "iPhone 17 Pro"},
			{ID: "android", Target: TargetApp, Kind: KindAndroidDevice, Nav: []string{"gesture", "3button"}, TextSizes: []string{"1.3"}},
			{ID: "phone", Target: TargetApp, Kind: KindIOSDevice},
			{ID: "web", Target: TargetUI, Kind: KindBrowser, URL: "http://10.8.0.10:3111"},
			{ID: "api", Target: TargetAPI, Kind: KindServer, URL: "http://10.8.0.10:3000/health"},
		},
		Screens: []Screen{
			{ID: "home", Title: "Home", Target: TargetApp, URL: "fixit://home", Area: "home"},
			{ID: "inquiry-new", Title: "New inquiry", Target: TargetApp, URL: "fixit://inquiries/new", Area: "inquiries", SettleMs: 100},
			{ID: "landing", Title: "Landing", Target: TargetUI, URL: "/"},
		},
	}
}

func gitRules(changed ...string) []rule {
	return []rule{
		{prefix: "git rev-parse --verify --quiet origin/main^{commit}", out: CmdOut{Stdout: "abc\n"}},
		{prefix: "git diff --name-only origin/main...HEAD", out: CmdOut{Stdout: strings.Join(changed, "\n") + "\n"}},
	}
}

func newTool(t *testing.T, cfg Config, fx *fakeExec) *Tool {
	t.Helper()
	root := t.TempDir()
	tool, err := New(root, filepath.Join(root, "vybava.config.ts"), root, cfg, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	tool.Exec = fx.run
	tool.Now = func() time.Time { return time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC) }
	tool.Sleep = func(time.Duration) {}
	tool.LookPath = func(s string) (string, error) { return "/usr/bin/" + s, nil }
	tool.HTTPGet = func(string, time.Duration) (int, error) { return 200, nil }
	tool.TempDir = t.TempDir
	return tool
}

func diagCode(t *testing.T, err error) string {
	t.Helper()
	var de runx.DiagError
	if !errors.As(err, &de) {
		t.Fatalf("not a DiagError: %v", err)
	}
	return de.Diag.Code
}

func TestConfigValidateReportsEveryProblem(t *testing.T) {
	cfg := Config{
		Targets: map[Target][]string{"mobile": {"x/**"}},
		Lanes: []Lane{
			{ID: "Bad Id", Target: "app", Kind: "ios-sim"},
			{ID: "web", Target: "ui", Kind: "browser", URL: "not a url"},
			{ID: "web", Target: "ui", Kind: "browser", URL: "http://x", Nav: []string{"gesture"}},
			{ID: "droid", Target: "app", Kind: "android-device", TextSizes: []string{"large"}},
		},
		Screens: []Screen{{ID: "s", Target: "api", URL: ""}},
	}
	problems := cfg.Validate()
	for _, want := range []string{"unknown target", "not kebab-case", "needs runtime and deviceType", "needs an http(s) url", "listed twice", "nav applies to android", "font_scale numbers", "not app or ui", "url is required"} {
		if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, want) }) {
			t.Errorf("missing problem %q in %v", want, problems)
		}
	}
	if len(testConfig().Validate()) != 0 {
		t.Fatalf("test config invalid: %v", testConfig().Validate())
	}
	got := testConfig().WithDefaults()
	if got.Base != DefaultBase || got.Out != DefaultOut || !slices.Equal(got.Lanes[0].Themes, Themes) || got.Screens[0].SettleMs != DefaultSettleMs || got.Screens[1].SettleMs != 100 {
		t.Fatalf("defaults not filled: %+v", got)
	}
}

func TestInferOrdersByWeightCwdAndFlag(t *testing.T) {
	fx := &fakeExec{}
	tool := newTool(t, testConfig(), fx)
	changed := []string{"apps/api/src/a.ts", "apps/api/src/b.ts", "packages/shared/c.ts", "apps/client/app/inquiries/new.tsx", "README.md"}
	plan := tool.Infer(changed, nil)
	if got := plan.TargetIDs(); !slices.Equal(got, []Target{TargetAPI, TargetApp}) {
		t.Fatalf("weight order: %v", got)
	}
	if plan.Targets[0].Reason != "diff" || len(plan.Targets[0].Files) != 3 || len(plan.Targets[1].Files) != 1 {
		t.Fatalf("files per target: %+v", plan.Targets)
	}
	if !slices.Equal(plan.Lanes, []string{"ios26", "android", "phone", "api"}) {
		t.Fatalf("lanes: %v", plan.Lanes)
	}
	// only the inquiries screen derives from the changed path; api has no screens
	if !slices.Equal(plan.ScreensTouched, []string{"inquiry-new"}) {
		t.Fatalf("screensTouched: %v", plan.ScreensTouched)
	}
	// nothing under a glob: empty lists, never null in the envelope
	plan = tool.Infer([]string{"scripts/x.ts"}, nil)
	if plan.Targets == nil || len(plan.Targets) != 0 || plan.Lanes == nil || plan.ScreensTouched == nil {
		t.Fatalf("empty plan must hold [] not null: %+v", plan)
	}
	// no derivable screen: every screen of the target
	plan = tool.Infer([]string{"apps/client/lib/util.ts"}, nil)
	if !slices.Equal(plan.ScreensTouched, []string{"home", "inquiry-new"}) {
		t.Fatalf("all screens expected: %v", plan.ScreensTouched)
	}
	// --target goes first with reason flag, the cwd next
	tool.Cwd = filepath.Join(tool.Root, "apps", "client", "app")
	plan = tool.Infer(changed, []Target{TargetUI})
	if got := plan.TargetIDs(); !slices.Equal(got, []Target{TargetUI, TargetApp, TargetAPI}) {
		t.Fatalf("flag+cwd order: %v", got)
	}
	if plan.Targets[0].Reason != "flag" || plan.Targets[1].Reason != "cwd" || plan.Targets[2].Reason != "diff" {
		t.Fatalf("reasons: %+v", plan.Targets)
	}
	if len(plan.Targets[0].Files) != 0 {
		t.Fatalf("ui has no changed files: %+v", plan.Targets[0])
	}
}

func TestPlanFallsBackToDefaultBranchAndReportsNoChanges(t *testing.T) {
	fx := &fakeExec{rules: []rule{
		{prefix: "git rev-parse --verify --quiet origin/main^{commit}", out: CmdOut{Code: 1}},
		{prefix: "git symbolic-ref --short refs/remotes/origin/HEAD", out: CmdOut{Code: 128, Stderr: "fatal: ref refs/remotes/origin/HEAD is not a symbolic ref"}},
		{prefix: "git rev-parse --verify --quiet main^{commit}", out: CmdOut{Stdout: "abc\n"}},
		{prefix: "git diff --name-only main...HEAD", out: CmdOut{Stdout: "apps/web/page.tsx\n"}},
	}}
	tool := newTool(t, testConfig(), fx)
	res, err := tool.Plan(context.Background(), PlanOptions{Intensity: "full", Findings: "board-x"})
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(PlanData)
	if data.Base != "main" || data.Intensity != "full" || data.Findings != "board-x" || !slices.Equal(data.TargetIDs(), []Target{TargetUI}) {
		t.Fatalf("%+v", data)
	}
	if len(res.Next) != 2 || !strings.HasPrefix(res.Next[0], "polish-kit lanes --target ui") || !strings.Contains(res.Next[1], "run init --pass 1 --target ui --intensity full") {
		t.Fatalf("next: %v", res.Next)
	}
	fx.rules[3].out = CmdOut{Stdout: "\n"}
	_, err = tool.Plan(context.Background(), PlanOptions{})
	if diagCode(t, err) != DiagNoChanges {
		t.Fatalf("want no-changes: %v", err)
	}
	_, err = tool.Plan(context.Background(), PlanOptions{Targets: []string{"mobile"}})
	if diagCode(t, err) != DiagUnknownTarget {
		t.Fatalf("want unknown-target: %v", err)
	}
	_, err = tool.Plan(context.Background(), PlanOptions{Intensity: "max"})
	if diagCode(t, err) != DiagUsage {
		t.Fatalf("want usage: %v", err)
	}
}

func TestChromeCellTable(t *testing.T) {
	cfg := testConfig().WithDefaults()
	cells := ChromeCells(cfg.Lanes, cfg.Screens)
	// ios26: 2 app screens x 2 themes; android: 2 x 2 themes x 2 nav x 2 sizes; phone: 2 x 2; web/api: none (landing is ui, no ui device lane shoots it... web is a browser lane of target ui: 1 screen x 2 themes)
	want := 4 + 16 + 4 + 2
	if len(cells) != want {
		t.Fatalf("got %d cells, want %d", len(cells), want)
	}
	if cells[0].ID != "ios26--home--light" || cells[1].ID != "ios26--home--dark" {
		t.Fatalf("first ids: %s %s", cells[0].ID, cells[1].ID)
	}
	ids := make([]string, len(cells))
	for i, c := range cells {
		ids[i] = c.ID
		if c.Verdict != VerdictPending || c.Kind != CellChrome {
			t.Fatalf("cell %s not pending chrome", c.ID)
		}
	}
	for _, id := range []string{"android--inquiry-new--dark--3button--1.3", "android--home--light--gesture", "web--landing--dark"} {
		if !slices.Contains(ids, id) {
			t.Fatalf("missing %s in %v", id, ids)
		}
	}
	if slices.Contains(ids, "ios26--landing--light") {
		t.Fatal("a ui screen must not land on an app lane")
	}
	c := cells[slices.Index(ids, "android--inquiry-new--dark--3button--1.3")]
	if c.ShotFile() != "shots/android/inquiry-new--dark--3button--1.3.png" {
		t.Fatalf("shot file: %s", c.ShotFile())
	}
	if len(ids) != len(slices.Compact(slices.Sorted(slices.Values(ids)))) {
		t.Fatal("cell ids are not unique")
	}
}

func TestRunLedgerRoundTrip(t *testing.T) {
	fx := &fakeExec{rules: gitRules("apps/client/app/home.tsx")}
	tool := newTool(t, testConfig(), fx)
	ctx := context.Background()
	_, err := tool.Status(0)
	if diagCode(t, err) != DiagPassMissing {
		t.Fatalf("want pass-missing: %v", err)
	}
	res, err := tool.Init(ctx, InitOptions{Lanes: []string{"ios26", "android"}})
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(InitData)
	if data.Pass != 1 || data.Existing || data.Cells != 2+8 || !slices.Equal(data.Screens, []string{"home"}) {
		t.Fatalf("%+v", data)
	}
	if b, err := os.ReadFile(filepath.Join(tool.OutDir(), ".gitignore")); err != nil || string(b) != "*\n" {
		t.Fatalf("out gitignore: %q %v", b, err)
	}
	if !slices.ContainsFunc(res.Next, func(s string) bool { return s == "polish-kit shoot ios26 --pass 1 --json" }) {
		t.Fatalf("next: %v", res.Next)
	}
	// idempotent
	res, err = tool.Init(ctx, InitOptions{Pass: 1})
	if err != nil || !res.Data.(InitData).Existing {
		t.Fatalf("second init: %+v %v", res.Data, err)
	}
	_, err = tool.Init(ctx, InitOptions{Pass: 2, Lanes: []string{"nope"}})
	if diagCode(t, err) != DiagUnknownLane {
		t.Fatalf("want unknown-lane: %v", err)
	}
	_, err = tool.Init(ctx, InitOptions{Pass: 2, Screens: []string{"nope"}})
	if diagCode(t, err) != DiagUnknownScreen {
		t.Fatalf("want unknown-screen: %v", err)
	}
	// round trip
	run, err := tool.LoadRun(0)
	if err != nil || run.V != RunVersion || run.Pass != 1 || run.CreatedAt != "2026-09-30T10:00:00Z" || run.Plan.Base != "origin/main" || len(run.Lanes) != 2 {
		t.Fatalf("load: %+v %v", run, err)
	}
	// matrix cells, idempotent by id
	add, err := tool.AddCell(AddCellOptions{Kind: CellMatrix, Lane: "ios26", Flow: "Create inquiry", Tier: "Network: offline mid-submit"})
	if err != nil || add.Data.(Cell).ID != "ios26--matrix--create-inquiry--network-offline-mid-submit" {
		t.Fatalf("add-cell: %+v %v", add.Data, err)
	}
	if _, err := tool.AddCell(AddCellOptions{Kind: CellMatrix, Lane: "ios26", Flow: "Create inquiry", Tier: "Network: offline mid-submit"}); err != nil {
		t.Fatal(err)
	}
	run, _ = tool.LoadRun(1)
	if len(run.Cells) != 11 {
		t.Fatalf("matrix cell added twice: %d", len(run.Cells))
	}
	_, err = tool.AddCell(AddCellOptions{Kind: "chrome", Lane: "ios26", Flow: "x", Tier: "y"})
	if diagCode(t, err) != DiagUsage {
		t.Fatalf("want usage: %v", err)
	}
	// verdicts
	_, err = tool.SetCell(CellOptions{ID: "ios26--home--dark", Verdict: "fail"})
	if diagCode(t, err) != DiagShotRequired {
		t.Fatalf("want shot-required: %v", err)
	}
	_, err = tool.SetCell(CellOptions{ID: "ghost", Verdict: "pass"})
	if diagCode(t, err) != DiagCellUnknown {
		t.Fatalf("want cell-unknown: %v", err)
	}
	_, err = tool.SetCell(CellOptions{ID: "ios26--home--dark", Verdict: "fail", Shot: "missing.png"})
	if diagCode(t, err) != DiagShotRequired {
		t.Fatalf("want shot-required for a missing file: %v", err)
	}
	shot := filepath.Join(run.PassDir, "shots", "ios26", "home--dark.png")
	if err := os.MkdirAll(filepath.Dir(shot), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shot, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = tool.SetCell(CellOptions{ID: "ios26--home--dark", Verdict: "fail", Shot: shot, Note: "clipped rim", Finding: "ann-12"})
	if err != nil {
		t.Fatal(err)
	}
	cell := res.Data.(Cell)
	if cell.Verdict != "fail" || cell.Shot != "shots/ios26/home--dark.png" || cell.Note != "clipped rim" || cell.Finding != "ann-12" {
		t.Fatalf("cell: %+v", cell)
	}
	if _, err := tool.SetCell(CellOptions{ID: "ios26--home--light", Verdict: "pass"}); err != nil {
		t.Fatal(err)
	}
	st, err := tool.Status(0)
	if err != nil {
		t.Fatal(err)
	}
	sd := st.Data.(StatusData)
	if sd.Total != 11 || sd.Verdict[VerdictPass] != 1 || sd.Verdict[VerdictFail] != 1 || sd.Verdict[VerdictPending] != 9 || sd.Lanes["android"][VerdictPending] != 8 || sd.Kinds[CellMatrix][VerdictPending] != 1 {
		t.Fatalf("status: %+v", sd)
	}
	if len(st.Next) != 9 || !strings.HasPrefix(st.Next[0], "polish-kit cell android--home--light--gesture pass|fail --shot <path> --pass 1") {
		t.Fatalf("status next: %v", st.Next)
	}
	// version gate
	b, _ := os.ReadFile(filepath.Join(run.PassDir, "run.json"))
	if err := os.WriteFile(filepath.Join(run.PassDir, "run.json"), []byte(strings.Replace(string(b), `"v": 1`, `"v": 99`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = tool.LoadRun(1)
	if diagCode(t, err) != DiagRunVersion {
		t.Fatalf("want run-version: %v", err)
	}
	res, err = tool.Init(ctx, InitOptions{Pass: 1, Force: true, Lanes: []string{"ios26"}})
	if err != nil || res.Data.(InitData).Existing || res.Data.(InitData).Cells != 2 {
		t.Fatalf("force init: %+v %v", res.Data, err)
	}
	if _, err := os.Stat(shot); err != nil {
		t.Fatal("force init must keep shots on disk")
	}
}

func TestReportRendersTablesAndDelta(t *testing.T) {
	fx := &fakeExec{rules: gitRules("apps/client/app/home.tsx")}
	tool := newTool(t, testConfig(), fx)
	ctx := context.Background()
	for pass := 1; pass <= 2; pass++ {
		if _, err := tool.Init(ctx, InitOptions{Pass: pass, Lanes: []string{"ios26"}, Screens: []string{"home", "inquiry-new"}}); err != nil {
			t.Fatal(err)
		}
	}
	run1, _ := tool.LoadRun(1)
	run2, _ := tool.LoadRun(2)
	shot := filepath.Join(run2.PassDir, "s.png")
	if err := os.WriteFile(shot, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	set := func(run *RunFile, id, verdict string) {
		i := slices.IndexFunc(run.Cells, func(c Cell) bool { return c.ID == id })
		run.Cells[i].Verdict = verdict
		if verdict == VerdictFail {
			run.Cells[i].Shot, run.Cells[i].Note = "s.png", "rim clipped"
		}
	}
	set(run1, "ios26--home--light", VerdictFail)
	set(run1, "ios26--home--dark", VerdictPass)
	set(run1, "ios26--inquiry-new--light", VerdictPass)
	set(run2, "ios26--home--light", VerdictPass)
	set(run2, "ios26--home--dark", VerdictFail)
	set(run2, "ios26--inquiry-new--light", VerdictSkip)
	run2.Cells = append(run2.Cells, Cell{ID: "ios26--matrix--create--offline", Kind: CellMatrix, Lane: "ios26", Flow: "Create", Tier: "Offline", Verdict: VerdictFail, Shot: "s.png", Note: "spinner forever", Finding: "ann-3"})
	for _, r := range []*RunFile{run1, run2} {
		if err := tool.SaveRun(r); err != nil {
			t.Fatal(err)
		}
	}
	res, err := tool.Report(ReportOptions{Pass: 2})
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(ReportData)
	md := data.Markdown
	for _, want := range []string{
		"# Polish pass 2", "## Lane ios26 (ios-sim)", "| Screen | light | dark |", "| Home | ✓ | ✗ |", "| New inquiry | - | · |",
		"## Matrix", "| ios26 | Create | Offline | ✗ fail | spinner forever | ann-3 |",
		"- `ios26--home--dark`: rim clipped - shot `s.png`",
		"## Delta vs pass 1", "- fixed: `ios26--home--light`", "- regressed: `ios26--home--dark`", "- new: `ios26--matrix--create--offline`",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report lacks %q:\n%s", want, md)
		}
	}
	if b, err := os.ReadFile(data.File); err != nil || string(b) != md {
		t.Fatalf("report.md not written: %v", err)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Severity != "warning" || res.Diagnostics[0].Code != DiagShotRequired {
		t.Fatalf("pending cells warn: %+v", res.Diagnostics)
	}
	if res, err := tool.Report(ReportOptions{Pass: 1, Previous: -1}); err != nil || res.Data.(ReportData).Delta != nil || !strings.Contains(res.Data.(ReportData).Markdown, "no previous pass") {
		t.Fatalf("no-delta: %v", err)
	}
	if _, err := tool.Report(ReportOptions{Pass: 2, Previous: 7}); diagCode(t, err) != DiagPassMissing {
		t.Fatalf("want pass-missing: %v", err)
	}
}

func writeTestPNG(t *testing.T, path string, w, h int, c color.RGBA) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

func TestSheetLayoutAndRender(t *testing.T) {
	fx := &fakeExec{rules: gitRules("apps/client/app/home.tsx")}
	tool := newTool(t, testConfig(), fx)
	if _, err := tool.Init(context.Background(), InitOptions{Lanes: []string{"ios26", "android"}, Screens: []string{"home"}}); err != nil {
		t.Fatal(err)
	}
	run, _ := tool.LoadRun(1)
	// ios: 300x640 shots (scale 1:1); android: 360x800 (scaled to 288x640); one android cell's file is missing
	for i := range run.Cells {
		c := &run.Cells[i]
		if c.Lane == "ios26" || (c.Nav == "gesture" && c.TextSize == "") || c.ID == "android--home--dark--3button--1.3" {
			c.Shot = c.ShotFile()
			if c.ID != "android--home--dark--3button--1.3" {
				w, h := 300, 640
				if c.Lane == "android" {
					w, h = 360, 800
				}
				writeTestPNG(t, run.ShotPath(*c), w, h, color.RGBA{R: 200, G: 100, B: 50, A: 255})
			}
		}
	}
	if err := tool.SaveRun(run); err != nil {
		t.Fatal(err)
	}
	grid := BuildGrid(run, "home", nil, func(c Cell) (int, int) {
		img, err := readPNG(run.ShotPath(c))
		if err != nil {
			return 0, 0
		}
		return img.Bounds().Dx(), img.Bounds().Dy()
	})
	if !slices.Equal(grid.Columns, []string{"ios26", "android"}) {
		t.Fatalf("columns: %v", grid.Columns)
	}
	// rows in cell order: light, dark (ios), then the android gesture rows are the same states, plus dark/3button/1.3
	wantRows := []SheetState{{Theme: "light"}, {Theme: "dark"}, {Theme: "light", Nav: "gesture"}, {Theme: "dark", Nav: "gesture"}, {Theme: "dark", Nav: "3button", TextSize: "1.3"}}
	if !slices.Equal(grid.Rows, wantRows) {
		t.Fatalf("rows: %+v", grid.Rows)
	}
	if !slices.Equal(grid.Widths, []int{300, 288}) {
		t.Fatalf("widths: %v", grid.Widths)
	}
	w, h := grid.Size()
	if w != 300+288+3*SheetGap || h != SheetGap+5*(SheetLabel+SheetCellHeight+SheetGap) {
		t.Fatalf("size: %d x %d", w, h)
	}
	if x, y := grid.Origin(4, 1); x != SheetGap+300+SheetGap || y != SheetGap+4*(SheetLabel+SheetCellHeight+SheetGap) {
		t.Fatalf("origin: %d,%d", x, y)
	}
	res, err := tool.Sheet(SheetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(SheetData)
	if len(data.Sheets) != 1 || data.Sheets[0].Shots != 4 || !slices.Equal(data.Sheets[0].Missing, []string{"android--home--dark--3button--1.3"}) {
		t.Fatalf("sheets: %+v", data.Sheets)
	}
	sheet, err := readPNG(data.Sheets[0].File)
	if err != nil || sheet.Bounds().Dx() != w || sheet.Bounds().Dy() != h {
		t.Fatalf("sheet png: %v %v", err, sheet.Bounds())
	}
	// the android cell in row 2 (light gesture) holds the scaled shot: its middle pixel is the shot colour, not the background
	x, y := grid.Origin(2, 1)
	if r, g, b, _ := sheet.At(x+144, y+SheetLabel+320).RGBA(); r>>8 != 200 || g>>8 != 100 || b>>8 != 50 {
		t.Fatalf("scaled shot not painted: %d %d %d", r>>8, g>>8, b>>8)
	}
	var legend Legend
	lb, _ := os.ReadFile(data.Sheets[0].Legend)
	if err := json.Unmarshal(lb, &legend); err != nil || legend.Screen != "home" || len(legend.Cells) != 5 || legend.Cells[0].Cell != "ios26--home--light" || legend.Cells[0].W != 300 {
		t.Fatalf("legend: %+v %v", legend, err)
	}
	edges, err := readPNG(data.Sheets[0].Edges)
	if err != nil {
		t.Fatal(err)
	}
	wantH := SheetGap + 2*(edgeBlockHeight(300)+SheetGap) + 2*(edgeBlockHeight(360)+SheetGap)
	if edges.Bounds().Dx() != EdgeWidth+2*SheetGap || edges.Bounds().Dy() != wantH {
		t.Fatalf("edges: %v want h %d", edges.Bounds(), wantH)
	}
	if bandZoom(300) != EdgeZoom || bandZoom(1179) >= EdgeZoom || bandHeight(1179) != EdgeBand*EdgeWidth/1179 {
		t.Fatalf("band zoom: %v %v", bandZoom(300), bandZoom(1179))
	}
	if _, err := tool.Sheet(SheetOptions{Screen: "nope"}); diagCode(t, err) != DiagUnknownScreen {
		t.Fatalf("want unknown-screen: %v", err)
	}
	if _, err := tool.Sheet(SheetOptions{Lanes: []string{"nope"}}); diagCode(t, err) != DiagUnknownLane {
		t.Fatalf("want unknown-lane: %v", err)
	}
}

func TestScalersAndFont(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 2, 2))
	src.SetRGBA(0, 0, color.RGBA{255, 0, 0, 255})
	src.SetRGBA(1, 0, color.RGBA{0, 0, 255, 255})
	src.SetRGBA(0, 1, color.RGBA{0, 0, 255, 255})
	src.SetRGBA(1, 1, color.RGBA{255, 0, 0, 255})
	up := scaleNearest(src, 4, 4)
	if r, _, b, _ := up.At(1, 1).RGBA(); r>>8 != 255 || b != 0 {
		t.Fatal("nearest zoom must keep the source pixel")
	}
	down := scaleBox(src, 1, 1)
	if r, _, b, _ := down.At(0, 0).RGBA(); r>>8 != 127 || b>>8 != 127 {
		t.Fatalf("box average: %d %d", r>>8, b>>8)
	}
	if textWidth("AB", 2) != (2*glyphW+glyphGap)*2 || fitLabel("ABCDEFGH", textWidth("ABC", 1), 1) != "ABC" {
		t.Fatal("text metrics")
	}
	img := image.NewRGBA(image.Rect(0, 0, 40, 10))
	drawText(img, 0, 0, "i1?", 1, color.White)
	if _, _, _, a := img.At(2, 0).RGBA(); a == 0 {
		t.Fatal("I's top bar not drawn")
	}
}

// simctlFixture is the shape of one `xcrun simctl list -j` on this Mac
// (2026-09-30): devices keyed by runtime and named per persona, the
// devicetypes list, runtimes with their supported types; no device on 18.6.
const simctlFixture = `{
  "devicetypes": [
    {"name": "iPhone SE (3rd generation)", "identifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-SE-3rd-generation", "productFamily": "iPhone"},
    {"name": "iPhone 17 Pro", "identifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro", "productFamily": "iPhone"}
  ],
  "devices": {
    "com.apple.CoreSimulator.SimRuntime.iOS-26-5": [
      {"udid": "BBBB-26", "name": "FixIt wt other-branch customer", "state": "Booted", "isAvailable": true, "deviceTypeIdentifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro", "dataPath": "/x", "logPath": "/y"},
      {"udid": "AAAA-26", "name": "FixIt template iPhone 17 Pro", "state": "Shutdown", "isAvailable": true, "deviceTypeIdentifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro", "dataPath": "/x", "logPath": "/y"},
      {"udid": "DDDD-26", "name": "FixIt QA customer 2026-09-25", "state": "Shutdown", "isAvailable": true, "deviceTypeIdentifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro"},
      {"udid": "EEEE-26", "name": "FixIt template iPhone SE (3rd generation)", "state": "Shutdown", "isAvailable": true, "deviceTypeIdentifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-SE-3rd-generation"}
    ],
    "com.apple.CoreSimulator.SimRuntime.iOS-18-6": []
  },
  "pairs": {},
  "runtimes": [
    {"identifier": "com.apple.CoreSimulator.SimRuntime.iOS-18-6", "version": "18.6", "name": "iOS 18.6", "platform": "iOS", "isAvailable": true, "isInternal": false,
     "supportedDeviceTypes": [{"name": "iPhone SE (3rd generation)", "identifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-SE-3rd-generation"}]},
    {"identifier": "com.apple.CoreSimulator.SimRuntime.iOS-26-5", "version": "26.5", "name": "iOS 26.5", "platform": "iOS", "isAvailable": true, "isInternal": false,
     "supportedDeviceTypes": [{"name": "iPhone 17 Pro", "identifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-17-Pro"}, {"name": "iPhone SE (3rd generation)", "identifier": "com.apple.CoreSimulator.SimDeviceType.iPhone-SE-3rd-generation"}]}
  ]
}`

func TestSimctlParsing(t *testing.T) {
	var list simList
	if err := json.Unmarshal([]byte(simctlFixture), &list); err != nil {
		t.Fatal(err)
	}
	// matched by deviceTypeIdentifier, never by display name; the template
	// (name ends in the type) beats the QA persona, and a shutdown sim beats
	// the booted one another session owns
	dev, rt, err := matchSim(list, Lane{ID: "ios26", Runtime: "26", DeviceType: "iPhone 17 Pro"})
	if err != nil || dev.UDID != "AAAA-26" || rt.Version != "26.5" {
		t.Fatalf("shutdown template first: %+v %+v %v", dev, rt, err)
	}
	// a pinned device wins, booted or not
	dev, _, err = matchSim(list, Lane{ID: "ios26", Runtime: "26", DeviceType: "iPhone 17 Pro", Device: "FixIt wt other-branch customer"})
	if err != nil || dev.UDID != "BBBB-26" {
		t.Fatalf("pinned by name: %+v %v", dev, err)
	}
	if dev, _, err = matchSim(list, Lane{ID: "ios26", Runtime: "26", DeviceType: "iPhone 17 Pro", Device: "dddd-26"}); err != nil || dev.UDID != "DDDD-26" {
		t.Fatalf("pinned by udid: %+v %v", dev, err)
	}
	if _, _, err = matchSim(list, Lane{ID: "ios26", Runtime: "26", DeviceType: "iPhone 17 Pro", Device: "ghost"}); diagCode(t, err) != DiagLaneMissing {
		t.Fatalf("pinned sim absent: %v", err)
	}
	// the SE exists on 26.5 only: runtime "18" holds none -> the exact create command with the identifier from devicetypes
	_, rt, err = matchSim(list, Lane{ID: "ios18", Runtime: "18", DeviceType: "iPhone SE (3rd generation)"})
	if diagCode(t, err) != DiagLaneMissing || rt.Version != "18.6" {
		t.Fatalf("want lane-missing on 18.6: %v", err)
	}
	var de runx.DiagError
	errors.As(err, &de)
	if de.Diag.Fix != `xcrun simctl create "iPhone SE (3rd generation)" "com.apple.CoreSimulator.SimDeviceType.iPhone-SE-3rd-generation" "com.apple.CoreSimulator.SimRuntime.iOS-18-6"` {
		t.Fatalf("create fix: %s", de.Diag.Fix)
	}
	if dev, _, err := matchSim(list, Lane{ID: "se26", Runtime: "26", DeviceType: "iPhone SE (3rd generation)"}); err != nil || dev.UDID != "EEEE-26" {
		t.Fatalf("SE on 26.5: %+v %v", dev, err)
	}
	// a type the lists do not know still gets a well-formed guess
	if id := list.deviceTypeID("iPad mini (A17 Pro)", nil); id != "com.apple.CoreSimulator.SimDeviceType.iPad-mini-A17-Pro" {
		t.Fatalf("guessed identifier: %s", id)
	}
	_, _, err = matchSim(list, Lane{ID: "ios17", Runtime: "17", DeviceType: "iPhone 15"})
	if diagCode(t, err) != DiagRuntimeMissing {
		t.Fatalf("want runtime-missing: %v", err)
	}
	// "1" never matches 18.6
	_, _, err = matchSim(list, Lane{ID: "x", Runtime: "1", DeviceType: "iPhone 16"})
	if diagCode(t, err) != DiagRuntimeMissing {
		t.Fatalf("prefix must stop at a dot: %v", err)
	}
	if !versionMatches("18.6", "18.6") || versionMatches("18.6", "18.65") || !versionMatches("26", "26.5") {
		t.Fatal("versionMatches")
	}
}

const devicectlFixture = `{"result": {"devices": [
  {"identifier": "1111", "connectionProperties": {"pairingState": "paired", "tunnelState": "connected"}, "deviceProperties": {"name": "Lukas iPhone", "osVersionNumber": "26.0"}, "hardwareProperties": {"udid": "00008130-AAA"}},
  {"identifier": "2222", "connectionProperties": {"pairingState": "paired", "tunnelState": "disconnected"}, "deviceProperties": {"name": "Test iPhone", "osVersionNumber": "18.6"}, "hardwareProperties": {"udid": "00008130-BBB"}}
]}}`

func TestDevicectlAndAdbParsing(t *testing.T) {
	var list devicectlList
	if err := json.Unmarshal([]byte(devicectlFixture), &list); err != nil {
		t.Fatal(err)
	}
	d, err := matchDevice(list, Lane{ID: "phone", Device: "00008130-AAA"})
	if err != nil || d.DeviceProperties.Name != "Lukas iPhone" {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := matchDevice(list, Lane{ID: "phone", Device: "Test iPhone"}); diagCode(t, err) != DiagDeviceUnavailable {
		t.Fatalf("disconnected: %v", err)
	}
	if _, err := matchDevice(list, Lane{ID: "phone"}); diagCode(t, err) != DiagDeviceUnavailable {
		t.Fatalf("two phones, none named: %v", err)
	}
	if _, err := matchDevice(list, Lane{ID: "phone", Device: "ghost"}); diagCode(t, err) != DiagDeviceUnavailable {
		t.Fatalf("unknown: %v", err)
	}

	adb := parseAdbDevices("List of devices attached\nR5CT30ABC              device usb:1-1 product:dm1qxeea model:SM_S911B device:dm1q transport_id:1\nemulator-5554          device product:sdk_gphone64_arm64 model:sdk_gphone64_arm64 device:emu64a transport_id:2\nZZZ offline\n\n")
	if len(adb) != 3 || adb[0].Serial != "R5CT30ABC" || adb[0].Model != "SM_S911B" || !adb[1].emulator() || adb[2].State != "offline" {
		t.Fatalf("%+v", adb)
	}
	if _, err := matchAdb(adb, Lane{ID: "android"}); diagCode(t, err) != DiagDeviceUnavailable {
		t.Fatalf("two physical devices: %v", err)
	}
	if d, err := matchAdb(adb, Lane{ID: "android", Device: "R5CT30ABC"}); err != nil || d.Model != "SM_S911B" {
		t.Fatalf("%+v %v", d, err)
	}
	if _, err := matchAdb(adb, Lane{ID: "android", Device: "ZZZ"}); diagCode(t, err) != DiagDeviceUnavailable {
		t.Fatalf("offline: %v", err)
	}
	if d, err := matchAdb(adb[:2], Lane{ID: "android"}); err != nil || d.Serial != "R5CT30ABC" {
		t.Fatalf("the only physical device: %+v %v", d, err)
	}
	if parseNavMode("2\n") != "gesture" || parseNavMode("0") != "3button" || parseNavMode("1") != "3button" || parseNavMode("null") != "" {
		t.Fatal("nav mode")
	}
	if parseNightMode("Night mode: yes\n") != "dark" || parseNightMode("Night mode: no") != "light" || parseNightMode("Night mode: auto") != "auto" || parseNightMode("garbage") != "" {
		t.Fatal("night mode")
	}
	if !slices.Equal(parseAvdList("INFO    | Storage size\nPixel_8\nPixel_Tablet_API_35\n"), []string{"Pixel_8", "Pixel_Tablet_API_35"}) {
		t.Fatal("avd list")
	}
}

func TestLanesVerbResolvesEveryKind(t *testing.T) {
	fx := &fakeExec{rules: []rule{
		{prefix: "xcrun simctl list -j", out: CmdOut{Stdout: simctlFixture}},
		{prefix: "xcrun simctl ui BBBB-26 appearance", out: CmdOut{Stdout: "dark\n"}},
		{prefix: "adb devices -l", out: CmdOut{Stdout: "List of devices attached\nR5CT30ABC device model:SM_S911B\n"}},
		{prefix: "adb -s R5CT30ABC shell settings get secure navigation_mode", out: CmdOut{Stdout: "2\n"}},
		{prefix: "adb -s R5CT30ABC shell cmd uimode night", out: CmdOut{Stdout: "Night mode: no\n"}},
		{prefix: "xcrun devicectl list devices --json-output", fn: func(c Cmd) CmdOut {
			_ = os.WriteFile(c.Args[len(c.Args)-1], []byte(devicectlFixture), 0o644)
			return CmdOut{}
		}},
	}}
	cfg := testConfig()
	cfg.Lanes[2].Device = "Lukas iPhone"
	tool := newTool(t, cfg, fx)
	tool.HTTPGet = func(url string, _ time.Duration) (int, error) {
		if strings.HasSuffix(url, "/health") {
			return 503, nil
		}
		return 200, nil
	}
	res, err := tool.Lanes(context.Background(), LanesOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lanes := res.Data.(LanesData).Lanes
	byID := map[string]LaneState{}
	for _, l := range lanes {
		byID[l.ID] = l
	}
	if s := byID["ios26"]; !s.Ready || s.Booted || s.UDID != "AAAA-26" || s.Theme != "" || s.Runtime != "26.5" || s.Boot != "xcrun simctl boot AAAA-26 && open -a Simulator" {
		t.Fatalf("ios26: %+v", s)
	}
	if !fx.ran("xcrun simctl list -j") || fx.ran("xcrun simctl list -j devices") {
		t.Fatalf("simctl takes one type filter at most; calls: %v", fx.calls)
	}
	if s := byID["android"]; !s.Ready || s.Serial != "R5CT30ABC" || s.Nav != "gesture" || s.Theme != "light" {
		t.Fatalf("android: %+v", s)
	}
	if s := byID["phone"]; !s.Ready || s.UDID != "00008130-AAA" {
		t.Fatalf("phone: %+v", s)
	}
	if s := byID["web"]; !s.Ready || s.Status != 200 {
		t.Fatalf("web: %+v", s)
	}
	if s := byID["api"]; s.Ready || s.Status != 503 || s.Fix == "" {
		t.Fatalf("api: %+v", s)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != DiagDeviceUnavailable {
		t.Fatalf("diagnostics: %+v", res.Diagnostics)
	}
	// the shutdown sim's boot command leads next
	if res.Next[0] != "xcrun simctl boot AAAA-26 && open -a Simulator" {
		t.Fatalf("boot next: %v", res.Next)
	}
	// pinned to the booted sim: booted, theme read
	tool.Config.Lanes[0].Device = "FixIt wt other-branch customer"
	res, _ = tool.Lanes(context.Background(), LanesOptions{Lane: "ios26"})
	if s := res.Data.(LanesData).Lanes[0]; !s.Booted || s.UDID != "BBBB-26" || s.Theme != "dark" || res.Next[0] != "polish-kit run init --pass 1 --json" {
		t.Fatalf("pinned booted sim: %+v next %v", s, res.Next)
	}
	tool.Config.Lanes[0].Device = ""
	tool.LookPath = func(string) (string, error) { return "", errors.New("nope") }
	res, _ = tool.Lanes(context.Background(), LanesOptions{Targets: []string{"app"}})
	if len(res.Diagnostics) != 3 || res.Diagnostics[0].Code != DiagToolMissing || !strings.Contains(res.Diagnostics[1].Fix, "android-platform-tools") || res.Diagnostics[2].Detail != "xcrun is not on PATH" {
		t.Fatalf("tool-missing: %+v", res.Diagnostics)
	}
	if _, err := tool.Lanes(context.Background(), LanesOptions{Lane: "ghost"}); diagCode(t, err) != DiagUnknownLane {
		t.Fatalf("want unknown-lane: %v", err)
	}
}

func TestLanesSetAndUnsupported(t *testing.T) {
	fx := &fakeExec{rules: []rule{{prefix: "xcrun simctl list -j", out: CmdOut{Stdout: simctlFixture}}}}
	cfg := testConfig()
	cfg.Lanes[0].Device = "BBBB-26" // the booted one
	tool := newTool(t, cfg, fx)
	ctx := context.Background()
	res, err := tool.Set(ctx, SetOptions{Lane: "ios26", Theme: "dark", Text: "accessibility-medium"})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Data.(SetData).Applied; !slices.Equal(got, []string{"xcrun simctl ui BBBB-26 appearance dark", "xcrun simctl ui BBBB-26 content_size accessibility-medium"}) {
		t.Fatalf("applied: %v", got)
	}
	res, _ = tool.Set(ctx, SetOptions{Lane: "ios26", Reset: true})
	if got := res.Data.(SetData).Applied; !slices.Equal(got, []string{"xcrun simctl ui BBBB-26 appearance light", "xcrun simctl ui BBBB-26 content_size medium"}) {
		t.Fatalf("reset: %v", got)
	}
	if _, err := tool.Set(ctx, SetOptions{Lane: "phone", Theme: "dark"}); diagCode(t, err) != DiagLaneUnsupported {
		t.Fatalf("ios-device: %v", err)
	}
	if _, err := tool.Set(ctx, SetOptions{Lane: "web", Theme: "dark"}); diagCode(t, err) != DiagLaneUnsupported {
		t.Fatalf("browser: %v", err)
	}
	if _, err := tool.Set(ctx, SetOptions{Lane: "ios26", Nav: "3button"}); diagCode(t, err) != DiagUsage {
		t.Fatalf("nav on ios: %v", err)
	}
	if _, err := tool.Set(ctx, SetOptions{Lane: "ios26"}); diagCode(t, err) != DiagUsage {
		t.Fatalf("nothing to set: %v", err)
	}
	if _, err := tool.Set(ctx, SetOptions{Lane: "ios26", Theme: "sepia"}); diagCode(t, err) != DiagUsage {
		t.Fatalf("bad theme: %v", err)
	}
}

func TestShootAndroidWritesShotsAndRestoresState(t *testing.T) {
	fx := &fakeExec{rules: append(gitRules("apps/client/app/home.tsx"),
		rule{prefix: "adb devices -l", out: CmdOut{Stdout: "List of devices attached\nR5CT30ABC device model:SM_S911B\n"}},
		rule{prefix: "adb -s R5CT30ABC exec-out screencap -p", out: CmdOut{Stdout: "\x89PNG-bytes"}},
	)}
	cfg := testConfig()
	cfg.Lanes[1].TextSizes = nil
	cfg.Lanes[1].Nav = []string{"gesture", "3button"}
	tool := newTool(t, cfg, fx)
	ctx := context.Background()
	if _, err := tool.Init(ctx, InitOptions{Lanes: []string{"android", "phone", "web"}, Screens: []string{"home"}}); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Shoot(ctx, ShootOptions{Lane: "android", Themes: []string{"dark"}})
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(ShootData)
	if len(data.Shots) != 2 || data.Shots[0].Cell != "android--home--dark--gesture" || data.Shots[1].File != "shots/android/home--dark--3button.png" {
		t.Fatalf("shots: %+v", data.Shots)
	}
	run, _ := tool.LoadRun(1)
	for _, c := range run.Cells {
		if c.Lane == "android" && c.Theme == "dark" {
			if c.Shot == "" || c.Verdict != VerdictPending {
				t.Fatalf("cell %s: %+v", c.ID, c)
			}
			if b, err := os.ReadFile(run.ShotPath(c)); err != nil || string(b) != "\x89PNG-bytes" {
				t.Fatalf("shot file %s: %v", c.Shot, err)
			}
		} else if c.Shot != "" {
			t.Fatalf("light cell shot outside the selection: %s", c.ID)
		}
	}
	// state set before the shots: dark + gesture, then 3button; url quoted for the device shell; reset at the end
	joined := make([]string, len(fx.calls))
	for i, c := range fx.calls {
		joined[i] = strings.Join(c, " ")
	}
	order := []string{
		"adb -s R5CT30ABC shell cmd uimode night yes",
		"adb -s R5CT30ABC shell cmd overlay enable-exclusive --category com.android.internal.systemui.navbar.gestural",
		"adb -s R5CT30ABC shell settings put system font_scale 1.0",
		"adb -s R5CT30ABC shell am start -a android.intent.action.VIEW -d 'fixit://home'",
		"adb -s R5CT30ABC exec-out screencap -p",
		"adb -s R5CT30ABC shell cmd overlay enable-exclusive --category com.android.internal.systemui.navbar.threebutton",
		"adb -s R5CT30ABC exec-out screencap -p",
		"adb -s R5CT30ABC shell cmd uimode night no",
		"adb -s R5CT30ABC shell cmd overlay enable-exclusive --category com.android.internal.systemui.navbar.gestural",
	}
	pos := 0
	for _, want := range order {
		idx := slices.Index(joined[pos:], want)
		if idx < 0 {
			t.Fatalf("command %q missing after position %d in:\n%s", want, pos, strings.Join(joined, "\n"))
		}
		pos += idx + 1
	}
	if len(data.Restored) != 3 {
		t.Fatalf("restored: %v", data.Restored)
	}
	if res.Next[0] != "polish-kit sheet --pass 1 --lanes android --json" {
		t.Fatalf("next: %v", res.Next)
	}
	// unsupported kinds name the alternative
	_, err = tool.Shoot(ctx, ShootOptions{Lane: "phone"})
	var de runx.DiagError
	if !errors.As(err, &de) || de.Diag.Code != DiagLaneUnsupported || !strings.Contains(de.Diag.Detail, "shots/phone") {
		t.Fatalf("ios-device: %v", err)
	}
	if _, err := tool.Shoot(ctx, ShootOptions{Lane: "web"}); diagCode(t, err) != DiagLaneUnsupported {
		t.Fatalf("browser: %v", err)
	}
	if _, err := tool.Shoot(ctx, ShootOptions{Lane: "android", Screens: []string{"nope"}}); diagCode(t, err) != DiagUnknownScreen {
		t.Fatalf("unknown screen: %v", err)
	}
	// a device that vanished is device-unavailable, not a crash
	fx.rules = append(gitRules(), rule{prefix: "adb devices -l", out: CmdOut{Stdout: "List of devices attached\n"}})
	if _, err := tool.Shoot(ctx, ShootOptions{Lane: "android"}); diagCode(t, err) != DiagDeviceUnavailable {
		t.Fatalf("gone device: %v", err)
	}
}

func TestShootIOSSimUsesSimctl(t *testing.T) {
	fx := &fakeExec{rules: append(gitRules("apps/client/app/home.tsx"),
		rule{prefix: "xcrun simctl list -j", out: CmdOut{Stdout: simctlFixture}},
	)}
	cfg := testConfig()
	cfg.Lanes[0].Device = "FixIt wt other-branch customer" // the booted one
	tool := newTool(t, cfg, fx)
	ctx := context.Background()
	if _, err := tool.Init(ctx, InitOptions{Lanes: []string{"ios26"}, Screens: []string{"inquiry-new"}}); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Shoot(ctx, ShootOptions{Lane: "ios26"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Data.(ShootData).Shots) != 2 {
		t.Fatalf("%+v", res.Data)
	}
	if !fx.ran("xcrun simctl openurl BBBB-26 fixit://inquiries/new") || !fx.ran("xcrun simctl ui BBBB-26 appearance dark") {
		t.Fatalf("simctl calls:\n%v", fx.calls)
	}
	for _, c := range fx.calls {
		if len(c) > 5 && c[1] == "simctl" && c[2] == "io" && !strings.HasSuffix(c[len(c)-1], "shots/ios26/inquiry-new--"+c[len(c)-1][strings.LastIndex(c[len(c)-1], "--")+2:]) {
			t.Fatalf("screenshot path: %v", c)
		}
	}
	// a shutdown simulator answers with its boot command
	fx.rules[len(fx.rules)-1].out = CmdOut{Stdout: strings.Replace(simctlFixture, `"state": "Booted"`, `"state": "Shutdown"`, 1)}
	_, err = tool.Shoot(ctx, ShootOptions{Lane: "ios26"})
	var de runx.DiagError
	if !errors.As(err, &de) || de.Diag.Code != DiagDeviceUnavailable || !strings.HasPrefix(de.Diag.Fix, "xcrun simctl boot") {
		t.Fatalf("not booted: %v", err)
	}
}

func TestOpenReportsMissingSection(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(`{"lok": {"catalogs": {}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(root, "test", nil)
	var de runx.DiagError
	if !errors.As(err, &de) || de.Diag.Code != DiagNoConfigSection || !strings.Contains(de.Diag.Fix, "polish: {") {
		t.Fatalf("%v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(`{"polish": {"targets": {"app": ["apps/**"]}, "lanes": [], "bogus": 1}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, "test", nil); diagCode(t, err) != DiagNoConfigSection {
		t.Fatalf("unknown key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(`{"polish": {"targets": {"app": ["apps/**"]}, "lanes": [{"id": "ios", "target": "app", "kind": "ios-sim", "runtime": "26", "deviceType": "iPhone 17 Pro"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	tool, err := Open(filepath.Join(root), "test", nil)
	if err != nil || tool.Config.Out != ".polish" || tool.OutDir() != filepath.Join(root, ".polish") {
		t.Fatalf("%+v %v", tool, err)
	}
}

func TestDiagnosticCodesAreClosedAndKebab(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range Codes {
		if seen[c] || c != strings.ToLower(c) || strings.ContainsAny(c, " _") {
			t.Fatalf("code %q", c)
		}
		seen[c] = true
	}
	if len(Codes) != 15 {
		t.Fatalf("the closed enum changed size (%d); update docs/polish-kit.md", len(Codes))
	}
}
