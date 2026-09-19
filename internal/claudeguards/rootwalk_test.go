package claudeguards

import (
	"os"
	"strings"
	"testing"
)

func TestRootWalkPatterns(t *testing.T) {
	home, _ := os.UserHomeDir()
	repo := t.TempDir()
	block := []string{
		"bfs / -name home-grid-1.png -newer /tmp/hcg-e2e.log",
		"find / -name x",
		"find ~ -name x",
		"find ~/ -name x",
		"find $HOME -type f",
		`find "$HOME" -type f`,
		"find ${HOME}/ -type f",
		"find /Users -name x",
		"find /Users/someone -name x",
		"find " + home + " -name x",
		"find /Volumes -name x",
		"find /Library -name x",
		"find -L / -name x",
		"find -f / -name x",
		"fd home-grid /",
		"fd -e png home-grid ~",
		"fd --search-path / home-grid",
		"sudo find / -name x",
		"timeout 60 bfs / -name x",
		"cd /tmp && find / -name x",
		"find / -name x | head -5",
		"ls; find /Users -name x",
		"FOO=1 find / -name x",
	}
	pass := []string{
		"find " + repo + " -name x",
		"find . -name x",
		"find /tmp -name x",
		"find /Users/someone/Work -name x",
		"find / -maxdepth 2 -name x",
		"find ~ -name x -maxdepth 3",
		"bfs / -maxdepth 1 -name x",
		"fd x",
		"fd -d 3 home-grid /",
		"fd --max-depth 2 home-grid ~",
		"fd --max-depth=2 home-grid /",
		"fd -d3 home-grid /",
		"fd home-grid " + repo,
		"mdfind -name home-grid-1.png",
		"echo find / -name x",
		"grep -rn 'find /' scripts/",
		`git commit -m "guards: block find / walks"`,
		"rg -e 'bfs /' docs/",
	}
	for _, c := range block {
		if cmd, _ := rootWalkMatch(c, repo); cmd == "" {
			t.Errorf("should block %q", c)
		}
	}
	for _, c := range pass {
		if cmd, root := rootWalkMatch(c, repo); cmd != "" {
			t.Errorf("should pass %q (matched %q at %s)", c, cmd, root)
		}
	}
}

// A walker with no explicit root starts at cwd, so the same command is a
// full-disk walk from ~ and a scoped one from a repo.
func TestRootWalkImplicitRoot(t *testing.T) {
	home, _ := os.UserHomeDir()
	for _, cmd := range []string{"find -name x", "bfs -name x", "fd x"} {
		if got, root := rootWalkMatch(cmd, home); got == "" || !strings.Contains(root, home) {
			t.Errorf("%q from %s should block, got %q %q", cmd, home, got, root)
		}
		if got, _ := rootWalkMatch(cmd, "/"); got == "" {
			t.Errorf("%q from / should block", cmd)
		}
		if got, _ := rootWalkMatch(cmd, t.TempDir()); got != "" {
			t.Errorf("%q from a temp dir should pass, got %q", cmd, got)
		}
	}
}

func TestRootWalkDenialText(t *testing.T) {
	in := &HookInput{CWD: t.TempDir()}
	in.ToolInput.Command = "bfs / -name home-grid-1.png"
	d := guardRootWalk(in)
	if d == nil || d.Rule != "context:root-walk" {
		t.Fatalf("denial: %v", d)
	}
	for _, want := range []string{"bfs / -name home-grid-1.png", "-maxdepth", "mdfind -name", "21 min"} {
		if !strings.Contains(d.Text(), want) {
			t.Errorf("stderr must carry %q:\n%s", want, d.Text())
		}
	}
	in.ToolInput.Command = "bfs / -maxdepth 2 -name home-grid-1.png"
	if d := guardRootWalk(in); d != nil {
		t.Fatalf("depth-capped walk must pass: %v", d)
	}
}
