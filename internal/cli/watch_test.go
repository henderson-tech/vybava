package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/watch"
)

// watchFake reads "open" until its third call, then "done".
type watchFake struct {
	mu    sync.Mutex
	calls int
}

func (*watchFake) Kind() string            { return "fake" }
func (*watchFake) Interval() time.Duration { return 10 * time.Millisecond }
func (*watchFake) Cost() int               { return 0 }
func (*watchFake) Conditions() map[string]watch.Condition {
	return map[string]watch.Condition{"done": {Holds: func(o watch.Observation) bool { return o.Fields["status"] == "done" }}}
}
func (*watchFake) Canonical(_ context.Context, ref, _ string) (string, error) { return ref, nil }
func (f *watchFake) Observe(context.Context, string, string) (watch.Observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	s := "open"
	if f.calls >= 3 {
		s = "done"
	}
	return watch.Observation{Fields: map[string]string{"status": s}, Summary: "status " + s}, nil
}

func watchTestDeps(t *testing.T, probe watch.Probe) watchDeps {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "wcli") // under the 104-byte socket path limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	paths := watch.DefaultPaths(dir)
	paths.Socket = filepath.Join(dir, "w.sock")
	return watchDeps{
		home:       "/Users/me",
		paths:      paths,
		probes:     func() []watch.Probe { return []watch.Probe{probe} },
		uid:        501,
		executable: func() (string, error) { return "/Users/me/.local/bin/vybava", nil },
		loaded:     func(context.Context) bool { return true },
		runPlan: func(context.Context, []watch.Step, io.Writer) error {
			t.Fatal("a dry run executed the plan")
			return nil
		},
		pathEnv: "/opt/homebrew/bin:/usr/bin",
		cwd:     func() (string, error) { return "/w/repo", nil },
	}
}

func runWatch(t *testing.T, deps watchDeps, jsonOut bool, args ...string) (string, string, int) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	rt := runtime{stdout: &stdout, stderr: &stderr, json: jsonOut}
	cmd := rt.watchCommandWith("watch", deps)
	cmd.SetArgs(args)
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	err := cmd.Execute()
	code := 0
	var coder runx.ExitCoder
	switch {
	case errors.As(err, &coder):
		code = coder.ExitCode()
	case err != nil:
		t.Fatalf("watch %v: %v", args, err)
	}
	return stdout.String(), stderr.String(), code
}

func TestWatchUntilFallsBackToProbingDirectly(t *testing.T) {
	deps := watchTestDeps(t, &watchFake{})
	out, errOut, code := runWatch(t, deps, false, "until", "fake:x", "done", "--timeout", "5s")
	if code != 0 || !strings.Contains(out, " met fake:x status done") || !strings.Contains(errOut, "probing directly") {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", code, out, errOut)
	}
}

func TestWatchVerbsThroughTheDaemon(t *testing.T) {
	deps := watchTestDeps(t, &watchFake{})
	e, err := watch.NewEngine(deps.probes(), watch.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ln, err := watch.Listen(deps.paths.Socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- watch.Serve(ctx, e, ln, 5*time.Millisecond, io.Discard) }()
	defer func() { cancel(); <-served }()

	out, _, code := runWatch(t, deps, true, "add", "--session", "s1", "--target", "fake:x", "--until", "done")
	var added struct {
		OK   bool `json:"ok"`
		Data struct {
			Subscription watch.Subscription `json:"subscription"`
		} `json:"data"`
		Next []string `json:"next"`
	}
	if err := json.Unmarshal([]byte(out), &added); err != nil || code != 0 || !added.OK {
		t.Fatalf("add: %d %q %v", code, out, err)
	}
	if added.Data.Subscription.Dir != "/w/repo" || !strings.Contains(added.Next[0], "watch next --session s1") {
		t.Fatalf("add envelope %+v", added)
	}

	out, _, _ = runWatch(t, deps, false, "ls")
	if !strings.Contains(out, added.Data.Subscription.ID+"  s1  fake:x until done") {
		t.Fatalf("ls: %q", out)
	}

	var events []watch.Event
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && (len(events) == 0 || events[len(events)-1].Kind != watch.EventMet) {
		out, _, code = runWatch(t, deps, true, "next", "--session", "s1", "--timeout", "1s")
		var got struct {
			Data struct {
				Events []watch.Event `json:"events"`
				After  int64         `json:"after"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 {
			t.Fatalf("next: %d %q", code, out)
		}
		events = got.Data.Events
	}
	if len(events) == 0 || events[len(events)-1].Kind != watch.EventMet {
		t.Fatalf("never met: %+v", events)
	}

	_, _, code = runWatch(t, deps, true, "rm", "nope")
	if code != 2 {
		t.Fatalf("rm of an unknown id exited %d", code)
	}
	out, _, code = runWatch(t, deps, true, "add", "--session", "s1", "--target", "fake:x", "--until", "merged")
	if code != 2 || !strings.Contains(out, diagWatchInvalid) {
		t.Fatalf("a bad condition: %d %q", code, out)
	}
}

func TestWatchStatusSaysTheDaemonIsDown(t *testing.T) {
	out, _, code := runWatch(t, watchTestDeps(t, &watchFake{}), true, "status")
	if code != 2 || !strings.Contains(out, diagWatchDaemonDown) || !strings.Contains(out, "vybava watch agent install") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestWatchAgentInstallDryRunRendersThePlan(t *testing.T) {
	out, _, code := runWatch(t, watchTestDeps(t, &watchFake{}), true, "agent", "install", "--dry-run")
	var env struct {
		Data struct {
			Plan []struct {
				Why   string   `json:"why"`
				Argv  []string `json:"argv"`
				Write string   `json:"write"`
			} `json:"plan"`
			Applied bool `json:"applied"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != 0 || env.Data.Applied {
		t.Fatalf("%d %q %v", code, out, err)
	}
	var plan []string
	for _, s := range env.Data.Plan {
		plan = append(plan, strings.Join(s.Argv, " ")+s.Write)
	}
	joined := strings.Join(plan, "\n")
	for _, want := range []string{"/Users/me/Library/LaunchAgents/vybava.watchd.plist", "launchctl bootout gui/501/vybava.watchd", "launchctl bootstrap gui/501"} {
		if !strings.Contains(joined, want) {
			t.Errorf("plan lacks %q:\n%s", want, joined)
		}
	}
}

func TestWatchAgentInstallRefusesAGoRunBinary(t *testing.T) {
	deps := watchTestDeps(t, &watchFake{})
	deps.executable = func() (string, error) {
		return "", runx.DiagError{Diag: runx.Diagnostic{Code: diagWatchBinaryUnstable, Severity: "error", Detail: "go run"}}
	}
	out, _, code := runWatch(t, deps, true, "agent", "install", "--dry-run")
	if code != 2 || !strings.Contains(out, diagWatchBinaryUnstable) {
		t.Fatalf("%d %q", code, out)
	}
}
