package doctor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/perflab/netfwd"
	"github.com/henderson-tech/vybava/internal/perflab/wda"
	"github.com/henderson-tech/vybava/internal/runx"
)

// Purpose is what the preflight is for; it picks the checks and their
// severity (a loaded host warns a build but refuses a run).
type Purpose string

const (
	ForBuild Purpose = "build"
	ForRun   Purpose = "run"
	ForProbe Purpose = "probe"
	ForAll   Purpose = "all"
)

// Status is one check's outcome.
type Status string

const (
	Pass  Status = "pass"
	Warn  Status = "warn"
	Fail  Status = "fail"
	Human Status = "human"
)

// Check is one row of data.checks.
type Check struct {
	ID     string `json:"id"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

// Device is the ledger row doctor checks (the verb layer resolves the
// handle through devlab).
type Device struct {
	ID           string
	Platform     string // ios | android
	HardwareUDID string // xctrace, Appium, the tunnel registry
	CoreDeviceID string // devicectl
	Serial       string // adb
	OS           string
	Transport    string // devicectl transportType: wired | localNetwork
	ExpectHz     int
}

// App is the measured app.
type App struct {
	BundleID string // iOS
	Package  string // Android
	// ExpectBundleVersion is the variant's CFBundleVersion stamp
	// (<build>.<sha6>) when the caller knows which variant must be
	// installed; empty skips the WRONG_BINARY comparison.
	ExpectBundleVersion string
}

// API is the adapter's api section, token-resolved by the verb layer.
type API struct {
	Origin    string // a resolved host-side origin; when empty OriginCmd is run
	OriginCmd string // e.g. `devbox url fixit-work-x api`, run with sh -c in ProjectDir
	Health    string // e.g. /api/v1/health/ready
	Hold      string // e.g. `devbox hold fixit-work-x --for 4h`
	// Android reverse strategy: the port the build bakes (0 skips the
	// forward check).
	DevicePort int
	// IOSOrigin is what the iOS bundle bakes (the bake strategy); the phone
	// must reach it on its own, which only a human can confirm.
	IOSOrigin string
}

// Options are the doctor flags plus the adapter values they need.
type Options struct {
	For        Purpose
	Platform   string // ios | android | "" (both, or the device's)
	Device     *Device
	Lease      string
	Wake       bool
	ProjectDir string
	App        App
	// AppiumHome is the adapter's runner.appiumHome made absolute;
	// AppiumServerLog its runner.appiumServerLog.
	AppiumHome      string
	AppiumServerLog string
	// WDA is the prebuilt WDA the run would use (nil skips the check).
	WDA            *wda.Spec
	API            *API
	CacheDir       string
	TunnelRegistry string
	// Then is the verb the preflight was for, appended to next.
	Then string
	// StartsForward skips the Android forward check: `run` starts (or
	// reuses) the forward after its preflight, so a missing one is expected.
	StartsForward bool
}

// DefaultTunnelRegistry is the xcuitest driver's RemoteXPC registry.
const DefaultTunnelRegistry = "http://127.0.0.1:42314/remotexpc/tunnels"

// Env is how doctor reaches the Mac; tests swap every field.
type Env struct {
	Run      hostexec.Runner
	LookPath func(string) (string, error)
	Getenv   func(string) string
	HTTPGet  func(ctx context.Context, url string, timeout time.Duration) (int, []byte, error)
	Now      func() time.Time
	Sleep    func(time.Duration)
	Log      io.Writer
	TempDir  func() string
	ReadFile func(string) ([]byte, error)
	ReadDir  func(string) ([]os.DirEntry, error)
	Stat     func(string) (os.FileInfo, error)
	// PipeCapacity and FreeBytes are hostexec's probes.
	PipeCapacity func() (int, error)
	FreeBytes    func(string) (uint64, error)
	// HostBuildHolder names the perflab build holding the Mac-wide build
	// lock, "" when none (buildindex.HostBuildHolder).
	HostBuildHolder func() (string, error)
	// ProbeDevice runs devlab's device probe and returns its rows (lock,
	// Developer Mode, Auto-Lock, refresh, thermal, battery, memory, the
	// human checklist). Required whenever Options.Device is set.
	ProbeDevice func(ctx context.Context, d Device) ([]runx.Diagnostic, error)
	WDA         wda.Env
	Net         netfwd.Env
}

// DefaultEnv is the real Mac minus the two hooks the verb layer wires
// (HostBuildHolder from buildindex, ProbeDevice from devlab).
func DefaultEnv(log io.Writer) (Env, error) {
	net, err := netfwd.DefaultEnv(log)
	if err != nil {
		return Env{}, err
	}
	return Env{
		Run: hostexec.OS{}, LookPath: exec.LookPath, Getenv: os.Getenv, HTTPGet: HTTPGet,
		Now: time.Now, Sleep: time.Sleep, Log: log, TempDir: os.TempDir,
		ReadFile: os.ReadFile, ReadDir: os.ReadDir, Stat: os.Stat,
		PipeCapacity: hostexec.PipeCapacity, FreeBytes: hostexec.FreeBytes,
		WDA: wda.DefaultEnv(log), Net: net,
	}, nil
}

// HTTPGet is one bounded GET returning the status and up to 1 MiB of body.
func HTTPGet(ctx context.Context, url string, timeout time.Duration) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, body, err
}

// HostFacts are the host readings behind the checks.
type HostFacts struct {
	NCPU         int     `json:"ncpu,omitempty"`
	Load1        float64 `json:"load1,omitempty"`
	LoadPerCore  float64 `json:"loadPerCore,omitempty"`
	PipeBytes    int     `json:"pipeBytes,omitempty"`
	DiskFreeGB   float64 `json:"diskFreeGB,omitempty"`
	CacheDir     string  `json:"cacheDir,omitempty"`
	XcodeVersion string  `json:"xcodeVersion,omitempty"`
	XcodeBuild   string  `json:"xcodeBuild,omitempty"`
	AndroidSDK   string  `json:"androidSdk,omitempty"`
	BuildTools   string  `json:"buildTools,omitempty"`
	JavaHome21   string  `json:"javaHome21,omitempty"`
	APIOrigin    string  `json:"apiOrigin,omitempty"`
	WDAKey       string  `json:"wdaKey,omitempty"`
}

// DeviceFacts are the device readings doctor itself took.
type DeviceFacts struct {
	ID           string `json:"id"`
	Platform     string `json:"platform"`
	OS           string `json:"os,omitempty"`
	Online       *bool  `json:"online,omitempty"`
	Woken        bool   `json:"woken,omitempty"`
	AppVersion   string `json:"appVersion,omitempty"`
	BundleVer    string `json:"bundleVersion,omitempty"`
	Profileable  *bool  `json:"profileable,omitempty"`
	Debuggable   *bool  `json:"debuggable,omitempty"`
	FocusPackage string `json:"focusPackage,omitempty"`
}

// Data is the envelope payload.
type Data struct {
	For    Purpose      `json:"for"`
	Checks []Check      `json:"checks"`
	Host   HostFacts    `json:"host"`
	Device *DeviceFacts `json:"device,omitempty"`
}

// Result is what the verb hands the envelope.
type Result struct {
	Data        Data
	Diagnostics []runx.Diagnostic
	Next        []string
}

// OK is false when any check failed.
func (r Result) OK() bool {
	for _, d := range r.Diagnostics {
		if d.Severity == "error" {
			return false
		}
	}
	return true
}

// run is one doctor invocation's state.
type run struct {
	ctx  context.Context
	env  Env
	o    Options
	data Data
	tun  *tunnelRegistry
}

func (r *run) add(c Check) {
	if c.Status == "" {
		return
	}
	r.data.Checks = append(r.data.Checks, c)
}

func (r *run) purposes() []Purpose {
	if r.o.For == ForAll {
		return []Purpose{ForBuild, ForRun, ForProbe}
	}
	return []Purpose{r.o.For}
}

// level is the check's failure status for the selected purposes: the
// strictest of build/run/probe, "" when no selected purpose runs it.
func (r *run) level(build, runS, probe Status) Status {
	var out Status
	for _, p := range r.purposes() {
		s := map[Purpose]Status{ForBuild: build, ForRun: runS, ForProbe: probe}[p]
		if rank(s) > rank(out) {
			out = s
		}
	}
	return out
}

func rank(s Status) int {
	return map[Status]int{"": 0, Pass: 1, Human: 2, Warn: 3, Fail: 4}[s]
}

// platforms are the platforms the host checks cover.
func (r *run) platforms() []string {
	switch {
	case r.o.Device != nil:
		return []string{r.o.Device.Platform}
	case r.o.Platform != "":
		return []string{r.o.Platform}
	}
	return []string{"ios", "android"}
}

func (r *run) has(platform string) bool { return slices.Contains(r.platforms(), platform) }

func (r *run) deviceID() string {
	if r.o.Device == nil {
		return "<device>"
	}
	return r.o.Device.ID
}

func (r *run) lease() string {
	if r.o.Lease == "" {
		return "<token>"
	}
	return r.o.Lease
}

// rerun is doctor's own invocation, the tail of fixes that need one.
func (r *run) rerun(extra ...string) string {
	parts := []string{"perflab doctor"}
	if r.o.Device != nil {
		parts = append(parts, "--device", r.o.Device.ID, "--lease", r.lease())
	} else if r.o.Platform != "" {
		parts = append(parts, "--platform", r.o.Platform)
	}
	parts = append(parts, "--for", string(r.o.For))
	parts = append(parts, extra...)
	return strings.Join(append(parts, "--json"), " ")
}

func (o Options) validate() error {
	switch o.For {
	case ForBuild, ForRun, ForProbe, ForAll:
	default:
		return usage(fmt.Sprintf("--for %q is not one of build, run, probe, all", o.For), "perflab doctor --for run --json")
	}
	if o.Platform != "" && o.Platform != "ios" && o.Platform != "android" {
		return usage(fmt.Sprintf("--platform %q is not ios or android", o.Platform), "perflab doctor --platform ios --json")
	}
	if o.Device != nil && o.Device.Platform != "ios" && o.Device.Platform != "android" {
		return usage(fmt.Sprintf("device %s has no platform in the ledger", o.Device.ID), "perflab device show "+o.Device.ID+" --json")
	}
	if o.Wake && (o.Device == nil || o.Lease == "") {
		return usage("--wake launches the app on the phone, so it needs --device and its --lease", "perflab lease acquire <device> --json")
	}
	return nil
}

// Run runs every check the purpose selects, in dependency order.
func Run(ctx context.Context, env Env, o Options) (Result, error) {
	if o.For == "" {
		o.For = ForAll
	}
	if err := o.validate(); err != nil {
		return Result{}, err
	}
	if o.Device != nil && o.For != ForBuild && env.ProbeDevice == nil {
		return Result{}, fmt.Errorf("doctor: Env.ProbeDevice is not wired (devlab's device probe)")
	}
	if o.TunnelRegistry == "" {
		o.TunnelRegistry = DefaultTunnelRegistry
	}
	r := &run{ctx: ctx, env: env, o: o, data: Data{For: o.For, Checks: []Check{}}}
	tag := "doctor"
	if o.Device != nil {
		tag += " " + o.Device.ID
	}
	prog := hostexec.NewProgress(env.Log, tag, env.Now)

	prog.Phase("host")
	r.tools()
	r.pipe()
	r.load()
	r.buildLock()
	r.disk()
	r.appiumDrivers()
	r.wda()
	r.tunnelRegistry()
	r.api()
	if o.Device != nil {
		prog.Phase("device")
		if err := r.deviceProbe(); err != nil {
			return Result{}, err
		}
		switch o.Device.Platform {
		case "ios":
			r.iosDevice(prog)
		case "android":
			r.androidDevice()
		}
	}
	return r.result(), nil
}

// result turns the checks into diagnostics and next: fixes in check order
// (deduplicated), then the verb the preflight was for.
func (r *run) result() Result {
	res := Result{Data: r.data, Diagnostics: []runx.Diagnostic{}, Next: []string{}}
	for _, c := range r.data.Checks {
		sev := map[Status]string{Fail: "error", Warn: "warning", Human: "info"}[c.Status]
		if sev == "" {
			continue
		}
		res.Diagnostics = append(res.Diagnostics, runx.Diagnostic{Code: c.Code, Severity: sev, Detail: c.Detail, Fix: c.Fix})
		if c.Fix != "" && c.Status != Human && !slices.Contains(res.Next, c.Fix) {
			res.Next = append(res.Next, c.Fix)
		}
	}
	if r.o.Then != "" {
		res.Next = append(res.Next, r.o.Then)
	}
	return res
}

// deviceProbe folds devlab's probe rows in; a row whose code doctor already
// decided (the tunnel rows) or an identical row is dropped.
func (r *run) deviceProbe() error {
	if r.level(Status(""), Fail, Fail) == "" {
		return nil
	}
	rows, err := r.env.ProbeDevice(r.ctx, *r.o.Device)
	if err != nil {
		return err
	}
	problems := 0
	for _, d := range rows {
		if r.decidesItself(d) {
			continue
		}
		dup := slices.ContainsFunc(r.data.Checks, func(c Check) bool { return c.Code == d.Code && c.Detail == d.Detail })
		if dup {
			continue
		}
		st := map[string]Status{"error": Fail, "warning": Warn, "info": Human}[d.Severity]
		if st == "" {
			st = Human
		}
		if st == Fail || st == Warn {
			problems++
		}
		r.add(Check{ID: "device-state", Status: st, Code: d.Code, Detail: d.Detail, Fix: d.Fix})
	}
	if problems == 0 {
		r.add(Check{ID: "device-state", Status: Pass, Detail: "device probe found no blocking state"})
	}
	return nil
}

// decidesItself drops a probe row doctor re-decides with a purpose-aware
// severity and a preflight fix: the tunnel rows (doctor's host check, error
// for an iOS 17+ phone over Wi-Fi), the iOS < 17 tooling row, and the iOS
// "xctrace lists it under Devices Offline" warning (doctor's ios-online check,
// which can --wake). devicectl's own DEVICE_OFFLINE errors (not connected)
// stay: they name the cable.
func (r *run) decidesItself(d runx.Diagnostic) bool {
	switch d.Code {
	case DiagTunnelMissing, DiagTunnelRegistryDown, DiagDeviceToolingUnsupported:
		return r.o.Device.Platform == "ios"
	case DiagDeviceOffline:
		return r.o.Device.Platform == "ios" && d.Severity == "warning" && strings.Contains(d.Detail, "Devices Offline")
	}
	return false
}

// tmpPath is a fresh file path for a command that only writes JSON to a
// file (devicectl --json-output).
func (r *run) tmpPath(name string) string {
	return filepath.Join(r.env.TempDir(), fmt.Sprintf("perflab-doctor-%d-%s", r.env.Now().UnixNano(), name))
}

func (r *run) exec(timeout time.Duration, argv ...string) (hostexec.Result, error) {
	return r.env.Run.Run(r.ctx, hostexec.Cmd{Argv: argv, Dir: r.o.ProjectDir, Timeout: timeout})
}
