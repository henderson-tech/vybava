package tokentime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// limitsFixture is newFixture — whose rollouts carry no rate-limit reading —
// plus a receipts thread A reading one and two windows, and B, a fork of A
// that copies A's history before its own. Rollouts are dated by name, A's
// and B's after the others, B newest.
func limitsFixture(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t)
	day := filepath.Join(f.codex, "sessions", "2026", "09", "24")
	meta := func(id, ts string) string {
		return rollout(ts, "session_meta", map[string]any{"id": id, "timestamp": ts, "cwd": f.repo, "source": "cli"})
	}
	model := rollout("2026-09-24T10:00:40Z", "turn_context", map[string]any{"model": "gpt-6-astra", "cwd": f.repo})
	call := limitCount("2026-09-24T10:01:00Z", use(2000, 1500, 0, 40, 0), use(2000, 1500, 0, 40, 0), window(10080, 1791000000, 21), window(300, 1790600000, 40))
	put(t, filepath.Join(day, "rollout-2026-09-24T10-00-00-thread-A.jsonl"), lines(
		meta("thread-A", "2026-09-24T10:00:00Z"),
		limitCount("2026-09-24T10:00:30Z", nil, nil, window(10080, 1791000000, 20)), // a refresh before any model
		model,
		call,
		receipt("2026-09-24T10:01:00Z", "thread-A", "a1", use(2000, 1500, 0, 40, 0)),
		limitCount("2026-09-24T10:02:00Z", use(2000, 1500, 0, 40, 0), use(2000, 1500, 0, 40, 0), window(10080, 1791000000, 21)), // unchanged: a refresh
		limitCount("2026-09-24T10:03:00Z", use(2100, 1500, 0, 45, 0), use(100, 0, 0, 5, 0)),                                     // no window
		receipt("2026-09-24T11:00:00Z", "thread-A", "a2", use(300, 100, 20, 10, 4)),
		limitCount("2026-09-24T11:00:00Z", use(2400, 1600, 20, 55, 4), use(300, 100, 20, 10, 4), window(10080, 1791000000, 23)),
	))
	put(t, filepath.Join(day, "rollout-2026-09-24T12-00-00-thread-B.jsonl"), lines(
		meta("thread-B", "2026-09-24T12:00:00Z"),
		meta("thread-A", "2026-09-24T10:00:00Z"), // copied ancestor history
		model,
		call,
		limitCount("2026-09-24T12:01:00Z", use(700, 0, 0, 7, 0), use(700, 0, 0, 7, 0), window(10080, 1791000000, 30)),
	))
	for name, at := range map[string]string{
		filepath.Join(f.codex, "sessions", "2026", "09", "22", "rollout-2026-09-22T08-00-00-thread-L.jsonl"): "2026-09-22T08:10:00Z",
		filepath.Join(f.codex, "sessions", "2026", "09", "23", "rollout-2026-09-23T10-00-00-thread-R.jsonl"): "2026-09-23T11:00:00Z",
		filepath.Join(day, "rollout-2026-09-24T10-00-00-thread-A.jsonl"):                                     "2026-09-24T11:00:00Z",
		filepath.Join(day, "rollout-2026-09-24T12-00-00-thread-B.jsonl"):                                     "2026-09-24T12:01:00Z",
	} {
		mtime, _ := time.Parse(time.RFC3339, at)
		if err := os.Chtimes(name, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func limitCount(ts string, total, last map[string]any, windows ...map[string]any) string {
	var info any
	if total != nil {
		info = map[string]any{"total_token_usage": total, "last_token_usage": last}
	}
	limits := map[string]any{"limit_id": "codex", "limit_name": nil, "primary": nil, "secondary": nil, "plan_type": "pro"}
	if len(windows) > 0 {
		limits["primary"] = windows[0]
	}
	if len(windows) > 1 {
		limits["secondary"] = windows[1]
	}
	return rollout(ts, "event_msg", map[string]any{"type": "token_count", "info": info, "rate_limits": limits})
}

func use(input, cached, write, output, reasoning int64) map[string]any {
	return map[string]any{"input_tokens": input, "cached_input_tokens": cached, "cache_write_input_tokens": write,
		"output_tokens": output, "reasoning_output_tokens": reasoning, "total_tokens": input + output}
}

func window(minutes int, resetsAt int64, pct float64) map[string]any {
	return map[string]any{"used_percent": pct, "window_minutes": minutes, "resets_at": resetsAt}
}

func ms(ts string) int64 {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		panic(err)
	}
	return t.UnixMilli()
}

// limitsWant is every point limitsFixture holds, oldest first.
func limitsWant() []LimitPoint {
	week := func(pct float64) LimitWindow { return LimitWindow{Minutes: 10080, ResetsAt: 1791000000, Pct: pct} }
	return []LimitPoint{
		{TS: ms("2026-09-24T10:00:30Z"), Thread: "thread-A", Plan: "pro", Model: UnknownCodexModel, Windows: []LimitWindow{week(20)}},
		{TS: ms("2026-09-24T10:01:00Z"), Thread: "thread-A", Plan: "pro", Model: "gpt-6-astra", Input: 2000, Cached: 1500, Output: 40,
			Windows: []LimitWindow{week(21), {Minutes: 300, ResetsAt: 1790600000, Pct: 40}}},
		{TS: ms("2026-09-24T10:02:00Z"), Thread: "thread-A", Plan: "pro", Model: "gpt-6-astra", Windows: []LimitWindow{week(21)}},
		{TS: ms("2026-09-24T11:00:00Z"), Thread: "thread-A", Plan: "pro", Model: "gpt-6-astra", Input: 300, Cached: 100, CacheWrite: 20, Output: 10, Reasoning: 4,
			Windows: []LimitWindow{week(23)}},
		{TS: ms("2026-09-24T12:01:00Z"), Thread: "thread-B", Plan: "pro", Model: "gpt-6-astra", Input: 700, Output: 7, Windows: []LimitWindow{week(30)}},
	}
}

func limitsOf(t *testing.T, s *Store, since int64) Limits {
	t.Helper()
	l, err := s.Limits(since)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// Every token_count carrying a reading is one point — a refresh too, with no
// tokens — owned by its thread: copied fork history, readings without a
// window and rollouts older than the readings add none, and neither does a
// re-read or an archived copy of the same rollout.
func TestLimitPointsAreEveryReadingWithTheCallItFollowed(t *testing.T) {
	f := limitsFixture(t)
	s := f.open(t)
	if r := f.index(t, s); r.Points != 5 {
		t.Fatalf("first pass added %d points, want 5", r.Points)
	}
	want := limitsWant()
	cases := []struct {
		since  int64
		points []LimitPoint
		cursor int64
	}{
		{0, want, want[4].TS},
		{want[1].TS, want[2:], want[4].TS}, // strictly after
		{want[4].TS, []LimitPoint{}, want[4].TS},
	}
	for _, c := range cases {
		got := limitsOf(t, s, c.since)
		if !reflect.DeepEqual(got.Points, c.points) || got.Cursor != c.cursor || !got.Backfill.Done || got.Backfill.PendingBytes != 0 {
			t.Fatalf("limits since %d =\n%+v\nwant points\n%+v\ncursor %d, backfill done", c.since, got, c.points, c.cursor)
		}
	}

	if r := f.index(t, s); r.Points != 0 {
		t.Fatalf("an idle pass added %d points", r.Points)
	}
	a := filepath.Join(f.codex, "sessions", "2026", "09", "24", "rollout-2026-09-24T10-00-00-thread-A.jsonl")
	raw, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(f.codex, "archived_sessions", "rollout-2026-09-24T10-00-00-thread-A.jsonl"), string(raw))
	appendFile(t, a, lines(limitCount("2026-09-24T13:00:00Z", nil, nil, window(10080, 1791000000, 31))))
	if r := f.index(t, s); r.Points != 1 {
		t.Fatalf("an archived copy plus one new reading added %d points, want 1", r.Points)
	}
	if got := limitsOf(t, s, want[4].TS).Points; len(got) != 1 || got[0].TS != ms("2026-09-24T13:00:00Z") || got[0].Input != 0 {
		t.Fatalf("points after the append = %+v, want the one refresh", got)
	}
}

// A store indexed before points existed has its rollouts read once more for
// them — points only, under the pass budget, newest first — while the beats
// backlog and its coverage, the rollup and the beats stay exactly as they were.
func TestThePointsBacklogRecoversOldRolloutsAndLeavesBeatsAlone(t *testing.T) {
	f := limitsFixture(t)
	s := f.open(t)
	f.index(t, s)
	answer := func() (string, string, string) {
		t.Helper()
		r, _ := json.Marshal(rollupOf(t, s, 3, 3))
		b, _ := json.Marshal(beatsOf(t, s, "2026-09-22", "2026-09-24"))
		rows, err := s.db.Query("SELECT path, beats FROM files UNION ALL SELECT key, value FROM meta WHERE key LIKE 'beats_%' ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var lags []string
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				t.Fatal(err)
			}
			lags = append(lags, k+"="+v)
		}
		l, _ := json.Marshal(lags)
		return string(r), string(b), string(l)
	}
	_, _, lags := answer()
	if _, err := s.db.Exec(dropPoints + "DELETE FROM meta WHERE key LIKE 'points_%'; PRAGMA user_version=4"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s = f.open(t)
	if _, err := s.Limits(0); !errors.Is(err, ErrStaleSchema) {
		t.Fatalf("limits of a schema 4 store = %v, want ErrStaleSchema", err)
	}
	// A reading appended after the upgrade is the token read's, the history
	// before it the backlog's: neither is recorded twice. A keeps its place
	// behind B, the newest.
	day := filepath.Join(f.codex, "sessions", "2026", "09", "24")
	a, b := filepath.Join(day, "rollout-2026-09-24T10-00-00-thread-A.jsonl"), filepath.Join(day, "rollout-2026-09-24T12-00-00-thread-B.jsonl")
	appended := lines(limitCount("2026-09-24T13:00:00Z", use(2500, 1600, 20, 60, 4), use(100, 0, 0, 5, 0), window(10080, 1791000000, 32)))
	appendFile(t, a, appended)
	if err := os.Chtimes(a, time.Date(2026, 9, 24, 11, 0, 0, 0, time.UTC), time.Date(2026, 9, 24, 11, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	opts := f.options()
	opts.Budget = int64(len(appended)) + info.Size() // the append, then B and nothing more
	r, err := s.Index(opts)
	if err != nil {
		t.Fatal(err)
	}
	want := append(limitsWant(), LimitPoint{TS: ms("2026-09-24T13:00:00Z"), Thread: "thread-A", Plan: "pro", Model: "gpt-6-astra", Input: 100, Output: 5,
		Windows: []LimitWindow{{Minutes: 10080, ResetsAt: 1791000000, Pct: 32}}})
	got := limitsOf(t, s, 0)
	if !reflect.DeepEqual(got.Points, want[4:]) || got.Backfill.Done || got.Backfill.PendingBytes == 0 || got.Backfill.PendingBytes != r.PointsPendingBytes {
		t.Fatalf("after one budgeted pass: %+v (report %d pending); want B's and the appended point, the rest owed", got, r.PointsPendingBytes)
	}
	if st, err := s.Status(); err != nil || st.PointsPendingBytes != r.PointsPendingBytes {
		t.Fatalf("status = %+v, %v; want pointsPendingBytes %d", st, err, r.PointsPendingBytes)
	}

	rollup, beats, _ := answer()
	if r = f.index(t, s); r.PointsPendingBytes != 0 || r.Points != 4 {
		t.Fatalf("unbounded pass: %d points added, %d bytes still owed; want A's other 4 and none", r.Points, r.PointsPendingBytes)
	}
	if got = limitsOf(t, s, 0); !reflect.DeepEqual(got.Points, want) || !got.Backfill.Done {
		t.Fatalf("after the backlog:\n%+v\nwant\n%+v, backfill done", got, want)
	}
	if gotRollup, gotBeats, gotLags := answer(); gotRollup != rollup || gotBeats != beats || gotLags != lags {
		t.Fatalf("the points backlog moved tokens, beats or the beats backlog:\nrollup %v, beats %v\nlags before %s\n      after %s",
			gotRollup == rollup, gotBeats == beats, lags, gotLags)
	}
}
