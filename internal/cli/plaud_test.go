package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Under `onyx run_command` the caller sees neither stdout nor stderr, so a
// failing data command must leave its reason in the --out file — created in a
// directory that did not exist yet, readable by the owner only.
func TestPlaudOutCarriesTheFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("PLAUD_REFRESH_TOKEN", "")
	path := filepath.Join(t.TempDir(), "plaud", "whoami.json")

	var out bytes.Buffer
	command, err := (App{Stdout: &out, Stderr: &out}).Command("plaud")
	if err != nil {
		t.Fatal(err)
	}
	command.SetArgs([]string{"whoami", "--json", "--out", path})
	if err := command.Execute(); err == nil {
		t.Fatal("whoami without an injected token must fail")
	}
	if strings.TrimSpace(out.String()) != "" {
		t.Fatalf("stdout must stay empty when --out is set, got %q", out.String())
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(written, &envelope); err != nil {
		t.Fatalf("file = %q: %v", written, err)
	}
	if !strings.Contains(envelope.Error, "PLAUD_REFRESH_TOKEN is not set") {
		t.Fatalf("error = %q", envelope.Error)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("--out file mode = %o, want 600", mode)
	}
}
