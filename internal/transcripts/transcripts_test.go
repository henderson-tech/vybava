package transcripts

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendTo(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
}

func collect(t *testing.T, path string, cur Cursor, known bool, opts ScanOptions) (ScanResult, []string) {
	t.Helper()
	var lines []string
	res, err := Scan(path, cur, known, opts, func(line []byte, _ int64) error {
		lines = append(lines, strings.TrimSpace(string(line)))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return res, lines
}

func TestScanWaitsForAnUnfinishedRecordAndResumes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, "a\nb\npart")
	res, lines := collect(t, path, Cursor{}, false, ScanOptions{})
	if strings.Join(lines, ",") != "a,b" || res.Pending != 4 {
		t.Fatalf("first sweep = %v pending %d, want a,b pending 4", lines, res.Pending)
	}
	appendTo(t, path, "ial\nc\n")
	res, lines = collect(t, path, res.Cursor, true, ScanOptions{})
	if strings.Join(lines, ",") != "partial,c" || res.Pending != 0 || res.Reset {
		t.Fatalf("resume = %v pending %d reset %v, want partial,c", lines, res.Pending, res.Reset)
	}
	// Caught up and unchanged: never reopened, the caller keeps its cursor.
	if again, _ := collect(t, path, res.Cursor, true, ScanOptions{}); !again.Skipped {
		t.Fatal("an unchanged completed file was reopened")
	}
}

func TestScanRestartsAReplacedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, "old-1\nold-2\n")
	res, _ := collect(t, path, Cursor{}, false, ScanOptions{})
	write(t, path, "new-1\nnew-2\nnew-3\n") // larger, different prefix
	res, lines := collect(t, path, res.Cursor, true, ScanOptions{})
	if !res.Reset || strings.Join(lines, ",") != "new-1,new-2,new-3" {
		t.Fatalf("replaced file = %v reset %v, want every new record", lines, res.Reset)
	}
}

func TestScanBoundsASweepAndSkipsOversizeRecordsOnRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	write(t, path, "one\n"+strings.Repeat("x", 100)+"\ntwo\nthree\n")
	res, lines := collect(t, path, Cursor{}, false, ScanOptions{Budget: 5, RecordLimit: 50, SkipOversize: true})
	if strings.Join(lines, ",") != "one" {
		t.Fatalf("budgeted sweep = %v, want one (the oversize record that crosses the budget skipped, not held)", lines)
	}
	_, lines = collect(t, path, res.Cursor, true, ScanOptions{})
	if strings.Join(lines, ",") != "two,three" {
		t.Fatalf("next sweep = %v, want two,three", lines)
	}
	if _, err := Scan(path, Cursor{}, false, ScanOptions{RecordLimit: 50}, func([]byte, int64) error { return nil }); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("oversize without SkipOversize = %v, want ErrRecordTooLarge", err)
	}
}

func TestWalkClaudeFindsSessionsSubagentsAndWorkflowAgentsOnly(t *testing.T) {
	root := t.TempDir()
	for _, p := range []string{
		"-repo/s1.jsonl",
		"-repo/s1/subagents/agent-a.jsonl",
		"-repo/s1/subagents/agent-a.meta.json",
		"-repo/s1/subagents/workflows/wf_1/agent-b.jsonl",
		"-repo/s1/subagents/workflows/wf_1/journal.jsonl",
		"-repo/memory/usage.jsonl",
	} {
		write(t, filepath.Join(root, p), "{}\n")
	}
	files, skipped, err := WalkClaude(root)
	if err != nil || skipped != 0 {
		t.Fatal(skipped, err)
	}
	var got []string
	for _, f := range files {
		rel, _ := filepath.Rel(root, f.Path)
		got = append(got, string(f.Kind)+":"+filepath.ToSlash(rel))
	}
	sort.Strings(got)
	want := "session:-repo/s1.jsonl subagent:-repo/s1/subagents/agent-a.jsonl workflow-agent:-repo/s1/subagents/workflows/wf_1/agent-b.jsonl"
	if strings.Join(got, " ") != want {
		t.Fatalf("WalkClaude = %v\nwant %s", got, want)
	}
	if missing, skipped, err := WalkClaude(filepath.Join(root, "absent")); err != nil || len(missing) != 0 || skipped != 1 {
		t.Fatalf("missing root = %v, %d, %v; want empty and reported as skipped", missing, skipped, err)
	}
}

func TestGitRootFoldsWorktreesAndSubmodules(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	repo := filepath.Join(base, "work", "app")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "fix"), 0o755))
	must(os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "elsewhere"), 0o755))
	must(os.MkdirAll(filepath.Join(repo, "src", "deep"), 0o755))
	// A worktree inside .worktrees/ and one outside the repository entirely.
	inside := filepath.Join(repo, ".worktrees", "fix")
	outside := filepath.Join(base, "work", "app-elsewhere")
	write(t, filepath.Join(inside, ".git"), "gitdir: "+filepath.Join(repo, ".git", "worktrees", "fix")+"\n")
	write(t, filepath.Join(repo, ".git", "worktrees", "fix", "commondir"), "../..\n")
	write(t, filepath.Join(outside, ".git"), "gitdir: ../app/.git/worktrees/elsewhere\n")
	write(t, filepath.Join(repo, ".git", "worktrees", "elsewhere", "commondir"), "../..\n")
	// A submodule: its gitdir has no commondir.
	sub := filepath.Join(repo, "vendor", "lib")
	must(os.MkdirAll(filepath.Join(repo, ".git", "modules", "lib"), 0o755))
	write(t, filepath.Join(sub, ".git"), "gitdir: ../../.git/modules/lib\n")

	cases := []struct {
		cwd, want string
		exact     bool
	}{
		{filepath.Join(repo, "src", "deep"), repo, true},
		{inside, repo, true},
		{outside, repo, true},
		{sub, sub, true},
		{filepath.Join(repo, ".worktrees", "removed"), repo, false}, // deleted worktree
		{filepath.Join(base, "plain"), filepath.Join(base, "plain"), false},
		// The whole checkout moved away: its worktrees still fold into it.
		{filepath.Join(base, "old", "app", ".worktrees", "gone", "apps", "api"), filepath.Join(base, "old", "app"), false},
		{filepath.Join(base, "old", "app", ".claude", "worktrees", "gone"), filepath.Join(base, "old", "app"), false},
	}
	for _, c := range cases {
		got, exact := GitRoot(c.cwd)
		if got != c.want || exact != c.exact {
			t.Errorf("GitRoot(%s) = %s, %v; want %s, %v", c.cwd, got, exact, c.want, c.exact)
		}
	}
}

func TestClaudeCacheWritesSplitByTTL(t *testing.T) {
	rec, err := DecodeClaude([]byte(`{"type":"assistant","message":{"id":"m","model":"claude-opus-5-5","usage":{"input_tokens":3,"output_tokens":5,"cache_creation_input_tokens":100,"cache_read_input_tokens":7,"cache_creation":{"ephemeral_5m_input_tokens":40,"ephemeral_1h_input_tokens":60}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if w5, w1 := rec.Message.Usage.CacheWrites(); w5 != 40 || w1 != 60 {
		t.Fatalf("split = %d/%d, want 40/60", w5, w1)
	}
	legacy := ClaudeUsage{CacheCreationInputTokens: 30}
	if w5, w1 := legacy.CacheWrites(); w5 != 30 || w1 != 0 {
		t.Fatalf("unsplit = %d/%d, want 30/0", w5, w1)
	}
}
