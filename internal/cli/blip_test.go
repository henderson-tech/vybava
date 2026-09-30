package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/runx"
)

// TestBlipEnvelopeSurface pins the cli-craft contract for the name-verb
// rewrite: `blip <name> <verb>` on a name that is not up answers one JSON
// envelope with NOT_RUNNING and exit 2, and a cobra parse error is a
// diagnostic, never a usage dump.
func TestBlipEnvelopeSurface(t *testing.T) {
	t.Setenv("BLIP_STATE_DIR", t.TempDir())
	for _, args := range [][]string{{"ghost", "status"}, {"ghost", "set", "delay", "1s"}, {"ghost", "ok"}, {"ghost", "record", "on"}, {"ghost", "authz", "--as", "none"}, {"ghost", "log", "--nope"}} {
		var out bytes.Buffer
		command, err := (App{Stdout: &out, Stderr: &out}).Command("blip")
		if err != nil {
			t.Fatal(err)
		}
		command.SetArgs(append(args, "--json"))
		err = command.Execute()
		var env runx.Envelope
		if jerr := json.Unmarshal(out.Bytes(), &env); jerr != nil {
			t.Fatalf("%v: stdout is not one envelope: %q", args, out.String())
		}
		if env.V != runx.EnvelopeVersion || env.OK || len(env.Diagnostics) != 1 || env.Diagnostics[0].Fix == "" {
			t.Fatalf("%v: %+v", args, env)
		}
		if d := env.Diagnostics[0]; args[1] != "log" && (d.Code != "NOT_RUNNING" || !strings.Contains(d.Detail, "ghost")) {
			t.Fatalf("%v: name not bound to the verb: %+v", args, d) // StringVar on the shared name once reset it
		}
		if ExitCode(err) != 2 || ErrorText(err) != "" {
			t.Fatalf("%v: exit=%d text=%q", args, ExitCode(err), ErrorText(err))
		}
	}
}
