package claudeguards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// heavyTree lays out parent/repo with a .git, a node_modules and a source dir.
func heavyTree(t *testing.T) (parent, repo string) {
	t.Helper()
	parent = t.TempDir()
	repo = filepath.Join(parent, "repo")
	for _, d := range []string{".git", "node_modules/pkg/lib", "src"} {
		if err := os.MkdirAll(filepath.Join(repo, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return parent, repo
}

func TestHeavyWalkPatterns(t *testing.T) {
	parent, repo := heavyTree(t)
	block := []string{
		"find . -name a.ts",
		"find -name a.ts",
		"bfs . -name a.ts",
		"find . -not -path '*/node_modules/*' -name a.ts",
		"find . -name a.ts | head -5",
		"find . -name src -prune -o -not -path '*/node_modules/*' -print",
		"bfs . -exclude -name src -name node_modules",
		"fd -u a.ts .",
		"fd --no-ignore a.ts",
		"find " + parent + " -path '*/.worktrees/*' -prune -o -name devbox.yaml -print",
		"find " + parent + " -name node_modules -prune -o -name a.ts -print",
	}
	pass := []string{
		"find . -name node_modules -prune -o -name a.ts -print",
		"find . -maxdepth 2 -name a.ts",
		"find src -name a.ts",
		"bfs . -exclude -name node_modules -name a.ts",
		"find node_modules/pkg -name '*.js'",
		"fd a.ts",
		"fd -u -E node_modules a.ts .",
		"rg --files -g a.ts .",
		"find " + parent + " \\( -name .git -o -name node_modules \\) -prune -o -name a.ts -print",
		"find $SOMEWHERE -name a.ts",
		"find /does/not/exist -name a.ts",
		"echo find . -name a.ts",
	}
	for _, c := range block {
		if cmd, _ := heavyWalkMatch(c, repo); cmd == "" {
			t.Errorf("should block %q", c)
		}
	}
	for _, c := range pass {
		if cmd, heavy := heavyWalkMatch(c, repo); cmd != "" {
			t.Errorf("should pass %q (matched %q at %s)", c, cmd, heavy)
		}
	}
}

func TestHeavyWalkDenialText(t *testing.T) {
	_, repo := heavyTree(t)
	in := &HookInput{CWD: repo}
	in.ToolInput.Command = "find . -name a.ts"
	d := guardHeavyWalk(in)
	if d == nil || d.Rule != "context:heavy-walk" {
		t.Fatalf("denial: %v", d)
	}
	for _, want := range []string{filepath.Join(repo, "node_modules"), "rg --files", "-name node_modules -prune", "-maxdepth"} {
		if !strings.Contains(d.Text(), want) {
			t.Errorf("stderr must carry %q:\n%s", want, d.Text())
		}
	}
}

// Codex gets the safety rules and not the ones that read a Claude transcript
// or budget: a long cat is Claude's context problem, a stash is everyone's.
func TestCodexRunsSafetyRulesOnly(t *testing.T) {
	_, repo := heavyTree(t)
	big := filepath.Join(repo, "src", "big.ts")
	if err := os.WriteFile(big, []byte(strings.Repeat("x\n", 600)), 0o644); err != nil {
		t.Fatal(err)
	}
	for cmd, rule := range map[string]string{"git stash": "destructive:git-stash", "find . -name a.ts": "context:heavy-walk"} {
		in := &HookInput{CWD: repo}
		in.ToolInput.Command = cmd
		if d := Codex(in); d == nil || d.Rule != rule {
			t.Errorf("Codex(%q) = %v, want %s", cmd, d, rule)
		}
	}
	in := &HookInput{CWD: repo}
	in.ToolInput.Command = "cat " + big
	if d := Bash(in); d == nil {
		t.Fatalf("Bash must block the whole-file dump the Codex side skips")
	}
	if d := Codex(in); d != nil {
		t.Errorf("Codex must not run the context-budget rules: %v", d)
	}
}
