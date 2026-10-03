package doctor

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/perflab/netfwd"
	"github.com/henderson-tech/vybava/internal/runx"
)

func (r *run) adb(timeout time.Duration, args ...string) (hostexec.Result, error) {
	return r.exec(timeout, append([]string{r.adbPath(), "-s", r.o.Device.Serial}, args...)...)
}

func (r *run) buildFix() string {
	return "perflab build native --platform android --profile perf --json, then " + r.installFix()
}

func (r *run) androidDevice() {
	if r.level("", Fail, Fail) == "" {
		return
	}
	d := r.o.Device
	r.data.Device = &DeviceFacts{ID: d.ID, Platform: "android", OS: d.OS}
	if r.adbPath() == "" {
		return // the tools row names the install
	}
	r.androidPackage()
	r.focus()
	r.forward()
}

// packageInfo is the slice of `dumpsys package <pkg>` doctor reads.
type packageInfo struct {
	Installed   bool
	VersionName string
	Flags       []string
}

var (
	versionNameRe = regexp.MustCompile(`(?m)^\s+versionName=(\S+)`)
	flagsRe       = regexp.MustCompile(`(?m)^\s+flags=\[([^\]]*)\]`)
)

// parsePackageDump reads `adb shell dumpsys package <pkg>`. A missing
// package answers "Unable to find package: <pkg>".
func parsePackageDump(out, pkg string) packageInfo {
	if !strings.Contains(out, "Package ["+pkg+"]") {
		return packageInfo{}
	}
	info := packageInfo{Installed: true}
	if m := versionNameRe.FindStringSubmatch(out); m != nil {
		info.VersionName = m[1]
	}
	if m := flagsRe.FindStringSubmatch(out); m != nil {
		info.Flags = strings.Fields(m[1])
	}
	return info
}

// parseManifest reads `aapt2 dump xmltree --file AndroidManifest.xml`:
// <profileable android:shell="true"> under <application>, and
// android:debuggable. Android 13's dumpsys does not print the profileable
// flag, so the installed APK's manifest is the only source.
func parseManifest(out string) (profileable, debuggable bool) {
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if strings.Contains(t, ":debuggable(") && strings.HasSuffix(t, "=true") {
			debuggable = true
		}
		if !strings.HasPrefix(t, "E: profileable") {
			continue
		}
		for _, attr := range lines[i+1 : min(i+4, len(lines))] {
			a := strings.TrimSpace(attr)
			if !strings.HasPrefix(a, "A: ") {
				break
			}
			if strings.Contains(a, ":shell(") && strings.HasSuffix(a, "=true") {
				profileable = true
			}
		}
	}
	return profileable, debuggable
}

func (r *run) androidPackage() {
	d := r.o.Device
	pkg := r.o.App.Package
	if pkg == "" {
		return
	}
	st := r.level("", Fail, Fail)
	res, err := r.adb(30*time.Second, "shell", "dumpsys", "package", pkg)
	if err != nil || res.Exit != 0 {
		r.add(Check{ID: "android-app", Status: Warn, Code: DiagDeviceOffline, Detail: "cannot read dumpsys package on " + d.ID + ": " + tail(res, err), Fix: "perflab device probe " + d.ID + " --json"})
		return
	}
	info := parsePackageDump(string(res.Stdout), pkg)
	if !info.Installed {
		r.add(Check{ID: "android-app", Status: st, Code: DiagAppNotInstalled, Detail: pkg + " is not installed on " + d.ID, Fix: r.installFix()})
		return
	}
	facts := r.data.Device
	facts.AppVersion = info.VersionName
	debuggable := slices.Contains(info.Flags, "DEBUGGABLE")
	facts.Debuggable = ptr(debuggable)
	if debuggable {
		r.add(Check{ID: "android-app", Status: st, Code: DiagDebuggableBuild,
			Detail: fmt.Sprintf("%s %s on %s is DEBUGGABLE (a debug or dev-client build): its frames are not release frames", pkg, info.VersionName, d.ID),
			Fix:    r.buildFix()})
		return
	}
	prof, known := r.profileable(pkg)
	if !known {
		r.add(Check{ID: "android-app", Status: Pass, Detail: fmt.Sprintf("%s %s installed, not debuggable (profileable unchecked)", pkg, info.VersionName)})
		return
	}
	facts.Profileable = ptr(prof)
	if !prof {
		r.add(Check{ID: "android-app", Status: st, Code: DiagNotProfileable,
			Detail: fmt.Sprintf("%s %s on %s has no <profileable android:shell=\"true\">: perflab measures only a profileable, non-debuggable perf build", pkg, info.VersionName, d.ID),
			Fix:    r.buildFix()})
		return
	}
	r.add(Check{ID: "android-app", Status: Pass, Detail: fmt.Sprintf("%s %s installed: release, profileable, not debuggable", pkg, info.VersionName)})
}

// profileable pulls the installed base APK and reads its manifest; known is
// false when aapt2 is missing or the pull failed (the tools row says why).
func (r *run) profileable(pkg string) (profileable, known bool) {
	aapt2 := r.aapt2Path()
	if aapt2 == "" {
		return false, false
	}
	res, err := r.adb(30*time.Second, "shell", "pm", "path", pkg)
	if err != nil || res.Exit != 0 {
		return false, false
	}
	var base string
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		p := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "package:"))
		if strings.HasSuffix(p, "/base.apk") {
			base = p
			break
		}
	}
	if base == "" {
		return false, false
	}
	local := r.tmpPath("base.apk")
	defer os.Remove(local)
	if res, err := r.adb(3*time.Minute, "pull", base, local); err != nil || res.Exit != 0 {
		return false, false
	}
	out, err := r.exec(60*time.Second, aapt2, "dump", "xmltree", "--file", "AndroidManifest.xml", local)
	if err != nil || out.Exit != 0 {
		return false, false
	}
	prof, _ := parseManifest(string(out.Stdout))
	return prof, true
}

var focusRe = regexp.MustCompile(`mCurrentFocus=Window\{\S+ \S+ ([^/\s}]+)(?:/([^\s}]+))?\}`)

// parseFocus reads the focused window's package (and activity) from
// `adb shell dumpsys window`; "" when nothing has focus.
func parseFocus(out string) (pkg, activity string) {
	if m := focusRe.FindStringSubmatch(out); m != nil {
		return m[1], m[2]
	}
	return "", ""
}

func (r *run) focus() {
	st := r.level("", "", Fail)
	pkg := r.o.App.Package
	if st == "" || pkg == "" {
		return
	}
	res, err := r.adb(30*time.Second, "shell", "dumpsys", "window")
	if err != nil || res.Exit != 0 {
		return
	}
	focus, activity := parseFocus(string(res.Stdout))
	r.data.Device.FocusPackage = focus
	if focus == pkg {
		r.add(Check{ID: "input-focus", Status: Pass, Detail: pkg + " has the input focus"})
		return
	}
	what := focus
	if activity != "" {
		what += "/" + activity
	}
	if what == "" {
		what = "nothing"
	}
	r.add(Check{ID: "input-focus", Status: st, Code: DiagInputFocusWrong,
		Detail: fmt.Sprintf("the focused window on %s is %s, not %s: adb-injected gestures would land there", r.o.Device.ID, what, pkg),
		Fix:    fmt.Sprintf("perflab app launch --device %s --lease %s", r.deviceID(), r.lease())})
}

// forward is the Android API reachability check: netfwd's status of the
// phone -> adb reverse -> Mac loopback -> origin chain.
func (r *run) forward() {
	st := r.level("", Fail, "")
	a := r.o.API
	if st == "" || r.o.StartsForward || a == nil || a.DevicePort == 0 || a.Origin == "" {
		return
	}
	d := r.o.Device
	spec := netfwd.Spec{DeviceID: d.ID, Serial: d.Serial, Lease: r.o.Lease, DevicePort: a.DevicePort, Origin: a.Origin, Health: a.Health, Hold: a.Hold}
	res, err := netfwd.Status(r.ctx, r.env.Net, spec)
	if err != nil {
		var de runx.DiagError
		if errors.As(err, &de) {
			r.add(Check{ID: "api-forward", Status: st, Code: de.Diag.Code, Detail: de.Diag.Detail, Fix: de.Diag.Fix})
			return
		}
		r.add(Check{ID: "api-forward", Status: st, Code: netfwd.DiagForwardDown, Detail: "cannot inspect the forward: " + err.Error(), Fix: netfwd.ForwardCommand(spec)})
		return
	}
	if len(res.Diagnostics) == 0 {
		r.add(Check{ID: "api-forward", Status: Pass, Detail: fmt.Sprintf("the phone's localhost:%d answers %s through adb reverse and the perflab forward", a.DevicePort, a.Health)})
		return
	}
	for _, dg := range res.Diagnostics {
		s := st
		if dg.Severity != "error" {
			s = Warn
		}
		r.add(Check{ID: "api-forward", Status: s, Code: dg.Code, Detail: dg.Detail, Fix: dg.Fix})
	}
}
