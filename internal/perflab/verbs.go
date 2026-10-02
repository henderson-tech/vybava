package perflab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/devlab"
	"github.com/henderson-tech/vybava/internal/perflab/buildindex"
	"github.com/henderson-tech/vybava/internal/perflab/doctor"
	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/perflab/netfwd"
	"github.com/henderson-tech/vybava/internal/perflab/wda"
	"github.com/henderson-tech/vybava/internal/runx"
)

// This file maps the lane packages (doctor, buildindex, wda, netfwd) onto
// perflab's Result, filling the adapter values each needs and the lease
// bookkeeping (holds, the install fence) they leave to the verb layer.

// retryable are the transient codes a verb retries once on its own
// (docs/perflab.md "Retries"); the attempt is recorded in its data.
var retryable = map[string]bool{
	DiagXctraceAttachFailed: true, DiagWDAStalled: true, DiagADBDisconnected: true,
	buildindex.DiagInstallTransport: true,
}

// CodeOf is the diagnostic code an error carries, "" for a plain error.
func CodeOf(err error) string {
	var de runx.DiagError
	if errors.As(err, &de) {
		return de.Diag.Code
	}
	return ""
}

// ---- doctor -----------------------------------------------------------

// DoctorOptions are the doctor flags.
type DoctorOptions struct {
	Device   string
	Lease    string
	Platform string
	For      string
	Wake     bool
	// Then is the verb the preflight serves (appended to next).
	Then string
	// StartsForward: the caller (run) brings the Android API forward up
	// itself, so its absence is not a preflight failure.
	StartsForward bool
}

func (t *Tool) doctorEnv() (doctor.Env, error) {
	env, err := doctor.DefaultEnv(t.Log)
	if err != nil {
		return env, err
	}
	env.HostBuildHolder = func() (string, error) {
		h, err := buildindex.HostBuildHolder(t.Store.Dirs)
		if err != nil || h == nil {
			return "", err
		}
		return h.String(), nil
	}
	env.WDA.HostLock = t.hostLockFor(0)
	return env, nil
}

// hostLockFor is wda's hook onto buildindex's Mac-wide build lock, waiting up
// to wait for a running build (`wda build --wait`, the lock's own retry line).
func (t *Tool) hostLockFor(wait time.Duration) func(context.Context, string) (func(), error) {
	return func(_ context.Context, key string) (func(), error) {
		return buildindex.AcquireHostBuild(t.Store.Dirs, wait, buildindex.LockHolder{PID: os.Getpid(), Verb: "wda build", Key: key, Worktree: t.ProjectDir, Since: t.Now()}, "perflab wda build --json")
	}
}

// wdaSpec is the prebuilt WDA the adapter's runner would use.
func (t *Tool) wdaSpec(team, bundleID string) (wda.Spec, error) {
	c, err := t.Cfg()
	if err != nil {
		return wda.Spec{}, err
	}
	if c.App.IOS == nil {
		return wda.Spec{}, diag(DiagConfigInvalid, "the perflab section has no app.ios, so there is no WDA to build", adapterFix)
	}
	if c.Runner.AppiumHome == "" {
		return wda.Spec{}, diag(DiagConfigInvalid, "runner.appiumHome is not set; the WDA comes from that APPIUM_HOME's xcuitest driver", adapterFix)
	}
	s := wda.Spec{
		AppiumHome: t.abs(c.Runner.AppiumHome), Team: c.App.IOS.Team,
		BundleID: wda.DefaultBundleID(c.App.IOS.BundleID), CacheDir: t.Store.Dirs.Cache,
	}
	if team != "" {
		s.Team = team
	}
	if bundleID != "" {
		s.BundleID = bundleID
	}
	return s, nil
}

func (t *Tool) abs(p string) string {
	p = expandHome(p)
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(t.ProjectDir, p)
}

func (t *Tool) doctorDevice(handle string) (*doctor.Device, error) {
	led, err := t.Lab.LoadLedger()
	if err != nil {
		return nil, err
	}
	id, dev, ok := led.Resolve(handle)
	if !ok {
		return nil, diag(devlab.DiagDeviceUnknown, fmt.Sprintf("%q is not a ledger id or a registered UDID/serial", handle), "perflab device scan --json")
	}
	return &doctor.Device{ID: id, Platform: string(dev.Platform), HardwareUDID: dev.HardwareUDID, CoreDeviceID: dev.CoreDeviceID,
		Serial: dev.Serial, OS: dev.OS, Transport: dev.Transport, ExpectHz: dev.ExpectHz}, nil
}

// Doctor runs the preflight: host checks always, device checks with
// --device, the adapter's app, API and WDA checks when the project has a
// perflab section.
func (t *Tool) Doctor(ctx context.Context, o DoctorOptions) (Result, error) {
	env, err := t.doctorEnv()
	if err != nil {
		return Result{}, err
	}
	opts := doctor.Options{For: doctor.Purpose(o.For), Platform: o.Platform, Lease: o.Lease, Wake: o.Wake,
		ProjectDir: t.ProjectDir, CacheDir: t.Store.Dirs.Cache, TunnelRegistry: tunnelRegistry(t.Getenv), Then: o.Then, StartsForward: o.StartsForward}
	switch opts.For {
	case "", doctor.ForBuild, doctor.ForRun, doctor.ForProbe, doctor.ForAll:
	default:
		return Result{}, diag(DiagUsage, fmt.Sprintf("--for %q is not build, run, probe or all", o.For), "perflab doctor --for all --json")
	}
	if o.Device != "" {
		dev, err := t.doctorDevice(o.Device)
		if err != nil {
			return Result{}, err
		}
		opts.Device = dev
		if opts.Platform == "" {
			opts.Platform = dev.Platform
		}
		env.ProbeDevice = func(ctx context.Context, d doctor.Device) ([]runx.Diagnostic, error) {
			res, err := t.Lab.Probe(ctx, d.ID, devlab.ProbeOptions{Lease: o.Lease})
			return res.Diagnostics, err
		}
	}
	if c := t.Config; c != nil {
		v := t.baseVars(ctx, opts.Platform)
		if c.App.IOS != nil {
			opts.App.BundleID = c.App.IOS.BundleID
		}
		if c.App.Android != nil {
			opts.App.Package = c.App.Android.Package
		}
		if c.Runner.AppiumHome != "" {
			opts.AppiumHome = t.abs(c.Runner.AppiumHome)
		}
		if c.Runner.AppiumServerLog != "" {
			opts.AppiumServerLog = t.abs(c.Runner.AppiumServerLog)
		}
		if opts.Platform != "android" && c.App.IOS != nil && c.Runner.AppiumHome != "" {
			if s, err := t.wdaSpec("", ""); err == nil {
				opts.WDA = &s
			}
		}
		if a := c.API; a != nil {
			api := &doctor.API{Health: a.Health}
			if api.OriginCmd, err = v.Expand("api.origin", a.Origin, true); err != nil {
				return Result{}, err
			}
			if a.Hold != "" {
				if api.Hold, err = v.Expand("api.hold", a.Hold, true); err != nil {
					return Result{}, err
				}
			}
			if a.Device != nil && a.Device.Android != nil && opts.Platform != "ios" {
				api.DevicePort = a.Device.Android.DevicePort
			}
			if a.Device != nil && a.Device.IOS != nil && opts.Platform == "ios" {
				if origin, err := t.deviceAPIOrigin(ctx, v, "ios"); err == nil {
					api.IOSOrigin = origin
				}
			}
			opts.API = api
		}
	}
	res, err := doctor.Run(ctx, env, opts)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: res.Data, Diagnostics: res.Diagnostics, Next: res.Next}, nil
}

func tunnelRegistry(getenv func(string) string) string {
	if v := getenv("PERFLAB_TUNNEL_REGISTRY"); v != "" {
		return v
	}
	return devlab.DefaultTunnelRegistry
}

// ---- fingerprint, build, bundle, pack ----------------------------------

// BuildOptions are the fingerprint / build / bundle flags.
type BuildOptions struct {
	Platform string
	Profile  string
	Kind     string
	Team     string
	Ref      string
	Label    string
	Wait     time.Duration
	Stall    time.Duration
	Timeout  time.Duration
}

func (o BuildOptions) target() buildindex.Target {
	// Android has one kind (Gradle always bundles the JS), so the protocol's
	// kind-less `build find --platform android` names it.
	kind := o.Kind
	if kind == "" && o.Platform == "android" {
		kind = buildindex.KindBundled
	} else if kind == "" {
		kind = buildindex.KindShell
	}
	return buildindex.Target{Platform: o.Platform, Profile: o.Profile, Kind: kind}
}

func (t *Tool) buildInputs(ctx context.Context, o BuildOptions) (buildindex.Project, buildindex.ProfileEnv, error) {
	if o.Platform != "ios" && o.Platform != "android" {
		return buildindex.Project{}, buildindex.ProfileEnv{}, diag(DiagUsage, "--platform must be ios or android", "pass --platform ios|android")
	}
	if o.Profile == "" {
		o.Profile = "perf"
	}
	p, v, err := t.Project(ctx, o.Platform, o.Profile)
	if err != nil {
		return p, buildindex.ProfileEnv{}, err
	}
	env, err := t.ProfileEnv(ctx, v, o.Profile)
	return p, env, err
}

func defaulted(o BuildOptions) BuildOptions {
	if o.Profile == "" {
		o.Profile = "perf"
	}
	return o
}

// Fingerprint computes the portable native key of the working tree.
func (t *Tool) Fingerprint(ctx context.Context, o BuildOptions) (Result, error) {
	o = defaulted(o)
	p, env, err := t.buildInputs(ctx, o)
	if err != nil {
		return Result{}, err
	}
	fp, err := buildindex.RunFingerprint(ctx, t.Exec, buildindex.FingerprintSpec{Project: p, Target: o.target(), Env: env.Vars, Timeout: o.Timeout})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: fp, Lines: []string{"key " + fp.Key},
		Next: []string{fmt.Sprintf("perflab build find --platform %s --profile %s --kind %s --json", o.Platform, o.Profile, o.target().Kind)}}, nil
}

func (t *Tool) buildSpec(ctx context.Context, o BuildOptions, verb string) (buildindex.BuildSpec, error) {
	o = defaulted(o)
	p, env, err := t.buildInputs(ctx, o)
	if err != nil {
		return buildindex.BuildSpec{}, err
	}
	return buildindex.BuildSpec{Project: p, Target: o.target(), Env: env, Team: o.Team, Ref: o.Ref, Wait: o.Wait,
		Stall: o.Stall, Timeout: o.Timeout, Progress: t.progress(verb + " " + o.Platform)}, nil
}

func buildResult(res buildindex.BuildResult) Result {
	return Result{Data: res, Diagnostics: res.Diagnostics, Next: res.Next,
		Lines: []string{fmt.Sprintf("%s %s (hit=%v)", res.Build.Manifest.Key, res.Build.Path, res.Hit)}}
}

// BuildFind looks the working tree's key up in the index.
func (t *Tool) BuildFind(ctx context.Context, o BuildOptions) (Result, error) {
	spec, err := t.buildSpec(ctx, o, "build find")
	if err != nil {
		return Result{}, err
	}
	res, err := t.Store.Find(ctx, t.Exec, spec)
	if err != nil {
		return Result{}, err
	}
	return buildResult(res), nil
}

// BuildNative returns the index hit or builds it under the host build lock,
// after doctor's build preflight.
func (t *Tool) BuildNative(ctx context.Context, o BuildOptions) (Result, error) {
	spec, err := t.buildSpec(ctx, o, "build native")
	if err != nil {
		return Result{}, err
	}
	spec.Preflight = func(ctx context.Context) error {
		res, err := t.Doctor(ctx, DoctorOptions{For: string(doctor.ForBuild), Platform: o.Platform})
		if err != nil {
			return err
		}
		for _, d := range res.Diagnostics {
			if d.Severity == "error" {
				return runx.DiagError{Diag: d}
			}
		}
		return nil
	}
	res, err := t.Store.BuildNative(ctx, t.Exec, spec)
	if err != nil {
		return Result{}, err
	}
	return buildResult(res), nil
}

// BuildImport adopts an existing .app or .apk built from this tree.
func (t *Tool) BuildImport(ctx context.Context, artifact string, o BuildOptions) (Result, error) {
	o = defaulted(o)
	p, env, err := t.buildInputs(ctx, o)
	if err != nil {
		return Result{}, err
	}
	res, err := t.Store.Import(ctx, t.Exec, buildindex.ImportSpec{Project: p, Target: o.target(), Env: env, Artifact: artifact, Progress: t.progress("build import " + o.Platform)})
	if err != nil {
		return Result{}, err
	}
	return buildResult(res), nil
}

// BuildList lists the native builds, bundles and variants in the cache.
func (t *Tool) BuildList() (Result, error) {
	builds, err := t.Store.ListBuilds()
	if err != nil {
		return Result{}, err
	}
	bundles, err := t.Store.ListBundles()
	if err != nil {
		return Result{}, err
	}
	variants, err := t.Store.ListVariants()
	if err != nil {
		return Result{}, err
	}
	lines := []string{fmt.Sprintf("%d builds, %d bundles, %d variants in %s", len(builds), len(bundles), len(variants), t.Store.Dirs.Cache)}
	for _, b := range builds {
		m := b.Manifest
		lines = append(lines, fmt.Sprintf("build %s %s/%s/%s %s", m.Key, m.Platform, m.Profile, m.Kind, b.Path))
	}
	for _, v := range variants {
		lines = append(lines, fmt.Sprintf("variant %s %s", v.Manifest.VariantID, v.Manifest.BundleLabel))
	}
	next := []string{}
	if len(builds) == 0 {
		next = append(next, "perflab build find --platform <ios|android> --profile perf --json")
	}
	return Result{Data: map[string]any{"builds": orEmpty(builds), "bundles": orEmpty(bundles), "variants": orEmpty(variants), "cache": t.Store.Dirs.Cache},
		Lines: lines, Next: next}, nil
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// BuildGC deletes the oldest unused entries; a lease's last install is
// pinned.
func (t *Tool) BuildGC(keep int, maxSize int64, dryRun bool) (Result, error) {
	pinned := []string{}
	if st, err := t.Lab.Status(""); err == nil {
		if data, ok := st.Data.(devlab.StatusData); ok {
			for _, l := range data.Leases {
				if l.LastInstalled != nil {
					pinned = append(pinned, l.LastInstalled.VariantID)
				}
			}
		}
	}
	res, err := t.Store.GC(keep, maxSize, pinned, dryRun)
	if err != nil {
		return Result{}, err
	}
	next := []string{}
	if dryRun && len(res.Removed) > 0 {
		next = append(next, fmt.Sprintf("perflab build gc --keep %d --json", keep))
	}
	return Result{Data: res, Lines: []string{fmt.Sprintf("removed %d, kept %d, pinned %d", len(res.Removed), res.Kept, len(res.Pinned))}, Next: next}, nil
}

// BundleExport exports the JS bundle (Hermes bytecode) into the cache.
func (t *Tool) BundleExport(ctx context.Context, o BuildOptions) (Result, error) {
	o = defaulted(o)
	p, env, err := t.buildInputs(ctx, o)
	if err != nil {
		return Result{}, err
	}
	res, err := t.Store.ExportBundle(ctx, t.Exec, buildindex.BundleSpec{Project: p, Platform: o.Platform, Profile: o.Profile, Env: env,
		Ref: o.Ref, Label: o.Label, Timeout: o.Timeout, Progress: t.progress("bundle export " + o.Platform)})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: res, Next: res.Next, Lines: []string{fmt.Sprintf("bundle %s (hit=%v)", res.Bundle.Manifest.SHA256, res.Hit)}}, nil
}

// BundleList lists the exported bundles.
func (t *Tool) BundleList(platform string) (Result, error) {
	all, err := t.Store.ListBundles()
	if err != nil {
		return Result{}, err
	}
	out := []buildindex.Bundle{}
	for _, b := range all {
		if platform == "" || b.Manifest.Platform == platform {
			out = append(out, b)
		}
	}
	lines := make([]string, 0, len(out))
	for _, b := range out {
		lines = append(lines, fmt.Sprintf("%s %s %s %s", b.Manifest.SHA256, b.Manifest.Platform, b.Manifest.Label, b.Manifest.Ref))
	}
	return Result{Data: map[string]any{"bundles": out}, Lines: lines}, nil
}

// Pack puts a bundle into a native build: a content-addressed variant.
func (t *Tool) Pack(ctx context.Context, native, bundle string) (Result, error) {
	res, err := t.Store.Pack(ctx, t.Exec, buildindex.PackSpec{NativeKey: native, BundleSHA: bundle, Progress: t.progress("pack")})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: res, Diagnostics: res.Diagnostics, Next: res.Next, Lines: []string{"variant " + res.Variant.Manifest.VariantID}}, nil
}

// ---- install -----------------------------------------------------------

func bdevice(h *devlab.Hold) buildindex.Device {
	return buildindex.Device{ID: h.ID, Platform: string(h.Device.Platform), CoreDeviceID: h.Device.CoreDeviceID,
		Serial: h.Device.Serial, ProtectedPackages: h.Device.ProtectedPackages}
}

func installedOf(res buildindex.InstallResult) devlab.Installed {
	return devlab.Installed{VariantID: res.ID, Package: res.AppID, ArtifactSHA256: res.Stamp.APKSHA256, BundleVersion: res.Stamp.BundleVersion}
}

// stampOf is the fence a lease recorded, as buildindex compares it.
func stampOf(platform string, in *devlab.Installed) buildindex.InstalledStamp {
	if in == nil {
		return buildindex.InstalledStamp{Platform: platform}
	}
	return buildindex.InstalledStamp{Platform: platform, AppID: in.Package, APKSHA256: in.ArtifactSHA256, BundleVersion: in.BundleVersion}
}

// installHeld installs under an existing hold, retrying a USB drop once,
// and writes the fence.
func (t *Tool) installHeld(ctx context.Context, h *devlab.Hold, id string, timeout time.Duration) (buildindex.InstallResult, int, error) {
	item, err := t.Store.Resolve(id)
	if err != nil {
		return buildindex.InstallResult{}, 0, err
	}
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	spec := buildindex.InstallSpec{Item: item, Device: bdevice(h), Timeout: timeout, Progress: t.progress("install " + h.ID)}
	attempts := 1
	res, err := buildindex.Install(ctx, t.Exec, spec)
	if err != nil && retryable[CodeOf(err)] {
		attempts++
		res, err = buildindex.Install(ctx, t.Exec, spec)
	}
	if err != nil {
		return res, attempts, err
	}
	return res, attempts, h.RecordInstall(installedOf(res))
}

// Install puts a variant (or a native build) on a leased device.
func (t *Tool) Install(ctx context.Context, handle, token, id string, timeout time.Duration) (Result, error) {
	h, err := t.Lab.Hold(handle, token, "install")
	if err != nil {
		return Result{}, err
	}
	defer h.Done()
	res, attempts, err := t.installHeld(ctx, h, id, timeout)
	if err != nil {
		return Result{}, err
	}
	data := map[string]any{"install": res, "attempts": attempts}
	return Result{Data: data, Diagnostics: res.Diagnostics,
		Lines: []string{fmt.Sprintf("%s installed on %s in %dms", res.ID, h.ID, res.DurationMs)},
		Next: []string{
			fmt.Sprintf("perflab run <scenario> --device %s --lease %s --variant %s --json", h.ID, token, res.ID),
			fmt.Sprintf("perflab probe rest --device %s --lease %s --json", h.ID, token),
		}}, nil
}

// ---- wda ---------------------------------------------------------------

func wdaResult(res wda.Result) Result {
	return Result{Data: res.Data, Diagnostics: res.Diagnostics, Next: res.Next}
}

func (t *Tool) wdaEnv(wait time.Duration) wda.Env {
	env := wda.DefaultEnv(t.Log)
	env.HostLock = t.hostLockFor(wait)
	return env
}

// WDAFind looks the prebuilt WDA up.
func (t *Tool) WDAFind(ctx context.Context, team, bundleID string) (Result, error) {
	s, err := t.wdaSpec(team, bundleID)
	if err != nil {
		return Result{}, err
	}
	res, err := wda.Find(ctx, t.wdaEnv(0), s)
	return wdaResult(res), err
}

// WDABuild builds the prebuilt WDA (a hit returns it).
func (t *Tool) WDABuild(ctx context.Context, team, bundleID string, stall, timeout, wait time.Duration) (Result, error) {
	s, err := t.wdaSpec(team, bundleID)
	if err != nil {
		return Result{}, err
	}
	res, err := wda.Build(ctx, t.wdaEnv(wait), s, wda.BuildOptions{Stall: stall, Timeout: timeout})
	return wdaResult(res), err
}

// WDAImport adopts an existing WDA derived-data dir.
func (t *Tool) WDAImport(ctx context.Context, dir, team, bundleID string) (Result, error) {
	s, err := t.wdaSpec(team, bundleID)
	if err != nil {
		return Result{}, err
	}
	res, err := wda.Import(ctx, t.wdaEnv(0), s, dir)
	return wdaResult(res), err
}

// WDAList lists the prebuilt WDAs.
func (t *Tool) WDAList() (Result, error) {
	entries, err := wda.List(wda.Spec{CacheDir: t.Store.Dirs.Cache})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"entries": orEmpty(entries)}}, nil
}

// ---- net ---------------------------------------------------------------

func (t *Tool) netSpec(ctx context.Context, handle, token string, port int) (netfwd.Spec, error) {
	c, err := t.Cfg()
	if err != nil {
		return netfwd.Spec{}, err
	}
	id, dev, _, err := t.Lab.Verify(handle, token)
	if err != nil {
		return netfwd.Spec{}, err
	}
	if dev.Platform != devlab.PlatformAndroid {
		return netfwd.Spec{}, diag(DiagUsage, id+" is an iOS device; net forward is Android only (iOS bakes an origin the phone reaches)", "perflab doctor --device "+id+" --lease "+token+" --json")
	}
	if c.API == nil {
		return netfwd.Spec{}, diag(DiagConfigInvalid, "the perflab section has no api to forward to", adapterFix)
	}
	s := netfwd.Spec{DeviceID: id, Serial: dev.Serial, Lease: token, DevicePort: port, Health: c.API.Health}
	if s.DevicePort == 0 && c.API.Device != nil && c.API.Device.Android != nil {
		s.DevicePort = c.API.Device.Android.DevicePort
	}
	if s.DevicePort == 0 {
		return netfwd.Spec{}, diag(DiagUsage, "no device port: pass --device-port or set api.device.android.devicePort", "")
	}
	v := t.baseVars(ctx, "android")
	if s.Origin, err = t.APIOrigin(ctx, v); err != nil {
		return s, err
	}
	if c.API.Hold != "" {
		if s.Hold, err = v.Expand("api.hold", c.API.Hold, true); err != nil {
			return s, err
		}
	}
	return s, nil
}

func netResult(res netfwd.Result) Result {
	return Result{Data: res.Data, Diagnostics: res.Diagnostics, Next: res.Next}
}

// NetForward reverses the device port and forwards it to the API until ctx
// ends (SIGINT); the process is recorded on the lease so a release or the
// SessionEnd hook stops it.
func (t *Tool) NetForward(ctx context.Context, handle, token string, port int) (Result, error) {
	s, err := t.netSpec(ctx, handle, token, port)
	if err != nil {
		return Result{}, err
	}
	h, err := t.Lab.Hold(handle, token, "net forward")
	if err != nil {
		return Result{}, err
	}
	pid := os.Getpid()
	if err := h.RecordChild(devlab.Child{PID: pid, PGID: hostexec.ProcessGroup(), What: fmt.Sprintf("perflab net forward :%d", s.DevicePort)}); err != nil {
		h.Done()
		return Result{}, err
	}
	// The forwarder streams for hours; it holds the lease, not the device
	// lock, so a run or an app verb can still take the device.
	h.Done()
	env, err := netfwd.DefaultEnv(t.Log)
	if err != nil {
		return Result{}, err
	}
	res, err := netfwd.Forward(ctx, env, s)
	if h2, herr := t.Lab.Hold(handle, token, "net forward"); herr == nil {
		_ = h2.ForgetChild(pid)
		h2.Done()
	}
	return netResult(res), err
}

// NetStatus reports the reverse, the forwarder and the API health.
func (t *Tool) NetStatus(ctx context.Context, handle, token string, port int) (Result, error) {
	s, err := t.netSpec(ctx, handle, token, port)
	if err != nil {
		return Result{}, err
	}
	env, err := netfwd.DefaultEnv(t.Log)
	if err != nil {
		return Result{}, err
	}
	res, err := netfwd.Status(ctx, env, s)
	return netResult(res), err
}

// NetStop stops a recorded forwarder and removes its reverse.
func (t *Tool) NetStop(ctx context.Context, handle, token string, port int) (Result, error) {
	s, err := t.netSpec(ctx, handle, token, port)
	if err != nil {
		return Result{}, err
	}
	env, err := netfwd.DefaultEnv(t.Log)
	if err != nil {
		return Result{}, err
	}
	res, err := netfwd.Stop(ctx, env, s)
	return netResult(res), err
}

// leaseFix is the acquire line a device verb names when it has no token.
func leaseFix(id string) string {
	return "perflab lease acquire " + id + " --purpose \"<why>\" --json"
}

// joinNonEmpty joins the non-empty parts.
func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}
