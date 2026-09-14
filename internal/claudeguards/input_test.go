package claudeguards

import (
	"slices"
	"testing"
)

// Quoting decides whether text is a command or an argument. These cases come
// from the field audit: every "want absent" line was a real denial of a command
// that runs nothing of the kind, and every "want present" line is a ban that
// must survive the fix.
func TestSegmentsQuoting(t *testing.T) {
	cases := []struct {
		name    string
		cmd     string
		present []string // segments that must be inspected
		absent  []string // text that must NOT be treated as a command
	}{
		{"alternation in a grep pattern is not a pipe",
			`grep -nE "vault|env|path" a.ts`,
			[]string{`grep -nE "vault|env|path" a.ts`}, []string{"env", "path"}},
		{"semicolon in a sed script does not split",
			`sed "s/SECRET.*/x/;s/KEY=.*/y/" f`,
			nil, []string{"s/KEY=.*/y/"}},
		{"semicolon in a commit message does not split",
			`git commit -m 'fix: stop the crash; git stash was the cause'`,
			nil, []string{"git stash was the cause"}},
		{"single quotes suppress control operators",
			`echo 'a && b || c'`, nil, []string{"b", "c"}},

		// Command substitution still expands inside double quotes, so it must
		// keep splitting there — this is what PR #60 closed.
		{"substitution splits outside quotes",
			"echo $(git stash)", []string{"git stash"}, nil},
		{"substitution splits inside double quotes",
			`echo "$(git stash)"`, []string{"git stash"}, nil},
		{"backtick substitution splits",
			"echo `git stash`", []string{"git stash"}, nil},
		{"subshell wrapper is trimmed",
			"(cd /tmp && git log -n 20)", []string{"git log -n 20"}, nil},

		// A quoted payload handed to a runner is a command line elsewhere.
		{"ssh payload is scanned",
			`ssh h 'env'`, []string{"env"}, nil},
		{"nested runner payload is scanned",
			`docker exec c sh -c "env"`, []string{"env"}, nil},
		{"runner payload keeps its own quoting",
			`ssh h 'grep -nE "a|env|b" f'`,
			[]string{`grep -nE "a|env|b" f`}, []string{"env"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := segments(tc.cmd)
			for _, want := range tc.present {
				if !slices.Contains(got, want) {
					t.Errorf("segments(%q) = %q, missing %q", tc.cmd, got, want)
				}
			}
			for _, bad := range tc.absent {
				if slices.Contains(got, bad) {
					t.Errorf("segments(%q) = %q, must not contain %q", tc.cmd, got, bad)
				}
			}
		})
	}
}

// A runner's quoted argument is a command; every other command's is data.
func TestRunnerPayloads(t *testing.T) {
	cases := []struct {
		name string
		seg  string
		want []string
	}{
		{"ssh", `ssh h 'env'`, []string{"env"}},
		{"assignment prefix does not hide the runner", `FOO=1 sh -c 'env'`, []string{"env"}},
		{"absolute path runner", `/usr/bin/ssh h 'env'`, []string{"env"}},
		{"grep is not a runner", `grep -nE "a|env" f`, nil},
		{"commit message is not a payload", `git commit -m 'env'`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runnerPayloads(tc.seg); !slices.Equal(got, tc.want) {
				t.Errorf("runnerPayloads(%q) = %q, want %q", tc.seg, got, tc.want)
			}
		})
	}
}

// An environment assignment prefixes a command; it is not one. Leaving it in
// place let `FOO=1 env` walk past every rule that reads the first token.
func TestTrimAssignments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no assignment", "env", "env"},
		{"valueless", "FOO= env", "env"},
		{"single", "FOO=1 env", "env"},
		{"stacked", "FOO=1 BAR=2 env", "env"},
		{"quoted value with a space", `FOO='a b' env`, "env"},
		{"double-quoted value", `FOO="a b" printenv`, "printenv"},
		{"assignment only runs nothing", "FOO=bar", ""},
		{"trailing assignment is an argument", "env FOO=bar make build", "env FOO=bar make build"},
		{"not an assignment", "git commit -m x=y", "git commit -m x=y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := trimAssignments(tc.in); got != tc.want {
				t.Errorf("trimAssignments(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Recursion through nested runners is bounded but must still reach the depths
// a real incident uses.
func TestSegmentsNestedRunnerDepth(t *testing.T) {
	cmd := `ssh a 'ssh b "sudo docker exec c env"'`
	if got := segments(cmd); !slices.Contains(got, "sudo docker exec c env") {
		t.Errorf("segments(%q) = %q, missing the innermost command", cmd, got)
	}
}
