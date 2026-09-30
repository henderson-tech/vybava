package polishkit

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// LaneState is one lane resolved to a live device NOW.
type LaneState struct {
	ID     string   `json:"id"`
	Kind   LaneKind `json:"kind"`
	Target Target   `json:"target"`
	// Ready: the device or URL exists and can be driven; Booted: it is up
	// right now (a shutdown simulator is ready but not booted).
	Ready  bool `json:"ready"`
	Booted bool `json:"booted"`
	// UDID (ios-sim, ios-device), Serial (android), Name (device or AVD name).
	UDID    string `json:"udid,omitempty"`
	Serial  string `json:"serial,omitempty"`
	Name    string `json:"name,omitempty"`
	Runtime string `json:"runtime,omitempty"`
	// Nav and Theme are the device's current state when readable.
	Nav   string `json:"nav,omitempty"`
	Theme string `json:"theme,omitempty"`
	// Fix is the exact command that makes the lane ready or boots it.
	Fix  string `json:"fix,omitempty"`
	Boot string `json:"boot,omitempty"`
	// URL for browser/server lanes, with the status the probe got.
	URL    string `json:"url,omitempty"`
	Status int    `json:"status,omitempty"`
	// problem is the diagnostic a not-ready lane reports.
	problem *runx.Diagnostic
}

// LanesOptions are the lanes verb's flags.
type LanesOptions struct {
	Targets []string
	Lane    string
}

// LanesData is the lanes verb's payload.
type LanesData struct {
	Lanes []LaneState `json:"lanes"`
}

// Tool installs, keyed by binary.
var toolFix = map[string]string{
	"xcrun":    "xcode-select --install (then open Xcode once to accept the license)",
	"adb":      "brew install --cask android-platform-tools",
	"emulator": "install the Emulator package with Android Studio's SDK Manager and add $ANDROID_HOME/emulator to PATH",
}

// Lanes resolves every declared lane (filtered by target or id).
func (t *Tool) Lanes(ctx context.Context, opts LanesOptions) (Result, error) {
	targets, err := ParseTargets(opts.Targets)
	if err != nil {
		return Result{}, err
	}
	var lanes []Lane
	if opts.Lane != "" {
		l, ok := t.Config.Lane(opts.Lane)
		if !ok {
			return Result{}, diag(DiagUnknownLane, fmt.Sprintf("lane %q is not declared (lanes: %s)", opts.Lane, strings.Join(t.Config.LaneIDs(nil), ", ")), "polish-kit lanes --json")
		}
		lanes = []Lane{l}
	} else {
		for _, l := range t.Config.Lanes {
			if len(targets) == 0 || slices.Contains(targets, l.Target) {
				lanes = append(lanes, l)
			}
		}
	}
	data := LanesData{Lanes: []LaneState{}}
	var res Result
	for _, l := range lanes {
		st := t.Resolve(ctx, l)
		if st.problem != nil {
			res.Diagnostics = append(res.Diagnostics, *st.problem)
		}
		data.Lanes = append(data.Lanes, st)
	}
	res.Data = data
	res.Lines = lanesLines(data)
	for _, st := range data.Lanes {
		if st.Ready && !st.Booted && st.Boot != "" {
			res.Next = append(res.Next, st.Boot)
			break
		}
	}
	res.Next = append(res.Next, fmt.Sprintf("polish-kit run init --pass %d --json", t.nextPass()))
	return res, nil
}

// Resolve resolves one lane; never returns an error, a problem becomes the
// state's diagnostic (infra failures too, as tool-missing or
// device-unavailable with the stderr summarised).
func (t *Tool) Resolve(ctx context.Context, l Lane) LaneState {
	st := LaneState{ID: l.ID, Kind: l.Kind, Target: l.Target}
	var err error
	switch l.Kind {
	case KindIOSSim:
		err = t.resolveIOSSim(ctx, l, &st)
	case KindIOSDevice:
		err = t.resolveIOSDevice(ctx, l, &st)
	case KindAndroidDevice:
		err = t.resolveAndroidDevice(ctx, l, &st)
	case KindAndroidEmulator:
		err = t.resolveAndroidEmulator(ctx, l, &st)
	case KindBrowser, KindServer:
		t.resolveURL(l, &st)
	}
	if err != nil {
		var de runx.DiagError
		if errorsAs(err, &de) {
			st.problem = &de.Diag
		} else {
			d := errDiag(DiagDeviceUnavailable, l.ID+": "+err.Error(), "")
			st.problem = &d
		}
		st.Ready = false
		if st.problem.Fix != "" {
			st.Fix = st.problem.Fix
		}
	}
	return st
}

// needTool answers tool-missing with the install for a binary not on PATH.
func (t *Tool) needTool(name string) error {
	if _, err := t.LookPath(name); err != nil {
		return diag(DiagToolMissing, name+" is not on PATH", toolFix[name])
	}
	return nil
}

// simList is `xcrun simctl list -j` (one call; simctl takes at most ONE
// type filter, so `-j devices,runtimes` is a usage error). Devices are
// keyed by runtime identifier; devicetypes maps a type name to its identifier.
type simList struct {
	Devices     map[string][]simDevice `json:"devices"`
	DeviceTypes []simDeviceType        `json:"devicetypes"`
	Runtimes    []simRuntime           `json:"runtimes"`
}

type simDevice struct {
	UDID                 string `json:"udid"`
	Name                 string `json:"name"`
	State                string `json:"state"`
	IsAvailable          bool   `json:"isAvailable"`
	DeviceTypeIdentifier string `json:"deviceTypeIdentifier"`
}

type simRuntime struct {
	Identifier           string          `json:"identifier"`
	Version              string          `json:"version"`
	Name                 string          `json:"name"`
	Platform             string          `json:"platform"`
	IsAvailable          bool            `json:"isAvailable"`
	SupportedDeviceTypes []simDeviceType `json:"supportedDeviceTypes"`
}

type simDeviceType struct {
	Name       string `json:"name"`
	Identifier string `json:"identifier"`
}

// versionMatches: "26" matches 26.0 and 26.5, "18.6" matches 18.6 only,
// never "1" against "18".
func versionMatches(prefix, version string) bool {
	return version == prefix || strings.HasPrefix(version, prefix+".")
}

// matchSim finds the lane's simulator: the newest matching runtime that has
// a device of the lane's type (matched by deviceTypeIdentifier, since a sim
// is named per persona, "FixIt template iPhone 17 Pro"), else lane-missing
// with the exact create command, else runtime-missing. Among the type's
// devices the pinned `device` (udid or name) wins, then a name equal to or
// ending in the type name, then any shutdown one; a booted sim that the
// lane did not name comes last, it is usually another session's.
func matchSim(list simList, l Lane) (simDevice, simRuntime, error) {
	var runtimes []simRuntime
	for _, rt := range list.Runtimes {
		if (rt.Platform == "" || strings.EqualFold(rt.Platform, "iOS")) && rt.IsAvailable && versionMatches(l.Runtime, rt.Version) {
			runtimes = append(runtimes, rt)
		}
	}
	if len(runtimes) == 0 {
		var have []string
		for _, rt := range list.Runtimes {
			if rt.IsAvailable {
				have = append(have, rt.Version)
			}
		}
		return simDevice{}, simRuntime{}, diag(DiagRuntimeMissing,
			fmt.Sprintf("no iOS %s runtime is installed (have: %s)", l.Runtime, strings.Join(have, ", ")),
			"Xcode > Settings > Components > install the iOS "+l.Runtime+" simulator runtime")
	}
	sort.SliceStable(runtimes, func(i, j int) bool { return runtimes[i].Version > runtimes[j].Version })
	typeID := list.deviceTypeID(l.DeviceType, runtimes)
	rank := func(d simDevice) int {
		switch {
		case l.Device != "" && (strings.EqualFold(d.UDID, l.Device) || strings.EqualFold(d.Name, l.Device)):
			return 0
		case l.Device != "":
			return 5 // the lane pins another sim; never pick this one over it
		case strings.EqualFold(d.Name, l.DeviceType):
			return 1
		case strings.HasSuffix(strings.ToLower(d.Name), strings.ToLower(l.DeviceType)) && d.State != "Booted":
			return 2
		case d.State != "Booted":
			return 3
		}
		return 4
	}
	for _, rt := range runtimes {
		var found []simDevice
		for _, d := range list.Devices[rt.Identifier] {
			if d.IsAvailable && (strings.EqualFold(d.DeviceTypeIdentifier, typeID) || strings.EqualFold(d.Name, l.DeviceType)) {
				found = append(found, d)
			}
		}
		if l.Device != "" {
			found = slices.DeleteFunc(found, func(d simDevice) bool { return rank(d) != 0 })
		}
		if len(found) == 0 {
			continue
		}
		sort.SliceStable(found, func(i, j int) bool {
			ri, rj := rank(found[i]), rank(found[j])
			if ri != rj {
				return ri < rj
			}
			return found[i].Name < found[j].Name
		})
		return found[0], rt, nil
	}
	rt := runtimes[0]
	if l.Device != "" {
		return simDevice{}, rt, diag(DiagLaneMissing,
			fmt.Sprintf("no %q simulator named or with udid %q on %s", l.DeviceType, l.Device, rt.Name),
			fmt.Sprintf("xcrun simctl create %q %q %q", l.Device, typeID, rt.Identifier))
	}
	return simDevice{}, rt, diag(DiagLaneMissing,
		fmt.Sprintf("no %q simulator on %s", l.DeviceType, rt.Name),
		fmt.Sprintf("xcrun simctl create %q %q %q", l.DeviceType, typeID, rt.Identifier))
}

// deviceTypeID resolves a device type name to CoreSimulator's identifier:
// the devicetypes list first, then the runtimes' supported types, then a
// guess from the name (spaces to dashes, parentheses dropped).
func (list simList) deviceTypeID(name string, runtimes []simRuntime) string {
	if strings.HasPrefix(name, "com.apple.CoreSimulator.SimDeviceType.") {
		return name
	}
	for _, dt := range list.DeviceTypes {
		if strings.EqualFold(dt.Name, name) {
			return dt.Identifier
		}
	}
	for _, rt := range runtimes {
		for _, dt := range rt.SupportedDeviceTypes {
			if strings.EqualFold(dt.Name, name) {
				return dt.Identifier
			}
		}
	}
	return "com.apple.CoreSimulator.SimDeviceType." + strings.NewReplacer(" ", "-", "(", "", ")", "").Replace(name)
}

func (t *Tool) resolveIOSSim(ctx context.Context, l Lane, st *LaneState) error {
	if err := t.needTool("xcrun"); err != nil {
		return err
	}
	out, err := t.run(ctx, 20*time.Second, "xcrun", "simctl", "list", "-j")
	if err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("xcrun simctl list: %s", stderrTail(out))
	}
	var list simList
	if err := json.Unmarshal([]byte(out.Stdout), &list); err != nil {
		return fmt.Errorf("xcrun simctl list: %v", err)
	}
	dev, rt, err := matchSim(list, l)
	st.Runtime = rt.Version
	if err != nil {
		return err
	}
	st.Ready, st.UDID, st.Name = true, dev.UDID, dev.Name
	st.Booted = dev.State == "Booted"
	st.Boot = fmt.Sprintf("xcrun simctl boot %s && open -a Simulator", dev.UDID)
	if st.Booted {
		if out, err := t.run(ctx, 10*time.Second, "xcrun", "simctl", "ui", dev.UDID, "appearance"); err == nil && out.Code == 0 {
			st.Theme = strings.TrimSpace(out.Stdout)
		}
	}
	return nil
}

// devicectl list shapes.
type devicectlList struct {
	Result struct {
		Devices []devicectlDevice `json:"devices"`
	} `json:"result"`
}

type devicectlDevice struct {
	Identifier           string `json:"identifier"`
	ConnectionProperties struct {
		PairingState string `json:"pairingState"`
		TunnelState  string `json:"tunnelState"`
	} `json:"connectionProperties"`
	DeviceProperties struct {
		Name            string `json:"name"`
		OSVersionNumber string `json:"osVersionNumber"`
	} `json:"deviceProperties"`
	HardwareProperties struct {
		UDID          string `json:"udid"`
		MarketingName string `json:"marketingName"`
	} `json:"hardwareProperties"`
}

// matchDevice picks the lane's phone: by name, udid or identifier, or the
// only listed device; paired and tunnel-connected is available.
func matchDevice(list devicectlList, l Lane) (devicectlDevice, error) {
	devices := list.Result.Devices
	var cands []devicectlDevice
	for _, d := range devices {
		if l.Device == "" || strings.EqualFold(d.DeviceProperties.Name, l.Device) || strings.EqualFold(d.HardwareProperties.UDID, l.Device) || strings.EqualFold(d.Identifier, l.Device) {
			cands = append(cands, d)
		}
	}
	if len(cands) == 0 {
		return devicectlDevice{}, diag(DiagDeviceUnavailable, fmt.Sprintf("no paired iPhone %s (devicectl lists %d devices)", quoteOr(l.Device, "at all"), len(devices)), "plug the phone in, trust the Mac, then: xcrun devicectl list devices")
	}
	if l.Device == "" && len(cands) > 1 {
		names := make([]string, len(cands))
		for i, d := range cands {
			names[i] = d.DeviceProperties.Name
		}
		return devicectlDevice{}, diag(DiagDeviceUnavailable, "several iPhones are paired: "+strings.Join(names, ", "), "set device: '<name>' on lane "+l.ID)
	}
	d := cands[0]
	if d.ConnectionProperties.PairingState != "paired" || d.ConnectionProperties.TunnelState != "connected" {
		return d, diag(DiagDeviceUnavailable, fmt.Sprintf("%s is %s/%s", d.DeviceProperties.Name, d.ConnectionProperties.PairingState, d.ConnectionProperties.TunnelState), "unlock the phone and reconnect it (USB or the same Wi-Fi), then: xcrun devicectl list devices")
	}
	return d, nil
}

func quoteOr(s, alt string) string {
	if s == "" {
		return alt
	}
	return fmt.Sprintf("%q", s)
}

func (t *Tool) resolveIOSDevice(ctx context.Context, l Lane, st *LaneState) error {
	if err := t.needTool("xcrun"); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(t.TempDir(), "polish-kit-devicectl-*.json")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(tmpPath)
	out, err := t.run(ctx, 30*time.Second, "xcrun", "devicectl", "list", "devices", "--json-output", tmpPath)
	if err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("xcrun devicectl list devices: %s", stderrTail(out))
	}
	b, err := os.ReadFile(tmpPath)
	if err != nil {
		return err
	}
	var list devicectlList
	if err := json.Unmarshal(b, &list); err != nil {
		return fmt.Errorf("xcrun devicectl list devices: %v", err)
	}
	d, err := matchDevice(list, l)
	st.Name, st.UDID, st.Runtime = d.DeviceProperties.Name, d.HardwareProperties.UDID, d.DeviceProperties.OSVersionNumber
	if err != nil {
		return err
	}
	st.Ready, st.Booted = true, true
	return nil
}

// adbDevice is one row of `adb devices -l`.
type adbDevice struct {
	Serial string
	State  string
	Model  string
}

func (d adbDevice) emulator() bool { return strings.HasPrefix(d.Serial, "emulator-") }

// parseAdbDevices reads `adb devices -l`.
func parseAdbDevices(out string) []adbDevice {
	var devices []adbDevice
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "List of devices") || strings.HasPrefix(line, "*") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		d := adbDevice{Serial: fields[0], State: fields[1]}
		for _, f := range fields[2:] {
			if v, ok := strings.CutPrefix(f, "model:"); ok {
				d.Model = v
			}
		}
		devices = append(devices, d)
	}
	return devices
}

// matchAdb picks the lane's physical device: the serial when set, else the
// only non-emulator device.
func matchAdb(devices []adbDevice, l Lane) (adbDevice, error) {
	var cands []adbDevice
	for _, d := range devices {
		if d.emulator() {
			continue
		}
		if l.Device == "" || d.Serial == l.Device {
			cands = append(cands, d)
		}
	}
	if len(cands) == 0 {
		return adbDevice{}, diag(DiagDeviceUnavailable, fmt.Sprintf("no Android device %s on adb", quoteOr(l.Device, "at all")), "plug the phone in with USB debugging on, accept the prompt, then: adb devices -l")
	}
	if l.Device == "" && len(cands) > 1 {
		serials := make([]string, len(cands))
		for i, d := range cands {
			serials[i] = d.Serial
		}
		return adbDevice{}, diag(DiagDeviceUnavailable, "several Android devices: "+strings.Join(serials, ", "), "set device: '<serial>' on lane "+l.ID)
	}
	d := cands[0]
	if d.State != "device" {
		fix := "accept the USB debugging prompt on the phone, then: adb devices -l"
		if d.State == "offline" {
			fix = "adb kill-server && adb start-server && adb devices -l"
		}
		return d, diag(DiagDeviceUnavailable, fmt.Sprintf("%s is %s", d.Serial, d.State), fix)
	}
	return d, nil
}

// parseNavMode reads `settings get secure navigation_mode`: 0/1 = 3button, 2 = gesture.
func parseNavMode(s string) string {
	switch strings.TrimSpace(s) {
	case "0", "1":
		return "3button"
	case "2":
		return "gesture"
	}
	return ""
}

// parseNightMode reads `cmd uimode night`: "Night mode: yes|no|auto|custom".
func parseNightMode(s string) string {
	_, v, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return ""
	}
	switch strings.TrimSpace(v) {
	case "yes":
		return "dark"
	case "no":
		return "light"
	}
	return strings.TrimSpace(v)
}

func (t *Tool) adbDevices(ctx context.Context) ([]adbDevice, error) {
	if err := t.needTool("adb"); err != nil {
		return nil, err
	}
	out, err := t.run(ctx, 20*time.Second, "adb", "devices", "-l")
	if err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("adb devices: %s", stderrTail(out))
	}
	return parseAdbDevices(out.Stdout), nil
}

func (t *Tool) readAndroidState(ctx context.Context, serial string, st *LaneState) {
	if out, err := t.run(ctx, 10*time.Second, "adb", "-s", serial, "shell", "settings", "get", "secure", "navigation_mode"); err == nil && out.Code == 0 {
		st.Nav = parseNavMode(out.Stdout)
	}
	if out, err := t.run(ctx, 10*time.Second, "adb", "-s", serial, "shell", "cmd", "uimode", "night"); err == nil && out.Code == 0 {
		st.Theme = parseNightMode(out.Stdout)
	}
}

func (t *Tool) resolveAndroidDevice(ctx context.Context, l Lane, st *LaneState) error {
	devices, err := t.adbDevices(ctx)
	if err != nil {
		return err
	}
	d, err := matchAdb(devices, l)
	st.Serial, st.Name = d.Serial, d.Model
	if err != nil {
		return err
	}
	st.Ready, st.Booted = true, true
	t.readAndroidState(ctx, d.Serial, st)
	return nil
}

// parseAvdList reads `emulator -list-avds` (one name per line, chatter skipped).
func parseAvdList(out string) []string {
	var avds []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, " ") {
			continue
		}
		avds = append(avds, line)
	}
	return avds
}

func (t *Tool) resolveAndroidEmulator(ctx context.Context, l Lane, st *LaneState) error {
	if err := t.needTool("emulator"); err != nil {
		return err
	}
	out, err := t.run(ctx, 20*time.Second, "emulator", "-list-avds")
	if err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("emulator -list-avds: %s", stderrTail(out))
	}
	avds := parseAvdList(out.Stdout)
	st.Name = l.DeviceType
	if !slices.Contains(avds, l.DeviceType) {
		return diag(DiagLaneMissing, fmt.Sprintf("no AVD %q (have: %s)", l.DeviceType, strings.Join(avds, ", ")),
			fmt.Sprintf("avdmanager create avd -n %q -k \"system-images;android-35;google_apis;arm64-v8a\" -d pixel_8", l.DeviceType))
	}
	st.Ready = true
	st.Boot = fmt.Sprintf("emulator -avd %s -no-snapshot-load >/dev/null 2>&1 &", l.DeviceType)
	devices, err := t.adbDevices(ctx)
	if err != nil {
		return err
	}
	for _, d := range devices {
		if !d.emulator() || d.State != "device" {
			continue
		}
		out, err := t.run(ctx, 10*time.Second, "adb", "-s", d.Serial, "emu", "avd", "name")
		if err != nil || out.Code != 0 {
			continue
		}
		if name := strings.TrimSpace(strings.Split(out.Stdout, "\n")[0]); name == l.DeviceType {
			st.Serial, st.Booted = d.Serial, true
			t.readAndroidState(ctx, d.Serial, st)
			break
		}
	}
	return nil
}

func (t *Tool) resolveURL(l Lane, st *LaneState) {
	st.URL = l.URL
	status, err := t.HTTPGet(l.URL, 3*time.Second)
	st.Status = status
	fix := "start the server the lane points at, then: polish-kit lanes --lane " + l.ID + " --json"
	if l.Kind == KindBrowser {
		fix = "start the web app the lane points at (a Devbox app or the dev server), then: polish-kit lanes --lane " + l.ID + " --json"
	}
	switch {
	case err != nil:
		d := errDiag(DiagDeviceUnavailable, fmt.Sprintf("%s does not answer within 3 s: %v", l.URL, sanitizeErr(err)), fix)
		st.problem, st.Fix = &d, fix
	case status >= 500:
		d := errDiag(DiagDeviceUnavailable, fmt.Sprintf("%s answers %d", l.URL, status), fix)
		st.problem, st.Fix = &d, fix
	default:
		st.Ready, st.Booted = true, true
	}
}

// sanitizeErr keeps a transport error short and free of the URL's query.
func sanitizeErr(err error) string {
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		return s[i+2:]
	}
	return s
}

// SetOptions are `lanes set`'s flags.
type SetOptions struct {
	Lane  string
	Theme string
	Nav   string
	Text  string
	Reset bool
}

// SetData is what `lanes set` reports.
type SetData struct {
	Lane    string   `json:"lane"`
	UDID    string   `json:"udid,omitempty"`
	Serial  string   `json:"serial,omitempty"`
	Applied []string `json:"applied"`
}

// Set applies device state to a lane.
func (t *Tool) Set(ctx context.Context, opts SetOptions) (Result, error) {
	l, ok := t.Config.Lane(opts.Lane)
	if !ok {
		return Result{}, diag(DiagUnknownLane, fmt.Sprintf("lane %q is not declared (lanes: %s)", opts.Lane, strings.Join(t.Config.LaneIDs(nil), ", ")), "polish-kit lanes --json")
	}
	if opts.Theme != "" && !slices.Contains(Themes, opts.Theme) {
		return Result{}, diag(DiagUsage, fmt.Sprintf("theme %q is not light or dark", opts.Theme), "polish-kit lanes set "+l.ID+" --theme dark --json")
	}
	if opts.Nav != "" && !slices.Contains(NavModes, opts.Nav) {
		return Result{}, diag(DiagUsage, fmt.Sprintf("nav %q is not gesture or 3button", opts.Nav), "polish-kit lanes set "+l.ID+" --nav gesture --json")
	}
	if opts.Theme == "" && opts.Nav == "" && opts.Text == "" && !opts.Reset {
		return Result{}, diag(DiagUsage, "set needs --theme, --nav, --text or --reset", "polish-kit lanes set "+l.ID+" --theme dark --json")
	}
	switch l.Kind {
	case KindIOSDevice:
		return Result{}, diag(DiagLaneUnsupported, "a phone's appearance and text size are set by hand: Settings > Display & Brightness (Light/Dark), Settings > Accessibility > Display & Text Size > Larger Text", "set it on the phone, then: polish-kit cell <id> pass|fail --shot <path> --json")
	case KindBrowser, KindServer:
		return Result{}, diag(DiagLaneUnsupported, "a "+string(l.Kind)+" lane has no device state; a browser lane's theme is the ui-loop harness's", "vybava ui-loop run --themes "+orDefault(opts.Theme, "light,dark")+" --json")
	}
	if opts.Nav != "" && !l.Kind.IsAndroid() {
		return Result{}, diag(DiagUsage, "--nav applies to android lanes only", "polish-kit lanes set "+l.ID+" --theme "+orDefault(opts.Theme, "dark")+" --json")
	}
	st := t.Resolve(ctx, l)
	if st.problem != nil {
		return Result{}, runx.DiagError{Diag: *st.problem}
	}
	if !st.Booted {
		return Result{}, diag(DiagDeviceUnavailable, l.ID+" is not booted", st.Boot)
	}
	applied, err := t.applyState(ctx, st, deviceState{Theme: opts.Theme, Nav: opts.Nav, Text: opts.Text, Reset: opts.Reset})
	data := SetData{Lane: l.ID, UDID: st.UDID, Serial: st.Serial, Applied: applied}
	if err != nil {
		return Result{Data: data}, err
	}
	return Result{Data: data, Lines: applied, Next: []string{"polish-kit shoot " + l.ID + " --json", "polish-kit lanes set " + l.ID + " --reset --json"}}, nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// deviceState is one combination shoot or set applies.
type deviceState struct {
	Theme, Nav, Text string
	Reset            bool
}

// applyState runs the device commands for a state and returns them.
func (t *Tool) applyState(ctx context.Context, st LaneState, s deviceState) ([]string, error) {
	var cmds [][]string
	switch st.Kind {
	case KindIOSSim:
		if s.Reset {
			s.Theme, s.Text = "light", "medium"
		}
		if s.Theme != "" {
			cmds = append(cmds, []string{"xcrun", "simctl", "ui", st.UDID, "appearance", s.Theme})
		}
		if s.Text != "" {
			cmds = append(cmds, []string{"xcrun", "simctl", "ui", st.UDID, "content_size", s.Text})
		}
	case KindAndroidDevice, KindAndroidEmulator:
		if s.Reset {
			s.Theme, s.Nav, s.Text = "light", "gesture", "1.0"
		}
		if s.Theme != "" {
			night := "no"
			if s.Theme == "dark" {
				night = "yes"
			}
			cmds = append(cmds, []string{"adb", "-s", st.Serial, "shell", "cmd", "uimode", "night", night})
		}
		if s.Nav != "" {
			overlay := "com.android.internal.systemui.navbar.gestural"
			if s.Nav == "3button" {
				overlay = "com.android.internal.systemui.navbar.threebutton"
			}
			cmds = append(cmds, []string{"adb", "-s", st.Serial, "shell", "cmd", "overlay", "enable-exclusive", "--category", overlay})
		}
		if s.Text != "" {
			cmds = append(cmds, []string{"adb", "-s", st.Serial, "shell", "settings", "put", "system", "font_scale", s.Text})
		}
	}
	applied := []string{}
	for _, c := range cmds {
		out, err := t.run(ctx, 20*time.Second, c...)
		if err != nil {
			return applied, err
		}
		if out.Code != 0 {
			return applied, fmt.Errorf("%s: %s", strings.Join(c, " "), stderrTail(out))
		}
		applied = append(applied, strings.Join(c, " "))
	}
	return applied, nil
}

func lanesLines(d LanesData) []string {
	rows := [][]string{{"LANE", "KIND", "READY", "BOOTED", "DEVICE", "THEME", "NAV"}}
	for _, st := range d.Lanes {
		dev := st.UDID
		if dev == "" {
			dev = st.Serial
		}
		if dev == "" {
			dev = st.URL
		}
		if st.Name != "" && st.URL == "" {
			dev = st.Name + " " + dev
		}
		rows = append(rows, []string{st.ID, string(st.Kind), yesNo(st.Ready), yesNo(st.Booted), strings.TrimSpace(dev), st.Theme, st.Nav})
	}
	return table(rows)
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
