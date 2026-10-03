package tokentime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

// focusFixture is newFixture plus two sibling repositories, lib and other,
// and a scratch directory outside every repository. A Claude session and a
// person's Codex thread, both started in app, work in all three; every
// transcript has gone quiet (written 2026-09-24).
type focusRepos struct{ lib, other, scratch string }

func focusFixture(t *testing.T) (fixture, focusRepos) {
	t.Helper()
	f := newFixture(t)
	r := focusRepos{lib: filepath.Join(f.base, "work", "lib"), other: filepath.Join(f.base, "work", "other"), scratch: filepath.Join(f.base, "scratch")}
	mkdir(t, filepath.Join(r.lib, ".git"))
	mkdir(t, filepath.Join(r.other, ".git"))
	mkdir(t, r.scratch)

	human := func(ts string) string {
		return mustJSON(map[string]any{"type": "user", "sessionId": "s4", "cwd": f.repo, "timestamp": ts,
			"origin": map[string]any{"kind": "human"}, "message": map[string]any{"role": "user", "content": "go"}})
	}
	text := func(ts, id string) string { return claudeLine("s4", f.repo, ts, id, "claude-opus-5-5", 1, 1, 0, 0, 0) }
	put(t, filepath.Join(f.claude, "-work-app", "s4.jsonl"), lines(
		human("2026-09-23T10:00:00Z"),
		// A read-only look into a sibling moves nothing.
		claudeTool("s4", f.repo, "2026-09-23T10:01:00Z", "fo_R", "Bash", map[string]any{"command": "cd " + r.other + " && rg foo src | head && git -C " + r.other + " log --oneline"}),
		// The Edit arrives a record later, under the same message id.
		text("2026-09-23T10:02:00Z", "fo_E"),
		claudeTool("s4", f.repo, "2026-09-23T10:02:40Z", "fo_E", "Edit", map[string]any{"file_path": filepath.Join(r.lib, "x.go")}),
		text("2026-09-23T10:03:00Z", "fo_F"),
		human("2026-09-23T10:04:00Z"),
		text("2026-09-23T10:05:00Z", "fo_G"),
		claudeTool("s4", f.repo, "2026-09-23T10:06:00Z", "fo_C", "Bash", map[string]any{"command": "(cd " + r.other + " && git commit -am wip)"}),
		text("2026-09-23T10:07:00Z", "fo_H"),
		// A scratch file is no project's work: the focus stays.
		claudeTool("s4", f.repo, "2026-09-23T10:08:00Z", "fo_W", "Write", map[string]any{"file_path": filepath.Join(r.scratch, "notes.md")}),
	))

	call := func(ts, name, input string) string {
		return rollout(ts, "response_item", map[string]any{"type": "custom_tool_call", "name": name, "call_id": "c-" + ts, "input": input})
	}
	prompt := func(ts string) string {
		return rollout(ts, "event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "UserMessage"}})
	}
	put(t, filepath.Join(f.codex, "sessions", "2026", "09", "23", "rollout-2026-09-23T11-00-00-thread-F.jsonl"), lines(
		rollout("2026-09-23T11:00:00Z", "session_meta", map[string]any{"id": "thread-F", "timestamp": "2026-09-23T11:00:00Z", "cwd": f.repo, "source": "cli"}),
		rollout("2026-09-23T11:00:01Z", "turn_context", map[string]any{"model": "gpt-6-astra", "cwd": f.repo}),
		prompt("2026-09-23T11:00:30Z"),
		call("2026-09-23T11:01:00Z", "exec", `await tools.exec_command({cmd: "make test", workdir: "`+r.other+`"})`),
		receipt("2026-09-23T11:01:30Z", "thread-F", "f1", usage(10, 0, 1)),
		call("2026-09-23T11:02:00Z", "apply_patch", "*** Begin Patch\n*** Update File: "+filepath.Join(r.lib, "y.go")+"\n@@\n-a\n+b\n*** End Patch"),
		receipt("2026-09-23T11:02:30Z", "thread-F", "f2", usage(10, 0, 1)),
		call("2026-09-23T11:03:00Z", "exec", `await tools.exec_command({cmd: "git status", workdir: "`+r.other+`"})`),
		receipt("2026-09-23T11:03:30Z", "thread-F", "f3", usage(10, 0, 1)),
		prompt("2026-09-23T11:04:00Z"),
		receipt("2026-09-23T11:04:30Z", "thread-F", "f4", usage(10, 0, 1)),
	))
	quiet := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
	for _, dir := range []string{f.claude, f.codex} {
		if err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			return os.Chtimes(path, quiet, quiet)
		}); err != nil {
			t.Fatal(err)
		}
	}
	return f, r
}

func claudeTool(session, cwd, ts, id, name string, input map[string]any) string {
	return mustJSON(map[string]any{
		"type": "assistant", "sessionId": session, "cwd": cwd, "timestamp": ts, "requestId": "req_" + id,
		"message": map[string]any{"id": id, "model": "claude-opus-5-5", "content": []any{map[string]any{"type": "tool_use", "id": "tu_" + id, "name": name, "input": input}},
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 1}},
	})
}

// beatsOn is one project's runs of the given kind within [from, to).
func beatsOn(b Beats, root string, ai bool) []BeatRun {
	for _, p := range b.Projects {
		if p.Root == root {
			if ai {
				return p.AI
			}
			return p.Human
		}
	}
	return []BeatRun{}
}

func TestAIMinutesFollowTheRepositoryTheAgentWroteIn(t *testing.T) {
	f, r := focusFixture(t)
	s := f.open(t)
	f.index(t, s)
	b := beatsOf(t, s, "2026-09-23", "2026-09-23")
	ten := func(m int) int64 { return at("2026-09-23T10:00:00Z") + int64(m) }
	eleven := func(m int) int64 { return at("2026-09-23T11:00:00Z") + int64(m) }

	for _, c := range []struct {
		root string
		ai   bool
		want []BeatRun
	}{
		// A prompt resets the focus to the cwd; a read-only Bash in other and a
		// Codex read-only exec there move nothing.
		// (newFixture's thread-R answers in app at 10:01 and 11:00 too.)
		{f.repo, true, []BeatRun{{ten(1), 1}, {ten(5), 1}, {eleven(0), 1}, {eleven(4), 1}}},
		// fo_E's Edit sat a record after its text: its minute and the next are lib's.
		// The Codex patch moves the focus there too, and the read-only exec after it keeps it.
		{r.lib, true, []BeatRun{{ten(2), 2}, {eleven(2), 2}}},
		// The subshell commit and the Codex workdir; a scratch Write keeps the focus.
		{r.other, true, []BeatRun{{ten(6), 3}, {eleven(1), 1}}},
		// Your minutes stay with the cwd.
		{f.repo, false, []BeatRun{{ten(0), 1}, {ten(4), 1}, {eleven(0), 1}, {eleven(4), 1}}},
	} {
		got := onlyIn(beatsOn(b, c.root, c.ai), ten(0), eleven(5))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s ai=%v = %v, want %v", filepath.Base(c.root), c.ai, got, c.want)
		}
	}

	// Tokens stay with the cwd: neither sibling becomes a token project.
	roll := rollupOf(t, s, 2, 3)
	for _, p := range roll.Projects {
		if p.Root == r.lib || p.Root == r.other {
			t.Errorf("rollup lists %s, which only focus beats reached", p.Name)
		}
	}
	rng, _ := ParseRange("2026-09-23", "2026-09-23", BucketDay)
	for _, sel := range []ProjectOptions{{Name: "lib", Range: rng}, {Root: r.other, Range: rng}} {
		if _, err := s.Project(sel); !errors.Is(err, ErrUnknownProject) {
			t.Errorf("project %+v = %v, want ErrUnknownProject", sel, err)
		}
	}
}

// onlyIn keeps the runs starting in [from, to): the fixture's own window.
func onlyIn(runs []BeatRun, from, to int64) []BeatRun {
	out := []BeatRun{}
	for _, r := range runs {
		if r[0] >= from && r[0] < to {
			out = append(out, r)
		}
	}
	return out
}

// A store an older binary indexed filed every AI minute under its cwd. Its
// history is re-read under the focus rule — beats only, under the pass
// budget, coverage incomplete until it is done — and a day is re-attributed
// only when every response charged on it is still on disk: a day a deleted
// transcript answered in keeps the minutes it had, its own included. Not one
// token moves.
func TestTheFocusBackfillReattributesHistoryAndKeepsDeletedTranscripts(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(map[bool]string{false: "kept", true: "deleted"}[deleted], func(t *testing.T) {
			f, r := focusFixture(t)
			day22 := filepath.Join(f.claude, "-work-app", "s5.jsonl")
			put(t, day22, lines(claudeTool("s5", f.repo, "2026-09-22T09:40:00Z", "fo_L", "Edit", map[string]any{"file_path": filepath.Join(r.lib, "z.go")})))
			doomed := filepath.Join(f.claude, "-work-app", "s6.jsonl")
			put(t, doomed, lines(claudeLine("s6", f.repo, "2026-09-22T09:30:00Z", "fo_D", "claude-opus-5-5", 1, 1, 0, 0, 0)))
			quiet := time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC)
			for _, p := range []string{day22, doomed} {
				if err := os.Chtimes(p, quiet, quiet); err != nil {
					t.Fatal(err)
				}
			}
			s := f.open(t)
			f.index(t, s)
			refocused := beatsOf(t, s, "2026-09-22", "2026-09-23").Projects
			if lib := beatsOn(beatsOf(t, s, "2026-09-22", "2026-09-22"), r.lib, true); !reflect.DeepEqual(lib, []BeatRun{{at("2026-09-22T09:40:00Z"), 1}}) {
				t.Fatalf("lib on 22.09 under the focus rule = %v, want 09:40", lib)
			}
			rollup, _ := json.Marshal(rollupOf(t, s, 3, 3))

			// As the schema 5 binary left it: every AI minute under its cwd, app.
			if _, err := s.db.Exec(`INSERT OR IGNORE INTO beats(minute, project, kind)
					SELECT minute, (SELECT id FROM projects WHERE root = ?), 1 FROM beats WHERE kind = 1 AND project IN (SELECT id FROM projects WHERE root IN (?, ?));
				DELETE FROM beats WHERE kind = 1 AND project IN (SELECT id FROM projects WHERE root IN (?, ?));
				UPDATE files SET state = CASE WHEN path LIKE '%rollout-%' THEN json_remove(state, '$.focus', '$.staged') ELSE '' END;
				DROP TABLE focus_beats; DROP TABLE focus_found; ALTER TABLE files DROP COLUMN focus;
				DELETE FROM meta WHERE key = 'focus_rule'; PRAGMA user_version=5`, f.repo, r.lib, r.other, r.lib, r.other); err != nil {
				t.Fatal(err)
			}
			legacy := beatsOf(t, s, "2026-09-22", "2026-09-23").Projects
			if reflect.DeepEqual(legacy, refocused) {
				t.Fatal("the older binary's beats equal the focus rule's: the test proves nothing")
			}
			s.Close()
			if deleted {
				if err := os.Remove(doomed); err != nil {
					t.Fatal(err)
				}
			}

			s = f.open(t)
			passes := 0
			for ; passes < 500; passes++ {
				opts := f.options()
				opts.Budget = 1 // one record per pass
				rep, err := s.Index(opts)
				if err != nil {
					t.Fatal(err)
				}
				if c := beatsOf(t, s, "2026-09-22", "2026-09-23").Coverage; passes == 0 && (rep.BeatsPendingBytes == 0 || c.Complete || c.PendingBytes == 0) {
					t.Fatalf("first budgeted pass: %d bytes owed, coverage %+v; want the re-read pending and coverage incomplete", rep.BeatsPendingBytes, c)
				}
				if rep.BeatsPendingBytes == 0 {
					break
				}
			}
			if passes == 500 {
				t.Fatal("the focus backfill never ended")
			}
			if got, _ := json.Marshal(rollupOf(t, s, 3, 3)); string(got) != string(rollup) {
				t.Fatalf("tokens changed by the backfill:\nbefore %s\n after %s", rollup, got)
			}
			if c := beatsOf(t, s, "2026-09-22", "2026-09-23").Coverage; !c.Complete {
				t.Fatalf("coverage after the backfill = %+v, want complete", c)
			}
			if got := beatsOf(t, s, "2026-09-23", "2026-09-23").Projects; !reflect.DeepEqual(got, onDay(refocused, "2026-09-23")) {
				t.Fatalf("23.09 after the backfill =\n%+v\nwant the focus rule's\n%+v", got, onDay(refocused, "2026-09-23"))
			}
			// With s6 gone, 22.09 cannot be vouched for: it keeps its cwd
			// minutes, s6's 09:30 and s5's 09:40 both under app.
			want22 := onDay(refocused, "2026-09-22")
			if deleted {
				want22 = onDay(legacy, "2026-09-22")
			}
			if got := beatsOf(t, s, "2026-09-22", "2026-09-22").Projects; !reflect.DeepEqual(got, want22) {
				t.Fatalf("22.09 after the backfill =\n%+v\nwant\n%+v", got, want22)
			}
			var staged int
			if err := s.db.QueryRow("SELECT (SELECT COUNT(*) FROM focus_beats) + (SELECT COUNT(*) FROM focus_found)").Scan(&staged); err != nil || staged != 0 {
				t.Fatalf("staging after settling = %d rows, %v; want empty", staged, err)
			}
		})
	}
}

// onDay cuts beat projects to one Prague day, dropping the ones left empty.
func onDay(ps []BeatProject, day string) []BeatProject {
	from, _ := time.ParseInLocation(time.DateOnly, day, prague)
	lo, hi := from.Unix()/60, from.AddDate(0, 0, 1).Unix()/60
	out := []BeatProject{}
	for _, p := range ps {
		p.Human, p.AI = onlyIn(p.Human, lo, hi), onlyIn(p.AI, lo, hi)
		if len(p.Human)+len(p.AI) > 0 {
			out = append(out, p)
		}
	}
	return out
}

// A pass can land between a message's text and its tool call: the minute
// waits in the transcript's state until the message is whole, so it still
// goes where the call wrote.
func TestAMessageSplitAcrossPassesWaitsForItsToolCall(t *testing.T) {
	f, r := focusFixture(t)
	live := filepath.Join(f.claude, "-work-app", "s7.jsonl")
	put(t, live, lines(claudeLine("s7", f.repo, "2026-09-25T09:00:00Z", "fo_S", "claude-opus-5-5", 1, 1, 0, 0, 0)))
	s := f.open(t)
	opts := f.options()
	opts.Now = func() time.Time { return time.Date(2026, 9, 25, 9, 0, 20, 0, time.UTC) }
	if _, err := s.Index(opts); err != nil {
		t.Fatal(err)
	}
	if b := beatsOf(t, s, "2026-09-25", "2026-09-25"); len(b.Projects) != 0 {
		t.Fatalf("beats of a message still streaming = %+v, want none yet", b.Projects)
	}
	appendFile(t, live, lines(claudeTool("s7", f.repo, "2026-09-25T09:00:30Z", "fo_S", "Write", map[string]any{"file_path": filepath.Join(r.lib, "new.go")})))
	opts.Now = func() time.Time { return time.Date(2026, 9, 25, 9, 20, 0, 0, time.UTC) }
	if _, err := s.Index(opts); err != nil {
		t.Fatal(err)
	}
	if got := beatsOn(beatsOf(t, s, "2026-09-25", "2026-09-25"), r.lib, true); !reflect.DeepEqual(got, []BeatRun{{at("2026-09-25T09:00:00Z"), 1}}) {
		t.Fatalf("lib AI = %v, want the message's 09:00", got)
	}
}

// Where commands write: a segment a cd, git -C or --cwd moved off the cwd,
// unless it only inspects (an output redirect still writes).
func TestWritingCommandsNameTheirDirectoryReadsDoNot(t *testing.T) {
	cwd := "/w/app"
	for cmd, want := range map[string][]string{
		"cd /w/lib && rg x | head -5 && git -C /w/lib log --oneline && sed -n 1,5p f": nil,
		"(cd /w/lib && git commit -am x)":                                             {"/w/lib"},
		"git -C ../lib push":                                                          {"/w/lib"},
		"bun --cwd /w/lib test":                                                       {"/w/lib"},
		"cd /w/lib && cat > notes.md <<'EOF'\nhi\nEOF":                                {"/w/lib"},
		"cd /w/lib && ls 2>/dev/null && git branch --list && git status 2>&1":         nil,
		"cd /w/lib && git branch topic":                                               {"/w/lib"},
		"make test":                                                                   nil,
		"cd /w/lib; make":                                                             nil, // unproven after `;`: the cwd
	} {
		var got []string
		for _, w := range commandDirs(cmd, cwd, cwd) {
			got = append(got, w.path)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q writes in %v, want %v", cmd, got, want)
		}
	}
	// An MCP tool's cwd is a workdir: onyx run_command's argv is judged the same way.
	for input, want := range map[string][]wrote{
		`{"argv":["bun","run","deploy"],"cwd":"/w/lib"}`:  {{path: "/w/lib", dir: true}},
		`{"argv":["git","log","-5"],"cwd":"/w/lib"}`:      nil,
		`{"argv":["bash","-c","make"],"cwd":"/w/app"}`:    nil,
		`{"command":"git push","cwd":"../lib"}`:           {{path: "/w/lib", dir: true}},
		`{"query":"select 1","database":"/w/lib/app.db"}`: nil,
	} {
		if got := claudeWrites(transcripts.ToolUse{Name: "mcp__onyx__run_command", Input: json.RawMessage(input)}, cwd); !reflect.DeepEqual(got, want) {
			t.Errorf("mcp %s writes in %v, want %v", input, got, want)
		}
	}
}
