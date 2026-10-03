package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/cmux"
	"github.com/henderson-tech/vybava/internal/fleet"
	"github.com/henderson-tech/vybava/internal/plugingc"
	"github.com/henderson-tech/vybava/internal/runx"
)

func fleetFixture(t *testing.T, records map[string]string) func(bool) (fleet.Env, error) {
	t.Helper()
	home := t.TempDir()
	registry := filepath.Join(home, ".claude", "sessions")
	if err := os.MkdirAll(registry, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range records {
		if err := os.WriteFile(filepath.Join(registry, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)
	processes := func(context.Context) (plugingc.ProcessTable, error) {
		return plugingc.ProcessTable{7: {PID: 7, Started: now.Add(-time.Hour), Command: "claude"}}, nil
	}
	return func(bool) (fleet.Env, error) {
		return fleet.Env{Home: home, Now: now, Processes: processes, GOOS: "darwin"}, nil
	}
}

func runFleet(t *testing.T, machine func(bool) (fleet.Env, error), stdin string, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	rt := runtime{stdout: &stdout, stderr: &stderr, stdin: strings.NewReader(stdin)}
	cmd := rt.fleetCommandWithEnv("fleet", machine)
	cmd.PersistentFlags().BoolVar(&rt.json, "json", false, "")
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), err
}

func TestFleetSnapshotEnvelope(t *testing.T) {
	machine := fleetFixture(t, map[string]string{
		"7.json": `{"pid":7,"pidDomain":"darwin","procStart":"Fri Oct  2 17:00:00 2026","sessionId":"live","status":"waiting","waitingFor":"input needed","cwd":"/tmp","statusUpdatedAt":1790960000000}`,
		"8.json": `{"pid":8,"pidDomain":"darwin","procStart":"Fri Oct  2 17:00:00 2026","sessionId":"gone","status":"busy","cwd":"/tmp","statusUpdatedAt":1790960000000}`,
	})
	out, err := runFleet(t, machine, "", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		V    int            `json:"v"`
		OK   bool           `json:"ok"`
		Verb string         `json:"verb"`
		Data fleet.Snapshot `json:"data"`
		Next []string       `json:"next"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !envelope.OK || envelope.V != runx.EnvelopeVersion || envelope.Verb != "snapshot" {
		t.Fatalf("envelope = %+v", envelope)
	}
	if envelope.Data.Counts.Waiting != 1 || envelope.Data.Counts.Dead != 1 || envelope.Data.Sessions[0].SessionID != "live" {
		t.Fatalf("snapshot = %+v", envelope.Data)
	}
	if len(envelope.Next) != 1 || envelope.Next[0] != "fleet revive" {
		t.Fatalf("next = %v", envelope.Next)
	}

	text, err := runFleet(t, machine, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "1 waiting on you") || !strings.Contains(text, "dead while working") {
		t.Fatalf("human output:\n%s", text)
	}
}

func TestFleetUnknownRegistryShapeExitsTwoWithTheField(t *testing.T) {
	machine := fleetFixture(t, map[string]string{"7.json": `{"pid":7,"session_id":"x","status":"busy"}`})
	out, err := runFleet(t, machine, "", "--json")
	var exit runx.ExitError
	if !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, fleet.DiagRegistryShapeUnknown) || !strings.Contains(out, `field \"sessionId\" missing`) {
		t.Fatalf("envelope:\n%s", out)
	}
}

func TestFleetLedgerRecordAndShow(t *testing.T) {
	machine := fleetFixture(t, nil)
	_, err := runFleet(t, machine, `{"kind":"workflow","id":"wf_1","runId":"wf_1","scriptPath":"/s.js","status":"started"}`,
		"ledger", "record", "--session", "abc-123", "--json")
	if err != nil {
		t.Fatal(err)
	}
	out, err := runFleet(t, machine, "", "ledger", "show", "--session", "abc-123", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Data fleet.Ledger `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Data.OpenJobs()) != 1 || envelope.Data.Jobs[0].ScriptPath != "/s.js" {
		t.Fatalf("ledger = %+v", envelope.Data)
	}

	_, err = runFleet(t, machine, `not json`, "ledger", "record", "--session", "abc-123", "--json")
	var exit runx.ExitError
	if !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("bad stdin err = %v", err)
	}
}

func TestFleetSchemaPrintsTheGeneratedDeclarations(t *testing.T) {
	out, err := runFleet(t, fleetFixture(t, nil), "", "schema", "--ts")
	if err != nil {
		t.Fatal(err)
	}
	if out != fleet.TypeScript() {
		t.Fatal("schema --ts differs from fleet.TypeScript()")
	}
}

// promptCmux hosts pid 7 at an empty prompt and records what is pasted.
type promptCmux struct{ pasted []string }

func (c *promptCmux) Check(context.Context) cmux.Status { return cmux.Status{State: cmux.StateOK} }
func (c *promptCmux) Resolve(_ context.Context, pid int) (cmux.Target, error) {
	return cmux.Target{SurfaceID: "S7", WorkspaceID: "W7"}, nil
}
func (c *promptCmux) ReadText(context.Context, string, int) (cmux.Screen, error) {
	return cmux.Screen{Text: "────────────\n❯ \n────────────\n  status\n"}, nil
}
func (c *promptCmux) Paste(_ context.Context, _, text string, submit bool) (cmux.PasteResult, error) {
	c.pasted = append(c.pasted, text)
	return cmux.PasteResult{Submitted: submit}, nil
}
func (c *promptCmux) SendText(context.Context, string, string) error      { return nil }
func (c *promptCmux) Focus(context.Context, string, string, string) error { return nil }

func TestFleetReplyReadsTheTextFromStdin(t *testing.T) {
	base := fleetFixture(t, map[string]string{
		"7.json": `{"pid":7,"pidDomain":"darwin","procStart":"Fri Oct  2 17:00:00 2026","sessionId":"live","status":"waiting","waitingFor":"input needed","cwd":"/tmp","statusUpdatedAt":1790960000000}`,
	})
	cx := &promptCmux{}
	machine := func(codex bool) (fleet.Env, error) {
		env, err := base(codex)
		env.Cmux = cx
		return env, err
	}
	out, err := runFleet(t, machine, "merge it\n", "reply", "--session", "live", "--json")
	if err != nil {
		t.Fatalf("reply: %v\n%s", err, out)
	}
	if len(cx.pasted) != 1 || cx.pasted[0] != "merge it" || !strings.Contains(out, `"submitted": true`) {
		t.Fatalf("pasted %q, out %s", cx.pasted, out)
	}
}
