package perflab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/devlab"
	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
)

// adb runs one adb command against a held device's serial.
func (t *Tool) adb(ctx context.Context, serial string, timeout time.Duration, args ...string) (hostexec.Result, error) {
	return t.Exec.Run(ctx, hostexec.Cmd{Argv: append([]string{"adb", "-s", serial}, args...), Timeout: timeout})
}

// devicectl runs `xcrun devicectl <args>`.
func (t *Tool) devicectl(ctx context.Context, timeout time.Duration, args ...string) (hostexec.Result, error) {
	return t.Exec.Run(ctx, hostexec.Cmd{Argv: append([]string{"xcrun", "devicectl"}, args...), Timeout: timeout})
}

// Crash is one crash report or error-boundary line seen on the device.
type Crash struct {
	Source  string    `json:"source"` // logcat-crash | js-error-boundary | ios-ips
	At      time.Time `json:"at"`
	Summary string    `json:"summary"`
	File    string    `json:"file,omitempty"`
}

// logcat -v epoch: "  1790905100.123  1234  1234 E Tag: message".
var epochLineRe = regexp.MustCompile(`^\s*(\d{9,11})\.(\d{3})\s+\d+\s+\d+\s+[VDIWEF]\s+(.*)$`)

// parseLogcatSince keeps the epoch-stamped lines at or after since whose
// text matches keep.
func parseLogcatSince(out string, since time.Time, keep func(string) bool) []Crash {
	var crashes []Crash
	for _, line := range strings.Split(out, "\n") {
		m := epochLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		sec, _ := strconv.ParseInt(m[1], 10, 64)
		ms, _ := strconv.ParseInt(m[2], 10, 64)
		at := time.Unix(sec, ms*int64(time.Millisecond)).UTC()
		if at.Before(since) || !keep(m[3]) {
			continue
		}
		crashes = append(crashes, Crash{At: at, Summary: strings.TrimSpace(m[3])})
	}
	return crashes
}

// <Process>-YYYY-MM-DD-HHMMSS.ips, the device's local time.
var ipsNameRe = regexp.MustCompile(`^(.+)-(\d{4}-\d\d-\d\d-\d{6})\.ips$`)

// ipsNames walks devicectl's file listing for crash report names; the JSON
// shape is not relied on beyond "some string ends in .ips".
func ipsNames(raw []byte) []string {
	var doc any
	if json.Unmarshal(raw, &doc) != nil {
		return nil
	}
	seen := map[string]bool{}
	var walk func(any)
	walk = func(n any) {
		switch x := n.(type) {
		case map[string]any:
			for _, v := range x {
				walk(v)
			}
		case []any:
			for _, v := range x {
				walk(v)
			}
		case string:
			if base := filepath.Base(x); strings.HasSuffix(base, ".ips") {
				seen[base] = true
			}
		}
	}
	walk(doc)
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// collectCrashes reads the crash evidence since a time from a held device;
// iOS reports are copied into outDir.
func (t *Tool) collectCrashes(ctx context.Context, h *devlab.Hold, since time.Time, outDir string) ([]Crash, error) {
	dev := h.Device
	var crashes []Crash
	switch dev.Platform {
	case devlab.PlatformAndroid:
		pkg := ""
		if t.Config != nil && t.Config.App.Android != nil {
			pkg = t.Config.App.Android.Package
		}
		res, err := t.adb(ctx, dev.Serial, 30*time.Second, "logcat", "-b", "crash", "-d", "-v", "epoch")
		if err != nil {
			return nil, err
		}
		for _, c := range parseLogcatSince(string(res.Stdout), since, func(s string) bool {
			return strings.Contains(s, "FATAL EXCEPTION") || (pkg != "" && strings.Contains(s, pkg)) || strings.Contains(s, "Fatal signal")
		}) {
			c.Source = "logcat-crash"
			crashes = append(crashes, c)
		}
		res, err = t.adb(ctx, dev.Serial, 30*time.Second, "logcat", "-d", "-v", "epoch", "ReactNativeJS:*", "*:S")
		if err != nil {
			return nil, err
		}
		for _, c := range parseLogcatSince(string(res.Stdout), since, func(s string) bool { return strings.Contains(s, "ErrorBoundary") }) {
			c.Source = "js-error-boundary"
			crashes = append(crashes, c)
		}
	case devlab.PlatformIOS:
		proc := ""
		if t.Config != nil && t.Config.App.IOS != nil {
			proc = t.Config.App.IOS.Scheme
		}
		tmp, err := os.MkdirTemp("", "perflab-crashes-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		listing := filepath.Join(tmp, "files.json")
		res, err := t.devicectl(ctx, 60*time.Second, "device", "info", "files", "--device", dev.CoreDeviceID, "--domain-type", "systemCrashLogs", "--json-output", listing)
		if err != nil {
			return nil, err
		}
		if res.Exit != 0 {
			return nil, diag(devlab.DiagDeviceCommandFailed, "devicectl could not list the crash logs: "+res.Tail(), "perflab doctor --device "+h.ID+" --lease "+h.Token+" --wake --json")
		}
		raw, _ := os.ReadFile(listing)
		for _, name := range ipsNames(raw) {
			m := ipsNameRe.FindStringSubmatch(name)
			if m == nil || (proc != "" && m[1] != proc) {
				continue
			}
			at, err := time.ParseInLocation("2006-01-02-150405", m[2], time.Local)
			if err != nil || at.Before(since) {
				continue
			}
			c := Crash{Source: "ios-ips", At: at.UTC(), Summary: name}
			if outDir != "" {
				if err := os.MkdirAll(outDir, 0o755); err == nil {
					dest := filepath.Join(outDir, name)
					cp, err := t.devicectl(ctx, 60*time.Second, "device", "copy", "from", "--device", dev.CoreDeviceID, "--domain-type", "systemCrashLogs", "--source", name, "--destination", dest)
					if err == nil && cp.Exit == 0 {
						c.File = dest
					}
				}
			}
			crashes = append(crashes, c)
		}
	}
	sort.SliceStable(crashes, func(i, j int) bool { return crashes[i].At.Before(crashes[j].At) })
	return crashes, nil
}

// CrashesOptions are the crashes flags.
type CrashesOptions struct {
	Device string
	Lease  string
	Since  time.Time
	Out    string
}

// Crashes lists crash reports and error-boundary lines since a time
// (default: the last hour).
func (t *Tool) Crashes(ctx context.Context, o CrashesOptions) (Result, error) {
	h, err := t.Lab.Hold(o.Device, o.Lease, "crashes")
	if err != nil {
		return Result{}, err
	}
	defer h.Done()
	since := o.Since
	if since.IsZero() {
		since = t.Now().Add(-time.Hour)
	}
	out := o.Out
	if out == "" {
		out = filepath.Join(t.Store.Dirs.Cache, "crashes", h.ID+"-"+t.Now().UTC().Format("20060102T150405Z"))
	}
	crashes, err := t.collectCrashes(ctx, h, since.UTC(), out)
	if err != nil {
		return Result{}, err
	}
	res := Result{Data: map[string]any{"device": h.ID, "since": since.UTC(), "crashes": orEmpty(crashes)}}
	for _, c := range crashes {
		res.Lines = append(res.Lines, fmt.Sprintf("%s %s %s", c.At.Format(time.RFC3339), c.Source, c.Summary))
	}
	if len(crashes) > 0 {
		res.Diagnostics = append(res.Diagnostics, errDiag(DiagAppCrashed, fmt.Sprintf("%d crash record(s) on %s since %s (first: %s)", len(crashes), h.ID, since.UTC().Format(time.RFC3339), crashes[0].Summary), ""))
	}
	return res, nil
}
