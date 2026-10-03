package perflab

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/devlab"
	"github.com/henderson-tech/vybava/internal/framestats"
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

// A result JSON may name its evidence relative to the case dir, under a
// name the <scenario>-<platform>- fallback would not find.
func TestDiscoverRecordReadsCaseRelativeResultPaths(t *testing.T) {
	runDir := t.TempDir()
	scenario := "calendar-view-switch-smooth"
	caseDir := filepath.Join(runDir, "01-before-1")
	copyTo(t, filepath.Join(framestatsData, "lab120-before."+scenario+"-android-2026-10-02T10-33-10-748Z.json.gz"), filepath.Join(caseDir, "evidence", "frames.json.gz"))
	result := `{"platform":"android","scenario":"` + scenario + `","recordedAt":"2026-10-02T10:33:10.748Z","framesPath":"evidence/frames.json.gz"}`
	if err := os.WriteFile(filepath.Join(caseDir, "result.json"), []byte(result), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := &Tool{Config: &Config{}, Now: time.Now}
	rec, missing := tool.discoverRecord(runDir, caseDir, "android", ScenarioRow{Name: scenario}, RunVariant{Label: "before"}, 1, time.Now(), nil, nil)
	if missing || rec.Evidence.Frames != "01-before-1/evidence/frames.json.gz" {
		t.Fatalf("frames %q (missing=%v): a case-relative framesPath is read against the case dir", rec.Evidence.Frames, missing)
	}
}

// app reset waits for the device lock like every device verb: a reset from
// a second copy sharing the token must not change the world a run is
// measuring under that lock.
func TestAppResetHoldsTheDeviceLock(t *testing.T) {
	ctx := context.Background()
	state := t.TempDir()
	if err := os.WriteFile(filepath.Join(state, "devices.json"), []byte(`{"schemaVersion":1,"devices":{"s20":{"platform":"android","serial":"RF8N21PY1BF"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	lab := &devlab.Lab{StateDir: state, Now: time.Now, TempDir: os.TempDir, LockWait: time.Second, Pid: os.Getpid(),
		Exec:      func(context.Context, devlab.Cmd) (devlab.CmdOut, error) { return devlab.CmdOut{Code: 1}, nil },
		Getenv:    func(string) string { return "" },
		ProcStart: func(int) (time.Time, bool, error) { return time.Time{}, false, nil },
		StopGroup: func(int) error { return nil }}
	acq, err := lab.Acquire(ctx, "s20", devlab.AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	token := acq.Data.(devlab.AcquireData).Token
	run, err := lab.Hold("s20", token, "run")
	if err != nil {
		t.Fatal(err)
	}
	defer run.Done()
	marker := filepath.Join(t.TempDir(), "reset-ran")
	tool := &Tool{Config: &Config{Hooks: &HooksConfig{ResetWorld: "touch " + marker}}, Lab: lab, Now: time.Now}
	if _, err := tool.AppReset(ctx, AppOptions{Device: "s20", Lease: token, World: "default"}); CodeOf(err) != devlab.DiagDeviceBusy || fileExists(marker) {
		t.Fatalf("reset under a held device lock: %v (hook ran: %v)", err, fileExists(marker))
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
	for _, c := range []struct {
		tap  string
		taps [][]int
		ok   bool
	}{
		{"", [][]int{{540, 210}}, true},
		{"760,160", [][]int{{760, 160}}, true},
		{"none", nil, true},
		{"1200,10", nil, false},
		{"top", nil, false},
	} {
		setup, err := restSetup(c.tap)
		var taps [][]int
		for _, g := range setup {
			if g.Tap != nil {
				taps = append(taps, g.Tap)
			}
		}
		if (err == nil) != c.ok || fmt.Sprint(taps) != fmt.Sprint(c.taps) {
			t.Errorf("--tap %q: taps %v err %v; a header control under the default spot needs another (FixIt Marketplace Pro's search pill)", c.tap, taps, err)
		}
	}
	if _, err := (&Tool{}).Probe(context.Background(), ProbeOptions{Kind: "drag", Tap: "760,160"}); CodeOf(err) != DiagUsage {
		t.Errorf("--tap on a drag probe is USAGE before any device call: %v", err)
	}
	still := func(frames int) analysis.RunAnalysis {
		m := &analysis.Metrics{Present: &framestats.PresentMetrics{FrameTimeline: true, Frames: frames}}
		return analysis.RunAnalysis{Records: []analysis.RecordResult{{Metrics: m}}}
	}
	for _, c := range []struct {
		kind   string
		frames int
		warns  int
	}{{"drag", 0, 1}, {"fling", 0, 1}, {"drag", 2600, 0}, {"rest", 0, 0}} {
		if d := gestureMovedNothing(c.kind, still(c.frames), "s20"); len(d) != c.warns || (c.warns == 1 && d[0].Code != framestats.DiagNoAppFrames) {
			t.Errorf("%s with %d frames: %+v (an empty Messages screen does not scroll)", c.kind, c.frames, d)
		}
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
	rows, err = parseScenarioRows(`[{"name":"probe-rest-search-journey","windowMs":20000,
	  "exempt":[{"name":"live-search-radar","reason":"the search keeps its radar","keys":["androidRestFramesMax","androidRestRunMsMax"]}]}]`, "fix")
	if err != nil || len(rows[0].Exempt) != 1 || rows[0].Exempt[0].Name != "live-search-radar" {
		t.Fatalf("an exemption row = %+v, %v", rows, err)
	}
	for _, bad := range []string{
		`[{"name":"a","windowMs":1,"budget":{"iosHitchRatioMax":5}}]`,
		`[{"name":"a","windowMs":1,"window":2}]`,
		`[{"name":"a","windowMs":0}]`,
		`[{"name":"a","windowMs":1},{"name":"a","windowMs":2}]`,
		`not json`,
		`[{"name":"a","windowMs":1,"exempt":[{"name":"radar","keys":["androidRestFramesMax"]}]}]`,
		`[{"name":"a","windowMs":1,"exempt":[{"name":"radar","reason":"kept","keys":[]}]}]`,
		`[{"name":"a","windowMs":1,"exempt":[{"name":"radar","reason":"kept","keys":["restFrames"]}]}]`,
	} {
		if _, err := parseScenarioRows(bad, "fix"); CodeOf(err) != DiagConfigInvalid {
			t.Errorf("%s: want CONFIG_INVALID, got %v", bad, err)
		}
	}
	_ = io.Discard
}

// One adapter runner.env serves both platforms: on the S20 the iOS-only
// {wdaBundleId} once failed the whole run CONFIG_INVALID (2026-10-02). A
// token that resolves to nothing leaves its variable unset in the child.
func TestRunnerCommandLeavesForeignPlatformEnvUnset(t *testing.T) {
	c := decodeSection(t, fixitSection)
	tool := &Tool{Config: &c}
	for _, tc := range []struct {
		platform  string
		vars      map[string]string
		wantEnv   string
		wantUnset string
	}{
		{"android", map[string]string{"udid": "", "serial": "RF8N21PY1BF", "wdaDerivedData": "", "timeoutMs": "1", "runDir": "/r"},
			"FIXIT_APPIUM_ANDROID_UDID=RF8N21PY1BF", "FIXIT_APPIUM_IOS_UDID FIXIT_APPIUM_WDA_DERIVED_DATA"},
		{"ios", map[string]string{"udid": "00008030-001E6D961122802E", "serial": "", "wdaDerivedData": "/c/wda", "timeoutMs": "1", "runDir": "/r"},
			"FIXIT_APPIUM_IOS_UDID=00008030-001E6D961122802E", "FIXIT_APPIUM_ANDROID_UDID"},
	} {
		v := NewVars(tc.vars)
		v.Set("platform", tc.platform)
		v.SetList("scenarios", []string{"calendar-view-switch-smooth"})
		_, env, unset, err := tool.runnerCommand(v)
		if err != nil {
			t.Fatalf("%s: %v", tc.platform, err)
		}
		if !slices.Contains(env, tc.wantEnv) {
			t.Errorf("%s: env %v lacks %s", tc.platform, env, tc.wantEnv)
		}
		for _, name := range strings.Fields(tc.wantUnset) {
			if !slices.Contains(unset, name) {
				t.Errorf("%s: %s not unset (%v)", tc.platform, name, unset)
			}
		}
		if !slices.Contains(unset, "FIXIT_APPIUM_XCODE_ORG_ID") {
			t.Errorf("%s: runner.unset dropped: %v", tc.platform, unset)
		}
		if got := slices.Contains(env, "ANDROID_SERIAL=RF8N21PY1BF"); got != (tc.platform == "android") {
			t.Errorf("%s: ANDROID_SERIAL exported = %v (bare adb in the runner must hit the leased phone)", tc.platform, got)
		}
	}
}

// Appium truncates its --log on every start: a case's server log can be
// shorter than the previous case's, and the WDA watchdog must still see the
// "started" line (it killed two healthy iPhone 11 cases at 196 s).
func TestReadFromSeesATruncatedServerLog(t *testing.T) {
	log := filepath.Join(t.TempDir(), "appium-server.log")
	if err := os.WriteFile(log, []byte(strings.Repeat("previous case line\n", 40)), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(log)
	if err != nil {
		t.Fatal(err)
	}
	off := st.Size()
	line := "2026-10-02 21:46:49:675 [b3f2a28c][XCUITestDriver@6a6c] WebDriverAgent successfully started after 4817ms\n"
	if err := os.WriteFile(log, []byte("2026-10-02 22:21:09:570 [Appium] Welcome to Appium v3.4.2\n"+line), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readFrom(log, off); !wdaStartedRe.MatchString(got) {
		t.Fatalf("the truncated log read %q", got)
	}
	// An appended log still reads only what this case wrote.
	if err := os.WriteFile(log, []byte(strings.Repeat("x\n", 200)+line), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readFrom(log, 400); got != line {
		t.Fatalf("appended read %q", got)
	}
}

// A watchdog kill reaches the runner's whole tree: Appium's WDA xcodebuild
// sits in a process group of its own and survived the group kill, holding
// the iPhone 11's XCTest session into the next case (2026-10-02).
func TestDescendantPIDsFollowTheTreeNotTheGroup(t *testing.T) {
	ps := ` 42613     1 perflab run calendar-view-switch-smooth --device iphone11
 50001 42613 /bin/sh -c bun scripts/perf/perflab-adapter.ts run ios calendar-view-switch-smooth
 50002 50001 bun scripts/perf/perflab-adapter.ts run ios calendar-view-switch-smooth
 50003 50002 node node_modules/.bin/wdio run appium/config/wdio.conf.ts
 50004 50003 node node_modules/.bin/appium --base-path / --address 127.0.0.1 --port 14021
 30256 50004 /Applications/Xcode.app/Contents/Developer/usr/bin/xcodebuild test-without-building -destination id=00008030-001E6D961122802E
 99688     1 ios forward --udid=00008030-001E6D961122802E 8120 8100
`
	if got, want := descendantPIDs(ps, 50001), []int{50002, 50003, 50004, 30256}; !slices.Equal(got, want) {
		t.Fatalf("descendants = %v, want %v (never the perflab parent or an unrelated forward)", got, want)
	}
}

// The resume line keeps the plan: without --alternate --repeat 2 a resume
// re-planned one block per variant and never re-ran the failed block 4.
func TestResumeCommandKeepsThePlan(t *testing.T) {
	o := RunOptions{Scenarios: []string{"calendar-view-switch-smooth"}, Lease: "plt_x",
		Variants: []string{"before=pf1-102b3c20-c341225a7536", "after=pf1-102b3c20-602c1c785cbf"}, Alternate: true, Repeat: 2}
	want := "perflab run calendar-view-switch-smooth --device iphone11 --lease plt_x --variant before=pf1-102b3c20-c341225a7536 --variant after=pf1-102b3c20-602c1c785cbf --alternate --repeat 2 --resume '/x/My runs' --json"
	if got := o.resumeCommand("iphone11", "/x/My runs"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

// A probe writes one record per run dir, so a probe A/B gives compare a
// side of several dirs joined by commas: two dirs a side reach the noise
// rule's two samples (one dir a side never could: TOO_FEW_RUNS). The same
// lab sidecars stand in for two rest probes of each variant.
func TestCompareJoinsRunDirsPerSide(t *testing.T) {
	scenario := "calendar-view-switch-smooth"
	row := ScenarioRow{Name: scenario, WindowMs: 105000}
	tool := &Tool{Config: &Config{App: AppConfig{Root: "apps/client", Android: &AndroidAppConfig{Package: "app.fixit.client"}}}, Now: time.Now}
	probeDir := func(label, prefix, stamp string) string {
		dir := t.TempDir()
		caseDir := filepath.Join(dir, caseDirName(RunBlock{Seq: 1, Variant: label, Attempt: 1}))
		copyTo(t, filepath.Join(framestatsData, prefix+"."+scenario+"-android-"+stamp+".json.gz"), filepath.Join(caseDir, "frames", scenario+"-android-"+stamp+".json.gz"))
		rec, missing := tool.discoverRecord(dir, caseDir, "android", row, RunVariant{Label: label, NativeKey: "pf1-0b95220e9a40cd6e79d2"}, 1, time.Now(), nil, nil)
		if missing {
			t.Fatal("the sidecar must be found")
		}
		rf := analysis.RunFile{Version: analysis.RunFileVersion, Provenance: analysis.Provenance{Device: "s20", Platform: analysis.PlatformAndroid, RefreshHz: 120, InputSource: analysis.InputADB}, Runs: []analysis.RunRecord{rec}}
		if err := writeRunFile(dir, rf); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	a1, a2 := probeDir("before", "lab120-before", "2026-10-02T10-33-10-748Z"), probeDir("before", "lab120-before", "2026-10-02T10-33-10-748Z")
	b1, b2 := probeDir("layer", "lab120-layer", "2026-10-02T10-48-32-456Z"), probeDir("layer", "lab120-layer", "2026-10-02T10-48-32-456Z")
	tool.Lab = &devlab.Lab{StateDir: t.TempDir()}
	res, err := tool.Compare(context.Background(), []string{a1 + "," + a2, b1 + "," + b2}, CompareOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range res.Diagnostics {
		if d.Code == analysis.DiagTooFewRuns {
			t.Fatalf("two dirs a side are two samples: %+v", res.Diagnostics)
		}
	}
	cmp := res.Data.(analysis.Comparison)
	if len(cmp.A.Runs) != 2 || len(cmp.B.Runs) != 2 || len(cmp.Rows) == 0 {
		t.Fatalf("sides %d/%d runs, %d rows", len(cmp.A.Runs), len(cmp.B.Runs), len(cmp.Rows))
	}
	if !strings.Contains(res.Next[0], a2) || strings.Contains(res.Next[0], ",") {
		t.Errorf("the report line lists every dir as its own word: %s", res.Next[0])
	}

	// The shell expands only a side's first ~ (the A16 sweep's
	// "~/probes-ios/a,~/probes-ios/b" read the second literally).
	home := filepath.Dir(a2)
	t.Setenv("HOME", home)
	tilde := "~/" + filepath.Base(a2)
	if _, err := tool.Compare(context.Background(), []string{a1 + "," + tilde, b1 + "," + b2}, CompareOptions{}); err != nil {
		t.Errorf("a ~ after the comma: %v", err)
	}

	// One dir a phone left without its perflab.run.json costs its own rows
	// in a sweep's report, never the whole report.
	broken := t.TempDir()
	rep, err := tool.Report(context.Background(), []string{a1, broken, b1}, ReportOptions{})
	if err != nil {
		t.Fatalf("report with one unreadable dir: %v", err)
	}
	skipped := false
	for _, d := range rep.Diagnostics {
		skipped = skipped || (d.Severity == "warning" && strings.Contains(d.Detail, "skipped "+broken))
	}
	if data := rep.Data.(analysis.ReportData); !skipped || len(data.Rows) == 0 {
		t.Errorf("report rows %d, diagnostics %+v", len(data.Rows), rep.Diagnostics)
	}
	if _, err := tool.Report(context.Background(), []string{broken}, ReportOptions{}); err == nil {
		t.Error("a lone unreadable dir is still an error")
	}
}

// A probe's --out dir keeps its evidence out of git like a run's: the FixIt
// sweep's probe dirs under ~/Exports held 10 MB pftraces with no ignore.
func TestProbeRunDirIgnoresTheEvidence(t *testing.T) {
	out := filepath.Join(t.TempDir(), "probes", "hub-rest-1")
	dir, err := (&Tool{Now: time.Now}).probeRunDir(ProbeOptions{Kind: "rest", Out: out})
	if err != nil || dir != out {
		t.Fatalf("dir %q err %v", dir, err)
	}
	raw, err := os.ReadFile(filepath.Join(out, ".gitignore"))
	if err != nil || !strings.Contains(string(raw), "*.pftrace") || !strings.Contains(string(raw), "*.trace/") {
		t.Fatalf(".gitignore = %q (%v)", raw, err)
	}
}

// An alternating run indexes one run dir under both labels: compare
// before layer reads each side's own variant there (both sides pooled
// every record and read identical), and a side compared with itself is
// USAGE.
func TestCompareLabelsOfOneAlternatingRunDir(t *testing.T) {
	scenario := "calendar-view-switch-smooth"
	row := ScenarioRow{Name: scenario, WindowMs: 105000}
	tool := &Tool{Config: &Config{App: AppConfig{Root: "apps/client", Android: &AndroidAppConfig{Package: "app.fixit.client"}}}, Now: time.Now,
		Lab: &devlab.Lab{StateDir: t.TempDir()}}
	runDir := t.TempDir()
	rf := analysis.RunFile{Version: analysis.RunFileVersion, Provenance: analysis.Provenance{Device: "s20", Platform: analysis.PlatformAndroid, RefreshHz: 120, InputSource: analysis.InputADB}}
	for i, side := range []struct{ label, prefix, stamp string }{
		{"before", "lab120-before", "2026-10-02T10-33-10-748Z"},
		{"layer", "lab120-layer", "2026-10-02T10-48-32-456Z"},
	} {
		caseDir := filepath.Join(runDir, caseDirName(RunBlock{Seq: i + 1, Variant: side.label, Attempt: 1}))
		copyTo(t, filepath.Join(framestatsData, side.prefix+"."+scenario+"-android-"+side.stamp+".json.gz"), filepath.Join(caseDir, "frames", scenario+"-android-"+side.stamp+".json.gz"))
		rec, missing := tool.discoverRecord(runDir, caseDir, "android", row, RunVariant{Label: side.label, NativeKey: "pf1-0b95220e9a40cd6e79d2"}, 1, time.Now(), nil, nil)
		if missing {
			t.Fatal("the sidecar must be found")
		}
		rf.Runs = append(rf.Runs, rec)
	}
	if err := writeRunFile(runDir, rf); err != nil {
		t.Fatal(err)
	}
	if err := tool.appendLedger(runDir, "s20", []RunVariant{{Label: "before"}, {Label: "layer"}}, []string{scenario}); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Compare(context.Background(), []string{"before", "layer"}, CompareOptions{MinRuns: 1})
	if err != nil {
		t.Fatal(err)
	}
	cmp := res.Data.(analysis.Comparison)
	if cmp.A.Variant != "before" || cmp.B.Variant != "layer" || len(cmp.Rows) == 0 {
		t.Errorf("sides %q/%q, %d rows", cmp.A.Variant, cmp.B.Variant, len(cmp.Rows))
	}
	moved := false
	for _, r := range cmp.Rows {
		moved = moved || r.A.Median != r.B.Median
	}
	if !moved {
		t.Errorf("each label must read only its own records: every row reads A == B %+v", cmp.Rows)
	}
	if _, err := tool.Compare(context.Background(), []string{"before", "before"}, CompareOptions{MinRuns: 1}); CodeOf(err) != DiagUsage {
		t.Errorf("a side compared with itself: %v", err)
	}
	if _, err := tool.Compare(context.Background(), []string{runDir, runDir}, CompareOptions{MinRuns: 1}); CodeOf(err) != DiagUsage {
		t.Errorf("a run dir compared with itself: %v", err)
	}
}

// report --gate and a confounded compare fail with their data kept: the
// envelope carries the rows and --md writes the report precisely when the
// gate fails.
func TestReportAndCompareKeepTheirDataOnAGateError(t *testing.T) {
	tool := &Tool{Now: time.Now, Lab: &devlab.Lab{StateDir: t.TempDir()}}
	dir := t.TempDir()
	rf := analysis.RunFile{Version: analysis.RunFileVersion, Provenance: analysis.Provenance{Device: "s20", Platform: analysis.PlatformAndroid, RefreshHz: 120, InputSource: analysis.InputADB},
		Runs: []analysis.RunRecord{{Scenario: "probe-drag-worker-hub", Attempt: 1, Failed: true, RecordedAt: time.Now(), Build: analysis.Build{Variant: "before"}}}}
	if err := writeRunFile(dir, rf); err != nil {
		t.Fatal(err)
	}
	md := filepath.Join(t.TempDir(), "report.md")
	res, err := tool.Report(context.Background(), []string{dir}, ReportOptions{Gate: true, MD: md})
	if CodeOf(err) != analysis.DiagNothingMeasured {
		t.Fatalf("gate: %v", err)
	}
	if data, ok := res.Data.(analysis.ReportData); !ok || len(data.Rows) != 1 {
		t.Errorf("the failing gate keeps its rows: %+v", res.Data)
	}
	if raw, err := os.ReadFile(md); err != nil || !strings.Contains(string(raw), "probe-drag-worker-hub") {
		t.Errorf("--md is written when the gate fails: %q %v", raw, err)
	}

	scenario := "calendar-view-switch-smooth"
	row := ScenarioRow{Name: scenario, WindowMs: 105000}
	tool.Config = &Config{App: AppConfig{Root: "apps/client", Android: &AndroidAppConfig{Package: "app.fixit.client"}}}
	side := func(label, prefix, stamp, nativeKey string) string {
		dir := t.TempDir()
		caseDir := filepath.Join(dir, caseDirName(RunBlock{Seq: 1, Variant: label, Attempt: 1}))
		copyTo(t, filepath.Join(framestatsData, prefix+"."+scenario+"-android-"+stamp+".json.gz"), filepath.Join(caseDir, "frames", scenario+"-android-"+stamp+".json.gz"))
		rec, _ := tool.discoverRecord(dir, caseDir, "android", row, RunVariant{Label: label, NativeKey: nativeKey}, 1, time.Now(), nil, nil)
		run := analysis.RunFile{Version: analysis.RunFileVersion, Provenance: rf.Provenance, Runs: []analysis.RunRecord{rec}}
		if err := writeRunFile(dir, run); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	a := side("before", "lab120-before", "2026-10-02T10-33-10-748Z", "pf1-0b95220e9a40cd6e79d2")
	b := side("layer", "lab120-layer", "2026-10-02T10-48-32-456Z", "pf1-ffffffffffffffffffff")
	res, err = tool.Compare(context.Background(), []string{a, b}, CompareOptions{MinRuns: 1})
	if CodeOf(err) != analysis.DiagConfounded {
		t.Fatalf("two native builds: %v", err)
	}
	if cmp, ok := res.Data.(analysis.Comparison); !ok || len(cmp.Confounders) == 0 {
		t.Errorf("the confounded compare keeps its confounders: %+v", res.Data)
	}
}
