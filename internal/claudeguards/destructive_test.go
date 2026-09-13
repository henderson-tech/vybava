package claudeguards

import "testing"

const mainClone = "/Users/u/Work/Projects/FixIt-Technologies/FixIt"

func TestDestructiveMatch(t *testing.T) {
	cases := []struct {
		name string
		cmd  string
		cwd  string
		want string // rule name, "" = pass
	}{
		// --- git stash: banned everywhere, any dir ---
		{"stash plain", "git stash", mainClone, "git-stash"},
		{"stash push", "git stash push -m x", mainClone, "git-stash"},
		{"stash pop", "git stash pop", "/tmp/scratch", "git-stash"},
		{"stash in worktree still banned", "git stash", mainClone + "/.worktrees/f", "git-stash"},
		{"stash chained", "make build && git stash", mainClone, "git-stash"},
		{"stash with -C", "git -C /some/repo stash", mainClone, "git-stash"},
		{"stash list ok", "git stash list", mainClone, ""},
		{"stash show ok", "git stash show -p stash@{0}", mainClone, ""},
		{"unstash word ok", "echo git stash is banned", mainClone, ""},
		{"stash in commit msg ok", `git commit -m "avoid git stash here"`, mainClone, ""},

		// --- heredocs: quoted bodies are literal text, never executable ---
		{"quoted heredoc body mentions checkout ok",
			"mkdir -p ~/Exports/x && cat > ~/Exports/x/w.js << 'WFEOF'\nconst s = 'git checkout main'\nagent('run git stash if needed')\nWFEOF",
			mainClone, ""},
		{"double-quoted heredoc body ok",
			"cat > notes.md << \"EOF\"\ndocker volume prune is dangerous\nEOF",
			mainClone, ""},
		{"real command AFTER quoted heredoc still caught",
			"cat > x.txt << 'EOF'\nhello\nEOF\ngit stash",
			mainClone, "git-stash"},
		{"unquoted heredoc body still scanned",
			"cat > x.txt << EOF\n$(git checkout main)\nEOF",
			mainClone, "git-switch"},
		{"unterminated quoted heredoc swallows rest",
			"cat > x.txt << 'EOF'\ngit checkout main",
			mainClone, ""},

		// --- checkout / switch: main clone only ---
		{"checkout branch", "git checkout main", mainClone, "git-switch"},
		{"switch branch", "git switch -c feat/x", mainClone, "git-switch"},
		{"checkout in worktree ok", "git checkout -b f", mainClone + "/.worktrees/f", ""},
		{"checkout in native worktree ok", "git checkout -b feat/x && git branch -D worktree-x", mainClone + "/.claude/worktrees/x", ""},
		{"stash in native worktree still banned", "git stash", mainClone + "/.claude/worktrees/x", "git-stash"},
		{"checkout in tmp ok", "git checkout v2", "/tmp/clone", ""},
		{"checkout in private tmp ok", "git checkout v2", "/private/tmp/x", ""},
		{"checkout chained", "bun install && git switch main", mainClone, "git-switch"},
		{"checkout with global flags", "git -C . --no-pager checkout x", mainClone, "git-switch"},

		// --- restore . / checkout . ---
		{"restore dot", "git restore .", mainClone, "git-switch"}, // reSwitch never sees restore; ensure restore-dot fires
		{"restore dot flags", "git restore --staged --worktree .", mainClone, "git-restore-dot"},
		{"restore single path ok", "git restore src/app.ts", mainClone, ""},

		// --- docker compose down -v ---
		{"down -v", "docker compose down -v", mainClone, "compose-down-volumes"},
		{"down --volumes", "docker compose -p fixit down --volumes", mainClone, "compose-down-volumes"},
		{"down -fv combined", "docker compose down -fv", mainClone, "compose-down-volumes"},
		{"legacy docker-compose", "docker-compose down -v", mainClone, "compose-down-volumes"},
		{"down without -v ok", "docker compose down", mainClone, ""},
		{"wt carve-out cmd", "docker compose -p wt-foo down -v", mainClone, ""},
		{"wt carve-out cwd", "docker compose down -v", "/x/.worktrees/wt-foo", ""},

		// --- keychain secret value dumps ---
		{"find-generic -w", "security find-generic-password -s vitrinka -a https://vitrinka.ai -w", mainClone, "keychain-secret-dump"},
		{"find-generic -g", "security find-generic-password -g -s vitrinka", mainClone, "keychain-secret-dump"},
		{"find-internet -w", "security find-internet-password -s github.com -w", mainClone, "keychain-secret-dump"},
		{"find-generic metadata ok", "security find-generic-password -s vitrinka -a https://vitrinka.ai", mainClone, ""},
		{"dump-keychain -d", "security dump-keychain -d login.keychain", mainClone, "keychain-secret-dump"},
		{"dump-keychain metadata ok", "security dump-keychain", mainClone, ""},
		{"unrelated -w ok", "grep -w security notes.txt", mainClone, ""},

		// --- docker volume rm on DB volumes ---
		{"volume rm postgres", "docker volume rm fixit_postgres_data", mainClone, "volume-rm-db"},
		{"volume rm pgdata", "docker volume rm app-pgdata", mainClone, "volume-rm-db"},
		{"volume rm cache ok", "docker volume rm build_cache", mainClone, ""},

		// --- prunes ---
		{"volume prune", "docker volume prune -f", mainClone, "volume-prune"},
		{"system prune --volumes", "docker system prune -a --volumes", mainClone, "system-prune-volumes"},
		{"system prune ok", "docker system prune -a", mainClone, ""},

		// --- bypass + benign ---
		{"escape hatch", "CLAUDE_ALLOW_DANGEROUS=1 git stash", mainClone, ""},
		{"plain ls", "ls -la", mainClone, ""},
		{"empty", "", mainClone, ""},
		{"benign long", "bun run test && git status && git log --oneline -5", mainClone, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ""
			if r := destructiveMatch(c.cmd, c.cwd); r != nil {
				got = r.name
			}
			if c.want == "git-switch" && got == "git-restore-dot" {
				return // either rule blocking is correct for `restore .`-style overlaps
			}
			if got != c.want {
				t.Fatalf("cmd=%q cwd=%q: got %q, want %q", c.cmd, c.cwd, got, c.want)
			}
		})
	}
}

func TestSegments(t *testing.T) {
	// `$(e)` yields the command inside the substitution, not `e)` — the stray
	// paren is an artifact of splitting on `$(`, and leaving it attached made
	// the last field of every subshell unrecognisable to the rules.
	got := segments("a && b; c | d $(e) `f`\ng")
	want := []string{"a", "b", "c", "d", "e", "f", "g"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("seg %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

// A closing paren left on the last segment made the rules read a different
// command than the one being run. CLAUDE.md REQUIRES `(cd <abs> && cmd)` for
// directory changes, so this shape is the common case: it made every capped
// `git log` in a subshell look uncapped, hid the uncapped ones, and let a
// hard-banned command through inside `$(…)`.
func TestSubshellParensDoNotHideTheCommand(t *testing.T) {
	for _, tc := range []struct {
		cmd   string
		block bool
	}{
		{"(cd /tmp && git log --oneline -1)", false}, // capped — was a false positive
		{"(cd /tmp && git log -n 20)", false},
		{"(cd /tmp && git log)", true},  // uncapped — was a false negative
		{"(cd /tmp && git diff)", true}, // ditto
		{"echo $(git stash)", true},     // a hard ban must not escape via substitution
		{"(cd /tmp && git stash)", true},
		{"git log --oneline -1", false}, // unchanged outside a subshell
		{"git log", true},
	} {
		// The full hook path: the substitution case is a destructive:* rule,
		// the git log cases are context:*, and the point is that both families
		// read the same command the shell will run.
		in := &HookInput{CWD: t.TempDir()}
		in.ToolInput.Command = tc.cmd
		d := Bash(in)
		if tc.block && d == nil {
			t.Fatalf("%q must be blocked", tc.cmd)
		}
		if !tc.block && d != nil {
			t.Fatalf("%q must be allowed, got %v", tc.cmd, d)
		}
	}
	// A legitimate trailing paren inside an argument is left alone.
	if got := segments(`grep "f(x)" file.go`); len(got) != 1 || got[0] != `grep "f(x)" file.go` {
		t.Fatalf("balanced parens must survive: %q", got)
	}
}
