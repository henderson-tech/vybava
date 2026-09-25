package claudeguards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardDevboxOnly(t *testing.T) {
	root := t.TempDir()
	cfg := `{"guards":{"devboxOnly":["^bun run (dev|run):(api|web|admin)\\b","^bun run test(:|$)"]}}`
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	hook := func(cmd string) *HookInput {
		in := &HookInput{CWD: root}
		in.ToolInput.Command = cmd
		return in
	}
	for _, c := range []string{
		"bun run dev:api",
		"bun run run:web",
		"bun run test",
		"bun run test:integration --filter x",
		"cd apps/api && bun run dev:api",
		"FOO=1 bun run dev:admin",
		"timeout 600 bun run test; echo done",
		"(bun run dev:web)",
	} {
		if d := guardDevboxOnly(hook(c)); d == nil || d.Rule != "machine:devbox-only" {
			t.Errorf("should block %q: %v", c, d)
		}
	}
	for _, c := range []string{
		"devbox run -- 'bun run dev:api'",
		"devbox run test",
		"ssh box 'bun run test'",
		"echo bun run dev:api",
		"git commit -m 'bun run test'",
		"bun run dev:mobile",
		"bun run typecheck",
		"bun test",
		"CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1 bun run dev:api",
	} {
		if d := guardDevboxOnly(hook(c)); d != nil {
			t.Errorf("should pass %q:\n%s", c, d.Text())
		}
	}
	d := guardDevboxOnly(hook("cd apps/api && bun run dev:api"))
	for _, want := range []string{"devbox run -- 'bun run dev:api'", "/devbox", "CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1"} {
		if !strings.Contains(d.Text(), want) {
			t.Errorf("message lacks %q:\n%s", want, d.Text())
		}
	}
	// An apostrophe in the command survives as one shell word in the rerun.
	d = guardDevboxOnly(hook("bun run test:integration -t 'it''s'"))
	if want := `devbox run -- 'bun run test:integration -t '\''it'\'''\''s'\'''`; d == nil || !strings.Contains(d.Text(), want) {
		t.Errorf("rerun command not single-quoted, want %s in:\n%v", want, d)
	}
	// No patterns configured: the rule is inert.
	if d := guardDevboxOnly(hookCmd("bun run dev:api")); d != nil {
		t.Errorf("unconfigured repo blocked: %s", d.Text())
	}
}
