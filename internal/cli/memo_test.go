package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/devboxguest"
	"github.com/henderson-tech/vybava/internal/memo"
	"github.com/henderson-tech/vybava/internal/runx"
)

// The --supersedes / --retires flags write the marker, accept one the author
// already wrote for the same row, and refuse one that names another row.
func TestMarkSentence(t *testing.T) {
	cases := []struct {
		name, sentence, want string
		supersedes, retires  int
		refused              bool
	}{
		{name: "flag writes the retires marker", sentence: "Fixed upstream.", retires: 7, want: "retires #7. Fixed upstream."},
		{name: "author's colon marker for the same row is kept", sentence: "retires #7: fixed upstream.", retires: 7, want: "retires #7: fixed upstream."},
		{name: "a marker for another row is refused", sentence: "retires #8: fixed upstream.", retires: 7, refused: true},
		{name: "flag writes the supersedes marker", sentence: "The new rule.", supersedes: 3, want: "supersedes #3: The new rule."},
		{name: "a malformed supersedes marker is refused", sentence: "supersedes #3 the new rule.", supersedes: 3, refused: true},
	}
	for _, c := range cases {
		got, problem, fix := markSentence(c.sentence, "", "feedback/git", c.supersedes, c.retires)
		if c.refused {
			if problem == "" || fix == "" {
				t.Errorf("%s: want a refusal with a fix, got %q", c.name, got)
			}
			continue
		}
		if problem != "" || got != c.want {
			t.Errorf("%s: got %q (%s), want %q", c.name, got, problem, c.want)
		}
	}
}

// A row's creation time is its `add` event, so add and import must stamp
// exactly one per new row and leave earlier events untouched.
func TestRecordAddedStampsOneAddEventPerRow(t *testing.T) {
	home := t.TempDir()
	env := memo.Env{Session: "sess-1"}
	now := time.Date(2026, 9, 21, 8, 0, 0, 0, time.UTC)

	first, err := recordAdded(env, home, []memo.Row{{ID: 1}, {ID: 2}}, now)
	if err != nil || len(first) != 2 {
		t.Fatalf("import of two rows: %d events, %v", len(first), err)
	}
	later, err := recordAdded(env, home, []memo.Row{{ID: 3}}, now.Add(time.Hour))
	if err != nil || len(later) != 3 {
		t.Fatalf("add after import: %d events, %v", len(later), err)
	}
	for i, e := range later {
		if e.Kind != "add" || e.Row != i+1 || e.Session != "sess-1" {
			t.Errorf("event %d = %+v", i, e)
		}
	}
	if later[0].At != now.Format(time.RFC3339) || later[2].At != now.Add(time.Hour).Format(time.RFC3339) {
		t.Errorf("timestamps: %v %v", later[0].At, later[2].At)
	}
	// Re-stamping the same row in the same session is a no-op, never a duplicate.
	again, err := recordAdded(env, home, []memo.Row{{ID: 3}}, now.Add(2*time.Hour))
	if err != nil || len(again) != 3 {
		t.Fatalf("duplicate add: %d events, %v", len(again), err)
	}
	if again[2].At != later[2].At || again[2].Row != 3 || again[2].Kind != "add" {
		t.Errorf("re-stamp must leave the original event untouched: %+v", again[2])
	}
}

// On a Devbox guest the personal home in the box's Claudik clone is read-only
// through the CLI: show reads it without recording a use, and a write refuses
// HOME_MIRROR with a fix a Mac session runs as written — every flag kept, the
// home spelled from ~ (the guest's HOME is not the Mac's).
func TestMemoMirrorHomeIsReadOnlyOnDevboxGuest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-box")
	clone := filepath.Join(home, ".claude")
	if err := os.Mkdir(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", clone, "init", "-q").CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	t.Chdir(t.TempDir())
	cwd, _ := os.Getwd()
	personal, _ := memo.Env{UserHome: home, Cwd: cwd}.SessionHomes()
	l, d, err := (memo.Env{UserHome: home, Cwd: cwd}).Open(personal, true)
	if d != nil || err != nil {
		t.Fatal(d, err)
	}
	if _, d, err := l.Append(memo.Row{Type: "feedback", Topic: "git", Sentence: "Never git stash."}); d != nil || err != nil {
		t.Fatal(d, err)
	}
	marker := filepath.Join(t.TempDir(), "runtime.env")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	save := devboxguest.Marker
	devboxguest.Marker = marker
	t.Cleanup(func() { devboxguest.Marker = save })

	run := func(args ...string) (runx.Envelope, error) {
		var out bytes.Buffer
		cmd, err := App{Stdout: &out, Stderr: io.Discard}.Command("memo")
		if err != nil {
			t.Fatal(err)
		}
		cmd.SetArgs(append(args, "--json"))
		runErr := cmd.Execute()
		var env runx.Envelope
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("memo %v: %v\n%s", args, err, out.String())
		}
		return env, runErr
	}
	env, err := run("show", "1")
	if err != nil || !env.OK {
		t.Fatalf("show must read the mirror home: %+v %v", env, err)
	}
	if env, err := run("ensure"); err != nil || !env.OK {
		t.Fatalf("ensure must pass over the mirror home: %+v %v", env, err)
	}
	if env, err := run("render", "--check"); err != nil || !env.OK {
		t.Fatalf("render --check must pass over the mirror home, never send a render fix the guest skips: %+v %v", env, err)
	}
	for _, f := range []string{memo.UsageFile, memo.IndexFile} {
		if _, err := os.Stat(filepath.Join(personal.Path, f)); !os.IsNotExist(err) {
			t.Errorf("show or ensure wrote %s into the mirror home: %v", f, err)
		}
	}
	rel, _ := filepath.Rel(home, personal.Path)
	want := "memo add feedback/git 'Use worktrees.' --link https://example.com/x --supersedes 1 --home ~/" + rel + "  # from a Mac session"
	env, err = run("add", "feedback/git", "Use worktrees.", "--supersedes", "1", "--link", "https://example.com/x")
	if err == nil || len(env.Diagnostics) != 1 || env.Diagnostics[0].Code != memo.DiagHomeMirror || !strings.HasPrefix(env.Diagnostics[0].Fix, want) {
		t.Errorf("add must refuse with the Mac re-run %q: %+v", want, env)
	}
	if env, err = run("touch", "1"); err == nil || len(env.Diagnostics) != 1 || env.Diagnostics[0].Code != memo.DiagHomeMirror {
		t.Errorf("touch must refuse HOME_MIRROR: %+v", env)
	}
}

// Mid-merge, `memo renumber` settles LEDGER.md and its `next` is the
// explicit `git add` of exactly the resolved files it wrote (the home's
// .gitignore its render created included), then the base check; a file that
// still conflicts is named for a hand resolution, never staged.
func TestMemoRenumberNextStagesExactlyWhatItWrote(t *testing.T) {
	cases := []struct {
		name                     string
		branchIgnore, mainIgnore string
		add                      string
		unmerged                 bool
	}{
		{name: "the .gitignore the render creates is staged", add: " add -- .claude/memory/LEDGER.md docs/x.md .claude/memory/.gitignore"},
		{name: "a .gitignore that still conflicts is not", branchIgnore: "*.log\n", mainIgnore: "*.tmp\n", add: " add -- .claude/memory/LEDGER.md docs/x.md", unmerged: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			repo := t.TempDir()
			home := filepath.Join(repo, ".claude", "memory")
			git := func(args ...string) error {
				out, err := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@x"}, args...)...).CombinedOutput()
				if err != nil {
					return &memo.Diag{Detail: string(out)}
				}
				return nil
			}
			write := func(rel, body string) {
				if body == "" {
					return
				}
				if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, rel)), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, rel), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			step := func(args ...string) {
				if err := git(args...); err != nil {
					t.Fatal(err)
				}
			}
			head := "---\nmemo: 1\nalias: acme-team\nkind: team\n---\n- #t1 project/api Shared fact. ^t1\n"
			step("init", "-q", "-b", "main")
			write(".claude/memory/LEDGER.md", head)
			step("add", "-A")
			step("commit", "-qm", "fork")
			step("checkout", "-qb", "work/x")
			write(".claude/memory/LEDGER.md", head+"- #t2 project/perf The branch's fact. ^t2\n")
			write(".claude/memory/.gitignore", tc.branchIgnore)
			write("docs/x.md", "Per #t2.\n")
			step("add", "-A")
			step("commit", "-qm", "branch")
			step("checkout", "-q", "main")
			write(".claude/memory/LEDGER.md", head+"- #t2 project/api Main's fact. ^t2\n")
			write(".claude/memory/.gitignore", tc.mainIgnore)
			step("add", "-A")
			step("commit", "-qm", "main")
			step("checkout", "-q", "work/x")
			if err := git("merge", "-q", "main", "-m", "merge"); err == nil {
				t.Fatal("both sides appended a row, the merge must conflict")
			}

			var out bytes.Buffer
			cmd, err := App{Stdout: &out, Stderr: io.Discard}.Command("memo")
			if err != nil {
				t.Fatal(err)
			}
			cmd.SetArgs([]string{"renumber", "--home", home, "--json"})
			runErr := cmd.Execute()
			var env runx.Envelope
			if err := json.Unmarshal(out.Bytes(), &env); err != nil || runErr != nil || !env.OK {
				t.Fatalf("renumber: %v %v\n%s", err, runErr, out.String())
			}
			want := 2
			if tc.unmerged {
				want = 3
			}
			if len(env.Next) != want || !strings.HasPrefix(env.Next[0], "git -C ") || !strings.HasSuffix(env.Next[0], tc.add) || !strings.HasPrefix(env.Next[1], "memorylint check ") {
				t.Fatalf("next = %q", env.Next)
			}
			if tc.unmerged && !strings.Contains(env.Next[2], "--diff-filter=U") {
				t.Fatalf("a conflicted .gitignore must be named for a hand resolution: %q", env.Next[2])
			}
			if data, _ := os.ReadFile(filepath.Join(repo, "docs", "x.md")); string(data) != "Per #t3.\n" {
				t.Fatalf("docs/x.md = %q, want the branch's citation moved to #t3", data)
			}
		})
	}
}
