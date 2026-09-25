package claudeguards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardDevboxWhenWorkspace(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := `{"guards":{"devboxWhenWorkspace":["^bun run typecheck(\\s|$)","^(bunx )?tsc(\\s|$)"]}}`
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	hook := func(cmd string) *HookInput {
		in := &HookInput{CWD: root}
		in.ToolInput.Command = cmd
		return in
	}
	register := func(sync string) {
		ws := filepath.Join(home, ".devbox", "workspaces", "fixit-work-x")
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		doc := "name: fixit-work-x\nport_base: 21706\napps:\n  api:\n    sync: " + sync + "\n    run: bun run dev\n  web:\n    run_in: api\n"
		if err := os.WriteFile(filepath.Join(ws, "workspace.yaml"), []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	blocked := []string{
		"bun run typecheck",
		"NODE_OPTIONS=--max-old-space-size=8192 bunx tsc -p tsconfig.spec.json --noEmit",
		"cd apps/api && bun run typecheck",
		"nice -n 10 bunx tsc",
		"(bun run typecheck)",
	}
	passing := []string{
		"devbox run --no-up -- 'bun run typecheck'",
		"ssh box 'bunx tsc'",
		"echo bunx tsc",
		"git commit -m 'bun run typecheck'",
		"bun run lint",
		"CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1 bun run typecheck",
	}

	// No workspace registered anywhere: the Mac may run every command.
	for _, c := range append(blocked, passing...) {
		if d := guardDevboxWhenWorkspace(hook(c)); d != nil {
			t.Errorf("no workspace, should pass %q:\n%s", c, d.Text())
		}
	}

	// A workspace synced to another checkout does not count.
	register(filepath.Join(home, "elsewhere"))
	if d := guardDevboxWhenWorkspace(hook("bun run typecheck")); d != nil {
		t.Errorf("foreign workspace, should pass:\n%s", d.Text())
	}

	// A workspace synced to this checkout routes the listed commands.
	register(`"` + root + `"`)
	for _, c := range blocked {
		if d := guardDevboxWhenWorkspace(hook(c)); d == nil || d.Rule != "machine:devbox-workspace" {
			t.Errorf("should block %q: %v", c, d)
		}
	}
	for _, c := range passing {
		if d := guardDevboxWhenWorkspace(hook(c)); d != nil {
			t.Errorf("should pass %q:\n%s", c, d.Text())
		}
	}
	// A subdirectory of the synced checkout is the same workspace.
	sub := filepath.Join(root, "apps", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := devboxWorkspaceFor(sub); got != "fixit-work-x" {
		t.Errorf("subdirectory lookup = %q, want fixit-work-x", got)
	}
	d := guardDevboxWhenWorkspace(hook("cd apps/api && bun run typecheck"))
	for _, want := range []string{"fixit-work-x", "devbox run --no-up -- 'bun run typecheck'", "CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1"} {
		if !strings.Contains(d.Text(), want) {
			t.Errorf("message lacks %q:\n%s", want, d.Text())
		}
	}
	// No patterns configured: the rule is inert even with a workspace.
	if d := guardDevboxWhenWorkspace(hookCmd("bun run typecheck")); d != nil {
		t.Errorf("unconfigured repo blocked: %s", d.Text())
	}
}
