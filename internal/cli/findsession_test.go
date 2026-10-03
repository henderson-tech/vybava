package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/runx"
)

// Arguments win over stdin; a match answers one ok envelope whose next is
// the reopen line, and a miss answers ok:false with NO_MATCH and exit 2.
func TestFindSessionEnvelope(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "-repo", "aaaaaaaa-0000-0000-0000-000000000001.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	record := `{"type":"assistant","cwd":"/repo","timestamp":"2026-10-01T09:00:00Z","effort":"high","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"the ferry waited for the last passengers"}]}}` + "\n"
	if err := os.WriteFile(path, []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	for _, tc := range []struct {
		args []string
		ok   bool
		exit int
		next string
	}{
		{[]string{"the ferry waited for the last passengers"}, true, 0, "cd /repo && cco -- --resume aaaaaaaa-0000-0000-0000-000000000001"},
		{[]string{"nothing like this was ever said here"}, false, 2, "find-session"},
	} {
		var out bytes.Buffer
		command, err := (App{Stdin: strings.NewReader("stdin loses to arguments entirely"), Stdout: &out, Stderr: &out}).Command("find-session")
		if err != nil {
			t.Fatal(err)
		}
		command.SetArgs(append(tc.args, "--json", "--full", "--root", root))
		err = command.Execute()
		var env runx.Envelope
		if jerr := json.Unmarshal(out.Bytes(), &env); jerr != nil {
			t.Fatalf("%v: stdout is not one envelope: %q", tc.args, out.String())
		}
		if env.OK != tc.ok || ExitCode(err) != tc.exit || len(env.Next) == 0 || !strings.HasPrefix(env.Next[len(env.Next)-1], tc.next) {
			t.Fatalf("%v: ok=%v exit=%d next=%v diags=%+v", tc.args, env.OK, ExitCode(err), env.Next, env.Diagnostics)
		}
	}
}
