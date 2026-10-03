package perflab

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/devlab"
	"github.com/henderson-tech/vybava/internal/framestats"
	"github.com/henderson-tech/vybava/internal/perflab/analysis"
	"github.com/henderson-tech/vybava/internal/perflab/buildindex"
	"github.com/henderson-tech/vybava/internal/perflab/doctor"
	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/shellword"
)

// perfettoConfig is the probe's trace config: a 128 MB ring for ftrace
// (sched, cpu/gpu frequency, idle) and atrace (gfx, view, input, hal,
// sched, freq, dalvik, the app), an 8 MB one for process stats, and
// SurfaceFlinger's FrameTimeline. Lifted from the S20 render kit.
//
//go:embed perfetto.cfg
var perfettoConfig string

// ProbeKinds are the probe recipes; iOS runs rest only.
var ProbeKinds = []string{"rest", "drag", "fling", "custom"}

// ProbeOptions are the probe flags.
type ProbeOptions struct {
	Kind        string
	Device      string
	Lease       string
	Package     string
	Seconds     int
	Label       string
	GestureFile string
	Out         string
}

// Gesture is one step of a gesture script, in the S20's 1080x2400
// coordinates (scaled to the device's `wm size`).
type Gesture struct {
	Swipe   []int `json:"swipe,omitempty"` // x1 y1 x2 y2 ms
	Tap     []int `json:"tap,omitempty"`   // x y
	SleepMs int   `json:"sleepMs,omitempty"`
}

const (
	baseW = 1080
	baseH = 2400
)

// Recipes are the gesture scripts per kind: setup runs before the trace,
// script inside it (after a 2 s lead-in).
func recipes(kind string) (setup, script []Gesture) {
	toTop := []Gesture{{Swipe: []int{540, 700, 540, 2100, 80}}, {SleepMs: 400}, {Swipe: []int{540, 700, 540, 2100, 80}}, {SleepMs: 600}}
	switch kind {
	case "rest":
		return []Gesture{{Tap: []int{540, 210}}, {SleepMs: 1000}}, nil
	case "drag":
		setup = append(toTop, Gesture{Swipe: []int{540, 1800, 540, 900, 1200}}, Gesture{SleepMs: 800})
		for i := 0; i < 6; i++ {
			script = append(script, Gesture{Swipe: []int{540, 1700, 540, 1100, 1200}}, Gesture{SleepMs: 300},
				Gesture{Swipe: []int{540, 1100, 540, 1700, 1200}}, Gesture{SleepMs: 300})
		}
		return setup, script
	case "fling":
		setup = append(toTop, Gesture{Swipe: []int{540, 1800, 540, 900, 1200}}, Gesture{SleepMs: 800})
		for i := 0; i < 6; i++ {
			script = append(script, Gesture{Swipe: []int{540, 1900, 540, 500, 90}}, Gesture{SleepMs: 1300},
				Gesture{Swipe: []int{540, 600, 540, 2000, 90}}, Gesture{SleepMs: 1300})
		}
		return setup, script
	}
	return nil, nil
}

// scaleArgs turns a gesture into `adb shell input …` arguments for a
// screen of w x h.
func scaleArgs(g Gesture, w, h int) []string {
	sx := func(x int) string { return strconv.Itoa(x * w / baseW) }
	sy := func(y int) string { return strconv.Itoa(y * h / baseH) }
	switch {
	case len(g.Swipe) == 5:
		return []string{"input", "swipe", sx(g.Swipe[0]), sy(g.Swipe[1]), sx(g.Swipe[2]), sy(g.Swipe[3]), strconv.Itoa(g.Swipe[4])}
	case len(g.Tap) == 2:
		return []string{"input", "tap", sx(g.Tap[0]), sy(g.Tap[1])}
	}
	return nil
}

func scriptDuration(gs []Gesture) time.Duration {
	var d time.Duration
	for _, g := range gs {
		d += time.Duration(g.SleepMs) * time.Millisecond
		if len(g.Swipe) == 5 {
			d += time.Duration(g.Swipe[4]) * time.Millisecond
		}
	}
	return d
}

var wmSizeRe = regexp.MustCompile(`(?m)^(?:Override|Physical) size:\s*(\d+)x(\d+)`)

// screenSize reads `wm size` (an override wins over the physical size).
func screenSize(out string) (int, int, bool) {
	w, h := 0, 0
	for _, m := range wmSizeRe.FindAllStringSubmatch(out, -1) {
		w, _ = strconv.Atoi(m[1])
		h, _ = strconv.Atoi(m[2])
		if strings.HasPrefix(m[0], "Override") {
			break
		}
	}
	return w, h, w > 0 && h > 0
}

// probeBudget is the default verdict per kind (analysis.ProbeBudget).
func probeBudget(kind, platform string) json.RawMessage {
	return analysis.ProbeBudget(kind, analysis.Platform(platform))
}

// Probe takes a quick device-only measurement: a Perfetto trace around a
// gesture recipe on Android, an xctrace window at rest on iOS. The trace
// and a perflab.run.json land in a run dir, so analyze, compare and report
// read it like any run.
func (t *Tool) Probe(ctx context.Context, o ProbeOptions) (Result, error) {
	if !contains(ProbeKinds, o.Kind) {
		return Result{}, diag(DiagUsage, fmt.Sprintf("probe kind %q is not rest, drag, fling or custom", o.Kind), "perflab probe rest --device "+orElse(o.Device, "<id>")+" --lease "+orElse(o.Lease, "<token>")+" --json")
	}
	if o.Kind == "custom" && o.GestureFile == "" {
		return Result{}, diag(DiagUsage, "probe custom needs --gesture-file <json>", "perflab probe custom --gesture-file <json> --device "+orElse(o.Device, "<id>")+" --lease "+orElse(o.Lease, "<token>")+" --json")
	}
	if o.Seconds <= 0 {
		o.Seconds = 20
	}
	h, err := t.Lab.Hold(o.Device, o.Lease, "probe")
	if err != nil {
		return Result{}, err
	}
	defer h.Done()
	if h.Device.Platform == devlab.PlatformIOS && o.Kind != "rest" {
		return Result{}, diag(DiagUsage, "on iOS probe runs rest only; drag and fling stay in the project's Appium scenarios", "perflab probe rest --device "+h.ID+" --lease "+o.Lease+" --json")
	}
	pkg := o.Package
	if pkg == "" && t.Config != nil {
		if h.Device.Platform == devlab.PlatformAndroid && t.Config.App.Android != nil {
			pkg = t.Config.App.Android.Package
		}
		if h.Device.Platform == devlab.PlatformIOS && t.Config.App.IOS != nil {
			pkg = t.Config.App.IOS.BundleID
		}
	}
	if pkg == "" {
		return Result{}, diag(DiagUsage, "no app to probe: pass --package, or run from a project whose perflab section names the app", "perflab probe "+o.Kind+" --device "+h.ID+" --lease "+o.Lease+" --package <app id> --json")
	}
	release, err := buildindex.AcquireMeasure(t.Store.Dirs, buildindex.LockHolder{PID: os.Getpid(), Verb: "probe", Device: h.ID, Worktree: t.ProjectDir, Since: t.Now()})
	if err != nil {
		return Result{}, err
	}
	defer release()
	label := o.Label
	if label == "" {
		label = o.Kind
	}
	scenario := "probe-" + o.Kind
	if o.Label != "" {
		scenario += "-" + slug(o.Label)
	}
	runDir := o.Out
	if runDir == "" {
		var c Config
		if t.Config != nil {
			c = *t.Config
		}
		if runDir, err = t.newRunDir(RunOptions{Topic: "probe-" + o.Kind}, &c); err != nil {
			return Result{}, err
		}
	} else if err := os.MkdirAll(runDir, 0o755); err != nil {
		return Result{}, err
	}
	var diags []runx.Diagnostic
	diags = append(diags, t.probeFence(ctx, h, pkg)...)
	prog := t.progress("probe " + h.ID)
	rf := analysis.RunFile{Version: analysis.RunFileVersion, StartedAt: t.Now().UTC(),
		Provenance: analysis.Provenance{Device: h.ID, Platform: analysis.Platform(h.Device.Platform), Model: h.Device.Model, OS: h.Device.OS, RefreshHz: float64(h.Device.ExpectHz), InputSource: analysis.InputADB}}
	rec := analysis.RunRecord{Scenario: scenario, Attempt: 1, RecordedAt: t.Now().UTC(), Build: analysis.Build{Variant: label}}
	if h.Device.Platform == devlab.PlatformIOS {
		rf.Provenance.InputSource = analysis.InputHuman
	}
	var gestures [][]string
	switch h.Device.Platform {
	case devlab.PlatformAndroid:
		g, ev, d, err := t.probeAndroid(ctx, h, o, pkg, label, runDir, prog)
		diags = append(diags, d...)
		if err != nil {
			return t.probeFailed(runDir, diags, err)
		}
		gestures, rec.Evidence = g, ev
	case devlab.PlatformIOS:
		ev, d, err := t.probeIOS(ctx, h, o, pkg, label, runDir, prog)
		diags = append(diags, d...)
		if err != nil {
			return t.probeFailed(runDir, diags, err)
		}
		rec.Evidence = ev
	}
	if li := lastInstalled(t, h); li != nil && li.Package == pkg {
		rec.Build.Variant, rec.Build.NativeKey = li.VariantID, ""
		if rv, err := t.resolveVariant(li.VariantID); err == nil {
			rec.Build = analysis.Build{Variant: label + "@" + rv.Label, NativeKey: rv.NativeKey, PublicEnvHash: rv.PublicEnvHash, ProductionEquivalent: rv.ProductionEquivalent}
		}
	}
	rf.Runs = []analysis.RunRecord{rec}
	if err := writeRunFile(runDir, rf); err != nil {
		return Result{}, err
	}
	prog.Phase("analyze")
	opts, _ := t.analysisOptions(AnalyzeOptions{})
	rf.Dir = runDir
	ra, ad := analysis.AnalyzeRunFile(ctx, rf, opts)
	diags = append(diags, ad...)
	data := map[string]any{"runDir": runDir, "device": h.ID, "kind": o.Kind, "package": pkg, "gestures": gestures, "runs": ra.Records}
	if budget := probeBudget(o.Kind, string(h.Device.Platform)); budget != nil {
		if b, err := analysis.ParseBudget(scenario, budget); err == nil {
			if rep, rd, err := analysis.Report([]analysis.RunAnalysis{ra}, analysis.ReportOptions{Budgets: map[string]analysis.Budget{scenario: b}}); err == nil {
				data["verdict"] = rep.Rows
				data["boardRows"] = rep.BoardRows
				diags = append(diags, rd...)
			}
		}
	}
	if err := t.appendLedger(runDir, h.ID, []RunVariant{{Label: rec.Build.Variant}}, []string{scenario}); err != nil {
		diags = append(diags, warn(DiagInfraError, "runs.jsonl not appended: "+err.Error(), ""))
	}
	return Result{Data: data, Diagnostics: diags,
		Lines: []string{"probe " + o.Kind + " on " + h.ID + ": " + runDir},
		Next:  []string{"perflab report " + shellword.Quote(runDir) + " --json", "perflab analyze " + shellword.Quote(runDir) + " --json"}}, nil
}

func (t *Tool) probeFailed(runDir string, diags []runx.Diagnostic, err error) (Result, error) {
	if de, ok := err.(runx.DiagError); ok {
		diags = append(diags, de.Diag)
		next := []string{}
		if de.Diag.Fix != "" {
			next = []string{de.Diag.Fix}
		}
		return Result{Data: map[string]any{"runDir": runDir}, Diagnostics: diags, Next: next}, nil
	}
	return Result{Data: map[string]any{"runDir": runDir}, Diagnostics: diags}, err
}

func lastInstalled(t *Tool, h *devlab.Hold) *devlab.Installed {
	_, _, ls, err := t.Lab.Verify(h.ID, h.Token)
	if err != nil || ls == nil {
		return nil
	}
	return ls.LastInstalled
}

// probeFence verifies the app perflab installed is still the one on the
// device; a package perflab never installed here is measured as found,
// with a warning (a probe may read any profileable app).
func (t *Tool) probeFence(ctx context.Context, h *devlab.Hold, pkg string) []runx.Diagnostic {
	li := lastInstalled(t, h)
	if li == nil || li.Package != pkg {
		return []runx.Diagnostic{warn(devlab.DiagDeviceStateChanged, fmt.Sprintf("perflab did not install %s on %s, so the probe measures whatever build is there", pkg, h.ID),
			fmt.Sprintf("perflab install <variant> --device %s --lease %s --json", h.ID, h.Token))}
	}
	if err := buildindex.VerifyInstalled(ctx, t.Exec, bdevice(h), stampOf(string(h.Device.Platform), li), li.VariantID); err != nil {
		if de, ok := withToken(err, h.Token).(runx.DiagError); ok {
			return []runx.Diagnostic{de.Diag}
		}
		return []runx.Diagnostic{errDiag(DiagInfraError, err.Error(), "")}
	}
	return nil
}

var focusRe = regexp.MustCompile(`mCurrentFocus=Window\{[^ ]+ [^ ]+ ([^/}\s]+)`)

func (t *Tool) probeAndroid(ctx context.Context, h *devlab.Hold, o ProbeOptions, pkg, label, runDir string, prog *hostexec.Progress) ([][]string, analysis.Evidence, []runx.Diagnostic, error) {
	serial := h.Device.Serial
	ev := analysis.Evidence{Package: pkg}
	var diags []runx.Diagnostic
	fail := func(what string, res hostexec.Result, err error) error {
		if err != nil {
			return err
		}
		return diag(devlab.DiagDeviceCommandFailed, fmt.Sprintf("adb %s on %s exited %d: %s", what, h.ID, res.Exit, res.Tail()), "perflab doctor --device "+h.ID+" --lease "+h.Token+" --for probe --json")
	}
	prog.Phase("prepare")
	res, err := t.adb(ctx, serial, 20*time.Second, "shell", "dumpsys", "window")
	if err != nil || res.Exit != 0 {
		return nil, ev, diags, fail("shell dumpsys window", res, err)
	}
	if m := focusRe.FindStringSubmatch(string(res.Stdout)); m == nil || m[1] != pkg {
		got := "nothing"
		if m != nil {
			got = m[1]
		}
		return nil, ev, diags, diag(doctor.DiagInputFocusWrong, fmt.Sprintf("%s has input focus on %s, not %s: the gestures would land elsewhere", h.ID, got, pkg),
			fmt.Sprintf("perflab app launch --device %s --lease %s --json", h.ID, h.Token))
	}
	res, err = t.adb(ctx, serial, 20*time.Second, "shell", "dumpsys", "SurfaceFlinger", "--latency")
	if err != nil || res.Exit != 0 {
		return nil, ev, diags, fail("shell dumpsys SurfaceFlinger --latency", res, err)
	}
	if p, ok := framestats.ParseVsyncPeriodNs(string(res.Stdout)); ok {
		ev.VsyncPeriodNs = p
	}
	res, err = t.adb(ctx, serial, 20*time.Second, "shell", "wm", "size")
	if err != nil || res.Exit != 0 {
		return nil, ev, diags, fail("shell wm size", res, err)
	}
	w, hgt, ok := screenSize(string(res.Stdout))
	if !ok {
		w, hgt = baseW, baseH
	}
	setup, script := recipes(o.Kind)
	if o.Kind == "custom" {
		raw, err := os.ReadFile(o.GestureFile)
		if err != nil {
			return nil, ev, diags, diag(DiagUsage, "cannot read --gesture-file: "+err.Error(), "pass a JSON list of {swipe:[x1,y1,x2,y2,ms]} | {tap:[x,y]} | {sleepMs:n}")
		}
		if err := json.Unmarshal(raw, &script); err != nil {
			return nil, ev, diags, diag(DiagUsage, "--gesture-file is not a JSON list of gestures: "+err.Error(), "pass a JSON list of {swipe:[x1,y1,x2,y2,ms]} | {tap:[x,y]} | {sleepMs:n}")
		}
	}
	var gestures [][]string
	input := func(gs []Gesture) error {
		for _, g := range gs {
			if args := scaleArgs(g, w, hgt); args != nil {
				gestures = append(gestures, append([]string{"adb", "-s", serial, "shell"}, args...))
				res, err := t.adb(ctx, serial, 30*time.Second, append([]string{"shell"}, args...)...)
				if err != nil || res.Exit != 0 {
					return fail("shell "+strings.Join(args, " "), res, err)
				}
			}
			if g.SleepMs > 0 {
				t.Sleep(time.Duration(g.SleepMs) * time.Millisecond)
			}
		}
		return nil
	}
	if err := input(setup); err != nil {
		return gestures, ev, diags, err
	}
	// Flashlight disables traced; Perfetto needs it back.
	_, _ = t.adb(ctx, serial, 20*time.Second, "shell", "setprop", "persist.traced.enable", "1")
	duration := time.Duration(o.Seconds) * time.Second
	if lead := 2*time.Second + scriptDuration(script) + 2*time.Second; o.Kind != "rest" && lead > duration {
		duration = lead
	}
	name := "perflab-" + slug(label) + "-" + strconv.FormatInt(t.Now().Unix(), 10) + ".pftrace"
	remote := "/data/misc/perfetto-traces/" + name
	cfg := strings.ReplaceAll(strings.ReplaceAll(perfettoConfig, "{{package}}", pkg), "{{duration_ms}}", strconv.FormatInt(duration.Milliseconds(), 10))
	prog.Phase("trace", "seconds="+strconv.Itoa(int(duration.Seconds())))
	res, err = t.Exec.Run(ctx, hostexec.Cmd{Argv: []string{"adb", "-s", serial, "shell", "perfetto", "--txt", "-c", "-", "-o", remote, "--background"},
		Stdin: strings.NewReader(cfg), Timeout: 30 * time.Second})
	if err != nil || res.Exit != 0 {
		return gestures, ev, diags, fail("shell perfetto --background", res, err)
	}
	tracerPid := strings.TrimSpace(lastLine(string(res.Stdout)))
	t.Sleep(2 * time.Second)
	if err := input(script); err != nil {
		return gestures, ev, diags, err
	}
	deadline := t.Now().Add(duration + 30*time.Second)
	for {
		res, err := t.adb(ctx, serial, 10*time.Second, "shell", "test -d /proc/"+tracerPid+" && echo running || echo done")
		if err == nil && strings.Contains(string(res.Stdout), "done") {
			break
		}
		if t.Now().After(deadline) {
			return gestures, ev, diags, diag(devlab.DiagDeviceCommandFailed, fmt.Sprintf("perfetto (pid %s) still runs %s after its %s window", tracerPid, 30*time.Second, duration), "perflab device shell "+h.ID+" --lease "+h.Token+" -- shell ls /data/misc/perfetto-traces")
		}
		t.Sleep(time.Second)
	}
	prog.Phase("pull")
	local := filepath.Join(runDir, name)
	res, err = t.adb(ctx, serial, 5*time.Minute, "pull", remote, local)
	if err != nil || res.Exit != 0 {
		return gestures, ev, diags, fail("pull "+remote, res, err)
	}
	_, _ = t.adb(ctx, serial, 20*time.Second, "shell", "rm", "-f", remote)
	ev.Pftrace = name
	return gestures, ev, diags, nil
}

type devicectlApps struct {
	Result struct {
		Apps []struct {
			BundleIdentifier string `json:"bundleIdentifier"`
			URL              string `json:"url"`
		} `json:"apps"`
	} `json:"result"`
}

type devicectlProcesses struct {
	Result struct {
		RunningProcesses []struct {
			Executable        string `json:"executable"`
			ProcessIdentifier int    `json:"processIdentifier"`
		} `json:"runningProcesses"`
	} `json:"result"`
}

func (t *Tool) devicectlJSON(ctx context.Context, h *devlab.Hold, v any, args ...string) error {
	tmp, err := os.MkdirTemp("", "perflab-devicectl-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	out := filepath.Join(tmp, "out.json")
	res, err := t.devicectl(ctx, 60*time.Second, append(args, "--json-output", out)...)
	if err != nil {
		return err
	}
	if res.Exit != 0 {
		return diag(devlab.DiagDeviceCommandFailed, "devicectl "+strings.Join(args, " ")+": "+res.Tail(), "perflab doctor --device "+h.ID+" --lease "+h.Token+" --wake --json")
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// appPid launches the app (waking it) and finds its pid the way the
// project's perf window does: the installed bundle url prefixes the
// running executable.
func (t *Tool) appPid(ctx context.Context, h *devlab.Hold, bundleID string) (int, error) {
	core := h.Device.CoreDeviceID
	res, err := t.devicectl(ctx, 60*time.Second, "device", "process", "launch", "--device", core, "--terminate-existing", bundleID)
	if err != nil {
		return 0, err
	}
	if res.Exit != 0 {
		return 0, diag(buildindex.DiagAppNotInstalled, fmt.Sprintf("devicectl could not launch %s on %s: %s", bundleID, h.ID, res.Tail()), "perflab install <variant> --device "+h.ID+" --lease "+h.Token+" --json")
	}
	t.Sleep(3 * time.Second)
	var apps devicectlApps
	if err := t.devicectlJSON(ctx, h, &apps, "device", "info", "apps", "--device", core, "--bundle-id", bundleID); err != nil {
		return 0, err
	}
	url := ""
	for _, a := range apps.Result.Apps {
		if a.BundleIdentifier == bundleID {
			url = a.URL
		}
	}
	if url == "" {
		return 0, diag(buildindex.DiagAppNotInstalled, bundleID+" is not installed on "+h.ID, "perflab install <variant> --device "+h.ID+" --lease "+h.Token+" --json")
	}
	var procs devicectlProcesses
	if err := t.devicectlJSON(ctx, h, &procs, "device", "info", "processes", "--device", core); err != nil {
		return 0, err
	}
	for _, p := range procs.Result.RunningProcesses {
		if strings.HasPrefix(p.Executable, url) {
			return p.ProcessIdentifier, nil
		}
	}
	return 0, diag(devlab.DiagDeviceOffline, bundleID+" launched but is not in "+h.ID+"'s process list", "perflab doctor --device "+h.ID+" --lease "+h.Token+" --wake --json")
}

// xctraceReady is the line xctrace prints once it records.
const xctraceReady = "Ctrl-C to stop the recording"

func (t *Tool) probeIOS(ctx context.Context, h *devlab.Hold, o ProbeOptions, bundleID, label, runDir string, prog *hostexec.Progress) (analysis.Evidence, []runx.Diagnostic, error) {
	ev := analysis.Evidence{}
	prog.Phase("launch")
	pid, err := t.appPid(ctx, h, bundleID)
	if err != nil {
		return ev, nil, err
	}
	name := "probe-" + slug(label) + "-ios-" + strconv.FormatInt(t.Now().UnixMilli(), 10) + ".trace"
	trace := filepath.Join(runDir, name)
	seconds := o.Seconds + 5
	var res hostexec.Result
	for i, pause := range []time.Duration{0, 2 * time.Second, 5 * time.Second, 10 * time.Second} {
		t.Sleep(pause)
		prog.Phase("trace", "attempt="+strconv.Itoa(i+1), "seconds="+strconv.Itoa(seconds))
		res, err = t.Exec.Run(ctx, hostexec.Cmd{Argv: []string{"xcrun", "xctrace", "record", "--instrument", "Hitches", "--instrument", "Time Profiler",
			"--device", h.Device.HardwareUDID, "--attach", strconv.Itoa(pid), "--time-limit", strconv.Itoa(seconds) + "s", "--output", trace},
			Timeout: time.Duration(seconds)*time.Second + 90*time.Second})
		if err != nil {
			return ev, nil, err
		}
		if res.Exit != 21 && !attachFailedRe.Match(res.Stdout) {
			break
		}
		_ = os.RemoveAll(trace)
	}
	_ = os.WriteFile(trace+".record.log", append(res.Stdout, res.Stderr...), 0o644)
	switch {
	case res.Exit == 21 || attachFailedRe.Match(res.Stdout):
		return ev, nil, diag(DiagXctraceAttachFailed, fmt.Sprintf("xctrace could not attach to pid %d after 4 tries", pid), "perflab doctor --device "+h.ID+" --lease "+h.Token+" --wake --json")
	case !strings.Contains(string(res.Stdout), xctraceReady):
		return ev, nil, diag(DiagXctraceNotReady, "xctrace never printed its ready line: "+res.Tail(), "perflab doctor --device "+h.ID+" --lease "+h.Token+" --wake --json")
	case !fileExists(trace):
		return ev, nil, diag(devlab.DiagDeviceCommandFailed, fmt.Sprintf("xctrace exited %d without saving %s: %s", res.Exit, name, res.Tail()), "perflab doctor --device "+h.ID+" --lease "+h.Token+" --json")
	}
	ev.Trace, ev.RecordLog = name, name+".record.log"
	return ev, nil, nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
