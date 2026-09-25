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
	cfg := `{"guards":{"devboxWhenWorkspace":["^bun\\s(.*\\s)?typecheck(\\s|$)","^(bunx )?tsc(\\s|$)"]}}`
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
	// devbox run starts at the checkout root: the rerun gets its cd back.
	for _, want := range []string{"fixit-work-x", `devbox run --no-up -- 'cd apps/api && bun run typecheck -- '\''a b'\'''`, "CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1"} {
		if !strings.Contains(d.Text(), want) {
			t.Errorf("message lacks %q:\n%s", want, d.Text())
		}
	}
	// ...and keeps the command's leading assignments.
	d = guardDevboxWhenWorkspace(hook("NODE_OPTIONS=--max-old-space-size=8192 bunx tsc -p tsconfig.spec.json"))
	if want := `devbox run --no-up -- 'NODE_OPTIONS=--max-old-space-size=8192 bunx tsc -p tsconfig.spec.json'`; d == nil || !strings.Contains(d.Text(), want) {
		t.Errorf("rerun dropped the assignment, want %s in:\n%v", want, d)
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
	// The command's cd / --cwd decides, never the session's cwd: from the
	// synced main clone, a typecheck that runs in the bare worktree is allowed.
	for _, c := range []string{
		"(cd .worktrees/bare && bun run typecheck)",
		"cd " + nested + " && bun run typecheck",
		"bun --cwd .worktrees/bare run typecheck",
	} {
		if d := guardDevboxWhenWorkspace(hook(c)); d != nil {
			t.Errorf("%q from the main clone runs in a bare worktree, should pass:\n%s", c, d.Text())
		}
	}
	// Only a pure && chain carries a cd to the command. After a closed
	// subshell, or behind `||`, the command may run in the session's own
	// synced checkout: refused.
	for _, c := range []string{
		"(cd .worktrees/bare && echo ok); bun run typecheck",
		"cd .worktrees/bare || bun run typecheck",
		"cd .worktrees/bare; bun run typecheck",
	} {
		if d := guardDevboxWhenWorkspace(hook(c)); d == nil || !strings.Contains(d.Text(), "fixit-work-x") {
			t.Errorf("%q may run in the synced main clone, should block: %v", c, d)
		}
	}
	// The matched command is located by position, never by its first textual
	// occurrence: an echoed copy before a pure chain does not pull the synced
	// main clone in.
	if d := guardDevboxWhenWorkspace(hook("echo 'bun run typecheck'; cd .worktrees/bare && bun run typecheck")); d != nil {
		t.Errorf("the typecheck runs only in the bare worktree, should pass:\n%s", d.Text())
	}
	// A command that runs outside every checkout has no workspace, whatever
	// the session's checkout has.
	outside := t.TempDir()
	if d := guardDevboxWhenWorkspace(hook("cd " + outside + " && bun run typecheck")); d != nil {
		t.Errorf("a typecheck outside any checkout should pass:\n%s", d.Text())
	}
	// ...until it has one of its own.
	register("fixit-bare", nested)
	if d := guardDevboxWhenWorkspace(hookAt(nested, "bun run typecheck")); d == nil || !strings.Contains(d.Text(), "fixit-bare") {
		t.Errorf("nested worktree with its own workspace should block naming fixit-bare: %v", d)
	}
	// ...and a session in another, unsynced worktree that cds into it is
	// judged by it, too.
	bare2 := filepath.Join(root, ".worktrees", "bare2")
	if err := os.MkdirAll(bare2, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{".git": "gitdir: " + filepath.Join(root, ".git", "worktrees", "bare2") + "\n", "vybava.config.json": cfg} {
		if err := os.WriteFile(filepath.Join(bare2, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if d := guardDevboxWhenWorkspace(hookAt(bare2, "bun run typecheck")); d != nil {
		t.Errorf("an unsynced worktree should pass:\n%s", d.Text())
	}
	if d := guardDevboxWhenWorkspace(hookAt(bare2, "(cd ../bare && bun run typecheck)")); d == nil || !strings.Contains(d.Text(), "fixit-bare") {
		t.Errorf("cd into a synced worktree should block naming fixit-bare: %v", d)
	}
	// A relative cd after a closed subshell is resolved from every directory
	// the shell may be in: this one really lands in the synced bare worktree.
	if d := guardDevboxWhenWorkspace(hookAt(bare2, "(cd /tmp && echo ok); cd ../bare && bun run typecheck")); d == nil || !strings.Contains(d.Text(), "fixit-bare") {
		t.Errorf("relative cd after a closed subshell should block naming fixit-bare: %v", d)
	}
	// The rerun enters the destination checkout and folds bun's --cwd into its
	// cd: devbox run starts at that checkout's root, where ../bare is wrong.
	if err := os.MkdirAll(filepath.Join(nested, "apps"), 0o755); err != nil {
		t.Fatal(err)
	}
	d = guardDevboxWhenWorkspace(hookAt(bare2, "bun --cwd ../bare/apps run typecheck"))
	if want := "(cd " + nested + " && devbox run --no-up -- 'cd apps && bun run typecheck')"; d == nil || !strings.Contains(d.Text(), want) {
		t.Errorf("rerun for --cwd into another checkout, want %s in:\n%v", want, d)
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
	// ...and the mirror: the cwd is the symlinked spelling.
	register("fixit-work-x", root)
	if d := guardDevboxWhenWorkspace(hookAt(link, "bun run typecheck")); d == nil {
		t.Error("a symlinked cwd should still match the registered checkout")
	}

	// An entry with only rendered/mutagen.yaml (no workspace.yaml, as a third
	// of real entries are) still names its checkout through the alpha path;
	// DEVBOX_WORKSPACES_DIR relocates the registry exactly as the CLI does.
	other := t.TempDir()
	t.Setenv("DEVBOX_WORKSPACES_DIR", other)
	if d := guardDevboxWhenWorkspace(hook("bun run typecheck")); d != nil {
		t.Errorf("the relocated registry is empty, should pass:\n%s", d.Text())
	}
	rendered := filepath.Join(other, "fixit-rendered-only", "rendered")
	if err := os.MkdirAll(rendered, 0o755); err != nil {
		t.Fatal(err)
	}
	mutagen := "sync:\n  defaults:\n    mode: one-way-replica\n  ws-fixit-rendered-only-api:\n    alpha: \"" + root + "\"\n    beta: \"devops:ws/fixit-rendered-only/api\"\n"
	if err := os.WriteFile(filepath.Join(rendered, "mutagen.yaml"), []byte(mutagen), 0o600); err != nil {
		t.Fatal(err)
	}
	if d := guardDevboxWhenWorkspace(hook("bun run typecheck")); d == nil || !strings.Contains(d.Text(), "fixit-rendered-only") {
		t.Errorf("a rendered-only entry should block naming its directory: %v", d)
	}

	// A quoted bun --cwd is folded into the rerun's cd as a whole word.
	register("fixit-work-x", root)
	if err := os.MkdirAll(filepath.Join(root, "sub dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	d = guardDevboxWhenWorkspace(hook("bun --cwd 'sub dir' run typecheck"))
	if want := `devbox run --no-up -- 'cd '\''sub dir'\'' && bun run typecheck'`; d == nil || !strings.Contains(d.Text(), want) {
		t.Errorf("quoted --cwd rerun, want %s in:\n%v", want, d)
	}

	// No patterns configured: the rule is inert even with a workspace.
	if d := guardDevboxWhenWorkspace(hookCmd("bun run typecheck")); d != nil {
		t.Errorf("unconfigured repo blocked: %s", d.Text())
	}
}

func TestCdsCertain(t *testing.T) {
	for prefix, want := range map[string]bool{
		"":                              true,
		"cd x && ":                      true,
		"(cd x && ":                     true,
		"echo 'a; b'; cd x && lint && ": true,
		"cd x && (":                     true,
		"cd x; ":                        false, // the cd may have failed
		"cd x || ":                      false,
		"cd x | ":                       false,
		"(cd x && echo ok); ":           false, // its subshell closed
		"(cd x && echo ok) && ":         false,
		"cd $(pwd) && ":                 false,
		"echo 'unterminated && ":        false,
		"bash -c 'cd x && ":             false,
	} {
		if got := cdsCertain(prefix); got != want {
			t.Errorf("cdsCertain(%q) = %v, want %v", prefix, got, want)
		}
	}
}
