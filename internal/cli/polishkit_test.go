package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/henderson-tech/vybava/internal/polishkit"
	"github.com/henderson-tech/vybava/internal/runx"
)

// TestPolishKitEnvelopeSurface pins the cli-craft contract: in a repo with
// no polish section every verb answers one JSON envelope with
// no-config-section and exit 2, a cobra parse error (unknown verb, missing
// argument, unknown flag) is a `usage` diagnostic with a fix, and text mode
// prints the same code, never a usage dump.
func TestPolishKitEnvelopeSurface(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	cases := []struct {
		args []string
		code string
	}{
		{[]string{"plan"}, polishkit.DiagNoConfigSection},
		{[]string{"lanes"}, polishkit.DiagNoConfigSection},
		{[]string{"lanes", "set", "ios26", "--theme", "dark"}, polishkit.DiagNoConfigSection},
		{[]string{"run", "init"}, polishkit.DiagNoConfigSection},
		{[]string{"run", "add-cell", "--lane", "x", "--flow", "f", "--tier", "t"}, polishkit.DiagNoConfigSection},
		{[]string{"cell", "x", "pass"}, polishkit.DiagNoConfigSection},
		{[]string{"status"}, polishkit.DiagNoConfigSection},
		{[]string{"shoot", "ios26"}, polishkit.DiagNoConfigSection},
		{[]string{"sheet"}, polishkit.DiagNoConfigSection},
		{[]string{"report"}, polishkit.DiagNoConfigSection},
		{[]string{"bogus"}, polishkit.DiagUsage},
		{[]string{"cell", "x"}, polishkit.DiagUsage},
		{[]string{"status", "--nope"}, polishkit.DiagUsage},
	}
	for _, c := range cases {
		var out bytes.Buffer
		command, err := (App{Stdout: &out, Stderr: &out}).Command("polish-kit")
		if err != nil {
			t.Fatal(err)
		}
		command.SetArgs(append(c.args, "--json"))
		err = command.Execute()
		var env runx.Envelope
		if jerr := json.Unmarshal(out.Bytes(), &env); jerr != nil {
			t.Fatalf("%v: stdout is not one envelope: %q", c.args, out.String())
		}
		if env.V != runx.EnvelopeVersion || env.OK || len(env.Diagnostics) != 1 || env.Diagnostics[0].Code != c.code || env.Diagnostics[0].Fix == "" || len(env.Next) == 0 {
			t.Fatalf("%v: %+v", c.args, env)
		}
		if ExitCode(err) != 2 || ErrorText(err) != "" {
			t.Fatalf("%v: exit=%d text=%q", c.args, ExitCode(err), ErrorText(err))
		}
	}
	var out bytes.Buffer
	command, _ := (App{Stdout: &out, Stderr: &out}).Command("polish-kit")
	command.SetArgs([]string{"plan"})
	err := command.Execute()
	if ExitCode(err) != 2 || !bytes.Contains(out.Bytes(), []byte("no-config-section:")) || bytes.Contains(out.Bytes(), []byte("Usage:")) {
		t.Fatalf("text mode: exit=%d out=%q", ExitCode(err), out.String())
	}
	out.Reset()
	command, _ = (App{Stdout: &out, Stderr: &out}).Command("vybava")
	command.SetArgs([]string{"polish-kit", "--help"})
	if err := command.Execute(); err != nil || !bytes.Contains(out.Bytes(), []byte("polish-kit plan")) {
		t.Fatalf("subcommand help: %v %q", err, out.String())
	}
}
