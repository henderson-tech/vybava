package tokentime

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

// Bucket is the width of one entry of a project's series.
type Bucket string

const (
	BucketHour  Bucket = "hour"
	BucketDay   Bucket = "day"
	BucketMonth Bucket = "month"
)

var (
	// ErrUnknownProject: no stored bucket was ever recorded under that root,
	// or no single project carries that name.
	ErrUnknownProject = errors.New("no project was ever indexed under this root")
	// ErrBadRange: a day does not parse or falls outside the calendar a range
	// may name, from is after to, the bucket is unknown, or the range is
	// longer than its bucket's cap.
	ErrBadRange = errors.New("bad range")
	// ErrNotInRepo: a directory inside no git repository names no project.
	ErrNotInRepo = errors.New("not inside a git repository")
)

// A series is allocated whole, one entry per bucket, so ParseRange bounds a
// range before any store is read: its days to 2000-01-01..2100-12-31, and its
// length per bucket — a month of hours (745 entries on a fall-back month),
// three years of days, a century of months.
var (
	earliestDay = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	latestDay   = time.Date(2100, 12, 31, 0, 0, 0, 0, time.UTC)
	rangeCaps   = map[Bucket]int{BucketHour: 31, BucketDay: 1100, BucketMonth: 1200}
)

// ProjectOptions select one project, by Root or by Name, and an inclusive
// range of local days.
type ProjectOptions struct {
	Root     string // repository root as the rollup reports it; "" is its "unknown" project
	Name     string // display name as the rollup shows it ("FixIt", "ADF/forge", "unknown"); set, it selects instead of Root
	Range    Range  // from ParseRange
	Location *time.Location
}

// RootForDir is the root a directory's responses are filed under, by the
// indexer's own rule: a subdirectory or a linked worktree kept anywhere on
// disk is its repository. A directory inside no repository — or one gone
// from disk — is ErrNotInRepo, never a guessed root.
func RootForDir(dir string) (string, error) {
	root, exact := transcripts.GitRoot(dir)
	if !exact {
		return "", fmt.Errorf("%s is %w", dir, ErrNotInRepo)
	}
	return root, nil
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
// midnight a DST jump skips would land them on the day before. A range
// outside the calendar window or longer than its bucket's cap is refused.
func ParseRange(from, to string, bucket Bucket) (Range, error) {
	f, err := time.Parse(time.DateOnly, from)
	if err != nil {
		return Range{}, fmt.Errorf("%w: from %q is not a YYYY-MM-DD day", ErrBadRange, from)
	}
	t, err := time.Parse(time.DateOnly, to)
	if err != nil {
		return Range{}, fmt.Errorf("%w: to %q is not a YYYY-MM-DD day", ErrBadRange, to)
	}
	if f.Before(earliestDay) || t.After(latestDay) {
		return Range{}, fmt.Errorf("%w: %s..%s is not within %s..%s", ErrBadRange, from, to,
			earliestDay.Format(time.DateOnly), latestDay.Format(time.DateOnly))
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
	n, unit := int(t.Sub(f)/(24*time.Hour))+1, "days"
	if bucket == BucketMonth {
		n, unit = (t.Year()-f.Year())*12+int(t.Month())-int(f.Month())+1, "months"
	}
	if limit := rangeCaps[bucket]; n > limit {
		return Range{}, fmt.Errorf("%w: by %s, a range covers at most %d %s; %s..%s is %d", ErrBadRange, bucket, limit, unit, from, to, n)
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
	if opts.Name != "" && opts.Root != "" {
		return ProjectDetail{}, errors.New("select a project by Root or by Name, not both")
	}
	from := dayStart(r.from.Year(), r.from.Month(), r.from.Day(), loc)
	end := dayStart(r.to.Year(), r.to.Month(), r.to.Day()+1, loc)

	prices, err := LoadPrices(s.Dir)
	if err != nil {
		return ProjectDetail{}, err
	}
	// Every query below reads one snapshot: a pass committing between them
	// cannot make one answer disagree with itself.
	tx, err := s.begin()
	if err != nil {
		return ProjectDetail{}, err
	}
	defer tx.Rollback()
	var ps projectSet
	var canon int64
	if opts.Name != "" {
		ps, canon, err = byName(tx, opts.Name)
	} else {
		ps, canon, err = byRoot(tx, opts.Root)
	}
	if err != nil {
		return ProjectDetail{}, err
	}
	// Every stored root that rolls up under this project: a dead checkout
	// folded into a live one is part of it.
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

	longest, err := longestRun(tx, since, until, ids, inRange)
	if err != nil {
		return ProjectDetail{}, err
	}
	out.LongestRunMinutes = longest * 60

	spans, err := projectSpans(tx, ps.of, ids)
	if err != nil {
		return ProjectDetail{}, err
	}
	if sp, ok := spans[canon]; ok {
		out.FirstDay, out.LastDay = dayKey(sp[0]), dayKey(sp[1])
	}
	return out, nil
}

// longestRun is the longest run, in hours, of the members' hours of one
// session among the sessions with a member hour in since..until — counted
// only when it ends inside the range, and then whole, even when it began
// before it. Hours are read with no lower bound, since a run may begin long
// before since, but only up to the first whole hour from until on: the
// hour a run ending past the range continues into, which is all it takes
// to leave that run out — never a session's later lifetime.
func longestRun(q querier, since, until int64, ids []int64, inRange func(hour int64) bool) (int64, error) {
	in, members := inList(ids)
	var longest int64
	args := append(append([]any{}, members...), until+3600, since, until)
	err := eachRun(q, `SELECT DISTINCT session, hour FROM session_hours WHERE project IN (`+in+`) AND hour < ?
		AND session IN (SELECT session FROM session_hours WHERE hour >= ? AND hour < ? AND project IN (`+in+`))
		ORDER BY session, hour`, append(args, members...), func(start, end int64) {
		if inRange(end) {
			longest = max(longest, (end-start)/3600+1)
		}
	})
	return longest, err
}

// byRoot selects the project a stored root rolls up under.
func byRoot(q querier, root string) (projectSet, int64, error) {
	if root != "" { // "" is stored as is: responses recorded without a cwd
		root = filepath.Clean(root)
	}
	var stored int64
	err := q.QueryRow("SELECT id FROM projects WHERE root = ?", root).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return projectSet{}, 0, fmt.Errorf("%w: %s", ErrUnknownProject, root)
	}
	if err != nil {
		return projectSet{}, 0, err
	}
	key := nameKey(root)
	ps, err := namesakes(q, func(k string) bool { return k == key })
	return ps, ps.of(stored), err
}

// byName selects the project the rollup shows under name: the one named
// exactly that, or else the one whose name matches ignoring case. Every
// name ends in its root's last element, so only the roots whose last element
// matches are read and named; a miss suggests from the root list alone.
func byName(q querier, name string) (projectSet, int64, error) {
	key := name[strings.LastIndex(name, "/")+1:]
	ps, err := namesakes(q, func(k string) bool { return strings.EqualFold(k, key) })
	if err != nil {
		return projectSet{}, 0, err
	}
	var exact, folded []int64
	for id, n := range ps.names {
		switch {
		case n == name:
			exact = append(exact, id)
		case strings.EqualFold(n, name):
			folded = append(folded, id)
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = folded
	}
	switch len(matches) {
	case 1:
		return ps, matches[0], nil
	case 0:
		near, err := suggestions(q, name, ps.names)
		if err != nil {
			return projectSet{}, 0, err
		}
		msg := fmt.Sprintf("no project is named %q", name)
		if len(near) > 0 {
			msg += "; closest: " + strings.Join(near, ", ")
		}
		return projectSet{}, 0, nameError(msg)
	}
	// Several: names differing only in case, or the cwd-less "unknown"
	// beside a repository named unknown. Their roots tell them apart.
	sort.Slice(matches, func(i, j int) bool { return ps.roots[matches[i]] < ps.roots[matches[j]] })
	var which []string
	for _, id := range matches {
		root := ps.roots[id]
		if root == "" {
			root = `--root ""`
		}
		which = append(which, fmt.Sprintf("%s (%s)", ps.names[id], root))
	}
	return projectSet{}, 0, nameError(fmt.Sprintf("%q names %d projects: %s; pick one by its exact name or --root",
		name, len(matches), strings.Join(which, ", ")))
}

// nameError is ErrUnknownProject for a name no single project carries.
type nameError string

func (e nameError) Error() string        { return string(e) }
func (e nameError) Is(target error) bool { return target == ErrUnknownProject }

// suggestions are the closest names a miss can offer without summing a
// bucket or stat'ing a root: every stored root's basename — the name of the
// root its basename's namesakes rank first, so each one selects a project —
// and the names already derived for the missed name's own basename, where a
// wrong parent ("x/lib") finds its qualified siblings ("a/lib").
func suggestions(q querier, name string, namesakes map[int64]string) ([]string, error) {
	rs, err := q.Query("SELECT root FROM projects")
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var names []string
	for rs.Next() {
		var root string
		if err := rs.Scan(&root); err != nil {
			return nil, err
		}
		names = append(names, nameKey(root))
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	for _, n := range namesakes {
		names = append(names, n)
	}
	return closestNames(name, names, 3), nil
}

// closestNames ranks names by how close they read to name, ignoring case:
// a name containing it first, then by the nearest edit distance between the
// two whole names or their last elements, the shorter name first on a tie.
// It keeps the first n non-empty ones, each once.
func closestNames(name string, names []string, n int) []string {
	last := func(s string) string { return s[strings.LastIndex(s, "/")+1:] }
	want := strings.ToLower(name)
	score := map[string]int{}
	for _, cand := range names {
		if cand == "" { // the name of a root at "/", which no --project selects
			continue
		}
		lower := strings.ToLower(cand)
		d := min(editDistance(want, lower), editDistance(want, last(lower)), editDistance(last(want), last(lower)))
		if strings.Contains(lower, want) {
			d = 0
		}
		score[cand] = d
	}
	ranked := make([]string, 0, len(score))
	for cand := range score {
		ranked = append(ranked, cand)
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if score[a] != score[b] {
			return score[a] < score[b]
		}
		return len(a) < len(b) || len(a) == len(b) && a < b
	})
	return ranked[:min(n, len(ranked))]
}

// editDistance is the Levenshtein distance between a and b, in runes.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev, cur := make([]int, len(rb)+1), make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			sub := prev[j-1]
			if ra[i-1] != rb[j-1] {
				sub++
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, sub)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

// namesakes is projects() cut down to the roots a project's fold and name
// can depend on: those whose nameKey same accepts. The root list is read
// without touching buckets, only the namesakes are stat'ed, and only their
// buckets are summed for the name's token tie-break — foldProjects gets the
// same inputs it gets in the rollup, so the fold and the lifetime-stable name
// are the rollup's own. Roots with different keys never touch each other's
// fold or name, so accepting several keys names each group as it would alone.
func namesakes(q querier, same func(key string) bool) (projectSet, error) {
	rs, err := q.Query("SELECT id, root FROM projects")
	if err != nil {
		return projectSet{}, err
	}
	defer rs.Close()
	var group []nameCandidate
	var ids []int64
	at := map[int64]int{}
	for rs.Next() {
		var c nameCandidate
		if err := rs.Scan(&c.id, &c.root); err != nil {
			return projectSet{}, err
		}
		if same(nameKey(c.root)) {
			c.live = onDisk(c.root)
			at[c.id] = len(group)
			group, ids = append(group, c), append(ids, c.id)
		}
	}
	if err := rs.Err(); err != nil {
		return projectSet{}, err
	}
	if len(group) == 0 { // no namesake: nothing to sum
		return foldProjects(nil), nil
	}
	in, args := inList(ids)
	sums, err := q.Query(`SELECT project, SUM(input + output + cache_write_5m + cache_write_1h + cache_read)
		FROM buckets WHERE project IN (`+in+`) GROUP BY project`, args...)
	if err != nil {
		return projectSet{}, err
	}
	defer sums.Close()
	for sums.Next() {
		var id, tokens int64
		if err := sums.Scan(&id, &tokens); err != nil {
			return projectSet{}, err
		}
		group[at[id]].tokens = tokens
	}
	if err := sums.Err(); err != nil {
		return projectSet{}, err
	}
	return foldProjects(group), nil
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
