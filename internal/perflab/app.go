package perflab

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/devlab"
	"github.com/henderson-tech/vybava/internal/perflab/buildindex"
	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/shellword"
)

// AppOptions are the app verb flags.
type AppOptions struct {
	Device  string
	Lease   string
	Account string
	Route   string
	World   string
}

func (t *Tool) appID(platform devlab.Platform) (string, string, error) {
	c, err := t.Cfg()
	if err != nil {
		return "", "", err
	}
	switch platform {
	case devlab.PlatformIOS:
		if c.App.IOS != nil {
			return c.App.IOS.BundleID, "", nil
		}
	case devlab.PlatformAndroid:
		if c.App.Android != nil {
			activity := c.App.Android.Activity
			if strings.HasPrefix(activity, ".") {
				activity = c.App.Android.Package + activity
			}
			return c.App.Android.Package, activity, nil
		}
	}
	return "", "", diag(DiagConfigInvalid, fmt.Sprintf("the perflab section has no app.%s", platform), adapterFix)
}

func (t *Tool) deviceFail(h *devlab.Hold, what string, res hostexec.Result, err error) error {
	if err != nil {
		return err
	}
	return diag(devlab.DiagDeviceCommandFailed, fmt.Sprintf("%s on %s exited %d: %s", what, h.ID, res.Exit, res.Tail()), "perflab doctor --device "+h.ID+" --lease "+h.Token+" --json")
}

// launch wakes the device and starts the app; payload, when set, is a deep
// link the app opens.
func (t *Tool) launch(ctx context.Context, h *devlab.Hold, payload string) error {
	id, activity, err := t.appID(h.Device.Platform)
	if err != nil {
		return err
	}
	switch h.Device.Platform {
	case devlab.PlatformIOS:
		args := []string{"device", "process", "launch", "--device", h.Device.CoreDeviceID, "--terminate-existing"}
		if payload != "" {
			args = append(args, "--payload-url", payload)
		}
		res, err := t.devicectl(ctx, 60*time.Second, append(args, id)...)
		if err != nil || res.Exit != 0 {
			if err == nil && strings.Contains(res.Combined(), "not installed") {
				return diag(buildindex.DiagAppNotInstalled, id+" is not installed on "+h.ID, "perflab install <variant> --device "+h.ID+" --lease "+h.Token+" --json")
			}
			return t.deviceFail(h, "devicectl process launch", res, err)
		}
	case devlab.PlatformAndroid:
		_, _ = t.adb(ctx, h.Device.Serial, 10*time.Second, "shell", "input", "keyevent", "KEYCODE_WAKEUP")
		args := androidLaunchArgs(payload, activity, id)
		res, err := t.adb(ctx, h.Device.Serial, 30*time.Second, args...)
		if err != nil || res.Exit != 0 || strings.Contains(res.Combined(), "Error:") {
			return t.deviceFail(h, "adb "+strings.Join(args[1:3], " "), res, err)
		}
	}
	return nil
}

// AppLaunch wakes the phone and starts the app.
func (t *Tool) AppLaunch(ctx context.Context, o AppOptions) (Result, error) {
	h, err := t.Lab.Hold(o.Device, o.Lease, "app launch")
	if err != nil {
		return Result{}, err
	}
	defer h.Done()
	if err := t.launch(ctx, h, ""); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"device": h.ID, "launched": true}, Lines: []string{"launched on " + h.ID},
		Next: []string{fmt.Sprintf("perflab doctor --device %s --lease %s --for run --json", h.ID, o.Lease)}}, nil
}

// AppLink runs the adapter's sign-in link hook and delivers the link. The
// link is a credential: it never reaches the envelope, only its host.
func (t *Tool) AppLink(ctx context.Context, o AppOptions) (Result, error) {
	c, err := t.Cfg()
	if err != nil {
		return Result{}, err
	}
	if c.Hooks == nil || c.Hooks.SignInLink == "" {
		return Result{}, diag(DiagConfigInvalid, "the perflab section has no hooks.signInLink", adapterFix)
	}
	if o.Account == "" {
		return Result{}, diag(DiagUsage, "app link needs --account", "perflab app link --device "+o.Device+" --lease "+o.Lease+" --account <email> [--route <path>] --json")
	}
	h, err := t.Lab.Hold(o.Device, o.Lease, "app link")
	if err != nil {
		return Result{}, err
	}
	defer h.Done()
	v := t.baseVars(ctx, string(h.Device.Platform))
	v.Set("account", o.Account)
	v.Set("route", o.Route)
	cmd, err := v.Expand("hooks.signInLink", c.Hooks.SignInLink, true)
	if err != nil {
		return Result{}, err
	}
	out, err := t.adapterCommand(ctx, "hooks.signInLink", cmd, nil, 2*time.Minute)
	if err != nil {
		return Result{}, err
	}
	link := ""
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.Contains(line, "://") {
			link = line
		}
	}
	if link == "" {
		return Result{}, diag(DiagAdapterCommandFailed, "hooks.signInLink printed no URL", t.byHand(cmd))
	}
	if err := t.launch(ctx, h, link); err != nil {
		return Result{}, err
	}
	host := ""
	if u, err := url.Parse(link); err == nil {
		host = u.Scheme + "://" + u.Host
	}
	return Result{Data: map[string]any{"device": h.ID, "account": o.Account, "route": o.Route, "linkHost": host, "delivered": true},
		Lines: []string{"sign-in link for " + o.Account + " delivered to " + h.ID},
		Next:  []string{fmt.Sprintf("perflab doctor --device %s --lease %s --for run --json", h.ID, o.Lease)}}, nil
}

// AppReset runs the adapter's world reset; the lease proves the caller owns
// the device whose app reads that world.
func (t *Tool) AppReset(ctx context.Context, o AppOptions) (Result, error) {
	c, err := t.Cfg()
	if err != nil {
		return Result{}, err
	}
	if c.Hooks == nil || c.Hooks.ResetWorld == "" {
		return Result{}, diag(DiagConfigInvalid, "the perflab section has no hooks.resetWorld", adapterFix)
	}
	id, dev, _, err := t.Lab.Verify(o.Device, o.Lease)
	if err != nil {
		return Result{}, err
	}
	v := t.baseVars(ctx, string(dev.Platform))
	v.Set("world", o.World)
	v.Set("scenario", o.World)
	cmd, err := v.Expand("hooks.resetWorld", c.Hooks.ResetWorld, true)
	if err != nil {
		return Result{}, err
	}
	if _, err := t.adapterCommand(ctx, "hooks.resetWorld", cmd, nil, 5*time.Minute); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"device": id, "world": o.World, "reset": true}, Lines: []string{"world " + o.World + " reset"},
		Next: []string{fmt.Sprintf("perflab app link --device %s --lease %s --account <email> --json", id, o.Lease)}}, nil
}

// androidLaunchArgs is the adb argv that starts the app: a deep link, the
// adapter's activity, or the launcher intent. adb shell joins its argv into
// ONE device shell line, so the link is quoted: an unquoted `&` in a sign-in
// link backgrounded `am start` and ran the rest as a command (exit 127).
func androidLaunchArgs(payload, activity, id string) []string {
	switch {
	case payload != "":
		return []string{"shell", "am", "start", "-a", "android.intent.action.VIEW", "-d", shellword.Quote(payload), id}
	case activity != "":
		return []string{"shell", "am", "start", "-n", id + "/" + activity}
	default:
		return []string{"shell", "monkey", "-p", id, "-c", "android.intent.category.LAUNCHER", "1"}
	}
}
