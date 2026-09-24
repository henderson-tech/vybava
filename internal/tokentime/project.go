package tokentime

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"time"
)

// Bucket is the width of one entry of a project's series.
type Bucket string

const (
	BucketHour  Bucket = "hour"
	BucketDay   Bucket = "day"
	BucketMonth Bucket = "month"
)

var (
	// ErrUnknownProject: no stored bucket was ever recorded under that root.
	ErrUnknownProject = errors.New("no project was ever indexed under this root")
	// ErrBadRange: a day does not parse, from is after to, or the bucket is unknown.
	ErrBadRange = errors.New("bad range")
)

// ProjectOptions select one project and an inclusive range of local days.
type ProjectOptions struct {
	Root     string // repository root as the rollup reports it; "" is its "unknown" project
	Range    Range  // from ParseRange
	Location *time.Location
}

// Range is an inclusive range of calendar days and the width of its series
// entries. Only ParseRange builds one, so a caller validates its flags
// before it opens — let alone creates — a store.
type Range struct {
	from, to time.Time // calendar days at UTC midnight; Location turns them into instants
	bucket   Bucket
}

// ParseRange reads from and to as YYYY-MM-DD calendar days, both included,
// and resolves bucket: "" picks hour for one day, month past 62 days, day
// otherwise. The days are parsed in UTC, never in the local zone, where a
// midnight a DST jump skips would land them on the day before.
func ParseRange(from, to string, bucket Bucket) (Range, error) {
	f, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return Range{}, fmt.Errorf("%w: from %q is not a YYYY-MM-DD day", ErrBadRange, from)
	}
	t, err := time.Parse(time.DateOnly, to)
	if err != nil {
		return Range{}, fmt.Errorf("%w: to %q is not a YYYY-MM-DD day", ErrBadRange, to)
	}
	if t.Before(f) {
		return Range{}, fmt.Errorf("%w: from %s is after to %s", ErrBadRange, from, to)
	}
	switch bucket {
	case "":
		bucket = defaultBucket(f, t)
	case BucketHour, BucketDay, BucketMonth:
	default:
		return Range{}, fmt.Errorf("%w: bucket %q is not hour, day or month", ErrBadRange, bucket)
	}
	return Range{from: f, to: t, bucket: bucket}, nil
}

// SeriesPoint is one bucket of a project's series; an empty bucket has no models.
type SeriesPoint struct {
	Start  string         `json:"start"`
	Models []ProjectModel `json:"models"`
}

// ProjectDetail is the `tokentime project --json` payload: one project, its
// moved checkouts folded in, across a range of local days. Everything but
// firstDay/lastDay (lifetime) covers the range.
type ProjectDetail struct {
	Name              string        `json:"name"`
	Root              string        `json:"root"`
	From              string        `json:"from"`
	To                string        `json:"to"`
	Bucket            Bucket        `json:"bucket"`
	Tokens            Tokens        `json:"tokens"`
	USD               *float64      `json:"usd"`
	Responses         int64         `json:"responses"`
	Sessions          int           `json:"sessions"`
	ActiveDays        int           `json:"activeDays"`
	LongestRunMinutes int64         `json:"longestRunMinutes"`
	PeakHour          *int          `json:"peakHour"`
	FirstDay          string        `json:"firstDay"`
	LastDay           string        `json:"lastDay"`
	Models            []ModelSlice  `json:"models"`
	Series            []SeriesPoint `json:"series"`
	// Unpriced lists models with tokens in the range but no price row. Not
	// part of the wire payload.
	Unpriced []string `json:"-"`
}

// Project reads one project across a range without indexing: it never takes
// the index lock, so a running pass cannot hold it up.
func (s *Store) Project(opts ProjectOptions) (ProjectDetail, error) {
	r := opts.Range
	if r.bucket == "" {
		return ProjectDetail{}, fmt.Errorf("%w: build the range with ParseRange", ErrBadRange)
	}
	loc := opts.Location
	if loc == nil {
		loc = time.Local
	}
	from := dayStart(r.from.Year(), r.from.Month(), r.from.Day(), loc)
	end := dayStart(r.to.Year(), r.to.Month(), r.to.Day()+1, loc)
	root := opts.Root
	if root != "" { // "" is stored as is: responses recorded without a cwd
		root = filepath.Clean(root)
	}

	prices, err := LoadPrices(s.Dir)
	if err != nil {
		return ProjectDetail{}, err
	}
	// Every query below reads one snapshot: a pass committing between them
	// cannot make one answer disagree with itself.
	tx, err := s.db.Begin()
	if err != nil {
		return ProjectDetail{}, err
	}
	defer tx.Rollback()
	var stored int64
	err = tx.QueryRow("SELECT id FROM projects WHERE root = ?", root).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectDetail{}, fmt.Errorf("%w: %s", ErrUnknownProject, opts.Root)
	}
	if err != nil {
		return ProjectDetail{}, err
	}
	ps, err := projects(tx)
	if err != nil {
		return ProjectDetail{}, err
	}
	// Every stored root that rolls up under this project: a dead checkout
	// folded into a live one is part of it.
	canon := ps.of(stored)
	ids := ps.members(canon)
	in, members := inList(ids)

	out := ProjectDetail{Name: ps.names[canon], Root: ps.roots[canon], From: r.from.Format(time.DateOnly), To: r.to.Format(time.DateOnly),
		Bucket: r.bucket, Models: []ModelSlice{}}
	dayKey := func(unix int64) string { return time.Unix(unix, 0).In(loc).Format(time.DateOnly) }
	// A bucket belongs to the local day its hour starts in — the rollup's rule.
	inRange := func(hour int64) bool { dk := dayKey(hour); return dk >= out.From && dk <= out.To }
	since, until := from.Unix()-from.Unix()%3600, end.Unix()

	rows, err := windowRows(tx, since, until, ids)
	if err != nil {
		return ProjectDetail{}, err
	}
	starts := seriesStarts(r.from, end, r.bucket, loc)
	series := make([]map[string]int64, len(starts))
	var total Counts
	var usd money
	models, modelUSD := map[string]*ModelSlice{}, map[string]*money{}
	unpriced, days := map[string]bool{}, map[string]bool{}
	var byHour [24]int64
	for _, r := range rows {
		if !inRange(r.hour) {
			continue
		}
		cst, ok := prices.Cost(r.model, r.c)
		tokens := r.c.Total()
		if !ok && tokens > 0 {
			unpriced[r.model] = true
		}
		total.add(r.c)
		usd.add(cst, ok)
		ms := models[r.model]
		if ms == nil {
			ms = &ModelSlice{Model: r.model, Lane: r.lane}
			models[r.model], modelUSD[r.model] = ms, &money{}
		}
		addTokens(&ms.Tokens, r.c)
		ms.Responses += r.c.Responses
		modelUSD[r.model].add(cst, ok)
		if tokens == 0 {
			continue
		}
		days[dayKey(r.hour)] = true
		byHour[time.Unix(r.hour, 0).In(loc).Hour()] += tokens
		i := sort.Search(len(starts), func(i int) bool { return starts[i].Unix() > r.hour }) - 1
		if series[i] == nil {
			series[i] = map[string]int64{}
		}
		series[i][r.model] += tokens
	}
	out.Tokens, out.USD, out.Responses, out.ActiveDays = tokensOf(total), usd.ptr(), total.Responses, len(days)
	for model, ms := range models {
		ms.USD = modelUSD[model].ptr()
		out.Models = append(out.Models, *ms)
	}
	sort.Slice(out.Models, func(i, j int) bool {
		a, b := sumTokens(out.Models[i].Tokens), sumTokens(out.Models[j].Tokens)
		return a > b || a == b && out.Models[i].Model < out.Models[j].Model
	})
	for model := range unpriced {
		out.Unpriced = append(out.Unpriced, model)
	}
	sort.Strings(out.Unpriced)
	for h, tokens := range byHour {
		if tokens > 0 && (out.PeakHour == nil || tokens > byHour[*out.PeakHour]) {
			out.PeakHour = &h
		}
	}
	out.Series = make([]SeriesPoint, len(starts))
	for i, start := range starts {
		p := SeriesPoint{Start: start.Format(time.RFC3339), Models: []ProjectModel{}}
		for model, tokens := range series[i] {
			p.Models = append(p.Models, ProjectModel{Model: model, Tokens: tokens})
		}
		sort.Slice(p.Models, func(i, j int) bool {
			return p.Models[i].Tokens > p.Models[j].Tokens || p.Models[i].Tokens == p.Models[j].Tokens && p.Models[i].Model < p.Models[j].Model
		})
		out.Series[i] = p
	}

	// Sessions with a response in this project inside the range.
	rs, err := tx.Query("SELECT DISTINCT session, hour FROM session_hours WHERE hour >= ? AND hour < ? AND project IN ("+in+")",
		append([]any{since, until}, members...)...)
	if err != nil {
		return ProjectDetail{}, err
	}
	defer rs.Close()
	sessions := map[int64]bool{}
	for rs.Next() {
		var session, hour int64
		if err := rs.Scan(&session, &hour); err != nil {
			return ProjectDetail{}, err
		}
		if inRange(hour) {
			sessions[session] = true
		}
	}
	if err := rs.Err(); err != nil {
		return ProjectDetail{}, err
	}
	out.Sessions = len(sessions)

	// The longest run counts only this project's hours of a session, and only
	// runs that end inside the range — whole, even when they began before it.
	var longest int64
	args := append(append([]any{}, members...), since, until)
	err = eachRun(tx, `SELECT DISTINCT session, hour FROM session_hours WHERE project IN (`+in+`)
		AND session IN (SELECT session FROM session_hours WHERE hour >= ? AND hour < ? AND project IN (`+in+`))
		ORDER BY session, hour`, append(args, members...), func(start, end int64) {
		if inRange(end) {
			longest = max(longest, (end-start)/3600+1)
		}
	})
	if err != nil {
		return ProjectDetail{}, err
	}
	out.LongestRunMinutes = longest * 60

	spans, err := projectSpans(tx, ps.of)
	if err != nil {
		return ProjectDetail{}, err
	}
	if sp, ok := spans[canon]; ok {
		out.FirstDay, out.LastDay = dayKey(sp[0]), dayKey(sp[1])
	}
	return out, nil
}

// defaultBucket: hours for a single day, months once the range outgrows
// 62 days (two months of daily bars), days in between. from and to are
// calendar days at UTC midnight, so every day between them is 24 hours.
func defaultBucket(from, to time.Time) Bucket {
	switch days := int(to.Sub(from)/(24*time.Hour)) + 1; {
	case days == 1:
		return BucketHour
	case days > 62:
		return BucketMonth
	}
	return BucketDay
}

// seriesStarts lists the local start of every bucket from the calendar day
// first up to end. Hours are the buckets' own grid, whole UTC hours from the
// first one starting inside the day — at +05:30 they read 00:30, 01:30, … —
// and absolute: a fall-back day has 25, a spring-forward day 23. Days and
// months are calendar ones, counted from first rather than from the previous
// start (a skipped midnight resolves into the day before, so stepping from it
// never advances), and the first month starts at first, not on the 1st, so no
// entry ever starts before the range.
func seriesStarts(first, end time.Time, bucket Bucket, loc *time.Location) []time.Time {
	y, m, d := first.Date()
	t := dayStart(y, m, d, loc)
	if bucket == BucketHour {
		u := t.Unix()
		t = time.Unix(u+(3600-u%3600)%3600, 0).In(loc)
	}
	var starts []time.Time
	for i := 1; t.Before(end); i++ {
		starts = append(starts, t)
		switch bucket {
		case BucketHour:
			t = t.Add(time.Hour)
		case BucketDay:
			t = dayStart(y, m, d+i, loc)
		default:
			t = dayStart(y, m+time.Month(i), 1, loc)
		}
	}
	return starts
}
