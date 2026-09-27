package claudeguards

import (
	"os"
	"path/filepath"
	"testing"
)

// memo's shell-write refusal runs inside `claude-guards bash`, so memo's own
// hook no longer spawns on every Bash call; memo's verbs still pass.
func TestBashRefusesMemoLedgerHandWrites(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "LEDGER.md"), []byte("# Ledger\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]string{
		"echo '- #9 x' >> LEDGER.md":                             "memo:ledger-hand-write",
		"sed -i '' 's/a/b/' " + filepath.Join(home, "MEMORY.md"): "memo:ledger-hand-write",
		`memo add feedback/x "one sentence"`:                     "",
		"grep -n foo LEDGER.md":                                  "",
	} {
		in := &HookInput{CWD: home}
		in.ToolInput.Command = cmd
		got := ""
		if d := Bash(in); d != nil {
			got = d.Rule
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", cmd, got, want)
		}
	}
}
