package devlab

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// ProbeOptions are `device probe`'s flags.
type ProbeOptions struct {
	// Lease is the holder's token. With it the probe runs every in-device
	// reading under the device lock; without it the in-device readings run
	// only when nobody leases the device (a probe must never load a phone
	// another holder is measuring).
	Lease string
}

// ProbeState is the probe's payload: what the lab could read off the phone.
type ProbeState struct {
	Device   string   `json:"device"`
	Platform Platform `json:"platform"`
	// Deep reports whether the in-device readings ran.
	Deep     bool   `json:"deep"`
	LeasedBy string `json:"leasedBy,omitempty"`
	Online   bool   `json:"online"`
	Paired   bool   `json:"paired"`
	Locked   *bool  `json:"locked,omitempty"`
	ScreenOn *bool  `json:"screenOn,omitempty"`
	// InstrumentsOnline is iOS only: xctrace lists the phone under "Devices",
	// not "Devices Offline", so Instruments can attach.
	InstrumentsOnline *bool  `json:"instrumentsOnline,omitempty"`
	DeveloperMode     string `json:"developerMode,omitempty"`
	Tunnel            string `json:"tunnel,omitempty"`
	OS                string `json:"os,omitempty"`
	// RefreshHz is the measured vsync (Android SurfaceFlinger), or the
	// ledger's expectHz on iOS, which exposes no refresh reading.
	RefreshHz       int          `json:"refreshHz,omitempty"`
	RefreshSource   string       `json:"refreshSource,omitempty"`
	VsyncPeriodNs   int64        `json:"vsyncPeriodNs,omitempty"`
	ActiveMode      *DisplayMode `json:"activeMode,omitempty"`
	RefreshRateMode string       `json:"refreshRateMode,omitempty"`
	Wakefulness     string       `json:"wakefulness,omitempty"`
	StayOn          *bool        `json:"stayOn,omitempty"`
	ScreenOffMs     int64        `json:"screenOffTimeoutMs,omitempty"`
	Thermal         *Thermal     `json:"thermal,omitempty"`
	Battery         *Battery     `json:"battery,omitempty"`
	Memory          *Memory      `json:"memory,omitempty"`
	Foreground      string       `json:"foregroundPackage,omitempty"`
}

// DisplayMode is Android's active SurfaceFlinger display mode.
type DisplayMode struct {
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	RefreshHz float64 `json:"refreshHz"`
}

// Thermal is `dumpsys thermalservice`: status 0 none .. 6 shutdown.
type Thermal struct {
	Status int                `json:"status"`
	TempsC map[string]float64 `json:"tempsC,omitempty"`
}

// Battery is `dumpsys battery`.
type Battery struct {
	Level   int     `json:"level"`
	Powered bool    `json:"powered"`
	TempC   float64 `json:"tempC,omitempty"`
}

// Memory is /proc/meminfo in kB.
type Memory struct {
	TotalKB     int64 `json:"totalKb"`
	AvailableKB int64 `json:"availableKb"`
	SwapTotalKB int64 `json:"swapTotalKb"`
	SwapFreeKB  int64 `json:"swapFreeKb"`
}

// Probe reads the device's state: connected, locked, developer mode,
// tunnel, refresh, stay-awake, thermal, battery, memory and the foreground
// package, each failing check a diagnostic with its fix. Settings the lab
// cannot read become HUMAN_CHECK rows.
func (l *Lab) Probe(ctx context.Context, handle string, opts ProbeOptions) (Result, error) {
	id, dev, err := l.resolve(handle)
	if err != nil {
		return Result{}, err
	}
	st := ProbeState{Device: id, Platform: dev.Platform, OS: dev.OS}
	var diags []runx.Diagnostic
	token := "<token>"
	if opts.Lease != "" {
		h, err := l.Hold(id, opts.Lease, "device probe")
		if err != nil {
			return Result{}, err
		}
		defer h.Done()
		st.Deep, token = true, opts.Lease
	} else {
		ls, err := l.readLease(id)
		if err != nil {
			return Result{}, err
		}
		if ls.Held() && !l.stale(ls) {
			st.LeasedBy = ls.Owner.String()
			diags = append(diags, infoRow(DiagDeviceLeased, fmt.Sprintf("%s is leased by %s: in-device checks skipped so the probe cannot load a measurement", id, st.LeasedBy), fmt.Sprintf("perflab lease status %s --json", id)))
		} else {
			release, err := l.LockDevice(id, "device probe")
			if err != nil {
				return Result{}, err
			}
			defer release()
			st.Deep = true
		}
	}
	switch dev.Platform {
	case PlatformAndroid:
		diags = append(diags, l.probeAndroid(ctx, id, dev, token, &st)...)
	case PlatformIOS:
		diags = append(diags, l.probeIOS(ctx, id, dev, token, &st)...)
	default:
		return Result{}, fmt.Errorf("ledger row %s has unknown platform %q", id, dev.Platform)
	}
	snap := &ProbeSnapshot{At: l.now(), Online: st.Online, RefreshHz: st.RefreshHz, DeveloperMode: st.DeveloperMode, Tunnel: st.Tunnel, Locked: st.Locked}
	if err := l.updateLedger("device probe", func(led *Ledger) error {
		if d, ok := led.Devices[id]; ok {
			d.LastProbe = snap
		}
		return nil
	}); err != nil {
		return Result{}, err
	}
	next := []string{fmt.Sprintf("perflab lease acquire %s --json", id)}
	if opts.Lease != "" {
		next = []string{fmt.Sprintf("perflab doctor --device %s --lease %s --json", id, opts.Lease)}
	}
	return Result{Data: st, Lines: probeLines(st), Diagnostics: diags, Next: next}, nil
}

func probeLines(st ProbeState) []string {
	lines := []string{fmt.Sprintf("%s (%s %s): online=%t deep=%t", st.Device, st.Platform, st.OS, st.Online, st.Deep)}
	if st.RefreshHz != 0 {
		lines = append(lines, fmt.Sprintf("refresh %d Hz (%s)", st.RefreshHz, st.RefreshSource))
	}
	if st.Battery != nil {
		lines = append(lines, fmt.Sprintf("battery %d%% powered=%t", st.Battery.Level, st.Battery.Powered))
	}
	if st.Thermal != nil {
		lines = append(lines, fmt.Sprintf("thermal status %d", st.Thermal.Status))
	}
	if st.Foreground != "" {
		lines = append(lines, "foreground "+st.Foreground)
	}
	return lines
}

func ptr[T any](v T) *T { return &v }

func (l *Lab) probeAndroid(ctx context.Context, id string, dev *Device, token string, st *ProbeState) []runx.Diagnostic {
	if _, err := l.LookPath("adb"); err != nil {
		return []runx.Diagnostic{errorRow(DiagToolMissing, "adb is not on PATH", "brew install --cask android-platform-tools")}
	}
	devices, err := l.adbDevices(ctx)
	if err != nil {
		return []runx.Diagnostic{errorRow(DiagDeviceOffline, err.Error(), "adb kill-server && adb start-server && adb devices -l")}
	}
	var row *adbDevice
	for i := range devices {
		if devices[i].Serial == dev.Serial {
			row = &devices[i]
		}
	}
	switch {
	case row == nil:
		return []runx.Diagnostic{errorRow(DiagDeviceOffline, fmt.Sprintf("adb does not list %s (%s)", id, dev.Serial), "plug it in with USB debugging on, then: perflab device scan --platform android --json")}
	case row.State == "unauthorized":
		return []runx.Diagnostic{errorRow(DiagDeviceUnpaired, id+": the USB debugging prompt was not accepted", "accept the prompt on the phone (human), then: perflab device probe "+id+" --json")}
	case row.State != "device":
		return []runx.Diagnostic{errorRow(DiagDeviceOffline, fmt.Sprintf("adb lists %s as %s", id, row.State), "reconnect the USB cable, then: adb devices -l")}
	}
	st.Online, st.Paired = true, true
	if !st.Deep {
		return nil
	}
	shellFix := func(args string) string {
		return fmt.Sprintf("perflab device shell %s --lease %s -- shell %s", id, token, args)
	}
	var diags []runx.Diagnostic
	read := func(what string, args ...string) (string, bool) {
		out, err := l.run(ctx, 15*time.Second, append([]string{"adb", "-s", dev.Serial, "shell"}, args...)...)
		if err != nil || out.Code != 0 {
			msg := stderrTail(out)
			if err != nil {
				msg = err.Error()
			}
			diags = append(diags, warnRow(DiagDeviceOffline, fmt.Sprintf("%s: reading %s failed: %s", id, what, msg), ""))
			return "", false
		}
		return out.Stdout, true
	}
	if out, ok := read("SurfaceFlinger latency", "dumpsys", "SurfaceFlinger", "--latency"); ok {
		if ns := parseVsyncPeriod(out); ns > 0 {
			st.VsyncPeriodNs = ns
			st.RefreshHz = int(math.Round(1e9 / float64(ns)))
			st.RefreshSource = "surfaceflinger"
		}
	}
	if out, ok := read("display modes", "dumpsys", "display"); ok {
		st.ActiveMode = parseActiveMode(out)
	}
	if out, ok := read("refresh_rate_mode", "settings", "get", "secure", "refresh_rate_mode"); ok {
		if v := strings.TrimSpace(out); v != "null" {
			st.RefreshRateMode = v
		}
	}
	if out, ok := read("power state", "dumpsys", "power"); ok {
		p := parsePower(out)
		st.Wakefulness, st.StayOn, st.ScreenOffMs = p.wakefulness, &p.stayOn, p.screenOffMs
		st.ScreenOn = ptr(p.wakefulness == "Awake")
		if p.wakefulness != "" && p.wakefulness != "Awake" {
			diags = append(diags, errorRow(DiagDeviceLocked, fmt.Sprintf("%s's screen is %s", id, p.wakefulness), shellFix("input keyevent KEYCODE_WAKEUP")))
		}
		if !p.stayOn && p.screenOffMs > 0 && p.screenOffMs < 10*60*1000 {
			diags = append(diags, warnRow(DiagAutoLockOn, fmt.Sprintf("%s sleeps after %s and stay-awake is off", id, time.Duration(p.screenOffMs)*time.Millisecond), shellFix("svc power stayon usb")))
		}
	}
	if out, ok := read("window state", "dumpsys", "window"); ok {
		keyguard, focus := parseWindow(out)
		st.Locked, st.Foreground = keyguard, focus
		if keyguard != nil && *keyguard {
			diags = append(diags, errorRow(DiagDeviceLocked, id+" shows its lock screen", "unlock the phone by hand (human)"))
		}
	}
	if out, ok := read("thermal state", "dumpsys", "thermalservice"); ok {
		th := parseThermal(out)
		st.Thermal = &th
		switch {
		case th.Status >= 3:
			diags = append(diags, errorRow(DiagThermalHot, fmt.Sprintf("%s thermal status %d (severe or worse): the SoC is throttling", id, th.Status), "let it cool (screen off, unplugged from load), then re-probe"))
		case th.Status >= 1:
			diags = append(diags, warnRow(DiagThermalHot, fmt.Sprintf("%s thermal status %d: throttling may skew frame times", id, th.Status), "let it cool, then re-probe"))
		}
	}
	if out, ok := read("battery", "dumpsys", "battery"); ok {
		b := parseBattery(out)
		st.Battery = &b
		if b.Level > 0 && b.Level < 20 {
			if b.Powered {
				diags = append(diags, warnRow(DiagLowBattery, fmt.Sprintf("%s battery %d%% (charging)", id, b.Level), "let it charge past 20% before measuring"))
			} else {
				diags = append(diags, errorRow(DiagLowBattery, fmt.Sprintf("%s battery %d%% and unplugged: power saving skews frame times", id, b.Level), "plug it in and charge past 20%"))
			}
		}
	}
	if out, ok := read("meminfo", "cat", "/proc/meminfo"); ok {
		m := parseMeminfo(out)
		st.Memory = &m
		if m.TotalKB > 0 && m.AvailableKB*10 < m.TotalKB {
			diags = append(diags, warnRow(DiagMemoryPressure, fmt.Sprintf("%s has %d MB of %d MB available", id, m.AvailableKB/1024, m.TotalKB/1024), shellFix("am kill-all")))
		}
	}
	if dev.ExpectHz != 0 && st.RefreshHz != 0 && absInt(st.RefreshHz-dev.ExpectHz) > 2 {
		diags = append(diags, errorRow(DiagRefreshRateMismatch, fmt.Sprintf("%s vsync is %d Hz, the ledger expects %d Hz", id, st.RefreshHz, dev.ExpectHz),
			"Samsung: Settings > Display > Motion smoothness > High (FHD+ only); others: the device's refresh-rate setting (human)"))
	}
	return diags
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// parseVsyncPeriod reads `dumpsys SurfaceFlinger --latency`'s first line,
// the refresh period in ns (8333333 at 120 Hz).
func parseVsyncPeriod(out string) int64 {
	first, _, _ := strings.Cut(strings.TrimSpace(out), "\n")
	ns, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil {
		return 0
	}
	return ns
}

var (
	activeSfModeRe = regexp.MustCompile(`mActiveSfDisplayMode=DisplayMode\{[^}]*?width=(\d+), height=(\d+),[^}]*?refreshRate=([0-9.]+)`)
	activeModeIDRe = regexp.MustCompile(`mActiveModeId=(\d+)`)
	modeRecordRe   = regexp.MustCompile(`DisplayModeRecord\{mMode=\{id=(\d+), width=(\d+), height=(\d+), fps=([0-9.]+)`)
)

// parseActiveMode reads the active display mode from `dumpsys display`:
// SurfaceFlinger's own active mode first, else the framework's active
// mode id looked up in its supported-mode records (the two id spaces
// differ, so they are never mixed).
func parseActiveMode(out string) *DisplayMode {
	if m := activeSfModeRe.FindStringSubmatch(out); m != nil {
		w, _ := strconv.Atoi(m[1])
		h, _ := strconv.Atoi(m[2])
		hz, _ := strconv.ParseFloat(m[3], 64)
		return &DisplayMode{Width: w, Height: h, RefreshHz: round2(hz)}
	}
	am := activeModeIDRe.FindStringSubmatch(out)
	if am == nil {
		return nil
	}
	for _, r := range modeRecordRe.FindAllStringSubmatch(out, -1) {
		if r[1] == am[1] {
			w, _ := strconv.Atoi(r[2])
			h, _ := strconv.Atoi(r[3])
			hz, _ := strconv.ParseFloat(r[4], 64)
			return &DisplayMode{Width: w, Height: h, RefreshHz: round2(hz)}
		}
	}
	return nil
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

type powerState struct {
	wakefulness string
	stayOn      bool
	screenOffMs int64
}

// wakefulnessNames are PowerManagerInternal's WAKEFULNESS_* numbers, as a
// power group prints them.
var wakefulnessNames = map[string]string{"0": "Asleep", "1": "Awake", "2": "Dreaming", "3": "Dozing"}

// parsePower reads `dumpsys power`: wakefulness, mStayOn (true while
// `svc power stayon` holds) and the screen-off timeout.
func parsePower(out string) powerState {
	var p powerState
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "mWakefulness":
			// Android 16 prints a second, per-power-group mWakefulness as a
			// number (a Galaxy A16: "Awake", later "1"); a name wins.
			if name, ok := wakefulnessNames[value]; ok {
				if p.wakefulness == "" {
					p.wakefulness = name
				}
			} else {
				p.wakefulness = value
			}
		case "mStayOn":
			p.stayOn = value == "true"
		case "mScreenOffTimeoutSetting":
			p.screenOffMs, _ = strconv.ParseInt(value, 10, 64)
		}
	}
	return p
}

var focusRe = regexp.MustCompile(`mCurrentFocus=Window\{\S+ \S+ ([^/\s}]+)`)

// parseWindow reads `dumpsys window`: the keyguard flag and the focused
// window's package.
func parseWindow(out string) (keyguard *bool, focus string) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "isKeyguardShowing="); ok {
			keyguard = ptr(v == "true")
		}
	}
	if m := focusRe.FindStringSubmatch(out); m != nil {
		focus = m[1]
	}
	return keyguard, focus
}

var tempRe = regexp.MustCompile(`Temperature\{mValue=([0-9.]+), mType=\d+, mName=(\w+)`)

// parseThermal reads `dumpsys thermalservice`: the status and the HAL's
// current temperatures (the cached block repeats them).
func parseThermal(out string) Thermal {
	th := Thermal{TempsC: map[string]float64{}}
	for _, line := range strings.Split(out, "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "Thermal Status:"); ok {
			th.Status, _ = strconv.Atoi(strings.TrimSpace(v))
		}
	}
	for _, m := range tempRe.FindAllStringSubmatch(out, -1) {
		v, _ := strconv.ParseFloat(m[1], 64)
		th.TempsC[m[2]] = v
	}
	if len(th.TempsC) == 0 {
		th.TempsC = nil
	}
	return th
}

// parseBattery reads `dumpsys battery`: level, any power source, and the
// temperature (tenths of a degree).
func parseBattery(out string) Battery {
	var b Battery
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "level":
			b.Level, _ = strconv.Atoi(value)
		case "AC powered", "USB powered", "Wireless powered", "Dock powered":
			if value == "true" {
				b.Powered = true
			}
		case "temperature":
			if t, err := strconv.Atoi(value); err == nil {
				b.TempC = float64(t) / 10
			}
		}
	}
	return b
}

// parseMeminfo reads /proc/meminfo's kB lines.
func parseMeminfo(out string) Memory {
	var m Memory
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			m.TotalKB = v
		case "MemAvailable:":
			m.AvailableKB = v
		case "SwapTotal:":
			m.SwapTotalKB = v
		case "SwapFree:":
			m.SwapFreeKB = v
		}
	}
	return m
}

// iOS settings the lab cannot read, each a HUMAN_CHECK row.
var iosHumanChecks = []string{
	"Settings > Developer > Enable UI Automation is on (else WDA times out enabling automation mode)",
	"Settings > Display & Brightness > Auto-Lock is Never",
	"Low Power Mode is off (it caps ProMotion at 60 Hz)",
	"Settings > Accessibility > Motion > Limit Frame Rate is off",
	"Settings > Accessibility > Motion > Reduce Motion is off",
}

type lockStateResult struct {
	Result struct {
		PasscodeRequired  bool `json:"passcodeRequired"`
		UnlockedSinceBoot bool `json:"unlockedSinceBoot"`
	} `json:"result"`
}

type displaysResult struct {
	Result struct {
		BacklightState string `json:"backlightState"`
	} `json:"result"`
}

func (l *Lab) probeIOS(ctx context.Context, id string, dev *Device, token string, st *ProbeState) []runx.Diagnostic {
	if _, err := l.LookPath("xcrun"); err != nil {
		return []runx.Diagnostic{errorRow(DiagToolMissing, "xcrun is not on PATH", "xcode-select --install")}
	}
	devices, err := l.devicectlDevices(ctx)
	if err != nil {
		return []runx.Diagnostic{errorRow(DiagDeviceOffline, err.Error(), "xcrun devicectl list devices")}
	}
	var row *devicectlDevice
	for i := range devices {
		d := &devices[i]
		if strings.EqualFold(d.Identifier, dev.CoreDeviceID) || strings.EqualFold(d.HardwareProperties.UDID, dev.HardwareUDID) {
			row = d
		}
	}
	if row == nil {
		return []runx.Diagnostic{errorRow(DiagDeviceUnpaired, fmt.Sprintf("devicectl does not list %s: it is not paired with this Mac", id), "plug it in, tap Trust on the phone (human), then: perflab device probe "+id+" --json")}
	}
	var diags []runx.Diagnostic
	st.OS = orElse(row.DeviceProperties.OSVersionNumber, st.OS)
	st.Paired = row.ConnectionProperties.PairingState == "paired"
	st.DeveloperMode = row.DeviceProperties.DeveloperModeStatus
	st.Online = row.reachable()
	if !st.Paired {
		diags = append(diags, errorRow(DiagDeviceUnpaired, fmt.Sprintf("%s pairing state is %s", id, row.ConnectionProperties.PairingState), "unlock it and tap Trust this Mac (human)"))
	}
	if st.DeveloperMode != "" && st.DeveloperMode != "enabled" {
		diags = append(diags, errorRow(DiagDeveloperModeOff, fmt.Sprintf("%s developer mode is %s", id, st.DeveloperMode), "Settings > Privacy & Security > Developer Mode, then restart the phone (human)"))
	}
	if !st.Online {
		diags = append(diags, errorRow(DiagDeviceOffline, fmt.Sprintf("devicectl cannot reach %s (tunnel %s)", id, row.ConnectionProperties.TunnelState), "plug it in (or join the same Wi-Fi) and unlock it, then: perflab device probe "+id+" --json"))
		return diags
	}
	if xt, err := l.xctraceDevices(ctx); err == nil {
		_, online := xt.Online[dev.HardwareUDID]
		st.InstrumentsOnline = &online
	}
	major := osMajor(st.OS)
	if major > 0 && major < 17 {
		diags = append(diags, infoRow(DiagDeviceToolingUnsupported, fmt.Sprintf("%s runs iOS %s: devicectl-only steps fall back to xctrace --launch", id, st.OS), ""))
	}
	if st.Deep {
		if b, _, err := l.devicectlJSON(ctx, 30*time.Second, "device", "info", "lockState", "--device", row.Identifier); err == nil {
			var ls lockStateResult
			if json.Unmarshal(b, &ls) == nil {
				st.Locked = ptr(ls.Result.PasscodeRequired)
				if ls.Result.PasscodeRequired {
					diags = append(diags, errorRow(DiagDeviceLocked, id+" is locked (passcode required)", "unlock the phone by hand (human)"))
				}
			}
		} else {
			diags = append(diags, warnRow(DiagDeviceLocked, id+": lock state unreadable: "+err.Error(), ""))
		}
		if b, _, err := l.devicectlJSON(ctx, 30*time.Second, "device", "info", "displays", "--device", row.Identifier); err == nil {
			var dr displaysResult
			if json.Unmarshal(b, &dr) == nil && dr.Result.BacklightState != "" {
				st.ScreenOn = ptr(dr.Result.BacklightState == "activeOn")
			}
		}
	}
	switch {
	case st.ScreenOn != nil && !*st.ScreenOn:
		diags = append(diags, warnRow(DiagDeviceLocked, id+"'s screen is off", "wake it (press the side button), or launch the app: perflab app launch --device "+id+" --lease "+token))
	case st.InstrumentsOnline != nil && !*st.InstrumentsOnline:
		diags = append(diags, warnRow(DiagDeviceOffline, id+": xctrace lists it under Devices Offline, so Instruments cannot attach (a sleeping screen does this; so does a phone whose developer services went idle)", "launch the app to wake it: perflab app launch --device "+id+" --lease "+token+"; still offline: replug the cable"))
	}
	if major >= 17 {
		reg, err := l.tunnels(ctx)
		switch {
		case err != nil:
			st.Tunnel = "registry-down"
			diags = append(diags, warnRow(DiagTunnelRegistryDown, fmt.Sprintf("the RemoteXPC tunnel registry (%s) did not answer OK: %v", l.tunnelRegistryURL(), err), TunnelCreationFix))
		default:
			if _, ok := reg.tunnelFor(dev.HardwareUDID); ok {
				st.Tunnel = "connected"
			} else {
				st.Tunnel = "missing"
				diags = append(diags, warnRow(DiagTunnelMissing, fmt.Sprintf("the tunnel registry lists no tunnel for %s (%s); iOS 17+ automation over Wi-Fi needs one", id, dev.HardwareUDID), TunnelCreationFix))
			}
		}
	}
	if dev.ExpectHz != 0 {
		st.RefreshHz, st.RefreshSource = dev.ExpectHz, "ledger"
	} else {
		diags = append(diags, infoRow(DiagRefreshRateMismatch, id+" has no expectHz and iOS exposes no refresh reading", fmt.Sprintf("perflab device add %s --udid %s --expect-hz 60|120 --json", id, dev.HardwareUDID)))
	}
	for _, c := range iosHumanChecks {
		diags = append(diags, infoRow(DiagHumanCheck, c, ""))
	}
	return diags
}
