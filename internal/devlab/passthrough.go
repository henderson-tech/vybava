package devlab

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/shellword"
)

// The passthroughs are how a lease holder reaches a leased device with raw
// tooling: claude-guards' `machine:device-leased` rule refuses any raw adb,
// devicectl, xctrace or appium command naming a leased device, so the
// holder runs it here, where the token is checked, the device lock is held
// and the device handle is injected (never typed, so never mistyped).

// DefaultShellTimeout bounds a passthrough command.
const DefaultShellTimeout = 5 * time.Minute

// maxCapturedStdout caps what a passthrough puts in the envelope; bigger
// output belongs in --out.
const maxCapturedStdout = 1 << 20

// ShellOptions are `device shell`'s flags.
type ShellOptions struct {
	// Out receives the command's stdout instead of the envelope.
	Out     string
	Timeout time.Duration
}

// CommandData is a passthrough's payload. Exit is the wrapped command's
// code; perflab's own exit never propagates it.
type CommandData struct {
	Device          string `json:"device"`
	Command         string `json:"command"`
	Exit            int    `json:"exit"`
	Stdout          string `json:"stdout,omitempty"`
	StdoutTruncated bool   `json:"stdoutTruncated,omitempty"`
	Stderr          string `json:"stderr,omitempty"`
	Out             string `json:"out,omitempty"`
	Bytes           int64  `json:"bytes,omitempty"`
}

// Shell runs adb (Android) or devicectl (iOS) arguments against the leased
// device: `adb -s <serial> <args>`, `xcrun devicectl <args>` with
// `--device <coreDeviceId>` placed after the subcommand.
func (l *Lab) Shell(ctx context.Context, handle, token string, args []string, opts ShellOptions) (Result, error) {
	if len(args) == 0 {
		return Result{}, usage("device shell needs the adb or devicectl arguments after --", fmt.Sprintf("perflab device shell %s --lease %s --json -- shell getprop ro.build.version.release", handle, orElse(token, "<token>")))
	}
	h, err := l.Hold(handle, token, "device shell")
	if err != nil {
		return Result{}, err
	}
	defer h.Done()
	argv, err := shellArgv(h.ID, h.Device, token, args)
	if err != nil {
		return Result{}, err
	}
	rerun := fmt.Sprintf("perflab device shell %s --lease %s --json -- %s", h.ID, token, joinWords(args))
	return l.runPassthrough(ctx, h.ID, argv, opts.Out, rerun, orDuration(opts.Timeout, DefaultShellTimeout))
}

func orDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// adbHostOnly are adb commands that act on the adb server or every device,
// never on one: a passthrough must not run them (kill-server drops every
// other holder's phone).
var adbHostOnly = map[string]bool{
	"kill-server": true, "start-server": true, "connect": true, "disconnect": true,
	"pair": true, "mdns": true, "devices": true, "version": true, "help": true,
	"keygen": true, "host-features": true, "nodaemon": true, "server": true,
}

// devicectlGroups are `devicectl device <group> <verb>` groups; the
// injected --device follows the verb.
var devicectlGroups = map[string]bool{
	"copy": true, "info": true, "install": true, "notification": true,
	"orientation": true, "process": true, "uninstall": true,
}

// devicectlLeaves are `devicectl device <verb>` commands without a group.
var devicectlLeaves = map[string]bool{"reboot": true, "sysdiagnose": true}

func shellArgv(id string, dev *Device, token string, args []string) ([]string, error) {
	example := fmt.Sprintf("perflab device shell %s --lease %s -- ", id, token)
	switch dev.Platform {
	case PlatformAndroid:
		if args[0] == "adb" {
			return nil, usage("drop the leading adb: the passthrough adds `adb -s <serial>`", example+joinWords(args[1:]))
		}
		if strings.HasPrefix(args[0], "-") {
			return nil, usage(fmt.Sprintf("adb global option %s is not allowed: the passthrough fixes the device and the server", args[0]), example+"shell <command>")
		}
		if adbHostOnly[args[0]] {
			return nil, usage(fmt.Sprintf("`adb %s` is not device-scoped and touches every holder's phone", args[0]), "run host-wide adb commands only when no device is leased: perflab lease status --json")
		}
		if err := protectedHit(id, dev, args); err != nil {
			return nil, err
		}
		return append([]string{"adb", "-s", dev.Serial}, args...), nil
	case PlatformIOS:
		if args[0] == "xcrun" || args[0] == "devicectl" {
			rest := args[1:]
			if len(rest) > 0 && rest[0] == "devicectl" {
				rest = rest[1:]
			}
			return nil, usage("drop the leading xcrun devicectl: the passthrough adds it", example+joinWords(rest))
		}
		if args[0] != "device" || len(args) < 2 {
			return nil, usage("the iOS passthrough runs device-scoped devicectl commands: device <group> <verb> ...", example+"device info lockState")
		}
		at := 0
		switch {
		case devicectlLeaves[args[1]]:
			at = 2
		case devicectlGroups[args[1]] && len(args) >= 3:
			at = 3
		default:
			return nil, usage(fmt.Sprintf("unknown devicectl device command %q", strings.Join(args[:min(3, len(args))], " ")), example+"device process launch --terminate-existing <bundleId>")
		}
		named, err := deviceFlag(id, dev, args)
		if err != nil {
			return nil, err
		}
		if err := protectedHit(id, dev, args); err != nil {
			return nil, err
		}
		argv := append([]string{"xcrun", "devicectl"}, args[:at]...)
		if !named {
			argv = append(argv, "--device", dev.CoreDeviceID)
		}
		return append(argv, args[at:]...), nil
	}
	return nil, fmt.Errorf("ledger row %s has unknown platform %q", id, dev.Platform)
}

// deviceFlag reports whether the arguments already name the device with
// --device / -d; naming any other device is refused.
func deviceFlag(id string, dev *Device, args []string) (bool, error) {
	for i, a := range args {
		var v string
		switch {
		case (a == "--device" || a == "-d") && i+1 < len(args):
			v = args[i+1]
		case strings.HasPrefix(a, "--device="):
			v = strings.TrimPrefix(a, "--device=")
		default:
			continue
		}
		if !strings.EqualFold(v, id) && !matchesAlias(dev, v) {
			return false, usage(fmt.Sprintf("--device %s is not %s: the passthrough only reaches the leased device", v, id), "drop --device; the passthrough adds it")
		}
		return true, nil
	}
	return false, nil
}

func matchesAlias(dev *Device, v string) bool {
	for _, a := range dev.Aliases() {
		if strings.EqualFold(a, v) {
			return true
		}
	}
	return false
}

// protectedHit refuses an uninstall or a data clear that names a protected
// package (a personal phone's production app), however it is spelled:
// `uninstall <pkg>`, `shell pm clear <pkg>`, `shell "pm uninstall <pkg>"`,
// `device uninstall app <bundleId>`.
func protectedHit(id string, dev *Device, args []string) error {
	if len(dev.ProtectedPackages) == 0 {
		return nil
	}
	// adb shell hands its arguments to the device shell, so a package word
	// may arrive quoted (`"pm clear 'app.fixit.client'"`) or glued to an
	// operator (`pm clear app.fixit.client;`): split on the operators, then
	// strip the quotes, before comparing.
	var words []string
	for _, a := range args {
		for _, w := range strings.FieldsFunc(a, func(r rune) bool { return unicode.IsSpace(r) || strings.ContainsRune(";&|()<>`", r) }) {
			if w = strings.Trim(w, `'"\\`); w != "" {
				words = append(words, w)
			}
		}
	}
	destructive := false
	for _, w := range words {
		if w == "uninstall" || w == "clear" {
			destructive = true
		}
	}
	if !destructive {
		return nil
	}
	for _, w := range words {
		if dev.Protects(w) {
			return diag(DiagPackageProtected, fmt.Sprintf("%s protects %s: an uninstall or data clear would hit it", id, w), "use the dev package (the adapter's app package), never the protected one")
		}
	}
	return nil
}

// runPassthrough runs a resolved command, capturing stdout into the
// envelope or streaming it into out.
// rerun is the perflab command a failure names as its fix: the raw adb or
// devicectl line is refused by claude-guards on a leased device.
func (l *Lab) runPassthrough(ctx context.Context, id string, argv []string, out, rerun string, timeout time.Duration) (Result, error) {
	data := CommandData{Device: id, Command: joinWords(argv)}
	cmd := Cmd{Args: argv, Timeout: timeout}
	var f *os.File
	// Captured stdout is capped while it streams (a `logcat` without -d runs
	// to the timeout): the first MiB is kept, the rest drained and counted.
	capped := &capBuffer{max: maxCapturedStdout}
	if out == "" {
		cmd.Stdout = capped
	}
	if out != "" {
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return Result{}, err
		}
		var err error
		if f, err = os.Create(out); err != nil {
			return Result{}, err
		}
		defer f.Close()
		cmd.Stdout = f
		data.Out = out
	}
	res, err := l.Exec(ctx, cmd)
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", data.Command, err)
	}
	data.Exit = res.Code
	data.Stderr = tail(res.Stderr, 4096)
	if f != nil {
		if fi, err := f.Stat(); err == nil {
			data.Bytes = fi.Size()
		}
	} else {
		data.Stdout, data.StdoutTruncated = capped.String(), capped.dropped > 0
	}
	r := Result{Data: data, Next: []string{}}
	if data.Out == "" {
		r.Lines = strings.Split(strings.TrimRight(data.Stdout, "\n"), "\n")
	} else {
		r.Lines = []string{fmt.Sprintf("%s: %d bytes", data.Out, data.Bytes)}
	}
	if data.StdoutTruncated {
		r.Diagnostics = append(r.Diagnostics, warnRow(DiagUsage, "stdout passed 1 MiB and was truncated in the envelope", "re-run with --out <file>"))
	}
	if res.Code != 0 {
		r.Diagnostics = append(r.Diagnostics, errorRow(DiagDeviceCommandFailed, fmt.Sprintf("%s exited %d: %s", data.Command, res.Code, stderrTail(res)), rerun))
	}
	return r, nil
}

// capBuffer keeps the first max bytes written and drains the rest, counting
// it, so a streaming command never grows the buffer without bound.
type capBuffer struct {
	max     int
	b       bytes.Buffer
	dropped int64
}

func (c *capBuffer) Write(p []byte) (int, error) {
	keep := min(len(p), max(c.max-c.b.Len(), 0))
	c.b.Write(p[:keep])
	c.dropped += int64(len(p) - keep)
	return len(p), nil
}

func (c *capBuffer) String() string { return c.b.String() }

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// ScreencapOptions are `device screencap`'s flags.
type ScreencapOptions struct{ Out string }

// pngMagic opens every PNG; a screencap that does not start with it failed
// even when the tool exited 0.
var pngMagic = []byte("\x89PNG\r\n\x1a\n")

// Screencap writes a PNG of the leased device's screen. Android streams
// `adb exec-out screencap -p`; iOS uses go-ios `ios screenshot`, over the
// RemoteXPC tunnel the registry lists on iOS 17+ (go-ios is never allowed
// to start its own tunnel agent, which it daemonises).
func (l *Lab) Screencap(ctx context.Context, handle, token string, opts ScreencapOptions) (Result, error) {
	if opts.Out == "" {
		return Result{}, usage("device screencap needs --out <file.png>", fmt.Sprintf("perflab device screencap %s --lease %s --out shot.png --json", handle, orElse(token, "<token>")))
	}
	h, err := l.Hold(handle, token, "device screencap")
	if err != nil {
		return Result{}, err
	}
	defer h.Done()
	var argv []string
	switch h.Device.Platform {
	case PlatformAndroid:
		argv = []string{"adb", "-s", h.Device.Serial, "exec-out", "screencap", "-p"}
	case PlatformIOS:
		if _, err := l.LookPath("ios"); err != nil {
			return Result{}, diag(DiagToolMissing, "iOS screencaps need go-ios (`ios`) on PATH", "install go-ios (the go-ios npm package or a release binary from github.com/danielpaulus/go-ios)")
		}
		argv = []string{"ios", "screenshot", "--udid", h.Device.HardwareUDID}
		if osMajor(h.Device.OS) >= 17 {
			reg, err := l.tunnels(ctx)
			if err != nil {
				return Result{}, diag(DiagTunnelRegistryDown, fmt.Sprintf("an iOS %s screencap needs the RemoteXPC tunnel; the registry did not answer: %v", h.Device.OS, err), TunnelCreationFix)
			}
			t, ok := reg.tunnelFor(h.Device.HardwareUDID)
			if !ok {
				return Result{}, diag(DiagTunnelMissing, fmt.Sprintf("an iOS %s screencap needs a tunnel and the registry lists none for %s", h.Device.OS, h.ID), TunnelCreationFix)
			}
			argv = append(argv, "--address", t.Address, "--rsd-port", fmt.Sprint(t.RSDPort))
		}
		argv = append(argv, "--output", opts.Out)
	default:
		return Result{}, fmt.Errorf("ledger row %s has unknown platform %q", h.ID, h.Device.Platform)
	}
	stream := ""
	if h.Device.Platform == PlatformAndroid {
		stream = opts.Out
	}
	rerun := fmt.Sprintf("perflab device screencap %s --lease %s --out %s --json", h.ID, token, shellword.Quote(opts.Out))
	res, err := l.runPassthrough(ctx, h.ID, argv, stream, rerun, time.Minute)
	if err != nil {
		return Result{}, err
	}
	return checkPNG(res, opts.Out), nil
}

// checkPNG turns a zero-exit screencap that left no PNG into a failure.
func checkPNG(res Result, path string) Result {
	if hasError(res.Diagnostics) {
		return res
	}
	b := make([]byte, len(pngMagic))
	f, err := os.Open(path)
	if err == nil {
		_, err = f.Read(b)
		f.Close()
	}
	if err != nil || !bytes.Equal(b, pngMagic) {
		data := res.Data.(CommandData)
		res.Diagnostics = append(res.Diagnostics, errorRow(DiagDeviceCommandFailed, fmt.Sprintf("%s exited 0 but %s is not a PNG", data.Command, path), data.Command))
		return res
	}
	data := res.Data.(CommandData)
	if fi, err := os.Stat(path); err == nil {
		data.Out, data.Bytes = path, fi.Size()
	}
	res.Data, res.Lines = data, []string{fmt.Sprintf("%s: %d bytes", path, data.Bytes)}
	return res
}

func hasError(diags []runx.Diagnostic) bool {
	for _, d := range diags {
		if d.Severity == "error" {
			return true
		}
	}
	return false
}

// PullOptions are `device pull`'s flags.
type PullOptions struct {
	Remote string
	Local  string
	// DomainType and DomainID address an iOS file (devicectl copy from):
	// appDataContainer needs the bundle id; systemCrashLogs needs none.
	DomainType string
	DomainID   string
}

// Pull copies one file off the leased device: `adb pull` on Android,
// `devicectl device copy from` on iOS.
func (l *Lab) Pull(ctx context.Context, handle, token string, opts PullOptions) (Result, error) {
	if opts.Remote == "" || opts.Local == "" {
		return Result{}, usage("device pull needs <remote> and <local>", fmt.Sprintf("perflab device pull %s --lease %s <remote> <local> --json", handle, orElse(token, "<token>")))
	}
	h, err := l.Hold(handle, token, "device pull")
	if err != nil {
		return Result{}, err
	}
	defer h.Done()
	var argv []string
	switch h.Device.Platform {
	case PlatformAndroid:
		argv = []string{"adb", "-s", h.Device.Serial, "pull", opts.Remote, opts.Local}
	case PlatformIOS:
		domain := orElse(opts.DomainType, "appDataContainer")
		if domain == "appDataContainer" && opts.DomainID == "" {
			return Result{}, usage("an appDataContainer pull needs the app's bundle id", fmt.Sprintf("perflab device pull %s --lease %s --domain-id <bundleId> %s %s --json", h.ID, token, opts.Remote, opts.Local))
		}
		argv = []string{"xcrun", "devicectl", "device", "copy", "from", "--device", h.Device.CoreDeviceID, "--domain-type", domain}
		if opts.DomainID != "" {
			argv = append(argv, "--domain-identifier", opts.DomainID)
		}
		argv = append(argv, "--source", opts.Remote, "--destination", opts.Local)
	default:
		return Result{}, fmt.Errorf("ledger row %s has unknown platform %q", h.ID, h.Device.Platform)
	}
	if err := os.MkdirAll(filepath.Dir(opts.Local), 0o755); err != nil {
		return Result{}, err
	}
	rerun := fmt.Sprintf("perflab device pull %s --lease %s", h.ID, token)
	if opts.DomainType != "" {
		rerun += " --domain-type " + shellword.Quote(opts.DomainType)
	}
	if opts.DomainID != "" {
		rerun += " --domain-id " + shellword.Quote(opts.DomainID)
	}
	rerun += " " + shellword.Quote(opts.Remote) + " " + shellword.Quote(opts.Local) + " --json"
	res, err := l.runPassthrough(ctx, h.ID, argv, "", rerun, 10*time.Minute)
	if err != nil || hasError(res.Diagnostics) {
		return res, err
	}
	data := res.Data.(CommandData)
	if fi, err := os.Stat(opts.Local); err == nil {
		data.Out, data.Bytes = opts.Local, fi.Size()
		if fi.IsDir() {
			data.Bytes = 0
		}
	}
	res.Data, res.Lines = data, []string{fmt.Sprintf("%s -> %s", opts.Remote, opts.Local)}
	return res, nil
}

// joinWords renders argv as one pasteable shell line.
func joinWords(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellword.Quote(a)
	}
	return strings.Join(quoted, " ")
}
