package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/devlab"
)

// device-lease-release runs at every session end: it releases the leases
// the hook payload's session holds and nobody else's.
func TestClaudeGuardsDeviceLeaseReleaseOnlyThisSession(t *testing.T) {
	t.Setenv("PERFLAB_STATE_DIR", t.TempDir())
	lab, err := devlab.Open()
	if err != nil {
		t.Fatal(err)
	}
	lab.LookPath = func(string) (string, error) { return "", errors.New("no device tooling in tests") }
	lab.Exec = func(context.Context, devlab.Cmd) (devlab.CmdOut, error) { return devlab.CmdOut{Code: 1}, nil }
	ctx := context.Background()
	for id, serial := range map[string]string{"s20": "RF8N21PY1BF", "a50": "R58M000000"} {
		if _, err := lab.Add(ctx, id, devlab.AddOptions{Serial: serial}); err != nil {
			t.Fatal(err)
		}
	}
	for id, session := range map[string]string{"s20": "sess-ending", "a50": "sess-other"} {
		lab.Getenv = func(k string) string { return map[string]string{"CLAUDE_CODE_SESSION_ID": session}[k] }
		if _, err := lab.Acquire(ctx, id, devlab.AcquireOptions{For: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}

	var out, errOut bytes.Buffer
	cmd, err := App{Stdin: strings.NewReader(`{"session_id":"sess-ending"}`), Stdout: &out, Stderr: &errOut}.Command("claude-guards")
	if err != nil {
		t.Fatal(err)
	}
	cmd.SetArgs([]string{"device-lease-release"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("device-lease-release must never fail the session: %v", err)
	}
	held, err := lab.HeldLeases()
	if err != nil {
		t.Fatal(err)
	}
	if len(held) != 1 || held[0].DeviceID != "a50" {
		t.Fatalf("held after the hook: %+v (stderr %q)", held, errOut.String())
	}
	if !strings.Contains(errOut.String(), "s20") {
		t.Fatalf("stderr %q should name the released device", errOut.String())
	}
}
