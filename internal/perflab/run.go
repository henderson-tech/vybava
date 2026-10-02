package perflab

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/henderson-tech/vybava/internal/devlab"
	"github.com/henderson-tech/vybava/internal/framestats"
	"github.com/henderson-tech/vybava/internal/perflab/analysis"
	"github.com/henderson-tech/vybava/internal/perflab/buildindex"
	"github.com/henderson-tech/vybava/internal/perflab/doctor"
	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/perflab/netfwd"
	"github.com/henderson-tech/vybava/internal/perflab/wda"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/shellword"
)

// RunOptions are the run flags.
type RunOptions struct {
	Scenarios []string
	Device    string
	Lease     string
	// Variants are `<id>` or `<label>=<id>` (a variant id or a native key).
	Variants  []string
	Alternate bool
	Repeat    int
	Out       string
	Topic     string
	Resume    string
	NoAnalyze bool
	Max       time.Duration
	Profile   string
}

// RunVariant is one --variant resolved against the index.
type RunVariant struct {
	Label                string `json:"label"`
	ID                   string `json:"id"`
	NativeKey            string `json:"nativeKey"`
	BundleSHA            string `json:"bundleSha,omitempty"`
	PublicEnvHash        string `json:"publicEnvHash,omitempty"`
	ProductionEquivalent bool   `json:"productionEquivalent"`
	Path                 string `json:"path"`
	Platform             string `json:"platform"`
}

// RunBlock is one runner invocation: every scenario on one variant.
type RunBlock struct {
	Seq      int      `json:"seq"`
	Variant  string   `json:"variant"`
	Attempt  int      `json:"attempt"`
	CaseDir  string   `json:"caseDir"`
	Exit     int      `json:"exit"`
	Attempts []string `json:"attempts,omitempty"`
	Skipped  bool     `json:"skipped,omitempty"`
}

// RunData is run's payload.
type RunData struct {
	RunDir   string                  `json:"runDir"`
	Device   string                  `json:"device"`
	Variants []RunVariant            `json:"variants"`
	Blocks   []RunBlock              `json:"blocks"`
	Runs     []analysis.RecordResult `json:"runs,omitempty"`
	Crashes  []Crash                 `json:"crashes,omitempty"`
	Runner   struct {
		Exit int `json:"exit"`
	} `json:"runner"`
}

// perCaseSlack is added to each scenario's window for the runner's own
// timeout (the 420 s default cut a 5 min trace pull).
const perCaseSlack = 20 * time.Minute

func (t *Tool) resolveVariant(arg string) (RunVariant, error) {
	label, id, ok := strings.Cut(arg, "=")
	if !ok {
		id, label = arg, ""
	}
	it, err := t.Store.Resolve(id)
	if err != nil {
		return RunVariant{}, err
	}
	rv := RunVariant{Label: label, ID: it.ID, Path: it.Path, Platform: it.Platform, ProductionEquivalent: it.ProductionEquivalent}
	if rv.Label == "" {
		rv.Label = it.ID
	}
	if v, err := t.Store.FindVariant(it.ID); err == nil {
		rv.NativeKey, rv.BundleSHA = v.Manifest.NativeKey, v.Manifest.BundleSHA
		if b, err := t.Store.FindBundle(v.Manifest.BundleSHA); err == nil {
			rv.PublicEnvHash = b.Manifest.PublicEnvHash
		}
	} else if b, err := t.Store.BuildByKey(it.ID); err == nil {
		rv.NativeKey, rv.PublicEnvHash = b.Manifest.Key, b.Manifest.PublicEnvHash
	}
	return rv, nil
}

// planBlocks orders the runner invocations: a,b,a,b with --alternate
// (the noise rule), else every repeat of a, then of b.
func planBlocks(variants []RunVariant, repeat int, alternate bool) []RunBlock {
	var out []RunBlock
	add := func(v RunVariant, attempt int) {
		out = append(out, RunBlock{Seq: len(out) + 1, Variant: v.Label, Attempt: attempt})
	}
	if alternate {
		for r := 1; r <= repeat; r++ {
			for _, v := range variants {
				add(v, r)
			}
		}
		return out
	}
	for _, v := range variants {
		for r := 1; r <= repeat; r++ {
			add(v, r)
		}
	}
	return out
}

func caseDirName(b RunBlock) string {
	return fmt.Sprintf("%02d-%s-%d", b.Seq, slug(b.Variant), b.Attempt)
}

// Run measures scenarios on a leased device: preflight, hold, install each
// variant inside the hold, run the project's runner per block, collect
// crashes and evidence, write perflab.run.json and analyze it.
func (t *Tool) Run(ctx context.Context, o RunOptions) (Result, error) {
	c, err := t.Cfg()
	if err != nil {
		return Result{}, err
	}
	if len(o.Scenarios) == 0 || o.Device == "" || len(o.Variants) == 0 {
		return Result{}, diag(DiagUsage, "run needs scenarios, --device, --lease and at least one --variant",
			"perflab run <scenario>... --device <id> --lease <token> --variant <id> --json")
	}
	if o.Repeat <= 0 {
		o.Repeat = 1
	}
	if o.Max <= 0 {
		o.Max = 90 * time.Minute
	}
	if o.Profile == "" {
		o.Profile = "perf"
	}
	ctx, cancel := context.WithTimeout(ctx, o.Max)
	defer cancel()

	id, dev, _, err := t.Lab.Verify(o.Device, o.Lease)
	if err != nil {
		return Result{}, err
	}
	platform := string(dev.Platform)
	var variants []RunVariant
	for _, arg := range o.Variants {
		rv, err := t.resolveVariant(arg)
		if err != nil {
			return Result{}, err
		}
		if rv.Platform != platform {
			return Result{}, diag(DiagUsage, fmt.Sprintf("variant %s is a %s artifact and %s a %s device", rv.ID, rv.Platform, id, platform), "perflab build list --json")
		}
		variants = append(variants, rv)
	}
	rows, err := t.Scenarios(ctx, platform)
	if err != nil {
		return Result{}, err
	}
	var picked []ScenarioRow
	var names []string
	for _, r := range rows {
		names = append(names, r.Name)
	}
	for _, s := range o.Scenarios {
		i := slices.IndexFunc(rows, func(r ScenarioRow) bool { return r.Name == s })
		if i < 0 {
			return Result{}, diag(DiagScenarioUnknown, fmt.Sprintf("scenario %q is not in the adapter's rows (have: %s)", s, strings.Join(names, ", ")), "perflab adapter check --json")
		}
		if len(rows[i].Platforms) > 0 && !slices.Contains(rows[i].Platforms, platform) {
			return Result{}, diag(DiagScenarioUnknown, fmt.Sprintf("scenario %q runs on %s, not %s", s, strings.Join(rows[i].Platforms, "/"), platform), "pick a scenario for "+platform)
		}
		picked = append(picked, rows[i])
	}

	// The run dir: --resume, --out, else the adapter's out template.
	runDir := o.Resume
	rf := analysis.RunFile{Version: analysis.RunFileVersion, StartedAt: t.Now().UTC(),
		Provenance: analysis.Provenance{Device: id, Platform: analysis.Platform(platform), Model: dev.Model, OS: dev.OS, RefreshHz: float64(dev.ExpectHz), InputSource: analysis.InputW3C}}
	if runDir != "" {
		prev, err := analysis.LoadRunDir(runDir)
		if err != nil {
			return Result{}, err
		}
		rf = prev
	} else {
		if runDir, err = t.newRunDir(o, c); err != nil {
			return Result{}, err
		}
	}
	data := RunData{RunDir: runDir, Device: id, Variants: variants}

	// Preflight before the hold: doctor's device probe takes the device
	// lock itself.
	pre, err := t.Doctor(ctx, DoctorOptions{Device: id, Lease: o.Lease, For: string(doctor.ForRun), Wake: true, StartsForward: true})
	if err != nil {
		return Result{}, err
	}
	if HasErrors(pre.Diagnostics) {
		return Result{Data: data, Diagnostics: pre.Diagnostics, Next: pre.Next}, nil
	}
	apiOrigin := ""
	if dd, ok := pre.Data.(doctor.Data); ok {
		apiOrigin = dd.Host.APIOrigin
	}

	h, err := t.Lab.Hold(id, o.Lease, "run")
	if err != nil {
		return Result{}, err
	}
	defer h.Done()
	stopHB, _ := h.StartHeartbeat(ctx, devlab.HeartbeatEvery)
	defer stopHB()
	releaseMeasure, err := buildindex.AcquireMeasure(t.Store.Dirs, buildindex.LockHolder{PID: os.Getpid(), Verb: "run", Device: id, Worktree: t.ProjectDir, Since: t.Now()})
	if err != nil {
		return Result{}, err
	}
	defer releaseMeasure()

	prog := t.progress("run " + id)
	v := t.baseVars(ctx, platform)
	v.Set("profile", o.Profile)
	v.Set("topic", o.topicOr(picked))
	if apiOrigin != "" {
		v.Set("apiOrigin", apiOrigin)
	}
	v.Set("udid", dev.HardwareUDID)
	v.Set("coreDeviceId", dev.CoreDeviceID)
	v.Set("serial", dev.Serial)
	var diags []runx.Diagnostic

	switch dev.Platform {
	case devlab.PlatformIOS:
		prog.Phase("wda")
		spec, err := t.wdaSpec("", "")
		if err != nil {
			return Result{}, err
		}
		wres, err := wda.Find(ctx, t.wdaEnv(0), spec)
		if err != nil {
			return Result{}, err
		}
		if HasErrors(wres.Diagnostics) {
			return Result{Data: data, Diagnostics: wres.Diagnostics, Next: wres.Next}, nil
		}
		if wd, ok := wres.Data.(wda.Data); ok {
			for k, val := range wd.Tokens {
				v.Set(k, val)
			}
		}
	case devlab.PlatformAndroid:
		// The prebuilt WDA is iOS-only: its tokens resolve to nothing here, so
		// a runner.env shared by both platforms leaves them unset.
		v.Set("wdaBundleId", "")
		v.Set("wdaDerivedData", "")
		// The screen must not sleep mid-window; background apps must not
		// compete for the big cores. The crash buffer is read by time, so it
		// is never cleared.
		prog.Phase("prepare")
		for _, args := range [][]string{{"shell", "svc", "power", "stayon", "usb"}, {"shell", "am", "kill-all"}} {
			if res, err := t.adb(ctx, dev.Serial, 20*time.Second, args...); err != nil || res.Exit != 0 {
				diags = append(diags, warn(devlab.DiagDeviceCommandFailed, "adb "+strings.Join(args, " ")+" failed: "+res.Tail(), ""))
			}
		}
		if a := c.API; a != nil && a.Device != nil && a.Device.Android != nil {
			prog.Phase("net-forward")
			origin, err := v.Expand("api.origin", "{apiOrigin}", false)
			if err != nil {
				return Result{}, err
			}
			env, err := netfwd.DefaultEnv(t.Log)
			if err != nil {
				return Result{}, err
			}
			spec := netfwd.Spec{DeviceID: id, Serial: dev.Serial, Lease: o.Lease, DevicePort: a.Device.Android.DevicePort, Origin: origin, Health: a.Health}
			// A standalone `net forward` (doctor's FORWARD_DOWN fix) already
			// serving this phone is reused; a second listener would fail.
			if _, serving := netfwd.Serving(ctx, env, spec); !serving {
				fwd, fd, err := netfwd.Start(ctx, env, spec)
				diags = append(diags, fd...)
				if err != nil {
					return Result{Data: data, Diagnostics: diags}, err
				}
				defer func() { _ = fwd.Close(context.Background()) }()
			}
		}
	}

	// One runner per project checkout at a time: the adapter's runner owns
	// per-checkout resources (FixIt's Appium port), so a second phone's run
	// from this checkout waits here, within its --max, for the first.
	runner := buildindex.LockHolder{PID: os.Getpid(), Verb: "run", Device: id, Worktree: t.ProjectDir, Since: t.Now()}
	releaseRunner, busy, err := buildindex.AcquireProjectRunner(t.Store.Dirs, t.ProjectDir, 0, runner)
	if errors.Is(err, buildindex.ErrRunnerBusy) {
		prog.Phase("runner-wait", "held="+strings.ReplaceAll(busy.String(), " ", "_"))
		wait := o.Max
		if dl, ok := ctx.Deadline(); ok {
			wait = time.Until(dl)
		}
		releaseRunner, busy, err = buildindex.AcquireProjectRunner(t.Store.Dirs, t.ProjectDir, wait, runner)
	}
	if errors.Is(err, buildindex.ErrRunnerBusy) {
		holder := "another run"
		if busy != nil {
			holder = busy.String()
		}
		return Result{Data: data, Diagnostics: append(diags, errDiag(DiagRunTimeout,
			fmt.Sprintf("this checkout's runner stayed busy (%s) for the whole --max %s", holder, o.Max),
			"perflab lease status --json, then re-run once that run ends"))}, nil
	}
	if err != nil {
		return Result{}, err
	}
	defer releaseRunner()

	blocks := planBlocks(variants, o.Repeat, o.Alternate)
	window := time.Duration(0)
	for _, r := range picked {
		window += time.Duration(r.WindowMs)*time.Millisecond + perCaseSlack
	}
	for bi := range blocks {
		b := &blocks[bi]
		b.CaseDir = caseDirName(*b)
		rv := variants[slices.IndexFunc(variants, func(x RunVariant) bool { return x.Label == b.Variant })]
		if blockDone(rf, b, picked) {
			b.Skipped = true
			continue
		}
		if ctx.Err() != nil {
			diags = append(diags, errDiag(DiagRunTimeout, fmt.Sprintf("the run passed --max %s before block %d", o.Max, b.Seq), "perflab run "+strings.Join(o.Scenarios, " ")+" … --resume "+shellword.Quote(runDir)))
			break
		}
		blockStart := t.Now().UTC()
		prog.Phase("install", "variant="+rv.Label, "block="+strconv.Itoa(b.Seq))
		if err := t.ensureInstalled(ctx, h, rv); err != nil {
			return t.runFailed(data, rf, runDir, diags, err)
		}
		before := t.deviceState(ctx, h)
		caseDir := filepath.Join(runDir, b.CaseDir)
		if err := os.MkdirAll(caseDir, 0o755); err != nil {
			return Result{}, err
		}
		v.Set("runDir", caseDir)
		v.Set("variant", rv.Label)
		v.Set("nativeKey", rv.NativeKey)
		v.Set("bundleSha", rv.BundleSHA)
		v.Set("appPath", rv.Path)
		v.Set("timeoutMs", strconv.FormatInt(window.Milliseconds(), 10))
		v.SetList("scenarios", o.Scenarios)
		v.Set("scenario", o.Scenarios[0])
		cmd, env, unset, err := t.runnerCommand(v)
		if err != nil {
			return Result{}, err
		}
		var res caseResult
		for attempt := 1; ; attempt++ {
			prog.Phase("runner", "variant="+rv.Label, "block="+strconv.Itoa(b.Seq), "attempt="+strconv.Itoa(attempt))
			res = t.runCase(ctx, h, caseDir, cmd, env, unset, window, prog)
			b.Exit = res.exit
			if res.code == "" {
				break
			}
			b.Attempts = append(b.Attempts, res.code)
			if attempt > 1 || !retryable[res.code] {
				break
			}
		}
		after := t.deviceState(ctx, h)
		crashes, cerr := t.collectCrashes(ctx, h, blockStart, filepath.Join(caseDir, "crashes"))
		if cerr != nil {
			diags = append(diags, warn(devlab.DiagDeviceCommandFailed, "crash collection failed: "+cerr.Error(), ""))
		}
		data.Crashes = append(data.Crashes, crashes...)
		if len(crashes) > 0 {
			diags = append(diags, errDiag(DiagAppCrashed, fmt.Sprintf("block %d (%s): %d crash record(s), first %s", b.Seq, rv.Label, len(crashes), crashes[0].Summary),
				fmt.Sprintf("perflab crashes --device %s --lease %s --since %s --json", id, o.Lease, blockStart.Format(time.RFC3339))))
		}
		if res.code != "" {
			diags = append(diags, errDiag(res.code, res.detail, res.fix(t, o, runDir)))
		}
		for _, row := range picked {
			rec, missing := t.discoverRecord(runDir, caseDir, platform, row, rv, b.Attempt, blockStart, before, after)
			if res.code != "" {
				rec.Failed = true
			} else if missing {
				rec.Failed = true
				diags = append(diags, errDiag(DiagEvidenceMissing, fmt.Sprintf("block %d left no trace, frames sidecar or pftrace named %s-%s-* under %s", b.Seq, row.Name, platform, caseDir),
					"read "+filepath.Join(caseDir, "run.log")+", then perflab run … --resume "+shellword.Quote(runDir)))
			}
			rf.Runs = append(rf.Runs, rec)
		}
		rf.Runner.Exit = b.Exit
		if err := writeRunFile(runDir, rf); err != nil {
			return Result{}, err
		}
	}
	data.Blocks = blocks
	data.Runner.Exit = rf.Runner.Exit
	if err := t.appendLedger(runDir, id, variants, o.Scenarios); err != nil {
		diags = append(diags, warn(DiagInfraError, "runs.jsonl not appended: "+err.Error(), ""))
	}
	if !o.NoAnalyze {
		prog.Phase("analyze")
		opts, _ := t.analysisOptions(AnalyzeOptions{})
		rf.Dir = runDir
		ra, ad := analysis.AnalyzeRunFile(ctx, rf, opts)
		data.Runs = ra.Records
		diags = append(diags, ad...)
	}
	next := []string{"perflab compare " + shellword.Quote(runDir) + " --json"}
	if len(variants) == 1 {
		next = []string{"perflab report " + shellword.Quote(runDir) + " --gate --json"}
	}
	if HasErrors(diags) {
		next = append([]string{o.resumeCommand(id, runDir)}, next...)
	}
	return Result{Data: data, Diagnostics: diags, Next: next,
		Lines: []string{fmt.Sprintf("run dir %s: %d blocks, runner exit %d", runDir, len(blocks), data.Runner.Exit)}}, nil
}

// resumeCommand re-runs the same plan into runDir: the plan (--alternate,
// --repeat) is not in perflab.run.json, so a resume without it re-plans
// one block per variant and silently skips every later failed block (the
// 2026-10-02 iPhone 11 resume never re-ran block 4).
func (o RunOptions) resumeCommand(device, runDir string) string {
	cmd := "perflab run " + strings.Join(o.Scenarios, " ") + " --device " + device + " --lease " + o.Lease + " " + variantFlags(o.Variants)
	if o.Alternate {
		cmd += " --alternate"
	}
	if o.Repeat > 1 {
		cmd += " --repeat " + strconv.Itoa(o.Repeat)
	}
	return cmd + " --resume " + shellword.Quote(runDir) + " --json"
}

func variantFlags(vs []string) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = "--variant " + shellword.Quote(v)
	}
	return strings.Join(out, " ")
}

func (o RunOptions) topicOr(rows []ScenarioRow) string {
	if o.Topic != "" {
		return slug(o.Topic)
	}
	if len(rows) > 0 {
		return slug(rows[0].Name)
	}
	return "perf"
}

// runFailed ends a run whose block could not start, keeping what ran.
func (t *Tool) runFailed(data RunData, rf analysis.RunFile, runDir string, diags []runx.Diagnostic, err error) (Result, error) {
	if werr := writeRunFile(runDir, rf); werr != nil {
		return Result{}, werr
	}
	var de runx.DiagError
	if errors.As(err, &de) {
		diags = append(diags, de.Diag)
		next := []string{}
		if de.Diag.Fix != "" {
			next = append(next, de.Diag.Fix)
		}
		return Result{Data: data, Diagnostics: diags, Next: next}, nil
	}
	return Result{Data: data, Diagnostics: diags}, err
}

// newRunDir creates <out>/run-<stamp>/ (out: --out, else the adapter's
// out template, else ~/Exports/perflab/<date>-<topic>) with its .gitignore.
func (t *Tool) newRunDir(o RunOptions, c *Config) (string, error) {
	root := o.Out
	if root == "" {
		tmpl := c.Out
		if tmpl == "" {
			tmpl = "~/Exports/perflab/{date}-{topic}"
		}
		v := NewVars(map[string]string{"date": t.Now().Format("2006-01-02"), "topic": o.topicOr(nil)})
		if len(o.Scenarios) > 0 && o.Topic == "" {
			v.Set("topic", slug(o.Scenarios[0]))
		}
		out, err := v.Expand("out", tmpl, false)
		if err != nil {
			return "", err
		}
		root = filepath.Join(expandHome(out), "run-"+t.Now().UTC().Format("20060102T150405Z"))
	}
	root = expandHome(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", err
	}
	ignore := "# perflab: evidence stays out of git; the numbers beside it are committable.\n*.trace/\n*.pftrace\n*.xml\nframes/*.txt\ncrashes/\n"
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(ignore), 0o644); err != nil {
		return "", err
	}
	return root, nil
}

func writeRunFile(dir string, rf analysis.RunFile) error {
	raw, err := json.MarshalIndent(rf, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".perflab.run.*.json")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, analysis.RunFileName))
}

func (t *Tool) appendLedger(runDir, device string, variants []RunVariant, scenarios []string) error {
	labels := make([]string, len(variants))
	for i, v := range variants {
		labels[i] = v.Label
	}
	raw, err := json.Marshal(analysis.LedgerEntry{RunDir: runDir, Device: device, Variants: labels, Scenarios: scenarios, At: t.Now().UTC()})
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.ledgerPath()), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(t.ledgerPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(raw, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// blockDone reports whether a resumed run already holds this block's
// records with evidence (none failed).
func blockDone(rf analysis.RunFile, b *RunBlock, rows []ScenarioRow) bool {
	for _, r := range rows {
		found := false
		for _, rec := range rf.Runs {
			if rec.Scenario == r.Name && rec.Build.Variant == b.Variant && rec.Attempt == b.Attempt && !rec.Failed {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return len(rows) > 0
}

// ensureInstalled installs the variant inside the hold when the lease's
// last install differs, then verifies the device still carries it.
func (t *Tool) ensureInstalled(ctx context.Context, h *devlab.Hold, rv RunVariant) error {
	_, _, ls, err := t.Lab.Verify(h.ID, h.Token)
	if err != nil {
		return err
	}
	last := ls.LastInstalled
	if last == nil || last.VariantID != rv.ID {
		res, _, err := t.installHeld(ctx, h, rv.ID, 0)
		if err != nil {
			return err
		}
		in := installedOf(res)
		last = &in
	}
	return buildindex.VerifyInstalled(ctx, t.Exec, bdevice(h), stampOf(string(h.Device.Platform), last), rv.ID)
}

// runnerCommand resolves runner.cmd and runner.env, and the names the child
// must not inherit: runner.unset plus every runner.env entry whose tokens
// resolve to nothing on this platform (one adapter env map serves both: on
// Android {wdaBundleId} is empty, on iOS {serial}), so the runner's own
// default applies instead of an empty value or a stale inherited one.
func (t *Tool) runnerCommand(v *Vars) (string, []string, []string, error) {
	c := t.Config
	cmd, err := v.Expand("runner.cmd", c.Runner.Cmd, true)
	if err != nil {
		return "", nil, nil, err
	}
	var env []string
	unset := append([]string(nil), c.Runner.Unset...)
	for _, name := range sortedKeys(c.Runner.Env) {
		tmpl := c.Runner.Env[name]
		val, err := v.Expand("runner.env."+name, tmpl, false)
		if err != nil {
			return "", nil, nil, err
		}
		if val == "" && tokenRe.MatchString(tmpl) {
			unset = append(unset, name)
			continue
		}
		env = append(env, name+"="+val)
	}
	// Bare `adb` in the runner's tools (Flashlight's `adb shell getprop`)
	// fails "more than one device/emulator" once another session attaches an
	// emulator: the leased phone is the default device of the whole tree.
	if p, _, _ := v.Get("platform"); p == "android" {
		if serial, ok, _ := v.Get("serial"); ok && serial != "" && c.Runner.Env["ANDROID_SERIAL"] == "" {
			env = append(env, "ANDROID_SERIAL="+serial)
		}
	}
	return cmd, env, unset, nil
}

// deviceState snapshots thermal status and swap (Android); iOS exposes
// neither, so it is nil there.
func (t *Tool) deviceState(ctx context.Context, h *devlab.Hold) *analysis.DeviceState {
	if h.Device.Platform != devlab.PlatformAndroid {
		return nil
	}
	st := &analysis.DeviceState{}
	if res, err := t.adb(ctx, h.Device.Serial, 20*time.Second, "shell", "dumpsys", "thermalservice"); err == nil && res.Exit == 0 {
		if m := thermalRe.FindStringSubmatch(string(res.Stdout)); m != nil {
			n, _ := strconv.Atoi(m[1])
			st.ThermalStatus = &n
		}
	}
	if res, err := t.adb(ctx, h.Device.Serial, 20*time.Second, "shell", "cat", "/proc/meminfo"); err == nil && res.Exit == 0 {
		if mb, ok := swapUsedMb(string(res.Stdout)); ok {
			st.SwapUsedMb = &mb
		}
	}
	return st
}

var thermalRe = regexp.MustCompile(`Current thermal status:\s*(\d+)`)

func swapUsedMb(meminfo string) (float64, bool) {
	var total, free float64
	var haveT, haveF bool
	sc := bufio.NewScanner(strings.NewReader(meminfo))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 2 {
			continue
		}
		n, err := strconv.ParseFloat(f[1], 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "SwapTotal:":
			total, haveT = n, true
		case "SwapFree:":
			free, haveF = n, true
		}
	}
	if !haveT || !haveF {
		return 0, false
	}
	return (total - free) / 1024, true
}

// caseResult is one runner invocation's outcome; code is "" on success.
type caseResult struct {
	exit   int
	code   string
	detail string
	log    string
}

func (r caseResult) fix(t *Tool, o RunOptions, runDir string) string {
	switch r.code {
	case DiagWDARebuilding:
		return "check runner.unset and the WDA env (" + adapterFix + "), then perflab run … --resume " + shellword.Quote(runDir)
	case DiagWDAStalled:
		return "perflab doctor --device " + o.Device + " --lease " + o.Lease + " --for run --json"
	case DiagADBDisconnected:
		return "reconnect the USB cable, then perflab run … --resume " + shellword.Quote(runDir)
	case DiagXctraceAttachFailed, DiagXctraceNotReady:
		return "perflab doctor --device " + o.Device + " --lease " + o.Lease + " --wake --json"
	}
	return "read " + r.log + ", then " + o.resumeCommand(o.Device, runDir)
}

// traceStampRe is the epoch-ms stamp a perf window puts in a trace name.
var traceStampRe = regexp.MustCompile(`-(\d{13})\.trace$`)

var (
	wdaStartedRe   = regexp.MustCompile(`ServerURLHere|WebDriverAgent.*started`)
	attachFailedRe = regexp.MustCompile(`Cannot find process`)
	notReadyRe     = regexp.MustCompile(`(?i)xctrace[^\n]*(not ready|ready line|never printed)`)
	adbGoneRe      = regexp.MustCompile(`device '[^']*' not found|device offline|no devices/emulators found`)
)

// runCase spawns the runner once, tees its output to <caseDir>/run.log
// (ending with EXIT=<code>), records its process group on the lease and
// watches it: WDA_REBUILDING, WDA_STALLED and the hard timeout stop it.
func (t *Tool) runCase(ctx context.Context, h *devlab.Hold, caseDir, cmd string, env, unset []string, timeout time.Duration, prog *hostexec.Progress) caseResult {
	logPath := filepath.Join(caseDir, "run.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return caseResult{exit: -1, code: DiagInfraError, detail: err.Error(), log: logPath}
	}
	defer lf.Close()
	fmt.Fprintf(lf, "# perflab run: %s\n", cmd)
	caseCtx, stop := context.WithCancel(ctx)
	defer stop()
	var mu sync.Mutex
	why := ""
	runnerPID := 0
	// sweep terminates the runner's whole tree BEFORE its group is killed:
	// Appium starts WDA's xcodebuild in a process group of its own, so a
	// group kill orphaned it, still holding the phone's XCTest session for
	// the next case (2026-10-02, iPhone 11).
	sweep := func() {
		mu.Lock()
		pid := runnerPID
		mu.Unlock()
		if pid > 0 {
			t.terminateDescendants(context.WithoutCancel(ctx), pid)
		}
	}
	kill := func(code string) {
		mu.Lock()
		if why == "" {
			why = code
		}
		mu.Unlock()
		sweep()
		stop()
	}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done(): // --max or SIGINT: best effort, the group kill races it
			sweep()
		case <-finished:
		}
	}()
	pidCh := make(chan int, 1)
	hbCtx, hbStop := context.WithCancel(ctx)
	defer hbStop()
	go prog.Heartbeat(hbCtx, 30*time.Second, nil)
	go t.watchRunner(caseCtx, pidCh, kill)
	res, err := t.Exec.Run(caseCtx, hostexec.Cmd{
		Argv: []string{"/bin/sh", "-c", cmd}, Dir: t.ProjectDir, Env: env, Unset: unset, Log: lf, Timeout: timeout,
		Started: func(pid int) {
			mu.Lock()
			runnerPID = pid
			mu.Unlock()
			_ = h.RecordChild(devlab.Child{PID: pid, PGID: pid, What: "perflab run: " + cmd})
			pidCh <- pid
		},
	})
	fmt.Fprintf(lf, "EXIT=%d\n", res.Exit)
	out := caseResult{exit: res.Exit, log: logPath}
	if err != nil {
		out.code, out.detail = DiagRunnerFailed, "the runner could not start: "+err.Error()
		return out
	}
	mu.Lock()
	killed := why
	mu.Unlock()
	switch {
	case killed == DiagWDARebuilding:
		out.code, out.detail = killed, "an xcodebuild build-for-testing appeared under the runner: the prebuilt WDA was ignored"
	case killed == DiagWDAStalled:
		out.code, out.detail = killed, "WebDriverAgent did not report started in the Appium server log within 180 s of its xcodebuild"
	case res.TimedOut || errors.Is(ctx.Err(), context.DeadlineExceeded):
		out.code, out.detail = DiagRunTimeout, fmt.Sprintf("the runner passed its %s limit (windows + %s per scenario); its process group was stopped", timeout, perCaseSlack)
	case res.Exit != 0:
		out.code, out.detail = t.classifyFailure(ctx, h, logPath, res.Exit)
	}
	return out
}

// classifyFailure maps a non-zero runner exit to its closed code by the
// log and the device.
func (t *Tool) classifyFailure(ctx context.Context, h *devlab.Hold, logPath string, exit int) (string, string) {
	tail := readTail(logPath, 64<<10)
	if h.Device.Platform == devlab.PlatformAndroid {
		gone := adbGoneRe.MatchString(tail)
		if !gone {
			if res, err := t.adb(ctx, h.Device.Serial, 10*time.Second, "get-state"); err == nil && strings.TrimSpace(string(res.Stdout)) != "device" {
				gone = true
			}
		}
		if gone {
			return DiagADBDisconnected, fmt.Sprintf("%s left adb during the run (runner exit %d)", h.ID, exit)
		}
	}
	if attachFailedRe.MatchString(tail) {
		return DiagXctraceAttachFailed, fmt.Sprintf("xctrace could not attach to the app (\"Cannot find process\"; runner exit %d)", exit)
	}
	if notReadyRe.MatchString(tail) {
		return DiagXctraceNotReady, fmt.Sprintf("xctrace never printed its ready line (runner exit %d)", exit)
	}
	return DiagRunnerFailed, fmt.Sprintf("the scenario runner exited %d: %s", exit, lastLine(tail))
}

func readTail(path string, n int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > n {
		_, _ = f.Seek(st.Size()-n, io.SeekStart)
	}
	raw, _ := io.ReadAll(f)
	return string(raw)
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !strings.HasPrefix(l, "EXIT=") {
			return l
		}
	}
	return "(no output)"
}

// watchRunner polls the runner's process tree every 10 s: a
// build-for-testing means the prebuilt WDA was ignored; a test-without-
// building with no "started" line in the Appium server log for 180 s is a
// stalled WDA.
func (t *Tool) watchRunner(ctx context.Context, pidCh <-chan int, kill func(string)) {
	var pid int
	select {
	case pid = <-pidCh:
	case <-ctx.Done():
		return
	}
	serverLog := ""
	var logStart int64
	if t.Config != nil && t.Config.Runner.AppiumServerLog != "" {
		serverLog = t.abs(t.Config.Runner.AppiumServerLog)
		if st, err := os.Stat(serverLog); err == nil {
			logStart = st.Size()
		}
	}
	var wdaSince time.Time
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		res, err := t.Exec.Run(ctx, hostexec.Cmd{Argv: []string{"ps", "-axo", "pid=,ppid=,command="}, Timeout: 10 * time.Second})
		if err != nil || res.Exit != 0 {
			continue
		}
		for _, cmd := range descendants(string(res.Stdout), pid) {
			if !strings.Contains(cmd, "xcodebuild") {
				continue
			}
			if strings.Contains(cmd, "build-for-testing") {
				kill(DiagWDARebuilding)
				return
			}
			if strings.Contains(cmd, "test-without-building") && wdaSince.IsZero() {
				wdaSince = t.Now()
			}
		}
		if !wdaSince.IsZero() && serverLog != "" {
			if wdaStartedRe.MatchString(readFrom(serverLog, logStart)) {
				wdaSince = time.Time{}
				serverLog = ""
			} else if t.Now().Sub(wdaSince) > 180*time.Second {
				kill(DiagWDAStalled)
				return
			}
		}
	}
}

// readFrom reads path from off, the size it had when the case began. Each
// Appium server truncates its --log file on start, so a file now shorter
// than off was rewritten: read it whole. Seeking past its end read nothing,
// the "started" line was never seen and WDA_STALLED killed a healthy
// measurement 180 s in (the 2026-10-02 iPhone 11 acceptance, twice).
func readFrom(path string, off int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() < off {
		off = 0
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return ""
	}
	raw, _ := io.ReadAll(io.LimitReader(f, 8<<20))
	return string(raw)
}

// terminateDescendants SIGTERMs every process under root (literal pids from
// one ps snapshot, never a pattern), whatever process group it moved to.
func (t *Tool) terminateDescendants(ctx context.Context, root int) {
	res, err := t.Exec.Run(ctx, hostexec.Cmd{Argv: []string{"ps", "-axo", "pid=,ppid=,command="}, Timeout: 10 * time.Second})
	if err != nil || res.Exit != 0 {
		return
	}
	for _, pid := range descendantPIDs(string(res.Stdout), root) {
		_ = hostexec.Terminate(pid)
	}
}

// descendantPIDs lists the pids under root in a `ps -axo pid=,ppid=,...`
// listing, parents before children.
func descendantPIDs(ps string, root int) []int {
	children := map[int][]int{}
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 == nil && err2 == nil {
			children[ppid] = append(children[ppid], pid)
		}
	}
	var out []int
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
				queue = append(queue, c)
			}
		}
	}
	return out
}

// descendants lists the commands of every process under root in a
// `ps -axo pid=,ppid=,command=` listing.
func descendants(ps string, root int) []string {
	children := map[int][]int{}
	cmds := map[int]string{}
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		children[ppid] = append(children[ppid], pid)
		cmds[pid] = strings.Join(f[2:], " ")
	}
	var out []string
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, c := range children[p] {
			if !seen[c] {
				seen[c] = true
				out = append(out, cmds[c])
				queue = append(queue, c)
			}
		}
	}
	return out
}

// resultFile is the subset of a runner's per-scenario result JSON perflab
// reads: where the evidence is. Its numbers are never read.
type resultFile struct {
	Scenario    string `json:"scenario"`
	Platform    string `json:"platform"`
	TracePath   string `json:"tracePath"`
	FramesPath  string `json:"framesPath"`
	PftracePath string `json:"pftracePath"`
	RecordedAt  string `json:"recordedAt"`
}

// discoverRecord finds one scenario's evidence in a case dir: a result
// JSON naming it, else files named <scenario>-<platform>-* (a .trace
// bundle, frames/<…>.json[.gz], a .pftrace). missing reports none found.
func (t *Tool) discoverRecord(runDir, caseDir, platform string, row ScenarioRow, rv RunVariant, attempt int, started time.Time, before, after *analysis.DeviceState) (analysis.RunRecord, bool) {
	rec := analysis.RunRecord{Scenario: row.Name, Attempt: attempt, RecordedAt: started,
		Build:  analysis.Build{Variant: rv.Label, NativeKey: rv.NativeKey, PublicEnvHash: rv.PublicEnvHash, ProductionEquivalent: rv.ProductionEquivalent},
		Before: before, After: after}
	rel := func(p string) string {
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(caseDir, p)
		}
		if r, err := filepath.Rel(runDir, p); err == nil {
			return r
		}
		return p
	}
	ev := &rec.Evidence
	ev.StepCycle, ev.StepGroups = row.StepCycle, row.StepGroups
	ev.Log = rel(filepath.Join(caseDir, "run.log"))
	for _, name := range []string{"marks.jsonl", "marks.json"} {
		if fileExists(filepath.Join(caseDir, name)) {
			ev.Marks = rel(filepath.Join(caseDir, name))
			break
		}
	}
	if platform == "android" && t.Config != nil && t.Config.App.Android != nil {
		ev.Package = t.Config.App.Android.Package
	}
	entries, _ := os.ReadDir(caseDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(caseDir, e.Name()))
		if err != nil {
			continue
		}
		var rf resultFile
		if json.Unmarshal(raw, &rf) != nil || rf.Scenario != row.Name || (rf.Platform != "" && rf.Platform != platform) {
			continue
		}
		if at, err := time.Parse(time.RFC3339Nano, rf.RecordedAt); err == nil {
			rec.RecordedAt = at.UTC()
		}
		switch {
		case rf.TracePath != "" && fileExists(rf.TracePath):
			ev.Trace = rel(rf.TracePath)
		case rf.FramesPath != "" && fileExists(rf.FramesPath):
			ev.Frames = rel(rf.FramesPath)
		case rf.PftracePath != "" && fileExists(rf.PftracePath):
			ev.Pftrace = rel(rf.PftracePath)
		}
	}
	prefix := row.Name + "-" + platform + "-"
	if ev.Trace == "" && ev.Frames == "" && ev.Pftrace == "" {
		for _, e := range entries {
			n := e.Name()
			if !strings.HasPrefix(n, prefix) {
				continue
			}
			switch {
			case e.IsDir() && strings.HasSuffix(n, ".trace"):
				ev.Trace = rel(filepath.Join(caseDir, n))
			case strings.HasSuffix(n, ".pftrace"):
				ev.Pftrace = rel(filepath.Join(caseDir, n))
			}
		}
		frames, _ := os.ReadDir(filepath.Join(caseDir, "frames"))
		for _, e := range frames {
			if n := e.Name(); strings.HasPrefix(n, prefix) && (strings.HasSuffix(n, ".json") || strings.HasSuffix(n, ".json.gz")) {
				ev.Frames = rel(filepath.Join(caseDir, "frames", n))
			}
		}
	}
	// The analysis anchors a sidecar's clock on recordedAt: without a result
	// JSON, the stamp in the evidence's own name, never the block start.
	if rec.RecordedAt.Equal(started) {
		if ev.Frames != "" {
			if ms, ok := framestats.RecordedAtFromName(filepath.Base(ev.Frames)); ok {
				rec.RecordedAt = time.UnixMilli(int64(ms)).UTC()
			}
		} else if m := traceStampRe.FindStringSubmatch(filepath.Base(ev.Trace)); m != nil {
			if ms, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				rec.RecordedAt = time.UnixMilli(ms).UTC()
			}
		}
	}
	if ev.Trace != "" {
		for _, suffix := range []string{".record.log", ".record.txt"} {
			if p := filepath.Join(runDir, ev.Trace+suffix); fileExists(p) {
				ev.RecordLog = ev.Trace + suffix
				break
			}
		}
	}
	return rec, ev.Trace == "" && ev.Frames == "" && ev.Pftrace == ""
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
