package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/henderson-tech/vybava/internal/runx"
)

func runFramestats(t *testing.T, args ...string) (runx.Envelope, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	command, err := (App{Stdout: &out, Stderr: &errOut}).Command("framestats")
	if err != nil {
		t.Fatal(err)
	}
	command.SetArgs(append(args, "--json"))
	err = command.Execute()
	var env runx.Envelope
	if jerr := json.Unmarshal(out.Bytes(), &env); jerr != nil {
		t.Fatalf("%v: stdout is not one envelope: %q (%v)", args, out.String(), jerr)
	}
	return env, err
}

// TestFramestatsEnvelopeSurface walks both verbs: a success carries the
// data, every forced failure one closed code, a fix and exit 2.
func TestFramestatsEnvelopeSurface(t *testing.T) {
	env, err := runFramestats(t, "parse", filepath.Join("..", "framestats", "testdata", "s20-drag.framestats.txt"))
	if err != nil || !env.OK || env.Verb != "parse" || env.Data == nil {
		t.Fatalf("parse success: %+v (%v)", env, err)
	}

	notATrace := filepath.Join(t.TempDir(), "trace.txt")
	if err := os.WriteFile(notATrace, []byte("# tracer: nop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	failures := []struct {
		args []string
		code string
	}{
		{[]string{"parse"}, "USAGE"},
		{[]string{"parse", notATrace}, "NOT_FRAMESTATS"},
		{[]string{"perfetto", notATrace}, "USAGE"},
		{[]string{"perfetto", notATrace, "--package", "app.test"}, "NOT_A_PERFETTO_TRACE"},
		{[]string{"perfetto", filepath.Join(t.TempDir(), "missing.pftrace"), "--package", "app.test"}, "FILE_UNREADABLE"},
	}
	for _, f := range failures {
		env, err := runFramestats(t, f.args...)
		if env.V != runx.EnvelopeVersion || env.OK || env.Verb != f.args[0] {
			t.Fatalf("%v: %+v", f.args, env)
		}
		if len(env.Diagnostics) != 1 || env.Diagnostics[0].Code != f.code || env.Diagnostics[0].Fix == "" || len(env.Next) == 0 {
			t.Fatalf("%v: diagnostics = %+v next = %v, want %s with a fix", f.args, env.Diagnostics, env.Next, f.code)
		}
		var exit runx.ExitError
		if !errors.As(err, &exit) || exit.Code != 2 {
			t.Fatalf("%v: err = %v, want exit 2", f.args, err)
		}
	}
}

// A malformed row is excluded and named by a warning beside the data.
func TestFramestatsParseWarnsOnMalformedRows(t *testing.T) {
	dumpFile := filepath.Join(t.TempDir(), "dump.txt")
	body := "---PROFILEDATA---\nFlags,IntendedVsync,SwapBuffers,FrameCompleted,\n0,1000000,4000000,6000000,\ngarbage\n---PROFILEDATA---\n"
	if err := os.WriteFile(dumpFile, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	env, err := runFramestats(t, "parse", dumpFile)
	if err != nil || !env.OK || len(env.Diagnostics) != 1 || env.Diagnostics[0].Code != "MALFORMED_ROWS" || len(env.Next) != 1 {
		t.Fatalf("envelope = %+v (%v), want ok with one MALFORMED_ROWS warning", env, err)
	}
}
