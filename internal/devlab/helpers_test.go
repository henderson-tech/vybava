package devlab

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// Recorded on the lab Mac on 2026-10-02 (names sanitised): an iPhone 11 on
// iOS 18.7.8 (wired) and a Samsung S20 on Android 13 at 120 Hz.
const (
	s20Serial     = "RF8N21PY1BF"
	iphone11UDID  = "00008030-001E6D961122802E"
	iphone11Core  = "F8C49F38-CB34-5B6D-9D34-07F194198EEA"
	airUDID       = "00008150-00116D4C1E40401C"
	airCore       = "55657883-7DF0-5900-B528-7E647BD21477"
	fixedNowRFC   = "2026-10-02T12:00:00Z"
	claudePID     = 32503
	deadClaudePID = 999001
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fakeOut is one recorded command result. JSON is written to the
// --json-output path devicectl is given.
type fakeOut struct {
	code   int
	stdout string
	stderr string
	json   string
}

// fakeExec answers commands from a transcript keyed by the joined argv
// (devicectl's --json-output path stripped); anything else is an error, so
// a test sees every command a verb runs.
type fakeExec struct {
	mu    sync.Mutex
	outs  map[string]fakeOut
	calls []string
}

func (f *fakeExec) run(_ context.Context, c Cmd) (CmdOut, error) {
	args := c.Args
	jsonPath := ""
	if n := len(args); n >= 2 && args[n-2] == "--json-output" {
		jsonPath, args = args[n-1], args[:n-2]
	}
	key := strings.Join(args, " ")
	f.mu.Lock()
	f.calls = append(f.calls, key)
	o, ok := f.outs[key]
	f.mu.Unlock()
	if !ok {
		return CmdOut{}, fmt.Errorf("unexpected command: %s", key)
	}
	if jsonPath != "" && o.json != "" {
		if err := os.WriteFile(jsonPath, []byte(o.json), 0o644); err != nil {
			return CmdOut{}, err
		}
	}
	if c.Stdout != nil {
		_, _ = c.Stdout.Write([]byte(o.stdout))
		return CmdOut{Code: o.code, Stderr: o.stderr}, nil
	}
	return CmdOut{Code: o.code, Stdout: o.stdout, Stderr: o.stderr}, nil
}

func (f *fakeExec) called(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// testLab is a Lab on a temp state dir, a fixed clock, a fake process
// table and the recorded transcripts.
type testLab struct {
	*Lab
	exec    *fakeExec
	env     map[string]string
	procs   map[int]time.Time // live pids and their start times
	stopped []int
	clock   time.Time
	tunnels string
}

func newTestLab(t *testing.T) *testLab {
	t.Helper()
	now, _ := time.Parse(time.RFC3339, fixedNowRFC)
	tl := &testLab{
		exec:    &fakeExec{outs: map[string]fakeOut{}},
		env:     map[string]string{},
		procs:   map[int]time.Time{claudePID: now.Add(-14 * time.Hour)},
		clock:   now,
		tunnels: fixture(t, "tunnels-empty.json"),
	}
	tmp := t.TempDir()
	tl.Lab = &Lab{
		StateDir:   filepath.Join(tmp, "state"),
		ProjectDir: tmp,
		Exec:       tl.exec.run,
		LookPath:   func(name string) (string, error) { return "/usr/bin/" + name, nil },
		Now:        func() time.Time { return tl.clock },
		Getenv:     func(k string) string { return tl.env[k] },
		ProcStart: func(pid int) (time.Time, bool, error) {
			start, ok := tl.procs[pid]
			return start, ok, nil
		},
		StopGroup: func(pgid int) error { tl.stopped = append(tl.stopped, pgid); return nil },
		HTTPGet: func(context.Context, string) ([]byte, error) {
			if tl.tunnels == "" {
				return nil, errors.New("connection refused")
			}
			return []byte(tl.tunnels), nil
		},
		TempDir:  func() string { return tmp },
		LockWait: 300 * time.Millisecond,
		Pid:      4242,
	}
	return tl
}

// as sets the session the next verbs run under.
func (tl *testLab) as(session string, pid int) {
	tl.env["CLAUDE_CODE_SESSION_ID"] = session
	tl.env["CLAUDE_PID"] = fmt.Sprint(pid)
}

// recordIOS installs the devicectl + xctrace transcripts.
func (tl *testLab) recordIOS(t *testing.T) {
	tl.exec.outs["xcrun devicectl list devices"] = fakeOut{json: fixture(t, "devicectl-list.json")}
	tl.exec.outs["xcrun xctrace list devices"] = fakeOut{stdout: fixture(t, "xctrace-devices.txt")}
}

// recordAndroid installs the adb identity transcripts for the S20.
func (tl *testLab) recordAndroid(t *testing.T) {
	tl.exec.outs["adb devices -l"] = fakeOut{stdout: fixture(t, "adb-devices.txt")}
	tl.exec.outs["adb -s "+s20Serial+" shell getprop"] = fakeOut{stdout: fixture(t, "getprop.txt")}
	tl.exec.outs["adb -s "+s20Serial+" shell wm size"] = fakeOut{stdout: fixture(t, "wm-size.txt")}
}

// seed writes ledger rows directly.
func (tl *testLab) seed(t *testing.T, rows map[string]*Device) {
	t.Helper()
	led := &Ledger{SchemaVersion: SchemaVersion, Devices: rows}
	if err := tl.saveLedger(led); err != nil {
		t.Fatal(err)
	}
}

func s20Row() *Device {
	return &Device{Platform: PlatformAndroid, Label: "Samsung S20", Model: "SM-G980F", Serial: s20Serial, OS: "13", SDK: 33, ExpectHz: 120, ProtectedPackages: []string{"app.fixit.client"}}
}

func iphone11Row() *Device {
	return &Device{Platform: PlatformIOS, Label: "iPhone 11", Model: "iPhone 11", ProductType: "iPhone12,1", HardwareUDID: iphone11UDID, CoreDeviceID: iphone11Core, OS: "18.7.8", ExpectHz: 60}
}

// acquire leases a device and returns its token.
func (tl *testLab) acquire(t *testing.T, id string, ttl time.Duration) string {
	t.Helper()
	res, err := tl.Acquire(context.Background(), id, AcquireOptions{For: ttl})
	if err != nil {
		t.Fatalf("acquire %s: %v", id, err)
	}
	return res.Data.(AcquireData).Token
}

// code extracts a DiagError's code ("" for nil, INFRA for anything else).
func code(err error) string {
	if err == nil {
		return ""
	}
	var de runx.DiagError
	if errors.As(err, &de) {
		return de.Diag.Code
	}
	return "INFRA: " + err.Error()
}

func diagErr(t *testing.T, err error) runx.Diagnostic {
	t.Helper()
	var de runx.DiagError
	if !errors.As(err, &de) {
		t.Fatalf("want a DiagError, got %v", err)
	}
	return de.Diag
}

func codes(diags []runx.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Severity+":"+d.Code)
	}
	return out
}

func hasCode(diags []runx.Diagnostic, severity, code string) bool {
	for _, d := range diags {
		if d.Code == code && d.Severity == severity {
			return true
		}
	}
	return false
}
