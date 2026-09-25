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
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	hookAt := func(cwd, cmd string) *HookInput {
		in := &HookInput{CWD: cwd}
		in.ToolInput.Command = cmd
		return in
	}
	hook := func(cmd string) *HookInput { return hookAt(root, cmd) }
	register := func(name, sync string) {
		ws := filepath.Join(home, ".devbox", "workspaces", name)
		if err := os.MkdirAll(ws, 0o755); err != nil {
			t.Fatal(err)
		}
		doc := "name: " + name + "\nport_base: 21706\napps:\n  api:\n    sync: " + sync + "\n    run: bun run dev\n  web:\n    run_in: api\n"
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
	register("fixit-work-x", filepath.Join(home, "elsewhere"))
	if d := guardDevboxWhenWorkspace(hook("bun run typecheck")); d != nil {
		t.Errorf("foreign workspace, should pass:\n%s", d.Text())
	}

	// A workspace synced to this checkout routes the listed commands.
	register("fixit-work-x", `"`+root+`"`)
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
	// A subdirectory of the checkout is the same checkout.
	sub := filepath.Join(root, "apps", "api")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := checkoutRoot(sub); got != root {
		t.Errorf("checkoutRoot(%s) = %q, want %q", sub, got, root)
	}
	d := guardDevboxWhenWorkspace(hookAt(sub, "bun run typecheck -- 'a b'"))
	if d == nil {
		t.Fatal("subdirectory of a synced checkout should block")
	}
	for _, want := range []string{"fixit-work-x", `devbox run --no-up -- 'bun run typecheck -- '\''a b'\'''`, "CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1"} {
		if !strings.Contains(d.Text(), want) {
			t.Errorf("message lacks %q:\n%s", want, d.Text())
		}
	}

	// A worktree nested inside the synced checkout is its own checkout: the
	// main clone's workspace never routes a bare .worktrees/<name>.
	nested := filepath.Join(root, ".worktrees", "bare")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("gitdir: "+filepath.Join(root, ".git", "worktrees", "bare")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A worktree carries the repo's tracked config like any checkout.
	if err := os.WriteFile(filepath.Join(nested, "vybava.config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := checkoutRoot(filepath.Join(nested, "apps")); got != nested {
		t.Errorf("checkoutRoot(nested) = %q, want %q", got, nested)
	}
	if d := guardDevboxWhenWorkspace(hookAt(nested, "bun run typecheck")); d != nil {
		t.Errorf("nested bare worktree inherited the main clone's workspace:\n%s", d.Text())
	}
	// ...until it has one of its own.
	register("fixit-bare", nested)
	if d := guardDevboxWhenWorkspace(hookAt(nested, "bun run typecheck")); d == nil || !strings.Contains(d.Text(), "fixit-bare") {
		t.Errorf("nested worktree with its own workspace should block naming fixit-bare: %v", d)
	}

	// The registry may hold a symlinked spelling of the checkout.
	link := filepath.Join(home, "link-to-root")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	register("fixit-work-x", link)
	if d := guardDevboxWhenWorkspace(hook("bun run typecheck")); d == nil {
		t.Error("symlinked registry path should still match the checkout")
	}

	// No patterns configured: the rule is inert even with a workspace.
	if d := guardDevboxWhenWorkspace(hookCmd("bun run typecheck")); d != nil {
		t.Errorf("unconfigured repo blocked: %s", d.Text())
	}
}
