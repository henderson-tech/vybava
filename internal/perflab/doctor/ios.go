package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
)

// WakeTimeout is how long --wake polls Instruments after launching the app.
const WakeTimeout = 120 * time.Second

const wakePoll = 5 * time.Second

func (r *run) hasCode(code string) bool {
	for _, c := range r.data.Checks {
		if c.Code == code {
			return true
		}
	}
	return false
}

func (r *run) installFix() string {
	return fmt.Sprintf("perflab install <variantId> --device %s --lease %s", r.deviceID(), r.lease())
}

func (r *run) iosDevice(prog *hostexec.Progress) {
	if r.level("", Fail, Fail) == "" {
		return
	}
	d := r.o.Device
	r.data.Device = &DeviceFacts{ID: d.ID, Platform: "ios", OS: d.OS}
	r.iosTooling()
	r.iosOnline(prog)
	r.iosApp()
	r.uiAutomation()
	r.iosAPIHuman()
}

func (r *run) iosTooling() {
	d := r.o.Device
	if major := osMajor(d.OS); major == 0 || major >= 17 || r.hasCode(DiagDeviceToolingUnsupported) {
		return
	}
	r.add(Check{ID: "ios-tooling", Status: r.level("", Fail, Fail), Code: DiagDeviceToolingUnsupported,
		Detail: fmt.Sprintf("%s runs iOS %s: below 17 CoreDevice cannot list the app's processes, which launch, attach and crash collection need (the xctrace --launch fallback is unverified)", d.ID, d.OS),
		Fix:    "measure on an iOS 17+ phone (perflab device list --json)"})
}

var udidTail = regexp.MustCompile(`\(([0-9A-Fa-f-]{20,})\)\s*$`)

// parseXctraceDevices reads `xcrun xctrace list devices`: the hardware
// UDIDs under "== Devices ==" (online) and "== Devices Offline ==" (the
// phone's screen sleeps, or it is unplugged but remembered).
func parseXctraceDevices(out string) (online, offline map[string]bool) {
	online, offline = map[string]bool{}, map[string]bool{}
	var section map[string]bool
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "== Devices ==":
			section = online
			continue
		case line == "== Devices Offline ==":
			section = offline
			continue
		case strings.HasPrefix(line, "=="):
			section = nil
			continue
		}
		if section == nil {
			continue
		}
		if m := udidTail.FindStringSubmatch(line); m != nil {
			section[m[1]] = true
		}
	}
	return online, offline
}

func (r *run) xctraceOnline() (online, listed bool, err error) {
	res, err := r.exec(60*time.Second, "xcrun", "xctrace", "list", "devices")
	if err != nil {
		return false, false, err
	}
	if res.Exit != 0 {
		return false, false, fmt.Errorf("xctrace list devices: %s", res.Tail())
	}
	on, off := parseXctraceDevices(string(res.Stdout))
	udid := r.o.Device.HardwareUDID
	return on[udid], on[udid] || off[udid], nil
}

func (r *run) iosOnline(prog *hostexec.Progress) {
	d := r.o.Device
	st := r.level("", Fail, Fail)
	online, listed, err := r.xctraceOnline()
	if err != nil {
		r.add(Check{ID: "ios-online", Status: st, Code: DiagDeviceOffline, Detail: "cannot ask Instruments for its devices: " + err.Error(), Fix: "sudo xcode-select -s /Applications/Xcode.app"})
		return
	}
	facts := r.data.Device
	if online {
		facts.Online = ptr(true)
		r.add(Check{ID: "ios-online", Status: Pass, Detail: "Instruments lists " + d.ID + " online"})
		return
	}
	facts.Online = ptr(false)
	if r.o.Wake && r.o.App.BundleID != "" && d.CoreDeviceID != "" {
		prog.Phase("wake", "bundle="+r.o.App.BundleID)
		if r.wake(prog) {
			return
		}
		if r.hasCode(DiagAppNotInstalled) {
			return
		}
		r.add(Check{ID: "ios-online", Status: st, Code: DiagDeviceOffline,
			Detail: fmt.Sprintf("%s is still offline for Instruments %s after launching %s: the phone is locked or its screen is off", d.ID, WakeTimeout, r.o.App.BundleID),
			Fix:    "unlock the phone and set Auto-Lock to Never (human), then " + r.rerun("--wake")})
		return
	}
	detail := "Instruments lists " + d.ID + " under Devices Offline: its screen sleeps, so xctrace cannot attach"
	if !listed {
		detail = "Instruments does not list " + d.ID + " (" + d.HardwareUDID + ") at all: unplugged, or not paired with this Mac"
	}
	fix := r.rerun("--wake")
	if r.o.Lease == "" {
		fix = "perflab lease acquire " + d.ID + " --json, then perflab doctor --device " + d.ID + " --lease <token> --for " + string(r.o.For) + " --wake --json"
	}
	r.add(Check{ID: "ios-online", Status: st, Code: DiagDeviceOffline, Detail: detail, Fix: fix})
}

// wake launches the app (devicectl brings the screen up) and polls
// Instruments until the phone is online or WakeTimeout passes.
func (r *run) wake(prog *hostexec.Progress) bool {
	d := r.o.Device
	res, err := r.exec(60*time.Second, "xcrun", "devicectl", "device", "process", "launch", "--device", d.CoreDeviceID, "--terminate-existing", r.o.App.BundleID)
	if err == nil && res.Exit != 0 && strings.Contains(res.Combined(), "is not installed") {
		r.add(Check{ID: "ios-app", Status: r.level("", Fail, Fail), Code: DiagAppNotInstalled, Detail: r.o.App.BundleID + " is not installed on " + d.ID + ", so --wake has nothing to launch", Fix: r.installFix()})
		return false
	}
	start := r.env.Now()
	for r.env.Now().Sub(start) < WakeTimeout {
		r.env.Sleep(wakePoll)
		online, _, err := r.xctraceOnline()
		if err == nil && online {
			after := r.env.Now().Sub(start).Round(time.Second)
			r.data.Device.Online, r.data.Device.Woken = ptr(true), true
			r.add(Check{ID: "ios-online", Status: Pass, Detail: fmt.Sprintf("woken by launching %s; Instruments lists %s online after %s", r.o.App.BundleID, d.ID, after)})
			return true
		}
		prog.Still("waiting=instruments")
	}
	return false
}

// devicectlApps is `devicectl device info apps --json-output`'s result.
type devicectlApps struct {
	Info struct {
		Outcome string `json:"outcome"`
	} `json:"info"`
	Result struct {
		Apps []struct {
			BundleIdentifier string `json:"bundleIdentifier"`
			BundleVersion    string `json:"bundleVersion"`
			Version          string `json:"version"`
			Name             string `json:"name"`
		} `json:"apps"`
	} `json:"result"`
}

func (r *run) iosApp() {
	d := r.o.Device
	bundle := r.o.App.BundleID
	if bundle == "" || d.CoreDeviceID == "" || r.hasCode(DiagAppNotInstalled) {
		return
	}
	st := r.level("", Fail, Fail)
	path := r.tmpPath("apps.json")
	defer os.Remove(path)
	res, err := r.exec(60*time.Second, "xcrun", "devicectl", "device", "info", "apps", "--device", d.CoreDeviceID, "--bundle-id", bundle, "--json-output", path)
	b, rerr := r.env.ReadFile(path)
	var apps devicectlApps
	if err != nil || res.Exit != 0 || rerr != nil || json.Unmarshal(b, &apps) != nil || apps.Info.Outcome != "success" {
		r.add(Check{ID: "ios-app", Status: Warn, Code: DiagDeviceOffline, Detail: "devicectl cannot list the apps on " + d.ID + ", so the installed build is unchecked: " + tail(res, err), Fix: "perflab device probe " + d.ID + " --json"})
		return
	}
	for _, a := range apps.Result.Apps {
		if a.BundleIdentifier != bundle {
			continue
		}
		r.data.Device.AppVersion, r.data.Device.BundleVer = a.Version, a.BundleVersion
		if want := r.o.App.ExpectBundleVersion; want != "" && a.BundleVersion != want {
			r.add(Check{ID: "ios-app", Status: st, Code: DiagWrongBinary,
				Detail: fmt.Sprintf("%s on %s has CFBundleVersion %s; the variant perflab installed stamps %s (another build was installed since)", bundle, d.ID, a.BundleVersion, want),
				Fix:    r.installFix()})
			return
		}
		r.add(Check{ID: "ios-app", Status: Pass, Detail: fmt.Sprintf("%s %s (%s) installed", bundle, a.Version, a.BundleVersion)})
		return
	}
	r.add(Check{ID: "ios-app", Status: st, Code: DiagAppNotInstalled, Detail: bundle + " is not installed on " + d.ID, Fix: r.installFix()})
}

// appiumOutcome is what the last Appium server start says about WDA.
type appiumOutcome string

const (
	appiumNone              appiumOutcome = "none"
	appiumSessionOK         appiumOutcome = "session-ok"
	appiumAutomationTimeout appiumOutcome = "automation-timeout"
)

var (
	appiumStart     = "Welcome to Appium"
	automationLine  = "Timed out while enabling automation mode"
	sessionOKLine   = regexp.MustCompile(`session created successfully|ServerURLHere|WebDriverAgent.*started`)
	appiumTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`)
)

// scanAppiumLog reads the adapter's Appium server log: after the last
// server start, which came last, a WDA session or the XCTest "Timed out
// while enabling automation mode" (Settings > Developer > Enable UI
// Automation off)?
func scanAppiumLog(text string) (appiumOutcome, string) {
	if i := strings.LastIndex(text, appiumStart); i >= 0 {
		text = text[i:]
	}
	outcome, at := appiumNone, ""
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.Contains(line, automationLine):
			outcome, at = appiumAutomationTimeout, appiumTimestamp.FindString(line)
		case sessionOKLine.MatchString(line):
			outcome, at = appiumSessionOK, appiumTimestamp.FindString(line)
		}
	}
	return outcome, at
}

func (r *run) uiAutomation() {
	st := r.level("", Fail, "")
	if st == "" || r.o.AppiumServerLog == "" {
		return
	}
	b, err := r.env.ReadFile(r.o.AppiumServerLog)
	if err != nil {
		return // no evidence either way; the probe's HUMAN_CHECK names the setting
	}
	outcome, at := scanAppiumLog(string(b))
	switch outcome {
	case appiumSessionOK:
		r.add(Check{ID: "ui-automation", Status: Pass, Detail: "the last WDA session started (" + at + ")"})
	case appiumAutomationTimeout:
		if per, ok := r.loadRatio(); ok && per > loadPerCoreLimit {
			r.add(Check{ID: "ui-automation", Status: Warn, Code: DiagUIAutomationOff,
				Detail: fmt.Sprintf("the last WDA start (%s) timed out enabling automation mode, and the Mac's load is %.1f per core now: retry at low load before blaming the setting", at, per),
				Fix:    r.rerun()})
			return
		}
		r.add(Check{ID: "ui-automation", Status: st, Code: DiagUIAutomationOff,
			Detail: "the last WDA start (" + at + ") failed with \"Timed out while enabling automation mode\" at low load: UI Automation is off on the phone",
			Fix:    "on the iPhone: Settings > Developer > Enable UI Automation (human), then " + r.rerun()})
	}
}

func (r *run) iosAPIHuman() {
	if r.level("", Human, "") == "" || r.o.API == nil {
		return
	}
	origin := r.o.API.IOSOrigin
	if origin == "" {
		origin = r.o.API.Origin
	}
	if origin == "" {
		return
	}
	r.add(Check{ID: "ios-api", Status: Human, Code: DiagHumanCheck,
		Detail: "the iPhone must reach " + origin + " on its own (no adb reverse on iOS; a Devbox origin needs the phone on the WireGuard mesh): open " + strings.TrimRight(origin, "/") + r.o.API.Health + " in Safari on the phone"})
}

func ptr[T any](v T) *T { return &v }
