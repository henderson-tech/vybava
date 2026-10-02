package doctor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/perflab/netfwd"
	"github.com/henderson-tech/vybava/internal/perflab/wda"
	"github.com/henderson-tech/vybava/internal/runx"
)

// lab is a recorded Mac: every command doctor runs answers from the
// fixtures captured on 2026-10-01/02 (iPhone 11 on iOS 18.7.8 wired, the
// S20 on Android 13), unless a test overrides it.
type lab struct {
	t        *testing.T
	mu       sync.Mutex
	calls    []string
	handlers map[string]func(c hostexec.Cmd) (hostexec.Result, error)
}

func (l *lab) Run(_ context.Context, c hostexec.Cmd) (hostexec.Result, error) {
	l.mu.Lock()
	joined := strings.Join(c.Argv, " ")
	l.calls = append(l.calls, joined)
	l.mu.Unlock()
	key := filepath.Base(c.Argv[0])
	if len(c.Argv) > 1 {
		key += " " + strings.Join(c.Argv[1:], " ")
	}
	best := ""
	for prefix := range l.handlers {
		if strings.HasPrefix(key, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		l.t.Errorf("unexpected command: %s", joined)
		return hostexec.Result{Exit: 127}, nil
	}
	return l.handlers[best](c)
}

func (l *lab) ran(prefix string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.ContainsFunc(l.calls, func(c string) bool { return strings.Contains(c, prefix) })
}

func out(s string) func(hostexec.Cmd) (hostexec.Result, error) {
	return func(hostexec.Cmd) (hostexec.Result, error) { return hostexec.Result{Stdout: []byte(s)}, nil }
}

func exit(code int, s string) func(hostexec.Cmd) (hostexec.Result, error) {
	return func(hostexec.Cmd) (hostexec.Result, error) {
		return hostexec.Result{Exit: code, Stderr: []byte(s)}, nil
	}
}

// jsonOutput answers a devicectl call by writing the fixture where
// --json-output points (devicectl's only machine-readable output).
func jsonOutput(t *testing.T, name string) func(hostexec.Cmd) (hostexec.Result, error) {
	return func(c hostexec.Cmd) (hostexec.Result, error) {
		for i, a := range c.Argv {
			if a == "--json-output" {
				if err := os.WriteFile(c.Argv[i+1], []byte(fixture(t, name)), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}
		return hostexec.Result{}, nil
	}
}

type httpAnswer struct {
	code int
	body string
	err  error
}

// world is one test's Mac: a temp SDK, Appium home and cache, plus the
// recorded commands and HTTP answers.
type world struct {
	lab   *lab
	env   Env
	http  map[string]httpAnswer
	home  string // Appium home
	sdk   string
	cache string
	probe []runx.Diagnostic
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{http: map[string]httpAnswer{}, home: t.TempDir(), sdk: t.TempDir(), cache: t.TempDir()}
	for _, d := range []string{"appium-xcuitest-driver", "appium-uiautomator2-driver"} {
		dir := filepath.Join(w.home, "node_modules", d)
		mustDo(t, os.MkdirAll(dir, 0o755))
		mustDo(t, os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"11.11.2"}`), 0o644))
	}
	wdaDir := filepath.Join(w.home, "node_modules", "appium-xcuitest-driver", "node_modules", "appium-webdriveragent")
	mustDo(t, os.MkdirAll(filepath.Join(wdaDir, "WebDriverAgent.xcodeproj"), 0o755))
	mustDo(t, os.WriteFile(filepath.Join(wdaDir, "package.json"), []byte(`{"version":"14.1.1"}`), 0o644))
	for _, v := range []string{"35.0.0", "36.0.0"} {
		mustDo(t, os.MkdirAll(filepath.Join(w.sdk, "build-tools", v), 0o755))
	}
	mustDo(t, os.WriteFile(filepath.Join(w.sdk, "build-tools", "36.0.0", "aapt2"), nil, 0o755))
	w.lab = &lab{t: t, handlers: map[string]func(hostexec.Cmd) (hostexec.Result, error){
		"xcodebuild -version":              out("Xcode 26.5\nBuild version 17F42\n"),
		"xcodebuild -showsdks -json":       out(fixture(t, "xcodebuild-showsdks-ios.json")),
		"xcrun simctl list runtimes -j":    out(fixture(t, "simctl-runtimes-ios26-5.json")),
		"xcrun --find":                     out("/Applications/Xcode.app/Contents/Developer/usr/bin/x\n"),
		"sysctl -n hw.ncpu vm.loadavg":     out("14\n{ 4.02 5.10 6.00 }\n"),
		"xcrun xctrace list devices":       out(fixture(t, "xctrace-list-iphone11-offline.txt")),
		"xcrun devicectl device info apps": jsonOutput(t, "devicectl-apps-iphone11.json"),
		"java_home -v 21":                  out("/opt/homebrew/Cellar/openjdk@21/21.0.10/libexec/openjdk.jdk/Contents/Home\n"),
		"java -version":                    exit(0, fixture(t, "java-version-graalvm21.txt")),
		"adb -s RF8N21PY1BF shell dumpsys package app.fixit.client": out(fixture(t, "dumpsys-package-perf-release.txt")),
		"adb -s RF8N21PY1BF shell pm path":                          out("package:/data/app/~~0A0iAOuiHkSi_S8WQ9uXDg==/app.fixit.client-2FFA4BiiX_Wb0nQX1yxElg==/base.apk\n"),
		"adb -s RF8N21PY1BF pull":                                   out("1 file pulled\n"),
		"aapt2 dump xmltree":                                        out(fixture(t, "aapt2-manifest-profileable.txt")),
		"adb -s RF8N21PY1BF shell dumpsys window":                   out(fixture(t, "dumpsys-window-resolver-focus.txt")),
		"adb -s RF8N21PY1BF reverse --list":                         out(""),
		"lsof":                                                      exit(1, ""),
	}}
	w.env = Env{
		Run: w.lab,
		LookPath: func(name string) (string, error) {
			if slices.Contains([]string{"xcrun", "adb"}, name) {
				return "/usr/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		},
		Getenv:  func(k string) string { return map[string]string{"ANDROID_HOME": w.sdk, "HOME": "/Users/x"}[k] },
		HTTPGet: w.get,
		Now:     time.Now, Sleep: func(time.Duration) {}, TempDir: t.TempDir,
		ReadFile: os.ReadFile, ReadDir: os.ReadDir,
		Stat: func(p string) (fs.FileInfo, error) {
			if p == "/opt/homebrew/opt/openjdk@21" {
				return nil, nil
			}
			return os.Stat(p)
		},
		PipeCapacity:    func() (int, error) { return 65536, nil },
		FreeBytes:       func(string) (uint64, error) { return 200 << 30, nil },
		HostBuildHolder: func() (string, error) { return "", nil },
		ProbeDevice:     func(context.Context, Device) ([]runx.Diagnostic, error) { return w.probe, nil },
		WDA:             wda.Env{Run: w.lab, Now: time.Now},
		Net: netfwd.Env{Run: w.lab, ADB: "adb", StateDir: t.TempDir(), Now: time.Now, Sleep: func(time.Duration) {},
			HTTPGet: func(ctx context.Context, url string, timeout time.Duration) (int, error) {
				code, _, err := w.get(ctx, url, timeout)
				return code, err
			}},
	}
	w.http[DefaultTunnelRegistry] = httpAnswer{200, fixture(t, "tunnels-empty.json"), nil}
	return w
}

func (w *world) get(_ context.Context, url string, _ time.Duration) (int, []byte, error) {
	a, ok := w.http[url]
	if !ok {
		return 0, nil, fmt.Errorf("dial tcp: connection refused (%s)", url)
	}
	return a.code, []byte(a.body), a.err
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

var iphone11 = Device{ID: "iphone11", Platform: "ios", HardwareUDID: iphone11UDID, CoreDeviceID: "F8C49F38-CB34-5B6D-9D34-07F194198EEA", OS: "18.7.8", Transport: "wired", ExpectHz: 60}

var s20 = Device{ID: "s20", Platform: "android", Serial: "RF8N21PY1BF", OS: "13", ExpectHz: 120}

func check(t *testing.T, res Result, id string) Check {
	t.Helper()
	var found []Check
	for _, c := range res.Data.Checks {
		if c.ID == id {
			found = append(found, c)
		}
	}
	if len(found) == 0 {
		t.Fatalf("no %s check in %+v", id, res.Data.Checks)
	}
	for _, c := range found {
		if c.Status != Pass {
			return c
		}
	}
	return found[0]
}

func codes(res Result) []string {
	var out []string
	for _, d := range res.Diagnostics {
		out = append(out, d.Severity+":"+d.Code)
	}
	return out
}

func TestHostForBuildOnAnExhaustedMac(t *testing.T) {
	w := newWorld(t)
	w.env.PipeCapacity = func() (int, error) { return 512, nil } // the 4 h "Planning build" hang
	w.lab.handlers["sysctl -n hw.ncpu vm.loadavg"] = out("14\n{ 98.00 90.00 80.00 }\n")
	w.env.HostBuildHolder = func() (string, error) { return "pid 41234, `perflab build native`, key pf1-3c9a", nil }
	res, err := Run(context.Background(), w.env, Options{For: ForBuild, Platform: "ios", CacheDir: w.cache, Then: "perflab build native --platform ios --profile perf --json"})
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("an exhausted pipe must fail a build preflight")
	}
	want := []string{"error:" + DiagPipeCapacityLow, "warning:" + DiagHostLoaded, "warning:" + DiagHostBusyBuilding}
	if got := codes(res); !slices.Equal(got, want) {
		t.Fatalf("diagnostics %v, want %v", got, want)
	}
	if !strings.HasPrefix(res.Next[0], "close idle Claude/Codex sessions") || res.Next[len(res.Next)-1] != "perflab build native --platform ios --profile perf --json" {
		t.Fatalf("next %v", res.Next)
	}
	if res.Data.Host.PipeBytes != 512 || res.Data.Host.XcodeBuild != "17F42" || res.Data.Host.NCPU != 14 {
		t.Fatalf("host facts %+v", res.Data.Host)
	}
	if w.lab.ran("xctrace") || w.lab.ran("devicectl") {
		t.Fatal("a build preflight touched device tooling")
	}
}

func TestIOSPlatformAfterAnXcodeUpdate(t *testing.T) {
	for name, tc := range map[string]struct {
		runtimes string
		fail     bool
	}{
		"26.5 platform downloaded (2026-09-30)":     {fixture(t, "simctl-runtimes-ios26-5.json"), false},
		"17F42 update, no iOS runtime (2026-10-02)": {fixture(t, "simctl-runtimes-none.json"), true},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.lab.handlers["xcrun simctl list runtimes -j"] = out(tc.runtimes)
			res, err := Run(context.Background(), w.env, Options{For: ForBuild, Platform: "ios"})
			if err != nil {
				t.Fatal(err)
			}
			failed := slices.ContainsFunc(res.Data.Checks, func(c Check) bool {
				return c.ID == "ios-platform" && c.Status == Fail && c.Code == DiagToolMissing && c.Fix == "xcodebuild -downloadPlatform iOS"
			})
			if failed != tc.fail {
				t.Fatalf("platform failed=%v, want %v: %+v", failed, tc.fail, res.Data.Checks)
			}
			if !tc.fail && !strings.Contains(check(t, res, "tools-ios").Detail, "iOS 26.5 platform") {
				t.Fatalf("tools row %+v", check(t, res, "tools-ios"))
			}
		})
	}
}

func TestIPhone11RunPreflight(t *testing.T) {
	w := newWorld(t)
	logPath := filepath.Join(t.TempDir(), "appium-server.log")
	mustDo(t, os.WriteFile(logPath, []byte(fixture(t, "appium-ui-automation-timeout.log")), 0o644))
	w.lab.handlers["sh -c devbox url fixit-work-x api"] = func(hostexec.Cmd) (hostexec.Result, error) {
		return hostexec.Result{Exit: 2, Stderr: []byte(strings.Replace(fixture(t, "devbox-url-parked.txt"), " - devbox up", " \u2014 devbox up", 1))}, nil
	}
	w.probe = []runx.Diagnostic{
		{Code: "TUNNEL_MISSING", Severity: "warning", Detail: "devlab's own tunnel row"},
		{Code: "DEVICE_OFFLINE", Severity: "warning", Detail: "iphone11: xctrace lists it under Devices Offline, so Instruments cannot attach"},
		{Code: "HUMAN_CHECK", Severity: "info", Detail: "Auto-Lock: Settings > Display & Brightness > Auto-Lock > Never"},
	}
	o := Options{
		For: ForRun, Device: &iphone11, Lease: "tok-1", App: App{BundleID: "app.fixit.client"},
		AppiumHome: w.home, AppiumServerLog: logPath, CacheDir: w.cache,
		WDA: &wda.Spec{AppiumHome: w.home, Team: "YJ77YV2PNA", BundleID: "app.fixit.WebDriverAgentRunner", CacheDir: w.cache},
		API: &API{OriginCmd: "devbox url fixit-work-x api", Health: "/api/v1/health/ready", Hold: "devbox hold fixit-work-x --for 4h"},
	}
	res, err := Run(context.Background(), w.env, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("this lab state must fail a run")
	}
	if c := check(t, res, "wda"); c.Code != DiagWDAMissing || c.Fix != "perflab wda build --team YJ77YV2PNA --bundle-id app.fixit.WebDriverAgentRunner --json" {
		t.Fatalf("wda %+v", c)
	}
	if c := check(t, res, "tunnel-registry"); c.Status != Warn || c.Code != DiagTunnelMissing || !strings.Contains(c.Fix, "APPIUM_HOME="+w.home) {
		t.Fatalf("a wired iOS 18 phone without a tunnel only warns: %+v", c)
	}
	if c := check(t, res, "api"); c.Code != DiagDevboxParked || c.Fix != "devbox up fixit-work-marketplace-ui-vt-4229 --box a && devbox hold fixit-work-x --for 4h" {
		t.Fatalf("api %+v", c)
	}
	if c := check(t, res, "ios-online"); c.Code != DiagDeviceOffline || c.Fix != "perflab doctor --device iphone11 --lease tok-1 --for run --wake --json" {
		t.Fatalf("offline %+v", c)
	}
	if c := check(t, res, "ios-app"); c.Status != Pass || res.Data.Device.AppVersion != "2.6.0" || res.Data.Device.BundleVer != "1" {
		t.Fatalf("app %+v %+v", c, res.Data.Device)
	}
	if c := check(t, res, "ui-automation"); c.Status != Fail || c.Code != DiagUIAutomationOff {
		t.Fatalf("ui automation at load 0.3 per core: %+v", c)
	}
	// devlab's tunnel row is dropped (doctor already decided it); its human
	// checklist row passes through as info.
	tunnelRows := 0
	for _, d := range res.Diagnostics {
		if d.Code == DiagTunnelMissing {
			tunnelRows++
		}
	}
	offlineRows := 0
	for _, d := range res.Diagnostics {
		if d.Code == DiagDeviceOffline {
			offlineRows++
		}
	}
	if offlineRows != 1 {
		t.Fatalf("devlab's xctrace-offline row must fold into doctor's: %v", res.Diagnostics)
	}
	if tunnelRows != 1 || !slices.ContainsFunc(res.Diagnostics, func(d runx.Diagnostic) bool {
		return d.Code == DiagHumanCheck && strings.Contains(d.Detail, "Auto-Lock")
	}) {
		t.Fatalf("probe rows not folded: %v", res.Diagnostics)
	}
	// Fixes in dependency order: host (WDA, tunnel, API) before device.
	idx := func(prefix string) int {
		return slices.IndexFunc(res.Next, func(n string) bool { return strings.HasPrefix(n, prefix) })
	}
	if !(idx("perflab wda build") < idx("restart the tunnel") && idx("restart the tunnel") < idx("devbox up") && idx("devbox up") < idx("perflab doctor --device iphone11") && idx("perflab doctor --device") < idx("on the iPhone")) {
		t.Fatalf("next out of order: %v", res.Next)
	}
}

func TestWakeBringsTheIPhoneOnline(t *testing.T) {
	w := newWorld(t)
	polls := 0
	w.lab.handlers["xcrun xctrace list devices"] = func(hostexec.Cmd) (hostexec.Result, error) {
		polls++
		if polls < 3 {
			return hostexec.Result{Stdout: []byte(fixture(t, "xctrace-list-iphone11-offline.txt"))}, nil
		}
		online := strings.Replace(fixture(t, "xctrace-list-iphone11-offline.txt"), "== Devices ==\n", "== Devices ==\nLukáš - iPhone (18.7.8) ("+iphone11UDID+")\n", 1)
		return hostexec.Result{Stdout: []byte(online)}, nil
	}
	w.lab.handlers["xcrun devicectl device process launch"] = out("Launched application with app.fixit.client bundle identifier.\n")
	res, err := Run(context.Background(), w.env, Options{For: ForProbe, Device: &iphone11, Lease: "tok-1", Wake: true, App: App{BundleID: "app.fixit.client"}})
	if err != nil {
		t.Fatal(err)
	}
	if c := check(t, res, "ios-online"); c.Status != Pass || !res.Data.Device.Woken || !strings.Contains(c.Detail, "woken by launching app.fixit.client") {
		t.Fatalf("wake: %+v %+v", c, res.Data.Device)
	}
	if !w.lab.ran("devicectl device process launch --device F8C49F38-CB34-5B6D-9D34-07F194198EEA --terminate-existing app.fixit.client") {
		t.Fatal("wake did not launch the app through devicectl's CoreDevice id")
	}
	if _, err := Run(context.Background(), w.env, Options{For: ForProbe, Device: &iphone11, Wake: true}); err == nil {
		t.Fatal("--wake without a lease must be USAGE")
	}
}

func TestTunnelSeverityFollowsTransport(t *testing.T) {
	air := Device{ID: "air", Platform: "ios", HardwareUDID: airUDID, CoreDeviceID: "55657883-7DF0-5900-B528-7E647BD21477", OS: "26.6.2", Transport: "localNetwork", ExpectHz: 120}
	cases := []struct {
		name     string
		device   Device
		registry httpAnswer
		status   Status
		code     string
	}{
		{"air over Wi-Fi, registry has it", air, httpAnswer{200, fixture(t, "tunnels-air-iphone11.json"), nil}, Pass, ""},
		{"air over Wi-Fi, registry without it", air, httpAnswer{200, fixture(t, "tunnels-empty.json"), nil}, Fail, DiagTunnelMissing},
		{"air over Wi-Fi, registry down", air, httpAnswer{0, "", errors.New("connection refused")}, Fail, DiagTunnelRegistryDown},
		{"wired iPhone 11, registry down", iphone11, httpAnswer{0, "", errors.New("connection refused")}, Warn, DiagTunnelRegistryDown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			w.http[DefaultTunnelRegistry] = tc.registry
			d := tc.device
			res, err := Run(context.Background(), w.env, Options{For: ForRun, Device: &d, AppiumHome: w.home})
			if err != nil {
				t.Fatal(err)
			}
			c := check(t, res, "tunnel-registry")
			if c.Status != tc.status || c.Code != tc.code {
				t.Fatalf("got %+v", c)
			}
			if tc.code == DiagTunnelRegistryDown && c.Fix != `sudo HOME="$HOME" env "PATH=$PATH" APPIUM_HOME=`+w.home+` appium driver run xcuitest tunnel-creation (a human terminal; leave it running)` {
				t.Fatalf("fix %q", c.Fix)
			}
		})
	}
}

func TestS20Preflights(t *testing.T) {
	cases := []struct {
		name     string
		purpose  Purpose
		pkg      string
		override map[string]func(hostexec.Cmd) (hostexec.Result, error)
		id       string
		status   Status
		code     string
		fix      string
	}{
		{name: "perf release is profileable", purpose: ForRun, pkg: "app.fixit.client", id: "android-app", status: Pass},
		{name: "the dev package is not profileable", purpose: ForRun, pkg: "app.fixit.client",
			override: map[string]func(hostexec.Cmd) (hostexec.Result, error){"aapt2 dump xmltree": out(fixture(t, "aapt2-manifest-not-profileable.txt"))},
			id:       "android-app", status: Fail, code: DiagNotProfileable, fix: "perflab build native --platform android --profile perf --json"},
		{name: "a debuggable build", purpose: ForProbe, pkg: "io.appium.settings",
			override: map[string]func(hostexec.Cmd) (hostexec.Result, error){"adb -s RF8N21PY1BF shell dumpsys package io.appium.settings": out(fixture(t, "dumpsys-package-debuggable.txt"))},
			id:       "android-app", status: Fail, code: DiagDebuggableBuild},
		{name: "not installed", purpose: ForRun, pkg: "app.fixit.client.no.such.pkg",
			override: map[string]func(hostexec.Cmd) (hostexec.Result, error){"adb -s RF8N21PY1BF shell dumpsys package app.fixit.client.no.such.pkg": out(fixture(t, "dumpsys-package-missing.txt"))},
			id:       "android-app", status: Fail, code: DiagAppNotInstalled, fix: "perflab install <variantId> --device s20 --lease tok-2"},
		{name: "a chooser dialog has the focus (probe)", purpose: ForProbe, pkg: "app.fixit.client", id: "input-focus", status: Fail, code: DiagInputFocusWrong, fix: "perflab app launch --device s20 --lease tok-2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			for k, v := range tc.override {
				w.lab.handlers[k] = v
			}
			res, err := Run(context.Background(), w.env, Options{For: tc.purpose, Device: &s20, Lease: "tok-2", App: App{Package: tc.pkg}})
			if err != nil {
				t.Fatal(err)
			}
			c := check(t, res, tc.id)
			if c.Status != tc.status || c.Code != tc.code || !strings.HasPrefix(c.Fix, tc.fix) {
				t.Fatalf("got %+v", c)
			}
		})
	}
}

func TestS20ForwardCheck(t *testing.T) {
	w := newWorld(t)
	o := Options{For: ForRun, Device: &s20, Lease: "tok-2", App: App{Package: "app.fixit.client"},
		API: &API{Origin: "http://10.8.0.10:21936", Health: "/api/v1/health/ready", DevicePort: 23936}}
	w.http["http://10.8.0.10:21936/api/v1/health/ready"] = httpAnswer{200, "{}", nil}
	res, err := Run(context.Background(), w.env, o)
	if err != nil {
		t.Fatal(err)
	}
	if c := check(t, res, "api"); c.Status != Pass {
		t.Fatalf("api %+v", c)
	}
	c := check(t, res, "api-forward")
	if c.Code != netfwd.DiagForwardDown || c.Fix != "perflab net forward --device s20 --lease tok-2 --device-port 23936" {
		t.Fatalf("forward %+v", c)
	}
	// run's own preflight: run starts (or reuses) the forward after it, so
	// a missing forward never refuses the run (the 2026-10-02 S20 run did).
	starts := o
	starts.StartsForward = true
	res, err = Run(context.Background(), w.env, starts)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range res.Data.Checks {
		if c.ID == "api-forward" {
			t.Fatalf("run's preflight checked the forward it starts itself: %+v", c)
		}
	}
	// With the reverse and a healthy perflab forward the chain passes.
	w.lab.handlers["adb -s RF8N21PY1BF reverse --list"] = out("UsbFfs tcp:23936 tcp:23936\n")
	w.lab.handlers["lsof"] = out("4242\n")
	w.lab.handlers["adb -s RF8N21PY1BF shell (printf"] = out("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n\r\n{}")
	mustDo(t, os.MkdirAll(filepath.Join(w.env.Net.StateDir, "net"), 0o755))
	mustDo(t, os.WriteFile(filepath.Join(w.env.Net.StateDir, "net", "s20-23936.json"), []byte(`{"pid":4242,"deviceId":"s20","devicePort":23936,"hostPort":23936,"origin":"http://10.8.0.10:21936"}`), 0o644))
	w.http["http://127.0.0.1:23936/api/v1/health/ready"] = httpAnswer{200, "{}", nil}
	o.API.Origin = "http://10.8.0.10:21936"
	res, err = Run(context.Background(), w.env, o)
	if err != nil {
		t.Fatal(err)
	}
	if c := check(t, res, "api-forward"); c.Status != Pass {
		t.Fatalf("healthy chain: %+v", c)
	}
}

func TestUsageAndWiring(t *testing.T) {
	w := newWorld(t)
	for name, o := range map[string]Options{
		"unknown purpose":  {For: "deploy"},
		"unknown platform": {For: ForRun, Platform: "web"},
		"wake, no lease":   {For: ForRun, Device: &iphone11, Wake: true},
	} {
		var d runx.DiagError
		if _, err := Run(context.Background(), w.env, o); !errors.As(err, &d) || d.Diag.Code != DiagUsage {
			t.Errorf("%s: want USAGE, got %v", name, err)
		}
	}
	w.env.ProbeDevice = nil
	if _, err := Run(context.Background(), w.env, Options{For: ForRun, Device: &iphone11}); err == nil {
		t.Fatal("a device run without the devlab probe wired must be an error")
	}
}
