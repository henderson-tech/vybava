package tokentime

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAProjectFoldsItsMovedCheckoutAndCutsLocalDays(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	app, moved, tools := filepath.Join(base, "work", "app"), filepath.Join(base, "old", "app"), filepath.Join(base, "work", "tools")
	mkdir(t, app)
	mkdir(t, tools)
	s := indexed(t, base,
		rec{"a", app, "2026-09-20T21:59:00Z", "claude-opus-5-5", 1},      // 23:59 on the 20th: before the range
		rec{"a", app, "2026-09-20T22:00:00Z", "claude-opus-5-5", 2},      // 00:00 on the 21st: inside
		rec{"old", moved, "2026-09-22T08:30:00Z", "claude-opus-5-5", 40}, // the checkout before it moved
		rec{"t", tools, "2026-09-22T08:30:00Z", "claude-opus-5-5", 1000}, // another project
		rec{"a", app, "2026-09-23T21:59:00Z", "claude-fable-5-1", 300},   // 23:59 on the 23rd: inside
		rec{"b", app, "2026-09-23T22:00:00Z", "claude-opus-5-5", 5000},   // 00:00 on the 24th: after
	)
	for _, root := range []string{app, moved} { // the moved root resolves to the project it folds into
		p := projectOf(t, s, root, "2026-09-21", "2026-09-23", "")
		got := fmt.Sprintf("%s %v %s input=%d responses=%d sessions=%d activeDays=%d lifetime=%s..%s peak=%d models=%s:%d,%s:%d",
			p.Name, p.Root == app, p.Bucket, p.Tokens.Input, p.Responses, p.Sessions, p.ActiveDays, p.FirstDay, p.LastDay, *p.PeakHour,
			p.Models[0].Model, sumTokens(p.Models[0].Tokens), p.Models[1].Model, sumTokens(p.Models[1].Tokens))
		want := "app true day input=342 responses=3 sessions=2 activeDays=3 lifetime=2026-09-20..2026-09-24 peak=23 models=claude-fable-5-1:300,claude-opus-5-5:42"
		if got != want {
			t.Errorf("--root %s:\n got %s\nwant %s", root, got, want)
		}
		if series, want := seriesOf(p), "2026-09-21T00:00:00+02:00=claude-opus-5-5:2 2026-09-22T00:00:00+02:00=claude-opus-5-5:40 2026-09-23T00:00:00+02:00=claude-fable-5-1:300"; series != want {
			t.Errorf("series = %s, want %s", series, want)
		}
	}
}

func TestProjectSeriesAreZeroFilledPerBucket(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	app := filepath.Join(base, "app")
	mkdir(t, app)
	s := indexed(t, base,
		rec{"s", app, "2026-10-25T00:30:00Z", "claude-opus-5-5", 1}, // 02:30 CEST
		rec{"s", app, "2026-10-25T01:30:00Z", "claude-opus-5-5", 2}, // 02:30 CET, one hour later
	)
	// The fall-back day has 25 hours; its repeated 02:00 are two entries.
	fallBack := projectOf(t, s, app, "2026-10-25", "2026-10-25", "")
	if fallBack.Bucket != BucketHour || len(fallBack.Series) != 25 ||
		!strings.Contains(seriesOf(fallBack), "T01:00:00+02:00 2026-10-25T02:00:00+02:00=claude-opus-5-5:1 2026-10-25T02:00:00+01:00=claude-opus-5-5:2 2026-10-25T03:00:00+01:00 ") ||
		fallBack.Series[24].Start != "2026-10-25T23:00:00+01:00" || fallBack.Series[0].Models == nil {
		t.Fatalf("fall-back day = %s %d entries: %s", fallBack.Bucket, len(fallBack.Series), seriesOf(fallBack))
	}
	cases := []struct {
		from, to string
		bucket   Bucket
		want     Bucket
		entries  int
	}{
		{"2026-03-29", "2026-03-29", "", BucketHour, 23}, // spring forward
		{"2026-09-24", "2026-09-24", "", BucketHour, 24},
		{"2026-10-25", "2026-10-25", BucketDay, BucketDay, 1},
		{"2026-08-24", "2026-10-24", "", BucketDay, 62},
		{"2026-08-23", "2026-10-24", "", BucketMonth, 3}, // 63 days: from the 23rd, then Sep 1 and Oct 1
	}
	for _, c := range cases {
		if p := projectOf(t, s, app, c.from, c.to, c.bucket); p.Bucket != c.want || len(p.Series) != c.entries {
			t.Errorf("%s..%s bucket %q = %s with %d entries, want %s with %d", c.from, c.to, c.bucket, p.Bucket, len(p.Series), c.want, c.entries)
		}
	}
	months := projectOf(t, s, app, "2026-01-15", "2026-10-31", "")
	if got := seriesOf(months); len(months.Series) != 10 || !strings.HasPrefix(got, "2026-01-15T00:00:00+01:00 2026-02-01T00:00:00+01:00 ") ||
		!strings.HasSuffix(got, " 2026-10-01T00:00:00+02:00=claude-opus-5-5:3") {
		t.Fatalf("months = %s", got)
	}
}

func TestProjectLongestRunStaysInsideTheProjectAndTheRange(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	app, tools := filepath.Join(base, "app"), filepath.Join(base, "tools")
	mkdir(t, app)
	mkdir(t, tools)
	at := func(day, hour int) string {
		return time.Date(2026, 9, day, hour, 30, 0, 0, prague).UTC().Format(time.RFC3339)
	}
	var recs []rec
	for h := 22; h < 26; h++ { // 22:30 on the 20th to 01:30 on the 21st: begins before the range, ends inside it
		recs = append(recs, rec{"early", app, at(20, h), "claude-opus-5-5", 1})
	}
	for h := 10; h < 15; h++ { // five hours on the 22nd, the fourth spent in tools
		cwd := app
		if h == 13 {
			cwd = tools
		}
		recs = append(recs, rec{"hop", cwd, at(22, h), "claude-opus-5-5", 1})
	}
	for h := 22; h < 28; h++ { // 22:30 on the 23rd into the 24th: ends after the range
		recs = append(recs, rec{"late", app, at(23, h), "claude-opus-5-5", 1})
	}
	s := indexed(t, base, recs...)
	// The early run counts whole: 240. Joining the tools hour would read 300, the late run 360.
	if p := projectOf(t, s, app, "2026-09-21", "2026-09-23", ""); p.LongestRunMinutes != 240 || p.Sessions != 3 {
		t.Fatalf("longest run = %d minutes over %d sessions, want 240 over 3", p.LongestRunMinutes, p.Sessions)
	}
}

// Santiago skips its midnight into DST: 2026-09-06 begins at 01:00. The
// day still reads as itself, keeps its neighbour's last hour apart, and
// every day loop — project and rollup — steps past it.
func TestADayWhoseMidnightIsSkippedStartsAtTheJump(t *testing.T) {
	santiago, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := filepath.EvalSymlinks(t.TempDir())
	app := filepath.Join(base, "app")
	mkdir(t, app)
	s := indexed(t, base,
		rec{"s", app, "2026-09-06T03:30:00Z", "claude-opus-5-5", 1}, // 23:30 -04 on the 5th, its last hour
		rec{"s", app, "2026-09-06T04:30:00Z", "claude-opus-5-5", 2}, // 01:30 -03 on the 6th, its first
	)
	days := projectIn(t, s, santiago, app, "2026-09-01", "2026-09-10", "")
	if got := seriesOf(days); len(days.Series) != 10 ||
		!strings.Contains(got, " 2026-09-05T00:00:00-04:00=claude-opus-5-5:1 2026-09-06T01:00:00-03:00=claude-opus-5-5:2 2026-09-07T00:00:00-03:00 ") {
		t.Fatalf("days = %s", got)
	}
	jump := projectIn(t, s, santiago, app, "2026-09-06", "2026-09-06", "")
	if jump.From != "2026-09-06" || jump.To != "2026-09-06" || jump.Tokens.Input != 2 || len(jump.Series) != 23 || jump.Series[0].Start != "2026-09-06T01:00:00-03:00" {
		t.Fatalf("the 6th = %s..%s input=%d %d entries from %s", jump.From, jump.To, jump.Tokens.Input, len(jump.Series), jump.Series[0].Start)
	}
	if before := projectIn(t, s, santiago, app, "2026-09-05", "2026-09-05", ""); before.Tokens.Input != 1 || len(before.Series) != 24 {
		t.Fatalf("the 5th = input=%d over %d entries, want 1 over 24", before.Tokens.Input, len(before.Series))
	}
	r, err := s.Rollup(RollupOptions{Days: 5, Hours: 1, Now: time.Date(2026, 9, 8, 12, 0, 0, 0, santiago), Location: santiago})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range r.Days {
		entry := d.Day
		for _, m := range d.Models {
			entry += fmt.Sprintf("=%d", m.Tokens.Input)
		}
		got = append(got, entry)
	}
	if want := "2026-09-04 2026-09-05=1 2026-09-06=2 2026-09-07 2026-09-08"; strings.Join(got, " ") != want {
		t.Fatalf("rollup days = %v, want %s", got, want)
	}
}

// At +05:30 local midnight falls mid-bucket: the bucket straddling it
// belongs to the day before, for the rollup's sessions as for the project's.
func TestProjectSessionsCutAtLocalMidnightOffTheHour(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := filepath.EvalSymlinks(t.TempDir())
	app := filepath.Join(base, "app")
	mkdir(t, app)
	s := indexed(t, base,
		rec{"early", app, "2026-09-23T18:10:00Z", "claude-opus-5-5", 1}, // 23:40 on the 23rd, in the 18:00Z bucket
		rec{"today", app, "2026-09-24T02:00:00Z", "claude-opus-5-5", 2},
	)
	r, err := s.Rollup(RollupOptions{Days: 1, Hours: 1, Now: time.Date(2026, 9, 24, 12, 0, 0, 0, kolkata), Location: kolkata})
	if err != nil {
		t.Fatal(err)
	}
	p := projectIn(t, s, kolkata, app, "2026-09-24", "2026-09-24", "")
	if len(r.Projects) != 1 || r.Projects[0].Sessions != 1 || r.Projects[0].Tokens.Input != 2 || p.Sessions != 1 || p.Tokens.Input != 2 {
		t.Fatalf("rollup %+v, project sessions=%d input=%d; want one session and 2 tokens in both", r.Projects, p.Sessions, p.Tokens.Input)
	}
}

// rec is one Claude response carrying only input tokens.
type rec struct {
	session, cwd, at, model string
	input                   int64
}

// indexed writes one transcript per session and indexes them into a fresh store.
func indexed(t *testing.T, base string, recs ...rec) *Store {
	t.Helper()
	claude := filepath.Join(base, "claude")
	bySession := map[string][]string{}
	for i, r := range recs {
		bySession[r.session] = append(bySession[r.session], claudeLine(r.session, r.cwd, r.at, fmt.Sprintf("m%d", i), r.model, r.input, 0, 0, 0, 0))
	}
	for session, records := range bySession {
		put(t, filepath.Join(claude, "-p", session+".jsonl"), lines(records...))
	}
	s, err := Open(filepath.Join(base, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.Index(Options{ClaudeRoot: claude, CodexDir: filepath.Join(base, "codex")}); err != nil {
		t.Fatal(err)
	}
	return s
}

func projectOf(t *testing.T, s *Store, root, from, to string, bucket Bucket) ProjectDetail {
	t.Helper()
	return projectIn(t, s, prague, root, from, to, bucket)
}

func projectIn(t *testing.T, s *Store, loc *time.Location, root, from, to string, bucket Bucket) ProjectDetail {
	t.Helper()
	r, err := ParseRange(from, to, bucket)
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Project(ProjectOptions{Root: root, Range: r, Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// seriesOf renders a series as "start=model:tokens" entries; an empty bucket is its start alone.
func seriesOf(p ProjectDetail) string {
	var out []string
	for _, point := range p.Series {
		entry, sep := point.Start, "="
		for _, m := range point.Models {
			entry += fmt.Sprintf("%s%s:%d", sep, m.Model, m.Tokens)
			sep = ","
		}
		out = append(out, entry)
	}
	return strings.Join(out, " ")
}
