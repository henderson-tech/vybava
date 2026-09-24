package tokentime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/rollup.golden.json")

// fixture is a home with a git repository (plus a linked worktree), a
// non-repository directory, Claude transcripts and Codex rollouts.
type fixture struct {
	base, repo, worktree, tools, claude, codex, state string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	base, _ := filepath.EvalSymlinks(t.TempDir())
	f := fixture{base: base, repo: filepath.Join(base, "work", "app"), tools: filepath.Join(base, "work", "tools"),
		claude: filepath.Join(base, "claude"), codex: filepath.Join(base, "codex"), state: filepath.Join(base, "state")}
	f.worktree = filepath.Join(f.repo, ".worktrees", "fix")
	mkdir(t, filepath.Join(f.repo, ".git", "worktrees", "fix"))
	mkdir(t, f.tools)
	put(t, filepath.Join(f.worktree, ".git"), "gitdir: "+filepath.Join(f.repo, ".git", "worktrees", "fix")+"\n")
	put(t, filepath.Join(f.repo, ".git", "worktrees", "fix", "commondir"), "../..\n")

	session := filepath.Join(f.claude, "-work-app")
	put(t, filepath.Join(session, "s1.jsonl"), lines(
		`{"type":"user","sessionId":"s1","cwd":"`+f.repo+`","timestamp":"2026-09-23T12:09:00Z","message":{"role":"user","content":"hi"}}`,
		// One message, three content blocks: counted once.
		claudeLine("s1", f.worktree, "2026-09-23T12:10:00Z", "msg_A", "claude-fable-5-1", 10, 20, 40, 60, 1000),
		claudeLine("s1", f.worktree, "2026-09-23T12:10:00Z", "msg_A", "claude-fable-5-1", 10, 20, 40, 60, 1000),
		claudeLine("s1", f.worktree, "2026-09-23T12:10:01Z", "msg_A", "claude-fable-5-1", 10, 20, 40, 60, 1000),
		claudeLine("s1", f.repo, "2026-09-23T12:11:00Z", "msg_S", "<synthetic>", 0, 0, 0, 0, 0),
		claudeLine("s1", f.repo, "2026-09-22T09:00:00Z", "msg_B", "claude-opus-5-5", 1, 2, 0, 0, 50),
	))
	put(t, filepath.Join(session, "s1", "subagents", "agent-a.jsonl"),
		lines(claudeLine("s1", f.repo, "2026-09-23T12:40:00Z", "msg_C", "claude-opus-5-5", 5, 5, 0, 0, 0)))
	// A cxb bridge turn: a Claude transcript carrying an OpenAI model.
	put(t, filepath.Join(session, "s1", "subagents", "workflows", "wf_1", "agent-b.jsonl"),
		lines(claudeLine("s1", f.tools, "2026-09-23T13:05:00Z", "msg_D", "gpt-6-astra", 100, 10, 0, 0, 0)))
	// Not transcripts: never counted.
	put(t, filepath.Join(session, "s1", "subagents", "workflows", "wf_1", "journal.jsonl"),
		lines(claudeLine("s1", f.repo, "2026-09-23T13:06:00Z", "msg_J", "claude-opus-5-5", 999, 999, 0, 0, 0)))
	put(t, filepath.Join(session, "memory", "usage.jsonl"),
		lines(claudeLine("s1", f.repo, "2026-09-23T13:07:00Z", "msg_M", "claude-opus-5-5", 999, 999, 0, 0, 0)))

	day := filepath.Join(f.codex, "sessions", "2026", "09", "22")
	// An old CLI: token_count only.
	put(t, filepath.Join(day, "rollout-2026-09-22T08-00-00-thread-L.jsonl"), lines(
		rollout("2026-09-22T08:00:00Z", "session_meta", map[string]any{"id": "thread-L", "timestamp": "2026-09-22T08:00:00Z", "cwd": f.tools}),
		rollout("2026-09-22T08:00:01Z", "session_meta", map[string]any{"id": "thread-P", "timestamp": "2026-09-20T08:00:00Z", "cwd": f.repo}),
		tokenCount("2026-09-22T07:00:00Z", usage(500, 0, 10), usage(500, 0, 10)), // copied ancestor history
		rollout("2026-09-22T08:05:00Z", "turn_context", map[string]any{"model": "gpt-6-astra", "cwd": f.tools}),
		tokenCount("2026-09-22T08:05:00Z", usage(1000, 800, 50), usage(1000, 800, 50)),
		tokenCount("2026-09-22T08:06:00Z", usage(1000, 800, 50), usage(1000, 800, 50)), // rate-limit refresh
		rollout("2026-09-22T08:07:00Z", "event_msg", map[string]any{"type": "token_count", "info": nil}),
		tokenCount("2026-09-22T08:10:00Z", usage(1600, 1300, 70), usage(600, 500, 20)),
	))
	// A new CLI: exact receipts, token_count as bookkeeping.
	put(t, filepath.Join(f.codex, "sessions", "2026", "09", "23", "rollout-2026-09-23T10-00-00-thread-R.jsonl"), lines(
		rollout("2026-09-23T10:00:00Z", "session_meta", map[string]any{"id": "thread-R", "timestamp": "2026-09-23T10:00:00Z", "cwd": f.repo}),
		rollout("2026-09-23T10:00:30Z", "turn_context", map[string]any{"model": "gpt-daybreak-blue", "cwd": f.repo}),
		tokenCount("2026-09-23T10:01:00Z", usage(2000, 1500, 40), usage(2000, 1500, 40)), // persisted before its receipt
		receipt("2026-09-23T10:01:00Z", "thread-R", "r1", usage(2000, 1500, 40)),
		receipt("2026-09-23T10:02:00Z", "thread-P", "p1", usage(9999, 0, 9999)), // copied fork history
		receipt("2026-09-23T11:00:00Z", "thread-R", "r2", usage(300, 100, 10)),
		tokenCount("2026-09-23T11:00:00Z", usage(2300, 1600, 50), usage(300, 100, 10)),
		receipt("2026-09-23T11:00:00Z", "thread-R", "r2", usage(300, 100, 10)), // duplicate
	))
	return f
}

func (f fixture) options() Options { return Options{ClaudeRoot: f.claude, CodexDir: f.codex} }

func (f fixture) open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(f.state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func (f fixture) index(t *testing.T, s *Store) IndexReport {
	t.Helper()
	r, err := s.Index(f.options())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.FileErrors) > 0 {
		t.Fatalf("file errors: %v", r.FileErrors)
	}
	return r
}

var prague, _ = time.LoadLocation("Europe/Prague")

func rollupOf(t *testing.T, s *Store, days, hours int) Rollup {
	t.Helper()
	r, err := s.Rollup(RollupOptions{Days: days, Hours: hours, Now: time.Date(2026, 9, 23, 15, 30, 0, 0, prague), Location: prague})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestIndexCountsEveryResponseOnceAndFoldsWorktrees(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	report := f.index(t, s)
	if report.Responses != 8 {
		t.Fatalf("responses = %d, want 8 (4 Claude incl. subagents, 2 legacy Codex, 2 receipts): %+v", report.Responses, report)
	}
	r := rollupOf(t, s, 2, 3)
	want := Tokens{Input: 10 + 1 + 5 + 100 + 200 + 100 + 500 + 200, Output: 20 + 2 + 5 + 10 + 50 + 20 + 40 + 10,
		CacheWrite: 100, CacheRead: 1000 + 50 + 800 + 500 + 1500 + 100}
	if r.Lifetime.Tokens != want || r.Lifetime.Responses != 8 || r.Lifetime.Sessions != 3 {
		t.Fatalf("lifetime = %+v, want tokens %+v, 8 responses, 3 sessions", r.Lifetime, want)
	}
	projects := map[string]Project{}
	for _, p := range r.Projects {
		projects[p.Name] = p
	}
	// The worktree's message lands on the repository, not on ".worktrees/fix".
	if app := projects["app"]; app.Root != f.repo || sumTokens(app.Tokens) != 1130+53+10+2040+310 {
		t.Fatalf("app = %+v, want root %s and every repo+worktree token", app, f.repo)
	}
	if _, ok := projects["fix"]; ok {
		t.Fatal("a worktree became its own project")
	}
	lanes := map[string]Lane{}
	for _, m := range r.Models {
		lanes[m.Model] = m.Lane
	}
	if lanes["gpt-6-astra"] != OpenAI || lanes["claude-fable-5-1"] != Anthropic || lanes["gpt-daybreak-blue"] != OpenAI {
		t.Fatalf("lanes = %v", lanes)
	}
	if _, ok := lanes["<synthetic>"]; ok {
		t.Fatal("a synthetic record was counted")
	}
	if len(r.Unpriced) != 0 {
		t.Fatalf("unpriced = %v, want every fixture model priced (daybreak bills as sol)", r.Unpriced)
	}
}

func TestIndexIsIncrementalAndNeverCountsTwice(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	f.index(t, s)
	before := rollupOf(t, s, 2, 3).Lifetime
	if again := f.index(t, s); again.ReadBytes != 0 || again.Opened != 0 || again.Responses != 0 {
		t.Fatalf("second pass over unchanged files = %+v, want nothing read", again)
	}
	path := filepath.Join(f.claude, "-work-app", "s1.jsonl")
	line := claudeLine("s1", f.repo, "2026-09-23T13:20:00Z", "msg_E", "claude-opus-5-5", 7, 0, 0, 0, 0)
	appendFile(t, path, line[:len(line)/2]) // still being written
	if r := f.index(t, s); r.Responses != 0 {
		t.Fatalf("a partial record was counted: %+v", r)
	}
	appendFile(t, path, line[len(line)/2:]+"\n")
	if r := f.index(t, s); r.Responses != 1 {
		t.Fatalf("completed record counted %d times, want 1", r.Responses)
	}
	// Replaced with a different prefix but the same messages: re-read, nothing recounted.
	raw, _ := os.ReadFile(path)
	put(t, path, `{"type":"summary","summary":"compacted"}`+"\n"+string(raw))
	r := f.index(t, s)
	if r.Resets != 1 || r.Responses != 0 {
		t.Fatalf("replaced file = %+v, want 1 reset and 0 new responses", r)
	}
	after := rollupOf(t, s, 2, 3).Lifetime
	if after.Responses != before.Responses+1 || after.Tokens.Input != before.Tokens.Input+7 {
		t.Fatalf("lifetime %+v → %+v, want exactly msg_E added", before, after)
	}
}

func TestABudgetedColdPassFillsTodayFirst(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root, cwd := filepath.Join(base, "claude"), filepath.Join(base, "work")
	mkdir(t, cwd)
	var history []string
	for i := range 200 {
		history = append(history, claudeLine("old", cwd, "2026-09-20T10:00:00Z", fmt.Sprintf("old_%d", i), "claude-opus-5-5", 1, 1, 0, 0, 0))
	}
	oldPath, newPath := filepath.Join(root, "-work", "old.jsonl"), filepath.Join(root, "-work", "new.jsonl")
	put(t, oldPath, lines(history...))
	put(t, newPath, lines(claudeLine("new", cwd, "2026-09-23T13:00:00Z", "today_1", "claude-opus-5-5", 40, 2, 0, 0, 0)))
	week := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(oldPath, week, week); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(newPath)
	s, err := Open(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	report, err := s.Index(Options{ClaudeRoot: root, CodexDir: filepath.Join(base, "codex"), Budget: info.Size()})
	if err != nil {
		t.Fatal(err)
	}
	r := rollupOf(t, s, 1, 1)
	if got := r.Days[0].Models; len(got) != 1 || sumTokens(got[0].Tokens) != 42 || report.PendingBytes == 0 {
		t.Fatalf("first budgeted pass: today = %+v, pending %d; want today's 42 tokens and history still pending", got, report.PendingBytes)
	}
}

func TestHoursStayDistinctAcrossDaylightSavingChanges(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root, cwd := filepath.Join(base, "claude"), filepath.Join(base, "work")
	mkdir(t, cwd)
	put(t, filepath.Join(root, "-work", "s.jsonl"), lines(
		claudeLine("s", cwd, "2026-10-25T00:30:00Z", "cest", "claude-opus-5-5", 1, 0, 0, 0, 0), // 02:30 CEST
		claudeLine("s", cwd, "2026-10-25T01:30:00Z", "cet", "claude-opus-5-5", 2, 0, 0, 0, 0),  // 02:30 CET, one hour later
	))
	s, err := Open(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Index(Options{ClaudeRoot: root, CodexDir: filepath.Join(base, "codex")}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		now  time.Time
		want []string
	}{
		{"fall-back", time.Date(2026, 10, 25, 4, 30, 0, 0, prague),
			[]string{"01:00+02:00", "02:00+02:00=1", "02:00+01:00=2", "03:00+01:00", "04:00+01:00"}},
		{"spring-forward", time.Date(2026, 3, 29, 4, 30, 0, 0, prague),
			[]string{"00:00+01:00", "01:00+01:00", "03:00+02:00", "04:00+02:00"}},
	}
	for _, c := range cases {
		r, err := s.Rollup(RollupOptions{Days: 1, Hours: len(c.want), Now: c.now, Location: prague})
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, h := range r.Hours {
			key := h.Hour[strings.IndexByte(h.Hour, 'T')+1:]
			key = strings.Replace(key, ":00:00", ":00", 1)
			if len(h.Models) > 0 {
				key += fmt.Sprintf("=%d", h.Models[0].Tokens)
			}
			got = append(got, key)
		}
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("%s hours = %v, want %v", c.name, got, c.want)
		}
	}
}

// At +05:30 no local whole hour starts a bucket: the hours are the buckets',
// so they start at :30 and carry their tokens.
func TestHoursFollowTheBucketsInAHalfHourZone(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := filepath.EvalSymlinks(t.TempDir())
	app := filepath.Join(base, "app")
	mkdir(t, app)
	s := indexed(t, base, rec{"s", app, "2026-09-24T06:10:00Z", "claude-opus-5-5", 7}) // 11:40 IST
	r, err := s.Rollup(RollupOptions{Days: 1, Hours: 2, Now: time.Date(2026, 9, 24, 12, 0, 0, 0, kolkata), Location: kolkata})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Hours) != 2 || r.Hours[0].Hour != "2026-09-24T10:30:00+05:30" || r.Hours[1].Hour != "2026-09-24T11:30:00+05:30" ||
		len(r.Hours[1].Models) != 1 || r.Hours[1].Models[0].Tokens != 7 {
		t.Fatalf("hours = %+v, want 10:30 and 11:30 with the 7 tokens in the latter", r.Hours)
	}
}

func TestStaleTailsAndFailingFilesNeverHoldCoverageOpen(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root, cwd := filepath.Join(base, "claude"), filepath.Join(base, "work")
	mkdir(t, cwd)
	torn := filepath.Join(root, "-work", "torn.jsonl")
	line := claudeLine("s", cwd, "2026-09-23T10:00:00Z", "cut", "claude-opus-5-5", 1, 0, 0, 0, 0)
	put(t, torn, lines(claudeLine("s", cwd, "2026-09-23T09:00:00Z", "ok", "claude-opus-5-5", 1, 0, 0, 0, 0))+line[:20])
	hourAgo := time.Now().Add(-time.Hour)
	if err := os.Chtimes(torn, hourAgo, hourAgo); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(root, "-work", "locked.jsonl")
	put(t, locked, lines(line))
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(locked, 0o644)
	s, err := Open(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	opts := Options{ClaudeRoot: root, CodexDir: filepath.Join(base, "codex")}
	for pass := 1; pass <= 2; pass++ {
		r, err := s.Index(opts)
		if err != nil {
			t.Fatal(err)
		}
		if r.PendingBytes != 0 || len(r.StaleTails) != 1 || r.StaleTailBytes != 20 || len(r.FileErrors) != 1 || r.ErroredBytes == 0 {
			t.Fatalf("pass %d = %+v; want 0 pending, the torn file as a 20-byte stale tail, the locked file as an error", pass, r)
		}
		if pass == 2 && r.Opened != 0 {
			t.Fatalf("pass 2 reopened an unchanged tail: %+v", r)
		}
	}
	if c := rollupOf(t, s, 1, 1).Coverage; !c.Complete || c.PendingBytes != 0 {
		t.Fatalf("coverage = %+v, want complete", c)
	}
}

func TestAnUnreadableDirectoryKeepsItsCursors(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	f.index(t, s)
	hidden := filepath.Join(f.claude, "-work-app", "s1", "subagents")
	if err := os.Chmod(hidden, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index(f.options()); err != nil {
		os.Chmod(hidden, 0o755)
		t.Fatal(err)
	}
	if err := os.Chmod(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	// Had the unseen subagent cursors been dropped, this pass would re-read them.
	if r := f.index(t, s); r.ReadBytes != 0 || r.Responses != 0 {
		t.Fatalf("after the directory came back: %+v, want nothing re-read", r)
	}
}

func TestTheLongestSessionIsItsLongestRunOfActiveHours(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root, cwd := filepath.Join(base, "claude"), filepath.Join(base, "work")
	mkdir(t, cwd)
	// Ten consecutive active hours, 20:xx on the 22nd to 05:xx on the 23rd (Prague).
	var night []string
	for h := range 10 {
		ts := time.Date(2026, 9, 22, 18+h, 30, 0, 0, time.UTC).Format(time.RFC3339)
		night = append(night, claudeLine("night", cwd, ts, fmt.Sprintf("n%d", h), "claude-opus-5-5", 1, 0, 0, 0, 0))
	}
	put(t, filepath.Join(root, "-work", "night.jsonl"), lines(night...))
	// Three active hours on the 22nd, resumed for one hour the next morning.
	put(t, filepath.Join(root, "-work", "resumed.jsonl"), lines(
		claudeLine("resumed", cwd, "2026-09-22T08:15:00Z", "r1", "claude-opus-5-5", 1, 0, 0, 0, 0), // 10:15 Prague
		claudeLine("resumed", cwd, "2026-09-22T09:15:00Z", "r2", "claude-opus-5-5", 1, 0, 0, 0, 0),
		claudeLine("resumed", cwd, "2026-09-22T10:15:00Z", "r3", "claude-opus-5-5", 1, 0, 0, 0, 0),
		claudeLine("resumed", cwd, "2026-09-23T07:15:00Z", "r4", "claude-opus-5-5", 1, 0, 0, 0, 0), // 09:15 next day
	))
	s, err := Open(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.Index(Options{ClaudeRoot: root, CodexDir: filepath.Join(base, "codex")}); err != nil {
		t.Fatal(err)
	}
	days := rollupOf(t, s, 2, 1).Days
	// The 22nd: the resumed session's 3-hour run (the overnight run ends on the 23rd).
	// The 23rd: the overnight 10-hour run — never the resumed session's 23-hour gap.
	if days[0].Sessions != 2 || days[0].LongestSessionMinutes != 180 || days[1].Sessions != 2 || days[1].LongestSessionMinutes != 600 {
		t.Fatalf("days = %+v; want 180 minutes on the 22nd and 600 on the 23rd, both sessions active both days", days)
	}
}

func TestAnInterruptedPassKeepsWhatItCommitted(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	// Killed dead after the third file: files one and two were committed.
	killed := f.options()
	killed.commitFiles, killed.killAfter = 1, 3
	if _, err := s.Index(killed); !errors.Is(err, errKilled) {
		t.Fatalf("killed pass = %v", err)
	}
	if st, _ := s.Status(); st.Files != 2 {
		t.Fatalf("after the kill %d cursors survive, want the 2 committed files", st.Files)
	}
	// Stopped by a signal before it starts: nothing read, everything pending.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped := f.options()
	stopped.Context = ctx
	if r, err := s.Index(stopped); !errors.Is(err, context.Canceled) || r.ReadBytes != 0 || r.PendingBytes == 0 {
		t.Fatalf("cancelled pass = %+v, %v; want nothing read and the rest pending", r, err)
	}
	// The next pass reads only what was never committed and counts nothing twice.
	r := f.index(t, s)
	if r.Opened != 3 {
		t.Fatalf("resumed pass opened %d files, want the 3 never committed", r.Opened)
	}
	if life := rollupOf(t, s, 2, 3).Lifetime; life.Responses != 8 {
		t.Fatalf("lifetime responses = %d after the interruptions, want exactly 8", life.Responses)
	}
}

func TestBucketsOutliveTheirSources(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	f.index(t, s)
	before := rollupOf(t, s, 2, 3).Lifetime
	if err := os.RemoveAll(filepath.Join(f.claude, "-work-app")); err != nil { // transcripts deleted, root still there
		t.Fatal(err)
	}
	f.index(t, s)
	if after := rollupOf(t, s, 2, 3).Lifetime; after.Tokens != before.Tokens || after.Responses != before.Responses || *after.USD != *before.USD {
		t.Fatalf("lifetime after transcript cleanup = %+v, want %+v", after, before)
	}
	if st, err := s.Status(); err != nil || st.Files != 2 {
		t.Fatalf("status = %+v, %v; want only the 2 rollout cursors left", st, err)
	}
}

func TestIndexRefusesToRunTwiceAtOnce(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	mkdir(t, s.Dir)
	unlock, err := tryLock(filepath.Join(s.Dir, "index.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := s.Index(f.options()); !errors.Is(err, ErrBusy) {
		t.Fatalf("second concurrent pass = %v, want ErrBusy", err)
	}
}

// Only an index pass holding the lock creates or migrates the store. While
// another holds it, a missing store stays missing (a rollup is ErrNoStore,
// status all zeros) and an older one stays at its schema: one its reads
// still run on is served, an older one is ErrStaleSchema. The first pass the
// lock lets through creates or migrates it.
func TestOnlyAPassHoldingTheLockCreatesOrMigratesTheStore(t *testing.T) {
	f := newFixture(t)
	db := filepath.Join(f.state, "tokentime.db")
	hold := func() func() {
		t.Helper()
		mkdir(t, f.state)
		unlock, err := tryLock(filepath.Join(f.state, "index.lock"))
		if err != nil {
			t.Fatal(err)
		}
		return unlock
	}
	version := func() int {
		t.Helper()
		ro, v, err := openDB(db, "ro")
		if err != nil {
			t.Fatal(err)
		}
		ro.Close()
		return v
	}

	unlock := hold()
	s := f.open(t)
	if _, err := s.Index(f.options()); !errors.Is(err, ErrBusy) {
		t.Fatalf("pass under a held lock = %v, want ErrBusy", err)
	}
	if _, err := s.Rollup(RollupOptions{}); !errors.Is(err, ErrNoStore) {
		t.Fatalf("rollup of a store never created = %v, want ErrNoStore", err)
	}
	if st, err := s.Status(); err != nil || st.Buckets != 0 || st.LastIndexAt != "" {
		t.Fatalf("status of a store never created = %+v, %v; want zeros", st, err)
	}
	if _, err := os.Stat(db); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tokentime.db without the lock: %v, want it never created", err)
	}
	unlock()
	f.index(t, s)
	want, _ := json.Marshal(rollupOf(t, s, 2, 3))
	s.Close()

	for _, old := range []struct {
		version  int
		ddl      string
		readable bool
	}{
		{2, "DROP INDEX buckets_by_project; PRAGMA user_version=2", true},
		{1, "ALTER TABLE files DROP COLUMN tail; PRAGMA user_version=1", false},
	} {
		w, _, err := openDB(db, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Exec(old.ddl); err != nil { // as that schema's binary left it
			t.Fatal(err)
		}
		w.Close()
		unlock := hold()
		s := f.open(t)
		if _, err := s.Index(f.options()); !errors.Is(err, ErrBusy) {
			t.Fatalf("schema %d: pass under a held lock = %v, want ErrBusy", old.version, err)
		}
		r, err := s.Rollup(RollupOptions{Days: 2, Hours: 3, Now: time.Date(2026, 9, 23, 15, 30, 0, 0, prague), Location: prague})
		got, _ := json.Marshal(r)
		st, stErr := s.Status()
		switch {
		case old.readable && (err != nil || string(got) != string(want) || stErr != nil || st.Buckets == 0):
			t.Fatalf("schema %d under a held lock: rollup %v, status %+v, %v; want both served as before", old.version, err, st, stErr)
		case !old.readable && (!errors.Is(err, ErrStaleSchema) || !errors.Is(stErr, ErrStaleSchema)):
			t.Fatalf("schema %d under a held lock: rollup %v, status %v; want ErrStaleSchema", old.version, err, stErr)
		}
		if v := version(); v != old.version {
			t.Fatalf("schema %d under a held lock is now %d: migrated without the lock", old.version, v)
		}
		unlock()
		f.index(t, s)
		if v := version(); v != schemaVersion {
			t.Fatalf("schema %d after the next pass = %d, want %d", old.version, v, schemaVersion)
		}
		s.Close()
	}
}

func TestRollupMatchesTheGolden(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	f.index(t, s)
	r := rollupOf(t, s, 2, 3)
	got, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = bytes.ReplaceAll(got, []byte(f.base), []byte("$BASE"))
	golden := filepath.Join("testdata", "rollup.golden.json")
	if *update {
		if err := os.WriteFile(golden, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Fatalf("rollup drifted from %s (go test -run Golden -update to accept):\n%s", golden, got)
	}
}

func TestNamesKeepTheLiveRepositoryShort(t *testing.T) {
	names := uniqueNames([]nameCandidate{
		{id: 1, root: "/old/Work/FixIt", tokens: 900, live: false}, // the checkout before it moved
		{id: 2, root: "/new/Projects/Org/FixIt", tokens: 100, live: true},
		{id: 3, root: "/old/Work/tools", tokens: 5, live: false},
		{id: 4, root: "/elsewhere/Work/FixIt", tokens: 1, live: false},
	})
	want := map[int64]string{1: "Work/FixIt", 2: "FixIt", 3: "tools", 4: "elsewhere/Work/FixIt"}
	for id, name := range want {
		if names[id] != name {
			t.Errorf("name %d = %q, want %q (all: %v)", id, names[id], name, names)
		}
	}
}

func TestAMovedCheckoutFoldsIntoItsOnlyLiveNamesake(t *testing.T) {
	ps := foldProjects([]nameCandidate{
		{id: 1, root: "/new/Org/FixIt", live: true},
		{id: 2, root: "/old/Work/FixIt"},                    // moved: one live FixIt → fold
		{id: 3, root: "/old/Work/tools"},                    // no live namesake → stays
		{id: 4, root: "/a/lib", live: true},                 // two live libs …
		{id: 5, root: "/b/lib", live: true},                 //
		{id: 6, root: "/old/Work/lib"},                      // … so the dead one never guesses
		{id: 7, root: "/new/Org/FixIt/.claude", live: true}, // a live root is never folded
	})
	for id, want := range map[int64]int64{1: 1, 2: 1, 3: 3, 4: 4, 5: 5, 6: 6, 7: 7} {
		if got := ps.of(id); got != want {
			t.Errorf("project %d rolls up under %d, want %d", id, got, want)
		}
	}
	if ps.names[1] != "FixIt" || ps.names[3] != "tools" || ps.names[6] != "Work/lib" {
		t.Errorf("names = %v; want FixIt, tools and a qualified dead lib", ps.names)
	}
	if _, ok := ps.names[2]; ok {
		t.Error("a folded root kept a name of its own")
	}
}

func TestPricesNormalizeNamesAndHonourTheOverrideFile(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "prices.json"), `{
		"claude-opus-5-5":{"input":1,"output":2,"cacheWrite5m":3,"cacheWrite1h":4,"cacheRead":0.5},
		"gpt-6-astra":{"output":60},
		"house-model":{"input":2,"output":8}
	}`)
	p, err := LoadPrices(dir)
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{
		"claude-haiku-4-5-20251001": "claude-haiku-4-5",
		"claude-fable-5-1[1m]":      "claude-fable-5-1",
		"gpt-daybreak-blue-latest":  "gpt-5.6-sol",
	} {
		if got := CanonicalModel(model); got != want {
			t.Errorf("CanonicalModel(%s) = %s, want %s", model, got, want)
		}
	}
	// Fable 5.1 reads cache at $0.25/MTok, a quarter of the usual 0.1×.
	if usd, ok := p.Cost("claude-fable-5-1", Counts{CacheRead: 1_000_000, Output: 1_000_000}); !ok || usd != 50.25 {
		t.Fatalf("fable cost = %v, %v; want 50.25", usd, ok)
	}
	if usd, _ := p.Cost("claude-opus-5-5", Counts{CacheWrite1h: 1_000_000}); usd != 4 {
		t.Fatalf("override not applied: %v", usd)
	}
	// A partial row merges over the built-in one: astra keeps its $10 input.
	if astra, _ := p.Lookup("gpt-6-astra"); astra.Output != 60 || astra.Input != 10 || astra.CacheRead != 1 {
		t.Fatalf("astra = %+v; want output overridden to 60 and every other built-in rate kept", astra)
	}
	// An unknown model without cache rates is said, never silently $0.
	if len(p.Incomplete) != 1 || p.Incomplete[0] != "house-model (no cacheWrite5m, cacheWrite1h, cacheRead)" {
		t.Fatalf("incomplete = %v", p.Incomplete)
	}
	if _, ok := p.Cost("mystery-model", Counts{Input: 1}); ok {
		t.Fatal("an unknown model was priced")
	}
}

func claudeLine(session, cwd, ts, id, model string, in, out, w5, w1, read int64) string {
	return mustJSON(map[string]any{
		"type": "assistant", "sessionId": session, "cwd": cwd, "timestamp": ts, "requestId": "req_" + id,
		"message": map[string]any{"id": id, "model": model, "content": []any{map[string]any{"type": "text", "text": "…"}},
			"usage": map[string]any{"input_tokens": in, "output_tokens": out, "cache_creation_input_tokens": w5 + w1,
				"cache_read_input_tokens": read, "cache_creation": map[string]any{"ephemeral_5m_input_tokens": w5, "ephemeral_1h_input_tokens": w1}}},
	})
}

func rollout(ts, typ string, payload any) string {
	return mustJSON(map[string]any{"timestamp": ts, "type": typ, "payload": payload})
}

func usage(input, cached, output int64) map[string]any {
	return map[string]any{"input_tokens": input, "cached_input_tokens": cached, "output_tokens": output, "total_tokens": input + output}
}

func tokenCount(ts string, total, last map[string]any) string {
	return rollout(ts, "event_msg", map[string]any{"type": "token_count", "info": map[string]any{"total_token_usage": total, "last_token_usage": last}})
}

func receipt(ts, thread, response string, u map[string]any) string {
	return rollout(ts, "token_usage_record", map[string]any{"thread_id": thread, "response_id": response, "usage": u})
}

func mustJSON(v any) string {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func lines(records ...string) string { return strings.Join(records, "\n") + "\n" }

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, path, content string) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
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
