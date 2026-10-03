package gitkit

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

func TestBucketOf(t *testing.T) {
	s := newSplitConfig(gitConfig{"PR_CENSUS_CONFIG_PATHS": "tools/release/**"})
	for path, want := range map[string]string{
		"apps/web/src/app/order.component.ts":        bucketCode,
		"apps/web/src/app/order.component.spec.ts":   bucketTest,
		"apps/web-e2e/src/order.ts":                  bucketTest,
		"internal/gitkit/prcensus_test.go":           bucketTest,
		"apps/web-e2e/README.md":                     bucketRest, // rest outranks test
		"libs/shared/src/lib/@types/app.d.ts":        bucketRest,
		"bun.lock":                                   bucketRest,
		"apps/web/src/assets/i18n/cs.json":           bucketRest,
		".claude/skills/ui/SKILL.md":                 bucketRest,
		"libs/email-editor/dist/email-editor.js":     bucketRest,
		"database/migrations/2026_10_01_add_col.php": bucketConfig,
		".github/workflows/verify.yml":               bucketConfig,
		"deploy/reservine-ssr.sh":                    bucketConfig,
		"devbox.worktree.yaml":                       bucketConfig,
		"package.json":                               bucketConfig,
		"tailwind.config.js":                         bucketConfig,
		"tools/release/cut.ts":                       bucketConfig, // the repo's own glob
		"apps/docs/src/components/search.tsx":        bucketCode,   // a docs app is code
	} {
		if got := s.bucketOf(path); got != want {
			t.Errorf("bucketOf(%q) = %s, want %s", path, got, want)
		}
	}
	if newSplitConfig(gitConfig{"GENERATED_PATHS": "resources/types/**"}).bucketOf("resources/types/app.ts") != bucketRest {
		t.Error("GENERATED_PATHS must count as rest")
	}
}

// A rename counts under its new path; a binary file is a file with no lines.
func TestSplitDiff(t *testing.T) {
	entries := parseNumstatZ("12\t3\tsrc/a.ts\x00" + "4\t0\t\x00src/old.spec.ts\x00src/new.spec.ts\x00" + "-\t-\tdocs/logo.png\x00")
	want := []numstatEntry{{"src/a.ts", 12, 3}, {"src/new.spec.ts", 4, 0}, {"docs/logo.png", 0, 0}}
	if !slices.Equal(entries, want) {
		t.Fatalf("parseNumstatZ = %+v", entries)
	}
	split, touches := newSplitConfig(gitConfig{}).splitDiff(append(entries, numstatEntry{"db/migrations/1.sql", 9, 0}))
	if split.Code != (Lines{1, 12, 3}) || split.Test != (Lines{1, 4, 0}) || split.Rest != (Lines{1, 0, 0}) ||
		split.Config != (Lines{1, 9, 0}) || !slices.Equal(touches, []string{"migration"}) {
		t.Fatalf("splitDiff = %+v, touches %v", split, touches)
	}
}

func TestCensusQuery(t *testing.T) {
	now := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	q, err := parseCensusQuery(map[string]string{"base": "devlp", "since": "7d", "author": "@me"}, []string{}, now)
	if err != nil || q.search("acme/app") != "repo:acme/app is:pr is:open base:devlp author:@me created:>=2026-09-26 sort:created-desc" {
		t.Fatalf("search = %q, %v", q.search("acme/app"), err)
	}
	q, err = parseCensusQuery(map[string]string{"state": "closed"}, []string{"#573", "680"}, now)
	if err != nil || !slices.Equal(q.Numbers, []int{573, 680}) || !strings.Contains(q.search("acme/app"), "is:closed is:unmerged") {
		t.Fatalf("query = %+v, %v", q, err)
	}
	for _, flags := range []map[string]string{{"state": "draft"}, {"since": "last week"}, {"since": "2026-13-01"}} {
		if _, err := parseCensusQuery(flags, nil, now); err == nil {
			t.Errorf("parseCensusQuery(%v) accepted", flags)
		}
	}
	for _, argv := range [][]string{{"--bogus"}, {"--base"}, {"--state", "draft"}, {"abc"}} {
		var stdout, stderr strings.Builder
		if code := runPRCensus(argv, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), prCensusArgs.usage) {
			t.Errorf("pr-census %q: exit %d, stderr %q", argv, code, stderr.String())
		}
	}
}

func censusGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Clean, conflicting and already-landed heads against one base.
func TestMergeFacts(t *testing.T) {
	repo := t.TempDir()
	censusGit(t, repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(repo, "a.txt"), "1\n2\n3\n")
	censusGit(t, repo, "add", ".")
	censusGit(t, repo, "commit", "-q", "-m", "c1")
	branch := func(name, file, text string) string {
		censusGit(t, repo, "checkout", "-q", "-b", name, "main")
		writeFile(t, filepath.Join(repo, file), text)
		censusGit(t, repo, "add", ".")
		censusGit(t, repo, "commit", "-q", "-m", name)
		return censusGit(t, repo, "rev-parse", "HEAD")
	}
	clean := branch("clean", "b.txt", "new\n")
	conflict := branch("conflict", "a.txt", "X\n2\n3\n")
	landed := branch("landed", "c.txt", "landed\n")
	censusGit(t, repo, "checkout", "-q", "main")
	writeFile(t, filepath.Join(repo, "a.txt"), "Y\n2\n3\n")
	censusGit(t, repo, "commit", "-q", "-am", "base moves")
	censusGit(t, repo, "cherry-pick", landed)

	opts := execOpts{dir: repo}
	var notes []string
	for _, tc := range []struct {
		head string
		want Merge
	}{
		{clean, Merge{Clean: true, ConflictFiles: []string{}}},
		{conflict, Merge{Conflicts: 1, ConflictFiles: []string{"a.txt"}}},
		{landed, Merge{Clean: true, ConflictFiles: []string{}, EmptyMerge: true, PatchOnBase: true}},
	} {
		got := mergeFacts(opts, "main", tc.head, &notes)
		if got == nil || got.Clean != tc.want.Clean || got.Conflicts != tc.want.Conflicts || !slices.Equal(got.ConflictFiles, tc.want.ConflictFiles) ||
			got.EmptyMerge != tc.want.EmptyMerge || got.PatchOnBase != tc.want.PatchOnBase {
			t.Errorf("mergeFacts(%s) = %+v, want %+v (notes %v)", tc.head[:7], got, tc.want, notes)
		}
	}
}

func TestPRTargetNames(t *testing.T) {
	main := "/w/app"
	target := prTarget{number: 42, slug: "acme/app", worktree: "/w/app/.worktrees/fix", repoDirs: []string{main, "/w/app/.worktrees/fix"}, home: "/w"}
	for _, tc := range []struct {
		touch touch
		want  bool
	}{
		{touch{cwd: "/w/app/.worktrees/fix/src", text: "bun test"}, true},
		{touch{cwd: main, text: "git -C .worktrees/fix status"}, true},
		{touch{cwd: "/tmp", text: "cat ~/app/.worktrees/fix/README.md"}, true},
		{touch{cwd: main, text: "git -C .worktrees/fix-2 status"}, false}, // a sibling worktree
		{touch{cwd: main, text: "gh pr checks 42 --watch"}, true},
		{touch{cwd: main, text: "gh pr view 42"}, false}, // reading is not working on it
		{touch{cwd: "/w/other", text: "gh pr merge 42"}, false},
		{touch{cwd: main, text: "gh pr merge 42 -R acme/back"}, false},
		{touch{cwd: "/tmp", text: "open https://github.com/acme/app/pull/42"}, true},
	} {
		if got := target.names(tc.touch); got != tc.want {
			t.Errorf("names(%+v) = %v, want %v", tc.touch, got, tc.want)
		}
	}
}

// A live session holds a PR after minTouches tool calls on it; the census's
// own session and a recycled pid never do, and a tool RESULT naming the
// worktree (git worktree list) is not a call.
func TestClaudeLogs(t *testing.T) {
	claude := t.TempDir()
	started := time.Date(2026, 10, 3, 9, 17, 31, 0, time.UTC)
	session := func(pid int, id, procStart string) {
		writeFile(t, filepath.Join(claude, "sessions", strconv.Itoa(pid)+".json"),
			`{"sessionId":"`+id+`","cwd":"/w/app","name":"app-`+id+`","status":"waiting","procStart":"`+procStart+`"}`)
		var lines []string
		for range 3 {
			lines = append(lines, `{"type":"assistant","timestamp":"2026-10-03T10:00:00Z","cwd":"/w/app","message":{"content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"git -C .worktrees/fix log"}}]}}`)
		}
		lines = append(lines, `{"type":"user","timestamp":"2026-10-03T10:01:00Z","cwd":"/w/app","message":{"content":[{"type":"tool_result","tool_use_id":"u","content":"/w/app/.worktrees/other  abc [other]"}]}}`)
		writeFile(t, filepath.Join(claude, "projects", transcripts.ClaudeSlug("/w/app"), id+".jsonl"), strings.Join(lines, "\n")+"\n")
	}
	session(101, "live", "Sat Oct  3 09:17:31 2026")
	session(102, "self", "Sat Oct  3 09:17:31 2026")
	session(103, "recycled", "Thu Oct  1 09:17:31 2026")
	procs := []claudeProc{{101, started}, {102, started}, {103, started}}
	logs := claudeLogs(claude, procs, "self", time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC))
	fix := prTarget{number: 7, slug: "acme/app", worktree: "/w/app/.worktrees/fix", repoDirs: []string{"/w/app"}}
	other := fix
	other.worktree = "/w/app/.worktrees/other"
	got := holdersOf(fix, logs)
	if len(got) != 1 || got[0].Name != "app-live" || got[0].PID != 101 || got[0].Status != "waiting" || got[0].Evidence != "3 tool calls" {
		t.Fatalf("holders = %+v", got)
	}
	if h := holdersOf(other, logs); len(h) != 0 {
		t.Fatalf("a tool result made a holder: %+v", h)
	}
}

// A Codex rollout's commands count with their workdir, its patches with the
// session's cwd.
func TestCodexLogs(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	home := t.TempDir()
	path := filepath.Join(home, ".codex", "sessions", "2026", "10", "03", "rollout-2026-10-03T09-00-00-01a0f2bb-ae9f-7430-8a3c-dc4497ed8d20.jsonl")
	call := `{"timestamp":"2026-10-03T10:00:00.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"c","arguments":"{\"cmd\":\"bun test\",\"workdir\":\"/w/app/.worktrees/fix\"}"}}`
	writeFile(t, path, `{"timestamp":"2026-10-03T09:00:00.000Z","type":"session_meta","payload":{"id":"01a0f2bb","cwd":"/w/app"}}`+"\n"+
		strings.Repeat(call+"\n", 3))
	logs := codexLogs(home, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)) // before the file's real mtime
	got := holdersOf(prTarget{number: 7, slug: "acme/app", worktree: "/w/app/.worktrees/fix", repoDirs: []string{"/w/app"}}, logs)
	if len(got) != 1 || got[0].Kind != "codex" || got[0].Name != "codex 01a0f2bb" || got[0].LastEventAt != "2026-10-03T10:00:00Z" {
		t.Fatalf("holders = %+v", got)
	}
}

// PR #167 review: a configured $CODEX_HOME holds the rollouts, not ~/.codex.
func TestCodexLogsHonorsCodexHome(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	call := `{"timestamp":"2026-10-03T10:00:00.000Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"c","arguments":"{\"cmd\":\"bun test\",\"workdir\":\"/w/app/.worktrees/fix\"}"}}`
	writeFile(t, filepath.Join(codexHome, "sessions", "2026", "10", "03", "rollout-2026-10-03T09-00-00-01a0f2bb-ae9f-7430-8a3c-dc4497ed8d20.jsonl"),
		strings.Repeat(call+"\n", 3))
	logs := codexLogs(t.TempDir(), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if got := holdersOf(prTarget{number: 7, slug: "acme/app", worktree: "/w/app/.worktrees/fix"}, logs); len(got) != 1 {
		t.Fatalf("holders under $CODEX_HOME = %+v", got)
	}
}

// PR #167 review: a gh command is read per shell segment with its own -R,
// and a quoted string is not a command.
func TestPRTargetNamesReadsShellSegments(t *testing.T) {
	target := prTarget{number: 42, slug: "acme/app", repoDirs: []string{"/w/app"}}
	if target.names(touch{cwd: "/w/app", text: "git commit -m 'merge after gh pr merge 42 lands'"}) {
		t.Error("a quoted gh pr merge counted as driving the PR")
	}
	if !target.names(touch{cwd: "/w/app", text: "gh pr checks 42 --watch && gh pr view 9 -R acme/back"}) {
		t.Error("another segment's -R cancelled gh pr checks 42")
	}
}

// PR #167 review: #42 must not match a link to #420 or #421.
func TestPRTargetNamesWholeNumberInLinks(t *testing.T) {
	target := prTarget{number: 42, slug: "acme/app"}
	for _, text := range []string{"open https://github.com/acme/app/pull/420", "gh api repos/acme/app/pulls/421/comments"} {
		if target.names(touch{cwd: "/tmp", text: text}) {
			t.Errorf("%q named #42", text)
		}
	}
	if !target.names(touch{cwd: "/tmp", text: "gh api repos/acme/app/pulls/42/comments"}) {
		t.Error("repos/acme/app/pulls/42 did not name #42")
	}
}

// PR #167 review: a session in one sibling worktree can work on another
// through a ../ path.
func TestPRTargetNamesSiblingRelativePath(t *testing.T) {
	target := prTarget{number: 42, slug: "acme/app", worktree: "/w/app/.worktrees/fix"}
	if !target.names(touch{cwd: "/w/app/.worktrees/other", text: "git -C ../fix status"}) {
		t.Error("git -C ../fix from a sibling worktree did not name it")
	}
	for _, text := range []string{"git -C ../fix-2 status", "git -C ../../fix status"} {
		if target.names(touch{cwd: "/w/app/.worktrees/other", text: text}) {
			t.Errorf("%q named ../fix", text)
		}
	}
}

// PR #167 review: a flag's value is not the PR selector.
func TestPRTargetNamesSkipsFlagValues(t *testing.T) {
	nine := prTarget{number: 9, slug: "acme/app", repoDirs: []string{"/w/app"}}
	if !nine.names(touch{cwd: "/w/app", text: "gh pr checks --interval 42 9 --watch"}) {
		t.Error("gh pr checks --interval 42 9 did not name #9")
	}
	fortyTwo := prTarget{number: 42, slug: "acme/app", repoDirs: []string{"/w/app"}}
	if fortyTwo.names(touch{cwd: "/w/app", text: "gh pr comment --body '42' 9"}) {
		t.Error("a --body value named #42")
	}
}

// A number that is no PR makes gh exit 1 beside the other aliases' data:
// the census keeps the found PRs and notes the miss (2026-10-03: one bad
// number failed the whole run).
func TestListCensusPRsKeepsFoundNumbers(t *testing.T) {
	bin := t.TempDir()
	shim := `#!/bin/sh
echo '{"data":{"repository":{"pr7":{"number":7,"url":"https://github.com/acme/app/pull/7"},"pr9":null}}}'
echo "gh: Could not resolve to a PullRequest with the number of 9." >&2
exit 1
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	prs, notes, err := listCensusPRs(execOpts{dir: bin}, "acme", "app", CensusQuery{Numbers: []int{7, 9}})
	if err != nil || len(prs) != 1 || prs[0].Number != 7 || !slices.Equal(notes, []string{"#9 is not a pull request in acme/app"}) {
		t.Fatalf("listCensusPRs = %+v, %v, %v", prs, notes, err)
	}
}

func TestRefsOf(t *testing.T) {
	got := refsOf("fix(ssr): bind loopback (vt-1002) #518", "Fixes #12. See VT-1405 and vt-1002 again; mentions #99 in passing.")
	if !slices.Equal(got, []string{"vt-1002", "vt-1405", "#518", "#12"}) {
		t.Fatalf("refsOf = %v", got)
	}
}
