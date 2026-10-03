package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/perflab/netfwd"
	"github.com/henderson-tech/vybava/internal/perflab/wda"
	"github.com/henderson-tech/vybava/internal/runx"
)

const (
	// loadPerCoreLimit is claude-guards' "machine loaded" line: the 1-min
	// load above the core count.
	loadPerCoreLimit = 1.0
	diskLowGB        = 20
)

var xcodeVersionRe = regexp.MustCompile(`(?m)^Xcode\s+(\S+)\s*\nBuild version\s+(\S+)`)

func (r *run) tools() {
	if r.has("ios") {
		r.iosTools()
	}
	if r.has("android") {
		r.androidTools()
	}
}

func (r *run) iosTools() {
	need := r.level(Fail, Fail, Fail)
	if _, err := r.env.LookPath("xcrun"); err != nil {
		r.add(Check{ID: "tools-ios", Status: need, Code: DiagToolMissing, Detail: "xcrun is not installed", Fix: "xcode-select --install (and install Xcode from the App Store)"})
		return
	}
	var have []string
	if r.level(Fail, Fail, "") != "" {
		res, err := r.exec(30*time.Second, "xcodebuild", "-version")
		m := xcodeVersionRe.FindStringSubmatch(string(res.Stdout))
		if err != nil || res.Exit != 0 || m == nil {
			r.add(Check{ID: "tools-ios", Status: r.level(Fail, Fail, ""), Code: DiagToolMissing, Detail: "xcodebuild -version failed: " + tail(res, err), Fix: "sudo xcode-select -s /Applications/Xcode.app && xcodebuild -runFirstLaunch"})
			return
		}
		r.data.Host.XcodeVersion, r.data.Host.XcodeBuild = m[1], m[2]
		have = append(have, fmt.Sprintf("Xcode %s (%s)", m[1], m[2]))
		if platform, ok := r.iosPlatform(); ok {
			have = append(have, "iOS "+platform+" platform")
		}
	}
	if r.level("", Fail, Fail) != "" {
		for _, t := range []string{"xctrace", "devicectl"} {
			res, err := r.exec(30*time.Second, "xcrun", "--find", t)
			if err != nil || res.Exit != 0 {
				r.add(Check{ID: "tools-ios", Status: r.level("", Fail, Fail), Code: DiagToolMissing, Detail: "xcrun cannot find " + t + " (Xcode 15+ ships it): " + tail(res, err), Fix: "sudo xcode-select -s /Applications/Xcode.app"})
				return
			}
			have = append(have, t)
		}
	}
	r.add(Check{ID: "tools-ios", Status: Pass, Detail: strings.Join(have, ", ")})
}

// iosSDKVersion reads the iphoneos SDK version from `xcodebuild -showsdks
// -json`.
func iosSDKVersion(b []byte) string {
	var sdks []struct {
		Platform   string `json:"platform"`
		SDKVersion string `json:"sdkVersion"`
	}
	if json.Unmarshal(b, &sdks) != nil {
		return ""
	}
	for _, s := range sdks {
		if s.Platform == "iphoneos" {
			return s.SDKVersion
		}
	}
	return ""
}

// hasIOSRuntime reports whether `xcrun simctl list runtimes -j` lists an
// available iOS runtime for the SDK version (26.5 matches 26.5 and 26.5.1).
func hasIOSRuntime(b []byte, sdk string) bool {
	var list struct {
		Runtimes []struct {
			Platform    string `json:"platform"`
			Version     string `json:"version"`
			IsAvailable bool   `json:"isAvailable"`
		} `json:"runtimes"`
	}
	if json.Unmarshal(b, &list) != nil {
		return false
	}
	for _, rt := range list.Runtimes {
		if rt.Platform == "iOS" && rt.IsAvailable && (rt.Version == sdk || strings.HasPrefix(rt.Version, sdk+".")) {
			return true
		}
	}
	return false
}

// iosPlatform: Xcode 26 builds nothing for iOS (not a device build, not
// WDA's test-without-building) until the iOS platform matching its SDK is
// downloaded; every destination reads "iOS 26.5 is not installed". An Xcode
// update leaves the new platform undownloaded (2026-10-02: 17F41 -> 17F42
// with no iOS runtime left).
func (r *run) iosPlatform() (string, bool) {
	sdks, err := r.exec(60*time.Second, "xcodebuild", "-showsdks", "-json")
	sdk := iosSDKVersion(sdks.Stdout)
	if err != nil || sdks.Exit != 0 || sdk == "" {
		return "", false
	}
	runtimes, err := r.exec(60*time.Second, "xcrun", "simctl", "list", "runtimes", "-j")
	if err != nil || runtimes.Exit != 0 {
		return "", false
	}
	if hasIOSRuntime(runtimes.Stdout, sdk) {
		return sdk, true
	}
	r.add(Check{ID: "ios-platform", Status: r.level(Fail, Fail, ""), Code: DiagToolMissing,
		Detail: fmt.Sprintf("the iOS %s platform is not installed (Xcode > Settings > Components): every iOS build and WDA start stops with \"iOS %s is not installed\"", sdk, sdk),
		Fix:    "xcodebuild -downloadPlatform iOS"})
	return sdk, false
}

// androidSDK is $ANDROID_HOME, $ANDROID_SDK_ROOT or the Android Studio
// default.
func (r *run) androidSDK() string {
	for _, k := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if v := r.env.Getenv(k); v != "" {
			return v
		}
	}
	if home := r.env.Getenv("HOME"); home != "" {
		return filepath.Join(home, "Library", "Android", "sdk")
	}
	return ""
}

// latestBuildTools is the newest build-tools/<version> dir of the SDK.
func (r *run) latestBuildTools(sdk string) string {
	entries, err := r.env.ReadDir(filepath.Join(sdk, "build-tools"))
	if err != nil {
		return ""
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	if len(versions) == 0 {
		return ""
	}
	sort.Slice(versions, func(i, j int) bool { return versionLess(versions[i], versions[j]) })
	return filepath.Join(sdk, "build-tools", versions[len(versions)-1])
}

// versionLess compares dotted numeric versions (35.0.0 < 36.0.0 < 36.1.0).
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x < y
		}
	}
	return a < b
}

// tool finds a binary on PATH, then at the fallback path.
func (r *run) tool(name, fallback string) string {
	if p, err := r.env.LookPath(name); err == nil {
		return p
	}
	if fallback != "" {
		if _, err := r.env.Stat(fallback); err == nil {
			return fallback
		}
	}
	return ""
}

// adbPath and aapt2Path are what the device checks use.
func (r *run) adbPath() string {
	if r.env.Net.ADB != "" && r.env.Net.ADB != "adb" {
		return r.env.Net.ADB
	}
	if p := r.tool("adb", filepath.Join(r.androidSDK(), "platform-tools", "adb")); p != "" {
		r.env.Net.ADB = p
		return p
	}
	return ""
}

func (r *run) aapt2Path() string {
	return r.tool("aapt2", filepath.Join(r.latestBuildTools(r.androidSDK()), "aapt2"))
}

var javaVersionRe = regexp.MustCompile(`version "(\d+)(?:\.\d+)*[^"]*"`)

// parseJavaVersion reads `java -version` (stderr): the major and whether
// it is GraalVM.
func parseJavaVersion(out string) (major int, graal bool) {
	if m := javaVersionRe.FindStringSubmatch(out); m != nil {
		major, _ = strconv.Atoi(m[1])
		if major == 1 { // "1.8.0_x"
			major = 8
		}
	}
	return major, strings.Contains(out, "GraalVM")
}

func (r *run) androidTools() {
	sdk := r.androidSDK()
	r.data.Host.AndroidSDK = sdk
	var have []string
	if st := r.level("", Fail, Fail); st != "" {
		if p := r.adbPath(); p == "" {
			r.add(Check{ID: "tools-android", Status: st, Code: DiagToolMissing, Detail: "adb is not installed (not on PATH, not in " + filepath.Join(sdk, "platform-tools") + ")", Fix: "brew install --cask android-platform-tools"})
		} else {
			have = append(have, "adb")
		}
	}
	bt := r.latestBuildTools(sdk)
	r.data.Host.BuildTools = bt
	install := "sdkmanager 'build-tools;36.0.0'"
	if st := r.level(Fail, "", ""); st != "" {
		for _, t := range []string{"zipalign", "apksigner"} {
			if r.tool(t, filepath.Join(bt, t)) == "" {
				r.add(Check{ID: "tools-android", Status: st, Code: DiagToolMissing, Detail: t + " is not installed (no Android build-tools)", Fix: install})
			} else {
				have = append(have, t)
			}
		}
		r.jdk()
	}
	if st := r.level(Fail, Warn, Warn); st != "" {
		if r.aapt2Path() == "" {
			r.add(Check{ID: "tools-android", Status: st, Code: DiagToolMissing, Detail: "aapt2 is not installed, so the installed APK's profileable flag cannot be read", Fix: install})
		} else {
			have = append(have, "aapt2")
		}
	}
	if r.level("", "", Warn) != "" {
		if r.tool("trace_processor_shell", "") == "" && r.tool("trace_processor", "") == "" {
			r.add(Check{ID: "tools-android", Status: Warn, Code: DiagToolMissing, Detail: "trace_processor_shell is not installed (optional: only the analyze --sql drill-downs need it)",
				Fix: "curl -fsSL -o ~/.local/bin/trace_processor_shell https://get.perfetto.dev/trace_processor && chmod +x ~/.local/bin/trace_processor_shell"})
		} else {
			have = append(have, "trace_processor")
		}
	}
	if len(have) > 0 {
		r.add(Check{ID: "tools-android", Status: Pass, Detail: strings.Join(have, ", ")})
	}
}

// jdk: Gradle needs a JDK 21 (perflab pins JAVA_HOME to it); a GraalVM or
// another major first on PATH is only a warning.
func (r *run) jdk() {
	home := ""
	if res, err := r.exec(15*time.Second, "/usr/libexec/java_home", "-v", "21"); err == nil && res.Exit == 0 {
		home = strings.TrimSpace(string(res.Stdout))
	}
	if home == "" {
		if _, err := r.env.Stat("/opt/homebrew/opt/openjdk@21"); err == nil {
			home = "/opt/homebrew/opt/openjdk@21"
		}
	}
	if home == "" {
		r.add(Check{ID: "jdk", Status: r.level(Fail, "", ""), Code: DiagJDKWrong, Detail: "no JDK 21 for Gradle", Fix: "brew install openjdk@21"})
		return
	}
	r.data.Host.JavaHome21 = home
	res, err := r.exec(15*time.Second, "java", "-version")
	if err != nil {
		r.add(Check{ID: "jdk", Status: Pass, Detail: "JDK 21 at " + home + " (no java on PATH; builds set JAVA_HOME)"})
		return
	}
	major, graal := parseJavaVersion(res.Combined())
	if graal || major != 21 {
		what := fmt.Sprintf("Java %d", major)
		if graal {
			what = fmt.Sprintf("Oracle GraalVM %d", major)
		}
		r.add(Check{ID: "jdk", Status: Warn, Code: DiagJDKWrong, Detail: "the first java on PATH is " + what + "; perflab builds pin JAVA_HOME to " + home + ", a hand-run Gradle does not",
			Fix: "export JAVA_HOME=$(/usr/libexec/java_home -v 21)"})
		return
	}
	r.add(Check{ID: "jdk", Status: Pass, Detail: "JDK 21 at " + home})
}

func tail(res hostexec.Result, err error) string {
	if err != nil {
		return err.Error()
	}
	return res.Tail()
}

func (r *run) pipe() {
	if r.env.PipeCapacity == nil {
		return
	}
	runLevel := Warn
	if r.has("ios") {
		runLevel = Fail // WDA starts through xcodebuild test-without-building
	}
	st := r.level(Fail, runLevel, Warn)
	n, err := r.env.PipeCapacity()
	if err != nil {
		return // windows: no such hang
	}
	r.data.Host.PipeBytes = n
	if n < hostexec.PipeCapacityLow {
		r.add(Check{ID: "pipe-capacity", Status: st, Code: DiagPipeCapacityLow,
			Detail: fmt.Sprintf("a fresh kernel pipe buffers %d B (healthy is 16-64 KiB): the kernel's pipe memory is exhausted and xcodebuild hangs at \"Planning build\"", n),
			Fix:    "close idle Claude/Codex sessions, then " + r.rerun()})
		return
	}
	r.add(Check{ID: "pipe-capacity", Status: Pass, Detail: fmt.Sprintf("a fresh pipe buffers %d B", n)})
}

// parseLoad reads `sysctl -n hw.ncpu vm.loadavg`.
func parseLoad(out string) (ncpu int, load1 float64, err error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		return 0, 0, fmt.Errorf("sysctl: %d lines", len(lines))
	}
	if ncpu, err = strconv.Atoi(strings.TrimSpace(lines[0])); err != nil {
		return 0, 0, err
	}
	f := strings.Fields(strings.Trim(strings.TrimSpace(lines[1]), "{}"))
	if len(f) == 0 {
		return 0, 0, errors.New("sysctl: empty vm.loadavg")
	}
	load1, err = strconv.ParseFloat(f[0], 64)
	return ncpu, load1, err
}

func (r *run) loadRatio() (float64, bool) {
	if r.data.Host.NCPU == 0 {
		return 0, false
	}
	return r.data.Host.LoadPerCore, true
}

func (r *run) load() {
	st := r.level(Warn, Fail, Fail)
	res, err := r.exec(10*time.Second, "sysctl", "-n", "hw.ncpu", "vm.loadavg")
	if err != nil || res.Exit != 0 {
		return
	}
	ncpu, load1, perr := parseLoad(string(res.Stdout))
	if perr != nil || ncpu == 0 {
		return
	}
	per := load1 / float64(ncpu)
	r.data.Host.NCPU, r.data.Host.Load1, r.data.Host.LoadPerCore = ncpu, load1, round2(per)
	if per > loadPerCoreLimit {
		r.add(Check{ID: "host-load", Status: st, Code: DiagHostLoaded,
			Detail: fmt.Sprintf("1-min load %.1f on %d cores (%.1f per core): a measurement now reads the host, not the app, and builds crawl (load 98 starved WDA's start)", load1, ncpu, per),
			Fix:    fmt.Sprintf("wait until `sysctl -n vm.loadavg` reads under %d, then %s", ncpu, r.rerun())})
		return
	}
	r.add(Check{ID: "host-load", Status: Pass, Detail: fmt.Sprintf("1-min load %.1f on %d cores", load1, ncpu)})
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

func (r *run) buildLock() {
	if r.env.HostBuildHolder == nil {
		return
	}
	st := r.level(Warn, Fail, Fail)
	holder, err := r.env.HostBuildHolder()
	if err != nil {
		r.add(Check{ID: "host-build-lock", Status: Warn, Code: DiagHostBusyBuilding, Detail: "cannot read the host build lock: " + err.Error()})
		return
	}
	if holder != "" {
		r.add(Check{ID: "host-build-lock", Status: st, Code: DiagHostBusyBuilding, Detail: "a perflab native build holds the Mac-wide build lock (" + holder + ")", Fix: "perflab build list --json"})
		return
	}
	r.add(Check{ID: "host-build-lock", Status: Pass, Detail: "no perflab build running"})
}

func (r *run) disk() {
	if r.env.FreeBytes == nil || r.o.CacheDir == "" {
		return
	}
	st := r.level(Fail, Warn, Warn)
	dir := r.o.CacheDir
	for {
		if _, err := r.env.Stat(dir); err == nil || filepath.Dir(dir) == dir {
			break
		}
		dir = filepath.Dir(dir)
	}
	free, err := r.env.FreeBytes(dir)
	if err != nil {
		return
	}
	gb := float64(free) / (1 << 30)
	r.data.Host.DiskFreeGB, r.data.Host.CacheDir = round2(gb), r.o.CacheDir
	if gb < diskLowGB {
		r.add(Check{ID: "disk", Status: st, Code: DiagDiskLow, Detail: fmt.Sprintf("%.1f GB free on the volume holding %s (a native build needs %d+)", gb, r.o.CacheDir, diskLowGB), Fix: "perflab build gc --json"})
		return
	}
	r.add(Check{ID: "disk", Status: Pass, Detail: fmt.Sprintf("%.0f GB free for the perflab cache", gb)})
}

var drivers = map[string]string{"ios": "xcuitest", "android": "uiautomator2"}

func (r *run) appiumDrivers() {
	st := r.level("", Fail, "")
	if st == "" || r.o.AppiumHome == "" {
		return
	}
	for _, p := range r.platforms() {
		name := drivers[p]
		pkg := filepath.Join(r.o.AppiumHome, "node_modules", "appium-"+name+"-driver", "package.json")
		b, err := r.env.ReadFile(pkg)
		var v struct {
			Version string `json:"version"`
		}
		if err != nil || json.Unmarshal(b, &v) != nil || v.Version == "" {
			r.add(Check{ID: "appium-" + name, Status: st, Code: DiagAppiumDriverMissing, Detail: "no Appium " + name + " driver under " + r.o.AppiumHome, Fix: fmt.Sprintf("APPIUM_HOME=%s appium driver install %s", r.o.AppiumHome, name)})
			continue
		}
		r.add(Check{ID: "appium-" + name, Status: Pass, Detail: fmt.Sprintf("appium-%s-driver %s", name, v.Version)})
	}
}

func (r *run) wda() {
	st := r.level("", Fail, "")
	if st == "" || !r.has("ios") || r.o.WDA == nil {
		return
	}
	res, err := wda.Find(r.ctx, r.env.WDA, *r.o.WDA)
	if err == nil {
		d := res.Data.(wda.Data)
		r.data.Host.WDAKey = d.Entry.Key
		r.add(Check{ID: "wda", Status: Pass, Detail: fmt.Sprintf("prebuilt WebDriverAgent %s (Xcode %s, xcuitest %s) at %s", d.Entry.Key, d.Entry.Toolchain.XcodeBuild, d.Entry.Toolchain.DriverVersion, d.Entry.DerivedDataPath)})
		return
	}
	var de runx.DiagError
	if !errors.As(err, &de) {
		r.add(Check{ID: "wda", Status: st, Code: DiagWDAMissing, Detail: "cannot look up the prebuilt WDA: " + err.Error(), Fix: wda.BuildCommand(*r.o.WDA)})
		return
	}
	code := de.Diag.Code
	if code == wda.DiagUsage {
		code = DiagWDAMissing
	}
	for _, c := range r.data.Checks {
		if c.Code == code && code == DiagAppiumDriverMissing {
			return // the driver row already says it
		}
	}
	r.add(Check{ID: "wda", Status: st, Code: code, Detail: de.Diag.Detail, Fix: de.Diag.Fix})
}

// tunnelRegistry is the RemoteXPC registry's answer.
type tunnelRegistry struct {
	Status  string                 `json:"status"`
	Tunnels map[string]tunnelEntry `json:"tunnels"`
}

type tunnelEntry struct {
	UDID           string `json:"udid"`
	Address        string `json:"address"`
	RSDPort        int    `json:"rsdPort"`
	ConnectionType string `json:"connectionType"`
}

func parseTunnelRegistry(b []byte) (tunnelRegistry, error) {
	var reg tunnelRegistry
	err := json.Unmarshal(b, &reg)
	return reg, err
}

func osMajor(v string) int {
	n, _ := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	return n
}

// tunnelSudoLine is the human's fix: the root tunnel keeps running in its
// own terminal (sudo drops PATH and HOME matters: the registry port is
// stored under ~).
func (r *run) tunnelSudoLine() string {
	home := ""
	if r.o.AppiumHome != "" {
		home = " APPIUM_HOME=" + r.o.AppiumHome
	}
	return `sudo HOME="$HOME" env "PATH=$PATH"` + home + " appium driver run xcuitest tunnel-creation (a human terminal; leave it running)"
}

func (r *run) tunnelRegistry() {
	if !r.has("ios") || r.level("", Fail, Warn) == "" {
		return
	}
	d := r.o.Device
	// An iOS 17+ phone over the local network has no usbmux fallback; a
	// wired one (the iPhone 11 on iOS 18) runs without a tunnel.
	st := Warn
	if d != nil && osMajor(d.OS) >= 17 && d.Transport == "localNetwork" {
		st = r.level("", Fail, Warn)
	}
	code, body, err := r.env.HTTPGet(r.ctx, r.o.TunnelRegistry, 5*time.Second)
	reg, perr := parseTunnelRegistry(body)
	if err != nil || code != 200 || perr != nil || reg.Status != "OK" {
		why := fmt.Sprintf("answered %d", code)
		if err != nil {
			why = err.Error()
		}
		r.add(Check{ID: "tunnel-registry", Status: st, Code: DiagTunnelRegistryDown, Detail: "the RemoteXPC tunnel registry " + r.o.TunnelRegistry + " is not up (" + why + ")", Fix: r.tunnelSudoLine()})
		return
	}
	r.tun = &reg
	if d == nil || d.Platform != "ios" {
		r.add(Check{ID: "tunnel-registry", Status: Pass, Detail: fmt.Sprintf("tunnel registry up, %d tunnel(s)", len(reg.Tunnels))})
		return
	}
	if t, ok := reg.Tunnels[d.HardwareUDID]; ok && t.Address != "" && t.RSDPort > 0 {
		r.add(Check{ID: "tunnel-registry", Status: Pass, Detail: fmt.Sprintf("tunnel for %s at [%s]:%d (%s)", d.ID, t.Address, t.RSDPort, t.ConnectionType)})
		return
	}
	detail := fmt.Sprintf("the tunnel registry lists no tunnel for %s (%s)", d.ID, d.HardwareUDID)
	if st == Warn {
		// Runs and probes fall back to usbmux; go-ios screenshots do not
		// (a wired iPhone 11 on iOS 18 refused `device screencap`).
		detail += "; a wired phone runs and probes over usbmux, but `perflab device screencap` needs the tunnel"
	}
	r.add(Check{ID: "tunnel-registry", Status: st, Code: DiagTunnelMissing, Detail: detail, Fix: "restart the tunnel so it picks the phone up: " + r.tunnelSudoLine()})
}

var (
	originRe = regexp.MustCompile(`https?://[^\s"'<>]+`)
	parkedRe = regexp.MustCompile(`WS_NOT_RUNNING|WS_PARKED|\bparked\b`)
)

// parseOriginOutput reads the adapter's origin command output (devbox url,
// text or --json): the first URL, or the parked/stopped verdict with the
// command's own fix.
func parseOriginOutput(out string) (origin, parked, fix string) {
	var env runx.Envelope
	if json.Unmarshal([]byte(out), &env) == nil && env.V > 0 {
		for _, d := range env.Diagnostics {
			if parkedRe.MatchString(d.Code + " " + d.Detail) {
				return "", d.Code + ": " + d.Detail, d.Fix
			}
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if !parkedRe.MatchString(line) {
			continue
		}
		line = strings.TrimSpace(line)
		for _, sep := range []string{" \u2014 ", " - "} {
			if i := strings.LastIndex(line, sep); i >= 0 {
				return "", strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+len(sep):])
			}
		}
		return "", line, ""
	}
	if m := originRe.FindString(out); m != "" {
		return strings.TrimRight(m, "/.,;"), "", ""
	}
	return "", "", ""
}

func (r *run) resolveOrigin() (string, bool) {
	a := r.o.API
	if a.Origin != "" {
		return a.Origin, true
	}
	if a.OriginCmd == "" {
		return "", false
	}
	st := r.level("", Fail, "")
	res, err := r.exec(60*time.Second, "/bin/sh", "-c", a.OriginCmd)
	out := res.Combined()
	origin, parked, ownFix := parseOriginOutput(out)
	if parked != "" {
		fix := ownFix
		if a.Hold != "" {
			if fix != "" {
				fix += " && "
			}
			fix += a.Hold
		}
		r.add(Check{ID: "api", Status: st, Code: DiagDevboxParked, Detail: "`" + a.OriginCmd + "` says the API's workspace is parked or stopped: " + parked, Fix: fix})
		return "", false
	}
	if err != nil || origin == "" {
		r.add(Check{ID: "api", Status: st, Code: DiagAdapterCommandFailed, Detail: "the adapter's api.origin command `" + a.OriginCmd + "` gave no URL: " + tail(res, err), Fix: a.OriginCmd})
		return "", false
	}
	return origin, true
}

func (r *run) api() {
	st := r.level("", Fail, "")
	if st == "" || r.o.API == nil {
		return
	}
	origin, ok := r.resolveOrigin()
	if !ok {
		return
	}
	r.data.Host.APIOrigin = netfwd.Redact(origin)
	r.o.API.Origin = origin
	url := strings.TrimRight(origin, "/") + r.o.API.Health
	start := r.env.Now()
	code, _, err := r.env.HTTPGet(r.ctx, url, netfwd.HealthTimeout)
	if err != nil || code < 200 || code >= 300 {
		fix := r.o.API.Hold
		if fix == "" {
			fix = "curl -sS -o /dev/null -w '%{http_code}' " + netfwd.Redact(url)
		}
		r.add(Check{ID: "api", Status: st, Code: DiagAPIUnreachable, Detail: fmt.Sprintf("GET %s from the Mac answered %s within %s", netfwd.Redact(url), netfwd.Answer(code, err), netfwd.HealthTimeout), Fix: fix})
		return
	}
	r.add(Check{ID: "api", Status: Pass, Detail: fmt.Sprintf("GET %s answered %d in %d ms", netfwd.Redact(url), code, r.env.Now().Sub(start).Milliseconds())})
}
