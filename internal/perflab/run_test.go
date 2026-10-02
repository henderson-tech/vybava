package perflab

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/devlab"
	"github.com/henderson-tech/vybava/internal/perflab/analysis"
)

const framestatsData = "../framestats/testdata"

func copyTo(t *testing.T, src, dst string) {
	t.Helper()
	raw, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPlanBlocks(t *testing.T) {
	vs := []RunVariant{{Label: "before"}, {Label: "after"}}
	label := func(bs []RunBlock) string {
		var out []string
		for _, b := range bs {
			out = append(out, caseDirName(b))
		}
		return strings.Join(out, " ")
	}
	if got := label(planBlocks(vs, 2, true)); got != "01-before-1 02-after-1 03-before-2 04-after-2" {
		t.Fatalf("alternate = %s", got)
	}
	if got := label(planBlocks(vs, 2, false)); got != "01-before-1 02-before-2 03-after-1 04-after-2" {
		t.Fatalf("grouped = %s", got)
	}
}

// TestRunDirRoundTrip lays two case dirs out as the FixIt runner leaves
// them (a result JSON naming the sidecar, frames/, run.log with the taps),
// discovers their records as run does, writes perflab.run.json and has
// compare read it back: the lab120 layer variant's mode switches 77 -> 108.
func TestRunDirRoundTrip(t *testing.T) {
	runDir := t.TempDir()
	scenario := "calendar-view-switch-smooth"
	row := ScenarioRow{Name: scenario, WindowMs: 105000,
		StepCycle:  strings.Split("menu,multi,menu,team,menu,list,menu,day,up-to-month,up-to-year,into-month,into-day", ","),
		StepGroups: map[string][]string{"modes": {"multi", "team", "list", "day"}}}
	tool := &Tool{Config: &Config{App: AppConfig{Root: "apps/client", Android: &AndroidAppConfig{Package: "app.fixit.client"}}}, Now: time.Now}
	rf := analysis.RunFile{Version: analysis.RunFileVersion, Provenance: analysis.Provenance{Device: "s20", Platform: analysis.PlatformAndroid, RefreshHz: 120, InputSource: analysis.InputW3C}}
	// before carries a result JSON (the runner's shape); layer leaves only
	// frames/<scenario>-android-<stamp>.json.gz, found by name and anchored on
	// the stamp.
	for i, side := range []struct{ label, prefix, stamp, iso string }{
		{"before", "lab120-before", "2026-10-02T10-33-10-748Z", "2026-10-02T10:33:10.748Z"},
		{"layer", "lab120-layer", "2026-10-02T10-48-32-456Z", ""},
	} {
		caseDir := filepath.Join(runDir, caseDirName(RunBlock{Seq: i + 1, Variant: side.label, Attempt: 1}))
		sidecar := filepath.Join(caseDir, "frames", scenario+"-android-"+side.stamp+".json.gz")
		copyTo(t, filepath.Join(framestatsData, side.prefix+"."+scenario+"-android-"+side.stamp+".json.gz"), sidecar)
		copyTo(t, filepath.Join(framestatsData, side.prefix+".taps.log"), filepath.Join(caseDir, "run.log"))
		if side.iso != "" {
			result := `{"platform":"android","scenario":"` + scenario + `","recordedAt":"` + side.iso +
				`","device":"RF8N21PY1BF","metrics":{"fpsP10":60},"framesPath":"` + sidecar + `"}`
			if err := os.WriteFile(filepath.Join(caseDir, scenario+"-android-"+side.stamp+".json"), []byte(result), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		rec, missing := tool.discoverRecord(runDir, caseDir, "android", row, RunVariant{Label: side.label, NativeKey: "pf1-0b95220e9a40cd6e79d2"}, 1, time.Now(), nil, nil)
		if missing || !strings.HasPrefix(rec.Evidence.Frames, filepath.Base(caseDir)+"/frames/") || rec.Evidence.Package != "app.fixit.client" || rec.Evidence.Log == "" {
			t.Fatalf("discovered %+v (missing=%v)", rec.Evidence, missing)
		}
		rf.Runs = append(rf.Runs, rec)
	}
	empty := filepath.Join(runDir, "03-after-1")
	if err := os.MkdirAll(empty, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, missing := tool.discoverRecord(runDir, empty, "android", row, RunVariant{Label: "after"}, 1, time.Now(), nil, nil); !missing {
		t.Fatal("a case dir without evidence must report it missing")
	}
	if err := writeRunFile(runDir, rf); err != nil {
		t.Fatal(err)
	}
	if _, err := analysis.LoadRunDir(runDir); err != nil {
		t.Fatalf("the run file perflab writes must load under the strict decoder: %v", err)
	}
	if !blockDone(rf, &RunBlock{Variant: "layer", Attempt: 1}, []ScenarioRow{row}) || blockDone(rf, &RunBlock{Variant: "after", Attempt: 1}, []ScenarioRow{row}) {
		t.Fatal("blockDone must see the finished block and not the missing one")
	}
	tool.Exec = nil
	tool.Lab = &devlab.Lab{StateDir: t.TempDir()}
	res, err := tool.Compare(context.Background(), []string{runDir}, CompareOptions{MinRuns: 1})
	if err != nil {
		t.Fatal(err)
	}
	cmp := res.Data.(analysis.Comparison)
	var modes *analysis.StepCompare
	for i := range cmp.Steps {
		if cmp.Steps[i].Step == "modes" {
			modes = &cmp.Steps[i]
		}
	}
	if modes == nil || int(modes.A+0.5) != 77 || int(modes.B+0.5) != 108 {
		t.Fatalf("modes = %+v; steps %+v; diags %+v", modes, cmp.Steps, res.Diagnostics)
	}
	if len(res.Next) == 0 || !strings.HasPrefix(res.Next[0], "perflab report ") {
		t.Fatalf("next = %v", res.Next)
	}
}

func TestDescendantsAndClassify(t *testing.T) {
	ps := `  100     1 /bin/sh -c bun scripts/perf/run.ts ios calendar
  101   100 bun scripts/perf/run.ts ios calendar
  102   101 node appium
  103   102 xcodebuild build-for-testing -project WebDriverAgent.xcodeproj
  104     1 xcodebuild test-without-building -xctestrun other`
	got := strings.Join(descendants(ps, 100), "|")
	if !strings.Contains(got, "build-for-testing") || strings.Contains(got, "other") {
		t.Fatalf("descendants = %s", got)
	}
	if !attachFailedRe.MatchString("Cannot find process for provided pid: 812") || !adbGoneRe.MatchString("error: device 'RF8N21PY1BF' not found") {
		t.Fatal("failure classifiers")
	}
	meminfo := "MemTotal:        7697856 kB\nSwapTotal:       4194300 kB\nSwapFree:        3145724 kB\n"
	if mb, ok := swapUsedMb(meminfo); !ok || mb != 1024 {
		t.Fatalf("swap = %v %v", mb, ok)
	}
}

func TestProbeHelpers(t *testing.T) {
	w, h, ok := screenSize("Physical size: 1080x2400\nOverride size: 720x1600\n")
	if !ok || w != 720 || h != 1600 {
		t.Fatalf("an override wins: %dx%d", w, h)
	}
	if got := strings.Join(scaleArgs(Gesture{Swipe: []int{540, 1700, 540, 1100, 1200}}, 720, 1600), " "); got != "input swipe 360 1133 360 733 1200" {
		t.Fatalf("scaled = %s", got)
	}
	_, drag := recipes("drag")
	if len(drag) != 24 || scriptDuration(drag) != 6*(2*1200+2*300)*time.Millisecond {
		t.Fatalf("drag recipe: %d steps, %s", len(drag), scriptDuration(drag))
	}
	if !strings.Contains(perfettoConfig, `atrace_apps: "{{package}}"`) || !strings.Contains(perfettoConfig, "duration_ms: {{duration_ms}}") ||
		!strings.Contains(perfettoConfig, "android.surfaceflinger.frametimeline") {
		t.Fatal("the embedded Perfetto config lost a placeholder or FrameTimeline")
	}
}

func TestCrashParsers(t *testing.T) {
	since := time.Unix(1790905100, 0).UTC()
	out := `--------- beginning of crash
 1790905050.120  4321  4321 E AndroidRuntime: FATAL EXCEPTION: main
 1790905200.500  4321  4321 E AndroidRuntime: FATAL EXCEPTION: main
 1790905200.501  4321  4321 E AndroidRuntime: Process: app.fixit.client.dev, PID: 4321
 1790905201.000   999   999 I ActivityManager: unrelated`
	got := parseLogcatSince(out, since, func(s string) bool {
		return strings.Contains(s, "FATAL") || strings.Contains(s, "app.fixit.client.dev")
	})
	if len(got) != 2 || !got[0].At.Equal(time.Unix(1790905200, 500e6).UTC()) {
		t.Fatalf("crashes = %+v", got)
	}
	names := ipsNames([]byte(`{"result":{"files":[{"name":"FixIt-2026-10-01-184319.ips"},{"path":"/x/JetsamEvent-2026-10-01-100000.ips"},{"name":"notes.txt"}]}}`))
	if strings.Join(names, ",") != "FixIt-2026-10-01-184319.ips,JetsamEvent-2026-10-01-100000.ips" {
		t.Fatalf("ips names = %v", names)
	}
	if m := ipsNameRe.FindStringSubmatch(names[0]); m == nil || m[1] != "FixIt" || m[2] != "2026-10-01-184319" {
		t.Fatalf("ips name parts = %v", m)
	}
}

func TestScenarioRows(t *testing.T) {
	rows, err := parseScenarioRows(`[{"name":"calendar-view-switch-smooth","windowMs":105000,"platforms":["ios","android"],
	  "budget":{"iosHitchRatioMsPerSMax":5,"androidAnimatingFpsP10MinShare":0.75},"stepCycle":["menu","multi"]}]`, "fix")
	if err != nil || len(rows) != 1 || rows[0].WindowMs != 105000 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	for _, bad := range []string{
		`[{"name":"a","windowMs":1,"budget":{"iosHitchRatioMax":5}}]`,
		`[{"name":"a","windowMs":1,"window":2}]`,
		`[{"name":"a","windowMs":0}]`,
		`[{"name":"a","windowMs":1},{"name":"a","windowMs":2}]`,
		`not json`,
	} {
		if _, err := parseScenarioRows(bad, "fix"); CodeOf(err) != DiagConfigInvalid {
			t.Errorf("%s: want CONFIG_INVALID, got %v", bad, err)
		}
	}
	_ = io.Discard
}
