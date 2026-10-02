package memo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testdata/renumber holds the real collision of 2026-10-02: FixIt's team
// ledger at the vt-4229 branch's fork point (fork.md, t325-t329), the branch
// before it merged main (branch.md, its own t330-t339), main (main.md,
// t330-t362; #1765 had landed the branch's t330-t334 there as t353-t357)
// and the branch after Lukáš settled it by hand (landed.md: main, then the
// branch's t335-t339 as t363-t367). Rows t325 on, sentences replaced by a
// hash of the original (FixIt is private, this repo is public), so equal
// facts stay equal and ids, types, topics and supersede markers are real.
func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "renumber", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// conflicted is what `git merge` leaves in LEDGER.md for the real fork.
func conflicted(t *testing.T, style string) []byte {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"fork.md", "branch.md", "main.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), fixture(t, name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"merge-file", "-p", "-L", "HEAD", "-L", "fork", "-L", "origin/main"}
	if style != "" {
		args = append(args, "--"+style)
	}
	cmd := exec.Command("git", append(args, "branch.md", "fork.md", "main.md")...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if exit, ok := err.(*exec.ExitError); err != nil && (!ok || exit.ExitCode() < 1) {
		t.Fatalf("git merge-file: %v", err)
	}
	if !bytes.Contains(out, []byte("<<<<<<< HEAD")) {
		t.Fatalf("the real fork no longer conflicts:\n%s", out)
	}
	return out
}

func TestRebaseRowsSettlesTheRealCollision(t *testing.T) {
	folded := []Move{
		{From: 330, To: 353, Topic: "project/marketplace", Folded: true},
		{From: 331, To: 354, Topic: "project/release", Folded: true},
		{From: 332, To: 355, Topic: "project/git", Folded: true},
		{From: 333, To: 356, Topic: "project/mobile", Folded: true},
		{From: 334, To: 357, Topic: "project/marketplace", Folded: true},
		{From: 335, To: 363, Topic: "project/skia"},
		{From: 336, To: 364, Topic: "project/appium"},
		{From: 337, To: 365, Topic: "project/mobile"},
		{From: 338, To: 366, Topic: "project/skia"},
		{From: 339, To: 367, Topic: "project/perf"},
	}
	// "Accept both" in an editor: the branch's rows, then main's from t330.
	main := fixture(t, "main.md")
	keepBoth := append(append([]byte{}, fixture(t, "branch.md")...), main[bytes.Index(main, []byte("- #t330 ")):]...)
	cases := []struct {
		name   string
		base   string
		branch []byte
		want   string
		moves  []Move
	}{
		{"the branch before it merged main", "main.md", fixture(t, "branch.md"), "landed.md", folded},
		{"git's conflict markers mid-merge", "main.md", conflicted(t, ""), "landed.md", folded},
		{"zdiff3 conflict markers mid-merge", "main.md", conflicted(t, "zdiff3"), "landed.md", folded},
		{"a keep-both resolution with duplicate ids", "main.md", keepBoth, "landed.md", folded},
		{"the hand-settled branch is already settled", "main.md", fixture(t, "landed.md"), "landed.md", []Move{}},
		{"its own fork point moves nothing", "fork.md", fixture(t, "branch.md"), "branch.md", []Move{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, d := RebaseRows(fixture(t, tc.base), tc.branch)
			if d != nil {
				t.Fatalf("diag: %+v", d)
			}
			if want := fixture(t, tc.want); !bytes.Equal(got.Text, want) {
				t.Fatalf("settled ledger differs from %s:\n%s", tc.want, got.Text)
			}
			if !reflect.DeepEqual(got.Moves, tc.moves) {
				t.Fatalf("moves = %+v, want %+v", got.Moves, tc.moves)
			}
			if len(got.Doubly) > 0 {
				t.Fatalf("doubly closed: %v", got.Doubly)
			}
		})
	}
}

func TestRebaseRowsMovesTheBranchsOwnCitations(t *testing.T) {
	head := "---\nmemo: 1\nalias: acme-team\nkind: team\n---\n- #t1 project/base Shared fact. ^t1\n"
	base := head + "- #t2 project/main Main fact. ^t2\n- #t3 project/main supersedes #t1: Main replaces it. ^t3\n"
	branch := head +
		"- #t2 project/perf Branch fact cites #t1 and acme-team#t2 and other-team#t2. ^t2\n" +
		"- #t3 project/perf supersedes #t2: Branch fact again. -> [[LEDGER#^t2]] [[acme-team/LEDGER#^t2]] [[other-team/LEDGER#^t2]] ^t3\n" +
		"- #t4 project/perf supersedes #t1: Branch also replaces it. ^t4\n"
	got, d := RebaseRows([]byte(base), []byte(branch))
	if d != nil {
		t.Fatalf("diag: %+v", d)
	}
	want := base +
		"- #t4 project/perf Branch fact cites #t1 and acme-team#t2 and other-team#t2. ^t4\n" +
		"- #t5 project/perf supersedes #t4: Branch fact again. -> [[LEDGER#^t4]] [[acme-team/LEDGER#^t4]] [[other-team/LEDGER#^t2]] ^t5\n" +
		"- #t6 project/perf supersedes #t1: Branch also replaces it. ^t6\n"
	if string(got.Text) != want {
		t.Fatalf("settled:\n%s\nwant:\n%s", got.Text, want)
	}
	if want := []string{"#t1 is closed by #t3 and #t6"}; !reflect.DeepEqual(got.Doubly, want) {
		t.Fatalf("doubly = %v, want %v", got.Doubly, want)
	}
}

func TestRewriteRefs(t *testing.T) {
	moves := map[int]int{330: 336, 336: 342}
	cases := []struct{ in, want string }{
		{"see #t330.", "see #t336."},
		{"supersedes #t330: x", "supersedes #t336: x"},
		{"(#t330, #t336)", "(#t336, #t342)"},
		{"-> [[LEDGER#^t330]] ^t336", "-> [[LEDGER#^t336]] ^t342"},
		{"[[acme-team/LEDGER#^t330]] acme-team#t330", "[[acme-team/LEDGER#^t336]] acme-team#t336"},
		{"[[other-team/LEDGER#^t330]] other-team#t330", "[[other-team/LEDGER#^t330]] other-team#t330"},
		{"PR#t330 a/#t330 &#t330 ##t330 #t3301 #t33", "PR#t330 a/#t330 &#t330 ##t330 #t3301 #t33"},
		{"#330 ^m330", "#330 ^m330"},
	}
	for _, tc := range cases {
		if got, _ := RewriteRefs(tc.in, moves, "acme-team"); got != tc.want {
			t.Errorf("RewriteRefs(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRenumberSettlesAMergeOfTheRealCollision replays the real fork in a
// repository: the merge conflicts in LEDGER.md, renumber settles it, moves
// the citations the branch wrote (a doc, a note, one line of a file both
// sides edited) and leaves main's citations of the same ids alone.
func TestRenumberSettlesAMergeOfTheRealCollision(t *testing.T) {
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@x", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@x"} {
		t.Setenv(k, v)
	}
	repo := t.TempDir()
	home := filepath.Join(repo, ".claude", "memory")
	run := func(args ...string) string {
		t.Helper()
		out, err := git(repo, args...)
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return out
	}
	write := func(rel string, data []byte) {
		t.Helper()
		path := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(rel string) string {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(repo, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	run("init", "-q", "-b", "main")
	write(".claude/memory/LEDGER.md", fixture(t, "fork.md"))
	write(".claude/memory/.gitignore", []byte("MEMORY.md\nusage.jsonl\n"))
	write("docs/shared.md", []byte("# Shared\n\nmiddle\n"))
	run("add", "-A")
	run("commit", "-qm", "fork")
	run("checkout", "-qb", "work/perf")
	write(".claude/memory/LEDGER.md", fixture(t, "branch.md"))
	write(".claude/memory/notes/skia-picture.md", []byte("Folds into #t330, extends #t338.\n"))
	write("docs/perf.md", []byte("Judge 120 Hz by framestats (#t339), not #t329.\n"))
	write("docs/shared.md", []byte("branch cites #t335\n# Shared\n\nmiddle\n"))
	run("add", "-A")
	run("commit", "-qm", "branch rows")
	usage := `{"row":335,"kind":"cite","at":"2026-10-01T10:00:00Z","session":"s1"}` + "\n" + `{"row":329,"kind":"cite","at":"2026-10-01T10:00:00Z","session":"s1"}` + "\n"
	write(".claude/memory/usage.jsonl", []byte(usage))
	run("checkout", "-q", "main")
	write(".claude/memory/LEDGER.md", fixture(t, "main.md"))
	write("docs/devbox.md", []byte("Web lanes need standard (#t335).\n"))
	write("docs/shared.md", []byte("# Shared\n\nmiddle\nmain cites #t335\n"))
	run("add", "-A")
	run("commit", "-qm", "main rows")
	run("checkout", "-q", "work/perf")

	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if _, _, err := Renumber(home, "main", false, now); err == nil || err.(*Diag).Code != DiagBaseNotMerged {
		t.Fatalf("before the merge: err = %v, want %s", err, DiagBaseNotMerged)
	}
	dry, _, err := Renumber(home, "main", true, now)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if dry.Written || len(dry.Moves) != 10 || dry.Usage != 1 || read(".claude/memory/LEDGER.md") != string(fixture(t, "branch.md")) {
		t.Fatalf("dry run wrote or misjudged: %+v", dry)
	}

	if _, err := git(repo, "merge", "-q", "main", "-m", "merge main"); err == nil {
		t.Fatal("the real fork merged without a LEDGER.md conflict")
	}
	res, warnings, err := Renumber(home, "", false, now)
	if err != nil || len(warnings) > 0 {
		t.Fatalf("renumber: %v %v", err, warnings)
	}
	if !res.Merging || res.Base == "" || len(res.Moves) != 10 || res.Added != 5 {
		t.Fatalf("result: %+v", res)
	}
	if got := read(".claude/memory/LEDGER.md"); got != string(fixture(t, "landed.md")) {
		t.Fatalf("settled ledger:\n%s", got)
	}
	for rel, want := range map[string]string{
		".claude/memory/notes/skia-picture.md": "Folds into #t353, extends #t366.\n",
		"docs/perf.md":                         "Judge 120 Hz by framestats (#t367), not #t329.\n",
		"docs/shared.md":                       "branch cites #t363\n# Shared\n\nmiddle\nmain cites #t335\n",
		"docs/devbox.md":                       "Web lanes need standard (#t335).\n",
	} {
		if got := read(rel); got != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
	wantPaths := []string{".claude/memory/LEDGER.md", ".claude/memory/notes/skia-picture.md", "docs/perf.md", "docs/shared.md"}
	if !reflect.DeepEqual(res.Paths, wantPaths) {
		t.Fatalf("paths = %v, want %v", res.Paths, wantPaths)
	}
	var rows []int
	for _, line := range strings.Split(strings.TrimSpace(read(".claude/memory/usage.jsonl")), "\n") {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, e.Row)
	}
	if !reflect.DeepEqual(rows, []int{363, 329}) {
		t.Fatalf("usage rows = %v, want the branch's #t335 credited to #t363", rows)
	}
	if _, err := os.Stat(filepath.Join(home, IndexFile)); err != nil {
		t.Fatalf("MEMORY.md not rendered: %v", err)
	}

	run("add", "--", ".claude/memory/LEDGER.md")
	run("commit", "-qm", "settled")
	if f := LintBase(home, "main"); len(f) > 0 {
		t.Fatalf("settled ledger still collides: %+v", f)
	}
	again, _, err := Renumber(home, "main", false, now)
	if err != nil || again.Written || len(again.Moves) != 0 {
		t.Fatalf("second run is not a no-op: %+v %v", again, err)
	}
}

func TestLintBaseFlagsAnIDThatNamesTwoRows(t *testing.T) {
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@x", "GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@x"} {
		t.Setenv(k, v)
	}
	repo := t.TempDir()
	home := filepath.Join(repo, ".claude", "memory")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	commit := func(ledger string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, LedgerFile), fixture(t, ledger), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", ledger}} {
			if out, err := git(repo, args...); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
	}
	if out, err := git(repo, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(out)
	}
	commit("main.md")
	if out, err := git(repo, "checkout", "-qb", "work/perf"); err != nil {
		t.Fatal(out)
	}
	cases := []struct {
		ledger string
		want   []int
	}{
		{"branch.md", []int{330, 331, 332, 333, 334, 335, 336, 337, 338, 339}},
		{"landed.md", nil},
		{"fork.md", nil},
	}
	for _, tc := range cases {
		commit(tc.ledger)
		var got []int
		for _, f := range LintBase(home, "main") {
			if f.Rule != RuleIDCollision || f.Severity != "error" {
				t.Fatalf("%s: unexpected finding %+v", tc.ledger, f)
			}
			var id int
			if _, err := fmt.Sscanf(f.Message, "row #t%d", &id); err != nil {
				t.Fatalf("%s: %q", tc.ledger, f.Message)
			}
			got = append(got, id)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: L009 on %v, want %v", tc.ledger, got, tc.want)
		}
	}
	if _, d := ResolveBase(home, "origin/nope"); d == nil || d.Code != DiagBaseUnresolved {
		t.Fatalf("an explicit base that does not resolve must fail, got %+v", d)
	}
	if ref, d := ResolveBase(home, BaseNone); ref != "" || d != nil {
		t.Fatalf("none = %q %+v", ref, d)
	}
}
