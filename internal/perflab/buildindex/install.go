package buildindex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/shellword"
)

// Device is the install target, taken from the caller's ledger row. The
// caller holds the lease and the device flock for the whole install.
type Device struct {
	ID                string
	Platform          string
	CoreDeviceID      string
	Serial            string
	ProtectedPackages []string
}

func (d Device) protects(pkg string) bool {
	for _, p := range d.ProtectedPackages {
		if p == pkg {
			return true
		}
	}
	return false
}

// InstallSpec is one `perflab install <variantId|nativeKey>`.
type InstallSpec struct {
	Item     Installable
	Device   Device
	Timeout  time.Duration
	Progress *Progress
}

// InstallResult is the install payload; Stamp is what the caller records
// as the lease's lastInstalled (fencing).
type InstallResult struct {
	ID          string            `json:"id"`
	Device      string            `json:"device"`
	AppID       string            `json:"appId"`
	Stamp       InstalledStamp    `json:"stamp"`
	Uninstalled bool              `json:"uninstalled"`
	Compiled    bool              `json:"compiled,omitempty"`
	Launched    bool              `json:"launched,omitempty"`
	DurationMs  int64             `json:"durationMs"`
	Diagnostics []runx.Diagnostic `json:"-"`
}

// Install puts a variant or native build on the device and verifies what
// the device then reports: iOS installs with devicectl and launches with
// --terminate-existing; Android installs with `adb install -r`, uninstalls
// first only on a downgrade or key change of an unprotected package, AOT
// compiles (speed) and compares the device's base APK sha256.
func Install(ctx context.Context, r Runner, spec InstallSpec) (InstallResult, error) {
	it, dev := spec.Item, spec.Device
	if it.Platform != dev.Platform {
		return InstallResult{}, diag(DiagUsage, fmt.Sprintf("%s is a %s artifact and %s a %s device", it.ID, it.Platform, dev.ID, dev.Platform),
			"perflab device list --json")
	}
	timeout := spec.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	started := time.Now()
	var (
		res InstallResult
		err error
	)
	if it.Platform == "ios" {
		res, err = installIOS(ctx, r, it, dev, timeout, spec.Progress)
	} else {
		res, err = installAndroid(ctx, r, it, dev, timeout, spec.Progress)
	}
	res.ID, res.Device, res.AppID = it.ID, dev.ID, it.AppID
	res.DurationMs = time.Since(started).Milliseconds()
	// An install under a protected id replaces the owner's app in place (the
	// iOS perf build signs the store bundle id): nothing was uninstalled or
	// cleared, but the phone's production app is now this build.
	if err == nil && dev.protects(it.AppID) {
		res.Diagnostics = append(res.Diagnostics, runx.Diagnostic{Code: DiagPackageProtected, Severity: "warning",
			Detail: fmt.Sprintf("%s replaced the owner's %s on %s in place (data kept): the phone's production app is now this build", it.ID, it.AppID, dev.ID),
			Fix:    "when the lab is done, reinstall " + it.AppID + " from the store; a dev bundle id (perflab.app.ios.bundleId) keeps the owner's app untouched"})
	}
	return res, err
}

var iosTransport = regexp.MustCompile(`(?i)disconnected|not connected|connection (was )?(interrupted|lost)|lost connection|device is not available|unable to locate (a )?device|timed out`)

func installIOS(ctx context.Context, r Runner, it Installable, dev Device, timeout time.Duration, progress *Progress) (InstallResult, error) {
	if dev.CoreDeviceID == "" {
		return InstallResult{}, diag(DiagUsage, "device "+dev.ID+" has no CoreDevice id in the ledger", "perflab device add "+dev.ID+" --core-device-id <id> (perflab device scan --json lists it)")
	}
	phase(progress, "install")
	res, err := run(ctx, r, Cmd{Argv: []string{"xcrun", "devicectl", "device", "install", "app", "--device", dev.CoreDeviceID, it.Path}, Timeout: timeout})
	if err != nil {
		return InstallResult{}, err
	}
	out := string(res.Stdout) + string(res.Stderr)
	if res.Exit != 0 || res.TimedOut {
		code := DiagInstallFailed
		if res.TimedOut || iosTransport.MatchString(out) {
			code = DiagInstallTransport
		}
		return InstallResult{}, diag(code, "devicectl install of "+it.ID+" on "+dev.ID+" failed: "+lastLines([]byte(out), 3),
			"reconnect the phone (USB, unlocked), then re-run perflab install "+it.ID+" --device "+dev.ID+" --lease <token> --json")
	}
	out2 := InstallResult{Stamp: it.Stamp}
	phase(progress, "launch")
	res, err = run(ctx, r, Cmd{Argv: []string{"xcrun", "devicectl", "device", "process", "launch", "--device", dev.CoreDeviceID, "--terminate-existing", it.AppID}, Timeout: 2 * time.Minute})
	if err != nil {
		return out2, err
	}
	if res.Exit != 0 {
		text := string(res.Stdout) + string(res.Stderr)
		if strings.Contains(strings.ToLower(text), "locked") {
			return out2, diag(DiagDeviceLocked, dev.ID+" is locked, so the installed app could not launch",
				"unlock the phone (Auto-Lock: Never for lab runs), then perflab app launch --device "+dev.ID+" --lease <token> --json")
		}
		return out2, diag(DiagInstallFailed, "the installed app does not launch: "+lastLines([]byte(text), 3),
			"perflab crashes --device "+dev.ID+" --lease <token> --json")
	}
	out2.Launched = true
	phase(progress, "verify")
	got, err := iosInstalledVersion(ctx, r, dev, it.AppID)
	if err != nil {
		return out2, err
	}
	if got != it.Stamp.BundleVersion {
		return out2, diag(DiagInstallFailed,
			fmt.Sprintf("after the install %s reports %s bundleVersion %q, expected %q", dev.ID, it.AppID, got, it.Stamp.BundleVersion),
			"perflab install "+it.ID+" --device "+dev.ID+" --lease <token> --json")
	}
	return out2, nil
}

// iosInstalledVersion is the installed app's CFBundleVersion, "" when the
// app is not installed.
func iosInstalledVersion(ctx context.Context, r Runner, dev Device, bundleID string) (string, error) {
	dir, err := os.MkdirTemp("", "perflab-apps-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "apps.json")
	res, err := run(ctx, r, Cmd{Argv: []string{"xcrun", "devicectl", "device", "info", "apps", "--device", dev.CoreDeviceID,
		"--bundle-id", bundleID, "--json-output", out, "--quiet"}, Timeout: time.Minute})
	if err != nil {
		return "", err
	}
	b, rerr := os.ReadFile(out)
	if res.Exit != 0 || rerr != nil {
		return "", diag(DiagInstallTransport, "devicectl cannot list the apps of "+dev.ID+": "+lastLines(res.Stderr, 2),
			"perflab device probe "+dev.ID+" --json")
	}
	return parseIOSApps(b, bundleID)
}

func parseIOSApps(b []byte, bundleID string) (string, error) {
	var doc struct {
		Result struct {
			Apps []struct {
				BundleIdentifier string `json:"bundleIdentifier"`
				BundleVersion    string `json:"bundleVersion"`
			} `json:"apps"`
		} `json:"result"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("devicectl apps json: %w", err)
	}
	for _, a := range doc.Result.Apps {
		if a.BundleIdentifier == bundleID {
			return a.BundleVersion, nil
		}
	}
	return "", nil
}

var (
	adbFailure   = regexp.MustCompile(`Failure \[([A-Z_]+)`)
	adbTransport = regexp.MustCompile(`(?i)device offline|no devices/emulators found|device '[^']*' not found|protocol fault|broken pipe|connection reset|device still authorizing|unauthorized`)
)

func adb(dev Device, args ...string) []string {
	return append([]string{"adb", "-s", dev.Serial}, args...)
}

// deviceShellFix is a fix that reaches the leased device. A raw `adb -s
// <serial>` is refused by claude-guards' machine:device-leased, so the fix
// goes through perflab's token-checked passthrough; the caller, which holds
// the token, fills <token>.
func deviceShellFix(dev Device, args ...string) string {
	words := make([]string, len(args))
	for i, a := range args {
		words[i] = shellword.Quote(a)
	}
	return "perflab device shell " + dev.ID + " --lease <token> --json -- " + strings.Join(words, " ")
}

func installAndroid(ctx context.Context, r Runner, it Installable, dev Device, timeout time.Duration, progress *Progress) (InstallResult, error) {
	if dev.Serial == "" {
		return InstallResult{}, diag(DiagUsage, "device "+dev.ID+" has no adb serial in the ledger", "perflab device add "+dev.ID+" --serial <serial>")
	}
	out := InstallResult{Stamp: it.Stamp}
	if out.Stamp.APKSHA256 == "" {
		sum, err := fileSHA256(it.Path)
		if err != nil {
			return out, err
		}
		out.Stamp.APKSHA256 = sum
	}
	install := func() (string, error) {
		phase(progress, "install")
		res, err := run(ctx, r, Cmd{Argv: adb(dev, "install", "-r", it.Path), Timeout: timeout})
		if err != nil {
			return "", err
		}
		text := string(res.Stdout) + string(res.Stderr)
		if res.Exit == 0 && strings.Contains(text, "Success") {
			return "", nil
		}
		if res.TimedOut || adbTransport.MatchString(text) {
			return "", diag(DiagInstallTransport, "adb lost "+dev.ID+" during the install: "+lastLines([]byte(text), 2),
				"reconnect the USB cable, then re-run perflab install "+it.ID+" --device "+dev.ID+" --lease <token> --json")
		}
		if m := adbFailure.FindStringSubmatch(text); m != nil {
			return m[1], nil
		}
		return "", diag(DiagInstallFailed, "adb install failed: "+lastLines([]byte(text), 3), deviceShellFix(dev, "install", "-r", it.Path))
	}
	failure, err := install()
	if err != nil {
		return out, err
	}
	if failure == "INSTALL_FAILED_VERSION_DOWNGRADE" || failure == "INSTALL_FAILED_UPDATE_INCOMPATIBLE" {
		if dev.protects(it.AppID) {
			return out, diag(DiagPackageProtected,
				fmt.Sprintf("installing %s needs %s uninstalled first (%s), and %s protects it", it.ID, it.AppID, failure, dev.ID),
				"build the adapter's dev package (perflab.app.android.package) so the owner's app stays untouched")
		}
		phase(progress, "uninstall")
		res, err := run(ctx, r, Cmd{Argv: adb(dev, "uninstall", it.AppID), Timeout: 2 * time.Minute})
		if err != nil {
			return out, err
		}
		if res.Exit != 0 || !strings.Contains(string(res.Stdout), "Success") {
			return out, diag(DiagInstallFailed, "adb uninstall "+it.AppID+" failed: "+lastLines(append(res.Stdout, res.Stderr...), 2),
				deviceShellFix(dev, "uninstall", it.AppID))
		}
		out.Uninstalled = true
		if failure, err = install(); err != nil {
			return out, err
		}
	}
	if failure != "" {
		return out, diag(DiagInstallFailed, it.ID+" was rejected by "+dev.ID+": "+failure,
			deviceShellFix(dev, "install", "-r", it.Path))
	}
	phase(progress, "compile")
	res, err := run(ctx, r, Cmd{Argv: adb(dev, "shell", "cmd", "package", "compile", "-m", "speed", "-f", it.AppID), Timeout: 5 * time.Minute})
	if err != nil {
		return out, err
	}
	if res.Exit == 0 && strings.Contains(string(res.Stdout), "Success") {
		out.Compiled = true
	} else {
		out.Diagnostics = append(out.Diagnostics, runx.Diagnostic{Code: DiagInstallFailed, Severity: "warning",
			Detail: "the AOT compile (cmd package compile -m speed) failed, so JIT warm-up adds variance: " + lastLines(append(res.Stdout, res.Stderr...), 2),
			Fix:    deviceShellFix(dev, "shell", "cmd", "package", "compile", "-m", "speed", "-f", it.AppID)})
	}
	phase(progress, "verify")
	got, err := androidInstalledSHA(ctx, r, dev, it.AppID)
	if err != nil {
		return out, err
	}
	if got != out.Stamp.APKSHA256 {
		return out, diag(DiagInstallFailed,
			fmt.Sprintf("after the install %s holds a base APK %s, expected %s", dev.ID, short(got), short(out.Stamp.APKSHA256)),
			"perflab install "+it.ID+" --device "+dev.ID+" --lease <token> --json")
	}
	return out, nil
}

// androidInstalledSHA is the sha256 of the installed base APK, "" when the
// package is not installed.
func androidInstalledSHA(ctx context.Context, r Runner, dev Device, pkg string) (string, error) {
	res, err := run(ctx, r, Cmd{Argv: adb(dev, "shell", "pm", "path", pkg), Timeout: time.Minute})
	if err != nil {
		return "", err
	}
	if adbTransport.Match(append(res.Stdout, res.Stderr...)) {
		return "", diag(DiagInstallTransport, "adb lost "+dev.ID+": "+lastLines(res.Stderr, 2), "reconnect the USB cable, then perflab device probe "+dev.ID+" --json")
	}
	var base string
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if p, ok := strings.CutPrefix(line, "package:"); ok && (base == "" || strings.HasSuffix(p, "/base.apk")) {
			base = p
		}
	}
	if base == "" {
		return "", nil
	}
	res, err = run(ctx, r, Cmd{Argv: adb(dev, "shell", "sha256sum", base), Timeout: 2 * time.Minute})
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(res.Stdout))
	if res.Exit != 0 || len(fields) == 0 || len(fields[0]) != 64 {
		return "", diag(DiagInstallTransport, "sha256sum of "+base+" on "+dev.ID+" failed: "+lastLines(append(res.Stdout, res.Stderr...), 2),
			deviceShellFix(dev, "shell", "sha256sum", base))
	}
	return fields[0], nil
}

// VerifyInstalled is the fencing check run and probe make before they
// measure: the device must hold exactly what perflab installed last.
func VerifyInstalled(ctx context.Context, r Runner, dev Device, want InstalledStamp, variantID string) error {
	fix := "perflab install " + variantID + " --device " + dev.ID + " --lease <token> --json"
	var got, expected string
	var err error
	if want.Platform == "ios" {
		got, err = iosInstalledVersion(ctx, r, dev, want.AppID)
		expected = want.BundleVersion
	} else {
		got, err = androidInstalledSHA(ctx, r, dev, want.AppID)
		expected = want.APKSHA256
	}
	if err != nil {
		return err
	}
	if got == "" {
		return diag(DiagAppNotInstalled, want.AppID+" is not installed on "+dev.ID, fix)
	}
	if got != expected {
		return diag(DiagDeviceStateChanged,
			fmt.Sprintf("%s on %s is not the variant perflab installed last (%s): the device reports %s, the lease expects %s", want.AppID, dev.ID, variantID, short(got), short(expected)),
			fix)
	}
	return nil
}
