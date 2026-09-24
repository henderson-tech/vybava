package tokentime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// beatsFixture is newFixture plus prompts: typed ones that count, injected
// and subagent ones that never do, and Codex threads driven by a person, by
// `codex exec` and by another thread. Every Claude transcript is dated
// 2026-09-21T22:30Z (00:30 in Prague), the oldest still on disk.
func beatsFixture(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t)
	human := func(ts, kind string, sidechain bool) string {
		return mustJSON(map[string]any{"type": "user", "sessionId": "s2", "cwd": f.worktree, "timestamp": ts, "isSidechain": sidechain,
			"origin": map[string]any{"kind": kind}, "message": map[string]any{"role": "user", "content": "go on"}})
	}
	session := filepath.Join(f.claude, "-work-app")
	put(t, filepath.Join(session, "s2.jsonl"), lines(
		human("2026-09-23T14:00:20Z", "human", false),
		human("2026-09-23T14:01:05Z", "human", false),
		claudeLine("s2", f.worktree, "2026-09-23T14:02:10Z", "msg_E", "claude-fable-5-1", 1, 1, 0, 0, 0),
		claudeLine("s2", f.worktree, "2026-09-23T14:03:00Z", "msg_F", "claude-fable-5-1", 1, 1, 0, 0, 0),
		human("2026-09-23T14:03:30Z", "task-notification", false),
	))
	put(t, filepath.Join(session, "s2", "subagents", "agent-x.jsonl"), lines(human("2026-09-23T14:05:00Z", "human", true)))

	day := filepath.Join(f.codex, "sessions", "2026", "09", "23")
	prompt := func(ts string) string {
		return rollout(ts, "event_msg", map[string]any{"type": "item_completed", "thread_id": "t", "item": map[string]any{"type": "UserMessage", "id": "u"}})
	}
	meta := func(id, ts, cwd string, source any) string {
		return rollout(ts, "session_meta", map[string]any{"id": id, "timestamp": ts, "cwd": cwd, "source": source})
	}
	put(t, filepath.Join(day, "rollout-2026-09-23T15-00-00-thread-H.jsonl"), lines(
		meta("thread-H", "2026-09-23T15:00:00Z", f.repo, "cli"),
		prompt("2026-09-23T15:00:30Z"),
		receipt("2026-09-23T15:01:00Z", "thread-H", "h1", usage(10, 0, 1)),
		rollout("2026-09-23T15:10:00Z", "event_msg", map[string]any{"type": "user_message", "message": "legacy"}),
	))
	put(t, filepath.Join(day, "rollout-2026-09-23T16-00-00-thread-X.jsonl"), lines(
		meta("thread-X", "2026-09-23T16:00:00Z", f.tools, "exec"),
		prompt("2026-09-23T16:00:10Z"),
		receipt("2026-09-23T16:01:00Z", "thread-X", "x1", usage(10, 0, 1)),
	))
	put(t, filepath.Join(day, "rollout-2026-09-23T16-30-00-thread-S.jsonl"), lines(
		meta("thread-S", "2026-09-23T16:30:00Z", f.tools, map[string]any{"subagent": map[string]any{"other": "guardian"}}),
		prompt("2026-09-23T16:30:10Z"),
	))

	oldest := time.Date(2026, 9, 21, 22, 30, 0, 0, time.UTC)
	if err := filepath.Walk(f.claude, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		return os.Chtimes(path, oldest, oldest)
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

func beatsOf(t *testing.T, s *Store, from, to string) Beats {
	t.Helper()
	r, err := ParseBeatsRange(from, to)
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Beats(BeatsOptions{Range: r, Location: prague})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func at(ts string) int64 {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		panic(err)
	}
	return t.Unix() / 60
}

func TestBeatsAreTheMinutesYouPromptedAndAgentsAnswered(t *testing.T) {
	f := beatsFixture(t)
	s := f.open(t)
	f.index(t, s)
	got := beatsOf(t, s, "2026-09-22", "2026-09-23")

	want := []BeatProject{
		{Name: "app", Root: f.repo,
			// Worktree prompts fold into the repo; a task notification and a
			// subagent's brief are not yours, nor is `codex exec`'s.
			Human: []BeatRun{{at("2026-09-23T14:00:00Z"), 2}, {at("2026-09-23T15:00:00Z"), 1}, {at("2026-09-23T15:10:00Z"), 1}},
			// Repeated content blocks are one minute; copied fork history none.
			AI: []BeatRun{{at("2026-09-22T09:00:00Z"), 1}, {at("2026-09-23T10:01:00Z"), 1}, {at("2026-09-23T11:00:00Z"), 1},
				{at("2026-09-23T12:10:00Z"), 1}, {at("2026-09-23T12:40:00Z"), 1}, {at("2026-09-23T14:02:00Z"), 2}, {at("2026-09-23T15:01:00Z"), 1}}},
		{Name: "tools", Root: f.tools, Human: []BeatRun{},
			AI: []BeatRun{{at("2026-09-22T08:05:00Z"), 1}, {at("2026-09-22T08:10:00Z"), 1}, {at("2026-09-23T13:05:00Z"), 1}, {at("2026-09-23T16:01:00Z"), 1}}},
	}
	if !reflect.DeepEqual(got.Projects, want) {
		t.Fatalf("beats =\n%+v\nwant\n%+v", got.Projects, want)
	}
	// The oldest transcript was written at 00:30 on 22.09: that day may have
	// lost sessions, so beats are complete from the 23rd.
	if got.Coverage.From == nil || *got.Coverage.From != "2026-09-23" || !got.Coverage.Complete || got.Timezone != "Europe/Prague" {
		t.Fatalf("coverage = %+v in %s, want complete from 2026-09-23 in Europe/Prague", got.Coverage, got.Timezone)
	}
	if empty := beatsOf(t, s, "2026-09-24", "2026-09-24"); len(empty.Projects) != 0 {
		t.Fatalf("a day without beats = %+v, want no projects", empty.Projects)
	}
}

// A store an older binary indexed has its history read once more for beats
// — beats only, under the pass budget, newest first — and not one token is
// counted twice. A rollout whose saved state predates beats still records
// the prompts appended to it after the upgrade.
func TestTheBeatsBacklogReadsOldHistoryOnceAndChargesNothing(t *testing.T) {
	f := beatsFixture(t)
	s := f.open(t)
	f.index(t, s)
	answer := func(s *Store) (string, []BeatProject) {
		t.Helper()
		r, _ := json.Marshal(rollupOf(t, s, 2, 3))
		return string(r), beatsOf(t, s, "2026-09-22", "2026-09-23").Projects
	}
	rollup, beats := answer(s)
	if _, err := s.db.Exec(dropBeats + `DELETE FROM meta WHERE key LIKE 'beats_%';
		UPDATE files SET state = json_remove(state, '$.human') WHERE state != '';
		PRAGMA user_version=3`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	appendFile(t, filepath.Join(f.codex, "sessions", "2026", "09", "23", "rollout-2026-09-23T15-00-00-thread-H.jsonl"), lines(
		rollout("2026-09-23T15:20:00Z", "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage"}})))
	beats[0].Human = append(beats[0].Human, BeatRun{at("2026-09-23T15:20:00Z"), 1})

	s = f.open(t)
	passes := 0
	for ; passes < 100; passes++ {
		opts := f.options()
		opts.Budget = 1 // one record per pass
		r, err := s.Index(opts)
		if err != nil {
			t.Fatal(err)
		}
		if passes == 0 && (r.BeatsPendingBytes == 0 || beatsOf(t, s, "2026-09-22", "2026-09-23").Coverage.Complete) {
			t.Fatalf("first budgeted pass left %d bytes owed; want a backlog and incomplete coverage", r.BeatsPendingBytes)
		}
		if r.BeatsPendingBytes == 0 {
			break
		}
	}
	if passes < 2 || passes == 100 {
		t.Fatalf("backlog paid after %d passes, want several and an end", passes)
	}
	gotRollup, gotBeats := answer(s)
	if gotRollup != rollup {
		t.Fatalf("tokens changed by the backlog:\nbefore %s\n after %s", rollup, gotRollup)
	}
	if !reflect.DeepEqual(gotBeats, beats) {
		t.Fatalf("backlog beats:\n got %+v\nwant %+v", gotBeats, beats)
	}
}
