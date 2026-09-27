package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `memo add --note` on a fresh topic: the preflight check accepts a row
// whose note does not exist yet, the note is written, and the row links the
// slug WriteNote returned. A second add on the same topic numbers the note.
// The mechanical grammar fixes ride the same call.
func TestMemoAddNoteLinksAFreshAndAColliding(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "LEDGER.md"), []byte("---\nmemo: 1\nalias: smoke\nkind: personal\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	add := func(sentence, note string) string {
		var stdout, stderr bytes.Buffer
		rt := &runtime{stdout: &stdout, stderr: &stderr, json: true}
		cmd := rt.memoCommand("memo")
		cmd.SetArgs([]string{"--home", home, "add", "feedback/zsh", sentence, "--note", note})
		if err := cmd.Execute(); err != nil {
			t.Fatalf("add: %v\nstdout %s\nstderr %s", err, stdout.String(), stderr.String())
		}
		return stdout.String()
	}
	out := add("Never name a loop variable path — zsh ties it to PATH. Every later command fails", "curl alone failed 1,286 times.")
	if !strings.Contains(out, "[[notes/feedback-zsh]]") || !strings.Contains(out, "ties it to PATH.\"") {
		t.Fatalf("first add did not link the fresh note or fix the grammar:\n%s", out)
	}
	raw, err := os.ReadFile(filepath.Join(home, "notes", "feedback-zsh.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Every later command fails", "curl alone failed 1,286 times."} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("note lacks %q:\n%s", want, raw)
		}
	}
	out = add("Same topic again.", "second note")
	if !strings.Contains(out, "[[notes/feedback-zsh-2]]") {
		t.Fatalf("second add must number its note:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "notes", "feedback-zsh-2.md")); err != nil {
		t.Fatal(err)
	}
	ledger, _ := os.ReadFile(filepath.Join(home, "LEDGER.md"))
	if c := strings.Count(string(ledger), "-> [[notes/feedback-zsh"); c != 2 {
		t.Errorf("ledger links %d notes, want 2:\n%s", c, ledger)
	}
}
