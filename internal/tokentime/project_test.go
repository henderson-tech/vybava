package tokentime

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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

// The runs are read up to the first hour past the range and no further:
// that hour alone tells a run ending inside the range from one running past
// it, in a zone off the hour too, where the range ends mid-hour.
func TestProjectLongestRunStaysInsideTheProjectAndTheRange(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	for _, loc := range []*time.Location{prague, kolkata} {
		base, _ := filepath.EvalSymlinks(t.TempDir())
		app, tools := filepath.Join(base, "app"), filepath.Join(base, "tools")
		mkdir(t, app)
		mkdir(t, tools)
		at := func(day, hour int) string {
			return time.Date(2026, 9, day, hour, 30, 0, 0, loc).UTC().Format(time.RFC3339)
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
		for h := 19; h < 28; h++ { // 19:30 on the 23rd into the 24th: five hours inside, ends after the range
			recs = append(recs, rec{"late", app, at(23, h), "claude-opus-5-5", 1})
		}
		s := indexed(t, base, recs...)
		// The early run counts whole: 240. Joining the tools hour would read 300,
		// the late run 540 — or 300, cut off at the range's end.
		if p := projectIn(t, s, loc, app, "2026-09-21", "2026-09-23", ""); p.LongestRunMinutes != 240 || p.Sessions != 3 {
			t.Fatalf("%s: longest run = %d minutes over %d sessions, want 240 over 3", loc, p.LongestRunMinutes, p.Sessions)
		}

		var id int64
		if err := s.db.QueryRow("SELECT id FROM projects WHERE root = ?", app).Scan(&id); err != nil {
			t.Fatal(err)
		}
		q := &planRecorder{q: s.db}
		since, until := time.Date(2026, 9, 21, 0, 0, 0, 0, loc).Unix(), time.Date(2026, 9, 24, 0, 0, 0, 0, loc).Unix()
		if _, err := longestRun(q, since-since%3600, until, []int64{id}, func(int64) bool { return true }); err != nil {
			t.Fatal(err)
		}
		if len(q.plans) != 1 || !strings.Contains(q.plans[0], "(session=? AND hour<?)") {
			t.Fatalf("%s: the runs' read plans as %q, want each session's hours bounded above", loc, q.plans)
		}
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

// At +05:30 a bucket starts on the half hour: the hour series sits on that
// grid, so every entry is labelled with the start of the bucket it counts.
func TestProjectHoursSitOnTheBucketsGridInAHalfHourZone(t *testing.T) {
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	base, _ := filepath.EvalSymlinks(t.TempDir())
	app := filepath.Join(base, "app")
	mkdir(t, app)
	s := indexed(t, base,
		rec{"s", app, "2026-09-23T18:10:00Z", "claude-opus-5-5", 1}, // 23:40 on the 23rd, in the 18:00Z bucket
		rec{"s", app, "2026-09-23T19:10:00Z", "claude-opus-5-5", 2}, // 00:40: the 24th's first bucket, 19:00Z
		rec{"s", app, "2026-09-24T02:00:00Z", "claude-opus-5-5", 4}, // 07:30
	)
	p := projectIn(t, s, kolkata, app, "2026-09-24", "2026-09-24", "")
	for _, point := range p.Series {
		if at, err := time.Parse(time.RFC3339, point.Start); err != nil || at.Unix()%3600 != 0 {
			t.Fatalf("entry %s is not a whole UTC hour (%v)", point.Start, err)
		}
	}
	got := seriesOf(p)
	if len(p.Series) != 24 || p.Tokens.Input != 6 || !strings.HasPrefix(got, "2026-09-24T00:30:00+05:30=claude-opus-5-5:2 ") ||
		!strings.Contains(got, " 2026-09-24T07:30:00+05:30=claude-opus-5-5:4 ") || !strings.HasSuffix(got, " 2026-09-24T23:30:00+05:30") {
		t.Fatalf("%d entries, input=%d: %s", len(p.Series), p.Tokens.Input, got)
	}
}

// The project's rows are filtered in SQL by its fold set: exactly the rows
// the rollup-wide read filtered by canonical project keeps.
func TestProjectRowsFilteredInSQLMatchTheFoldedSet(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	app, moved, tools := filepath.Join(base, "work", "app"), filepath.Join(base, "old", "app"), filepath.Join(base, "work", "tools")
	mkdir(t, app)
	mkdir(t, tools)
	s := indexed(t, base,
		rec{"a", app, "2026-09-20T10:00:00Z", "claude-opus-5-5", 1}, // before the window
		rec{"old", moved, "2026-09-21T10:00:00Z", "claude-opus-5-5", 2},
		rec{"t", tools, "2026-09-21T10:00:00Z", "claude-opus-5-5", 3},
		rec{"a", app, "2026-09-23T10:00:00Z", "claude-fable-5-1", 4},
	)
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	ps, err := projects(tx)
	if err != nil {
		t.Fatal(err)
	}
	var id int64
	if err := tx.QueryRow("SELECT id FROM projects WHERE root = ?", app).Scan(&id); err != nil {
		t.Fatal(err)
	}
	canon := ps.of(id)
	since, until := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC).Unix(), time.Date(2026, 9, 24, 0, 0, 0, 0, time.UTC).Unix()
	all, err := windowRows(tx, since, until, nil)
	if err != nil {
		t.Fatal(err)
	}
	var want []row
	for _, r := range all {
		if ps.of(r.project) == canon {
			want = append(want, r)
		}
	}
	got, err := windowRows(tx, since, until, ps.members(canon))
	if err != nil {
		t.Fatal(err)
	}
	for _, rows := range [][]row{want, got} {
		sort.Slice(rows, func(i, j int) bool {
			a, b := rows[i], rows[j]
			return a.hour < b.hour || a.hour == b.hour && (a.project < b.project || a.project == b.project && a.model < b.model)
		})
	}
	if len(want) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("SQL-filtered rows = %v\nGo-filtered rows = %v (want the moved checkout's and the fable one)", got, want)
	}
}

// The verb reads only its namesakes' buckets, yet its name, root and lifetime
// span are the rollup's own: the folded dead checkout's older hours count,
// another project's wider span does not.
func TestProjectLifetimeMatchesTheRollupFromItsNamesakesAlone(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	app, moved, tools := filepath.Join(base, "work", "app"), filepath.Join(base, "old", "app"), filepath.Join(base, "work", "tools")
	mkdir(t, app)
	mkdir(t, tools)
	s := indexed(t, base,
		rec{"t1", tools, "2026-08-01T10:00:00Z", "claude-opus-5-5", 1},  // another project, older still
		rec{"old", moved, "2026-09-02T10:00:00Z", "claude-opus-5-5", 2}, // the dead checkout: app's first day
		rec{"a", app, "2026-09-20T10:00:00Z", "claude-opus-5-5", 3},
		rec{"a", app, "2026-09-22T10:00:00Z", "claude-fable-5-1", 4},
		rec{"t2", tools, "2026-09-23T10:00:00Z", "claude-opus-5-5", 5}, // another project, newer
	)
	r, err := s.Rollup(RollupOptions{Days: 7, Hours: 1, Now: time.Date(2026, 9, 24, 12, 0, 0, 0, prague), Location: prague})
	if err != nil {
		t.Fatal(err)
	}
	var want string
	for _, p := range r.Projects {
		if p.Root == app {
			want = fmt.Sprintf("%s %s %s..%s", p.Name, p.Root, p.FirstDay, p.LastDay)
		}
	}
	if want != fmt.Sprintf("app %s 2026-09-02..2026-09-22", app) {
		t.Fatalf("rollup reads app as %q", want)
	}
	for _, root := range []string{app, moved} {
		p := projectOf(t, s, root, "2026-09-21", "2026-09-23", "")
		if got := fmt.Sprintf("%s %s %s..%s", p.Name, p.Root, p.FirstDay, p.LastDay); got != want {
			t.Errorf("--root %s: got %s, want the rollup's %s", root, got, want)
		}
	}
}

// Two LIVE roots share a basename: lifetime tokens pick which one keeps the
// short name, as in the rollup — never the range's tokens, never the path.
func TestProjectNamesLiveNamesakesByLifetimeTokens(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	small, big := filepath.Join(base, "a", "lib"), filepath.Join(base, "b", "lib") // small sorts first by path
	mkdir(t, small)
	mkdir(t, big)
	s := indexed(t, base,
		rec{"b1", big, "2026-09-01T10:00:00Z", "claude-opus-5-5", 1000}, // before the range: lifetime only
		rec{"b2", big, "2026-09-22T10:00:00Z", "claude-opus-5-5", 1},
		rec{"a", small, "2026-09-22T10:00:00Z", "claude-opus-5-5", 50}, // more in the range, less in a lifetime
	)
	r, err := s.Rollup(RollupOptions{Days: 7, Hours: 1, Now: time.Date(2026, 9, 24, 12, 0, 0, 0, prague), Location: prague})
	if err != nil {
		t.Fatal(err)
	}
	rollup := map[string]string{}
	for _, p := range r.Projects {
		rollup[p.Root] = p.Name
	}
	if len(rollup) != 2 || rollup[big] != "lib" || rollup[small] != "a/lib" {
		t.Fatalf("rollup names = %v, want lib for %s and a/lib for %s", rollup, big, small)
	}
	for root, want := range rollup {
		if got := projectOf(t, s, root, "2026-09-21", "2026-09-23", "").Name; got != want {
			t.Errorf("--root %s: name %q, want the rollup's %q", root, got, want)
		}
	}
}

// A name selects the project the rollup shows under it — bare, parent-
// qualified or the cwd-less "unknown" — exactly, else ignoring case when that
// picks one. A name no project carries suggests the closest; one several
// carry lists them with their roots. Both are ErrUnknownProject.
func TestProjectSelectsByTheRollupsName(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	big, small := filepath.Join(base, "b", "lib"), filepath.Join(base, "a", "lib")
	upper, lower := filepath.Join(base, "Forge"), filepath.Join(base, "x", "forge")
	for _, dir := range []string{big, small, upper, lower} {
		mkdir(t, dir)
	}
	s := indexed(t, base,
		rec{"b", big, "2026-09-22T10:00:00Z", "claude-opus-5-5", 1000},
		rec{"a", small, "2026-09-22T10:00:00Z", "claude-opus-5-5", 50},
		rec{"f", upper, "2026-09-22T10:00:00Z", "claude-opus-5-5", 1},
		rec{"g", lower, "2026-09-22T10:00:00Z", "claude-opus-5-5", 1},
		rec{"n", "", "2026-09-22T10:00:00Z", "claude-opus-5-5", 1},
	)
	r, err := ParseRange("2026-09-21", "2026-09-23", "")
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"lib": big, "a/lib": small, "LIB": big, "forge": lower, "Forge": upper, "unknown": ""} {
		p, err := s.Project(ProjectOptions{Name: name, Range: r, Location: prague})
		if err != nil || p.Root != want {
			t.Errorf("--project %s = %q, %v; want %q", name, p.Root, err, want)
		}
	}
	for name, want := range map[string]string{
		"lbi":   `no project is named "lbi"; closest: lib, a/lib, `,
		"FORGE": fmt.Sprintf(`"FORGE" names 2 projects: Forge (%s), forge (%s)`, upper, lower),
	} {
		_, err := s.Project(ProjectOptions{Name: name, Range: r, Location: prague})
		if !errors.Is(err, ErrUnknownProject) || !strings.Contains(fmt.Sprint(err), want) {
			t.Errorf("--project %s = %v; want ErrUnknownProject saying %s", name, err, want)
		}
	}
}

// OpenReadOnly creates nothing and changes nothing: a missing store is
// ErrNoStore with no directory made, an existing one is read without its
// database moving, and one an older binary wrote is refused, never migrated.
func TestReadOnlyOpenNeverCreatesMigratesOrWrites(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	missing := filepath.Join(base, "missing")
	if _, err := OpenReadOnly(missing); !errors.Is(err, ErrNoStore) {
		t.Fatalf("missing store = %v, want ErrNoStore", err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the missing store's directory after OpenReadOnly: %v, want it never created", err)
	}

	app, state := filepath.Join(base, "app"), filepath.Join(base, "state")
	mkdir(t, app)
	indexed(t, base, rec{"s", app, "2026-09-23T10:00:00Z", "claude-opus-5-5", 7}).Close()
	db := filepath.Join(state, "tokentime.db")
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(db, past, past); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	listing := func() map[string]bool {
		entries, err := os.ReadDir(state)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, e := range entries {
			names[e.Name()] = true
		}
		return names
	}
	was := listing()
	ro, err := OpenReadOnly(state)
	if err != nil {
		t.Fatal(err)
	}
	if p := projectOf(t, ro, app, "2026-09-23", "2026-09-23", ""); p.Tokens.Input != 7 {
		t.Fatalf("read-only project input = %d, want 7", p.Tokens.Input)
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(db)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("tokentime.db went from %d bytes @ %v to %d @ %v", before.Size(), before.ModTime(), after.Size(), after.ModTime())
	}
	for name := range listing() { // a WAL reader's own coordination files may appear, nothing else
		if !was[name] && name != "tokentime.db-wal" && name != "tokentime.db-shm" {
			t.Fatalf("OpenReadOnly created %s", name)
		}
	}

	old := filepath.Join(base, "old-state")
	mkdir(t, old)
	v1, _, err := openDB(filepath.Join(old, "tokentime.db"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v1.Exec("CREATE TABLE files(path TEXT PRIMARY KEY, cursor TEXT NOT NULL, state TEXT NOT NULL DEFAULT ''); PRAGMA user_version=1"); err != nil {
		t.Fatal(err)
	}
	v1.Close()
	if _, err := OpenReadOnly(old); !errors.Is(err, ErrStaleSchema) {
		t.Fatalf("schema 1 store = %v, want ErrStaleSchema", err)
	}
	check, version, err := openDB(filepath.Join(old, "tokentime.db"), "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer check.Close()
	if version != 1 {
		t.Fatalf("schema after a read-only open = %d, want 1: it was migrated", version)
	}
}

// A schema 2 store — no buckets_by_project yet — is refused read-only and
// migrated by the next index pass. The project verb's bucket reads (its
// namesakes' lifetime sum, its range, its span) then seek that index instead
// of scanning the permanent table, and the rollup and the verb answer byte
// for byte what they answered without it.
func TestSchema3SeeksAProjectsBucketsWithoutChangingAnAnswer(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	f.index(t, s)
	answers := func(s *Store) string {
		t.Helper()
		out, err := json.Marshal([]any{rollupOf(t, s, 2, 3), projectOf(t, s, f.repo, "2026-09-22", "2026-09-23", "")})
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if _, err := s.db.Exec("DROP INDEX buckets_by_project; PRAGMA user_version=2"); err != nil { // as the schema 2 binary left it
		t.Fatal(err)
	}
	before := answers(s)
	s.Close()
	if _, err := OpenReadOnly(f.state); !errors.Is(err, ErrStaleSchema) {
		t.Fatalf("schema 2 store opened read-only = %v, want ErrStaleSchema", err)
	}

	s = f.open(t)
	f.index(t, s)
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema after an index pass = %d, want %d", version, schemaVersion)
	}
	if after := answers(s); after != before {
		t.Fatalf("answers changed with the index:\nbefore %s\n after %s", before, after)
	}

	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	q := &planRecorder{q: tx}
	var stored int64
	if err := tx.QueryRow("SELECT id FROM projects WHERE root = ?", f.repo).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	ps, err := namesakes(q, func(key string) bool { return key == nameKey(f.repo) })
	if err != nil {
		t.Fatal(err)
	}
	ids := ps.members(ps.of(stored))
	since, until := time.Date(2026, 9, 21, 22, 0, 0, 0, time.UTC).Unix(), time.Date(2026, 9, 23, 22, 0, 0, 0, time.UTC).Unix()
	if _, err := windowRows(q, since, until, ids); err != nil {
		t.Fatal(err)
	}
	if _, err := projectSpans(q, ps.of, ids); err != nil {
		t.Fatal(err)
	}
	var reads int
	for _, plan := range q.plans {
		if !strings.Contains(plan, "buckets") { // the root list
			continue
		}
		reads++
		if !strings.Contains(plan, "SEARCH buckets USING") || !strings.Contains(plan, "INDEX buckets_by_project (project=?") {
			t.Errorf("a project's bucket read plans as %q, want a SEARCH on buckets_by_project", plan)
		}
	}
	if reads != 3 {
		t.Fatalf("%d bucket reads planned, want the sum, the window and the span: %q", reads, q.plans)
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

// planRecorder runs every Query through its querier after recording the
// query's EXPLAIN QUERY PLAN, its steps joined by "; ".
type planRecorder struct {
	q     querier
	plans []string
}

func (p *planRecorder) Query(query string, args ...any) (*sql.Rows, error) {
	rs, err := p.q.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		return nil, err
	}
	var steps []string
	for rs.Next() {
		var id, parent, unused int
		var detail string
		if err := rs.Scan(&id, &parent, &unused, &detail); err != nil {
			rs.Close()
			return nil, err
		}
		steps = append(steps, detail)
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	p.plans = append(p.plans, strings.Join(steps, "; "))
	return p.q.Query(query, args...)
}

func (p *planRecorder) QueryRow(query string, args ...any) *sql.Row {
	return p.q.QueryRow(query, args...)
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
