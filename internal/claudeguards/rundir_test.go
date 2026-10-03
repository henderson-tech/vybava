package claudeguards

import (
	"slices"
	"testing"

	"github.com/henderson-tech/vybava/internal/shellseg"
)

func TestRunDirs(t *testing.T) {
	const m = "/Users/u/Work/Projects/R/r"
	wt := m + "/.worktrees/h"
	cases := []struct {
		name, cmd, cwd string
		seg, dir       string // the segment looked up and where it must run
		known          bool
	}{
		{"cwd", "git checkout main", m, "git checkout main", m, true},
		{"cd absolute &&", "cd " + wt + " && git fetch -q origin x && git checkout -q --detach origin/x", m, "git checkout -q --detach origin/x", wt, true},
		{"cd relative &&", "cd .worktrees/h && git checkout x", m, "git checkout x", wt, true},
		{"cd quoted", `cd "` + wt + `" && git checkout x`, m, "git checkout x", wt, true},
		{"cd with option", "cd -P .worktrees/h && git checkout x", m, "git checkout x", wt, true},
		{"cd ;", "cd .worktrees/h; git checkout x", m, "git checkout x", m, false},
		{"cd ||", "cd .worktrees/h || git checkout x", m, "git checkout x", m, false},
		{"cd newline", "cd .worktrees/h\ngit checkout x", m, "git checkout x", m, false},
		{"chain broken after the cd", "cd .worktrees/h && ls; git checkout x", m, "git checkout x", m, false},
		{"break before the cd is fine", "ls; cd .worktrees/h && git checkout x", m, "git checkout x", wt, true},
		{"absolute cd re-establishes", "cd a; cd " + wt + " && git checkout x", m, "git checkout x", wt, true},
		{"relative cd on an unknown base", "cd a; cd .worktrees/h && git checkout x", m, "git checkout x", m, false},
		{"cd variable", "cd .worktrees/$B && git checkout x", m, "git checkout x", m, false},
		{"cd -", "cd - && git checkout x", wt, "git checkout x", wt, false},
		{"pushd", "pushd .worktrees/h && git checkout x", m, "git checkout x", m, false},
		{"cd in a pipeline runs in a subshell", "echo | cd .worktrees/h && git checkout x", m, "git checkout x", m, true},
		{"subshell in a pipeline keeps its cd", "echo | (cd .worktrees/h && git checkout x)", m, "git checkout x", wt, true},
		{"subshell cd applies inside", "(cd .worktrees/h && git checkout x)", m, "git checkout x", wt, true},
		{"subshell cd ends at its paren", "(cd .worktrees/h && bun test); git checkout x", m, "git checkout x", m, true},
		{"quoted paren is not a close", "cd .worktrees/h && (cd " + m + ` && echo ")" && git checkout x)`, m, "git checkout x", m, true},
		{"substitution cd ends at its paren", `echo "$(cd .worktrees/h && pwd)" && git checkout x`, m, "git checkout x", m, true},
		{"-C literal", "git -C " + wt + " checkout x", m, "git -C " + wt + " checkout x", wt, true},
		{"-C relative", "git -C .worktrees/h checkout x", m, "git -C .worktrees/h checkout x", wt, true},
		{"-C among globals", "git -C .worktrees/h -c core.pager=cat --no-pager checkout x", m, "git -C .worktrees/h -c core.pager=cat --no-pager checkout x", wt, true},
		{"-C variable", "W=" + wt + "; git -C $W checkout x", m, "git -C $W checkout x", m, false},
		{"-C relative after a cd", "cd .worktrees/h && git -C ../.. checkout x", m, "git -C ../.. checkout x", m, true},
		{"payload starts where the runner runs", "cd .worktrees/h && bash -c 'git checkout x'", m, "git checkout x", wt, true},
		{"cd inside a payload", "bash -c 'cd .worktrees/h && git checkout x'", m, "git checkout x", wt, true},
		{"cd inside a payload stays inside", "bash -c 'cd .worktrees/h' && git checkout x", m, "git checkout x", m, true},
		{"remote payload", "ssh box 'cd " + wt + " && git checkout x'", m, "git checkout x", m, false},
		{"worktree cwd back into the clone", "cd ../.. && git checkout main", m + "/.worktrees/f", "git checkout main", m, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, rs := range runDirs(c.cmd, c.cwd) {
				if rs.text != c.seg {
					continue
				}
				if rs.dir != c.dir || rs.known != c.known {
					t.Fatalf("%q in %q: got dir=%q known=%v, want dir=%q known=%v", c.seg, c.cmd, rs.dir, rs.known, c.dir, c.known)
				}
				return
			}
			t.Fatalf("segment %q not listed for %q", c.seg, c.cmd)
		})
	}
}

// RunDirs is the attribution walk: every literal cd taken as applied, so a
// `;`, `||` or newline keeps its directory unless the cd ran in its own
// subshell, and Moved tells a segment a cd or -C put in its directory from
// one that just started there.
func TestRunDirsForAttributionTakesEveryLiteralCdAsApplied(t *testing.T) {
	for _, c := range []struct {
		cmd, seg, dir string
		moved         bool
	}{
		{"make", "make", "/w/app", false},
		{"cd /w/lib; make", "make", "/w/lib", true},
		{"cd /w/lib\nmake", "make", "/w/lib", true},
		{"cd /w/lib || exit 1; make", "make", "/w/lib", true},
		{"cd /w/app && git commit -am x", "git commit -am x", "/w/app", true},
		{"git -C /w/app commit -am x", "git -C /w/app commit -am x", "/w/app", true},
		{"cd /w/lib && bash -c 'make'", "make", "/w/lib", true},
		{"(cd /w/lib; make); git commit", "git commit", "/w/app", false},
		{"cd /w/lib | cat; make", "make", "/w/app", false},
		{"cd /w/lib & make", "make", "/w/app", false},
		{"cd $W; make", "make", "/w/app", false},
	} {
		segs := RunDirs(c.cmd, "/w/app")
		i := slices.IndexFunc(segs, func(rs RunSeg) bool { return rs.Text == c.seg })
		if i < 0 {
			t.Errorf("segment %q not listed for %q", c.seg, c.cmd)
			continue
		}
		if rs := segs[i]; rs.Dir != c.dir || rs.Moved != c.moved {
			t.Errorf("%q in %q: dir=%q moved=%v, want dir=%q moved=%v", c.seg, c.cmd, rs.Dir, rs.Moved, c.dir, c.moved)
		}
	}
}

// runDirs is the destructive family's iterator, so it must list exactly what
// shellseg.Segments lists or a ban hides in the difference.
func TestRunDirsListsSegments(t *testing.T) {
	for _, cmd := range []string{
		"cd .worktrees/h && git checkout x",
		"(cd /w && a); b | c || d; e &",
		`echo "$(cd /w && pwd)" && git checkout x`,
		"bash -c 'cd /w && git checkout x' && ssh box 'git stash'",
		"cat > f << 'EOF'\ngit checkout main\nEOF\nls",
		"FOO=1 git -C /w checkout x; X=$(git rev-parse HEAD)",
	} {
		var got []string
		for _, rs := range runDirs(cmd, "/m") {
			got = append(got, rs.text)
		}
		if want := shellseg.Segments(cmd); !slices.Equal(got, want) {
			t.Errorf("%q: runDirs lists %q, Segments %q", cmd, got, want)
		}
	}
}
