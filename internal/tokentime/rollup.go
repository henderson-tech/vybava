package tokentime

import (
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The rollup's JSON is a cross-repo contract: claude-switcheroo's
// src/arcade/contract.ts (TokentimeRollup) mirrors it field for field. Days
// and hours are local; every token count is split into disjoint components.

// Tokens are the contract's disjoint components (cache writes of both TTLs summed).
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheWrite int64 `json:"cacheWrite"`
	CacheRead  int64 `json:"cacheRead"`
}

func tokensOf(c Counts) Tokens {
	return Tokens{Input: c.Input, Output: c.Output, CacheWrite: c.CacheWrite5m + c.CacheWrite1h, CacheRead: c.CacheRead}
}

// ModelSlice is one model's share of a day.
type ModelSlice struct {
	Model     string   `json:"model"`
	Lane      Lane     `json:"lane"`
	Tokens    Tokens   `json:"tokens"`
	USD       *float64 `json:"usd"`
	Responses int64    `json:"responses"`
}

// DayProject is one project's share of a day.
type DayProject struct {
	Name     string   `json:"name"`
	Root     string   `json:"root"`
	Tokens   int64    `json:"tokens"`
	USD      *float64 `json:"usd"`
	Sessions int      `json:"sessions"`
}

// Day is one local day.
type Day struct {
	Day                   string       `json:"day"`
	Sessions              int          `json:"sessions"`
	LongestSessionMinutes int64        `json:"longestSessionMinutes"`
	Models                []ModelSlice `json:"models"`
	Projects              []DayProject `json:"projects"`
}

// HourModel is one model's tokens in a local hour.
type HourModel struct {
	Model  string `json:"model"`
	Lane   Lane   `json:"lane"`
	Tokens int64  `json:"tokens"`
}

// HourProject is one project's tokens in a local hour.
type HourProject struct {
	Name   string `json:"name"`
	Root   string `json:"root"`
	Tokens int64  `json:"tokens"`
}

// Hour is one local hour.
type Hour struct {
	Hour     string        `json:"hour"`
	Models   []HourModel   `json:"models"`
	Projects []HourProject `json:"projects"`
}

// ProjectModel is one model's tokens inside a project.
type ProjectModel struct {
	Model  string `json:"model"`
	Tokens int64  `json:"tokens"`
}

// Project is one repository with activity inside the requested days. Tokens,
// usd, sessions and activeDays cover those days; firstDay/lastDay are lifetime.
type Project struct {
	Name       string         `json:"name"`
	Root       string         `json:"root"`
	FirstDay   string         `json:"firstDay"`
	LastDay    string         `json:"lastDay"`
	ActiveDays int            `json:"activeDays"`
	Sessions   int            `json:"sessions"`
	Tokens     Tokens         `json:"tokens"`
	USD        *float64       `json:"usd"`
	Models     []ProjectModel `json:"models"`
}

// Model is one model over its whole lifetime.
type Model struct {
	Model    string   `json:"model"`
	Lane     Lane     `json:"lane"`
	FirstDay string   `json:"firstDay"`
	LastDay  string   `json:"lastDay"`
	Tokens   Tokens   `json:"tokens"`
	USD      *float64 `json:"usd"`
}

// Coverage says how complete the index is.
type Coverage struct {
	From         *string `json:"from"`
	Complete     bool    `json:"complete"`
	PendingBytes int64   `json:"pendingBytes"`
}

// Lifetime is everything ever indexed.
type Lifetime struct {
	Tokens    Tokens   `json:"tokens"`
	USD       *float64 `json:"usd"`
	Responses int64    `json:"responses"`
	Sessions  int64    `json:"sessions"`
}

// Rollup is the `tokentime rollup --json` payload.
type Rollup struct {
	GeneratedAt string    `json:"generatedAt"`
	Timezone    string    `json:"timezone"`
	Coverage    Coverage  `json:"coverage"`
	Lifetime    Lifetime  `json:"lifetime"`
	Days        []Day     `json:"days"`
	Hours       []Hour    `json:"hours"`
	Projects    []Project `json:"projects"`
	Models      []Model   `json:"models"`
	// Unpriced lists models with tokens but no price row: their share is
	// missing from every usd figure. Not part of the wire payload.
	Unpriced []string `json:"-"`
}

// RollupOptions bound the rollup.
type RollupOptions struct {
	Days     int
	Hours    int
	Now      time.Time
	Location *time.Location
}

// money sums priced costs; it stays null until something was priced.
type money struct {
	usd    float64
	priced bool
}

func (m *money) add(cost float64, ok bool) {
	if ok {
		m.usd += cost
		m.priced = true
	}
}

func (m money) ptr() *float64 {
	if !m.priced {
		return nil
	}
	v := m.usd
	return &v
}

type row struct {
	hour    int64
	project int64
	model   string
	lane    Lane
	c       Counts
}

// Rollup aggregates the buckets into the contract shape.
func (s *Store) Rollup(opts RollupOptions) (Rollup, error) {
	loc := opts.Location
	if loc == nil {
		loc = time.Local
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.In(loc)
	days, hours := max(opts.Days, 1), max(opts.Hours, 1)
	prices, err := LoadPrices(s.Dir)
	if err != nil {
		return Rollup{}, err
	}
	// One snapshot: a pass committing mid-rollup cannot hand a bucket to a
	// project this read has no name for.
	tx, err := s.db.Begin()
	if err != nil {
		return Rollup{}, err
	}
	defer tx.Rollback()
	ps, err := projects(tx)
	if err != nil {
		return Rollup{}, err
	}
	names, roots := ps.names, ps.roots

	firstDay := dayStart(now.Year(), now.Month(), now.Day()-(days-1), loc)
	// The hour grid is the buckets' own, whole UTC hours: at +05:30 no local
	// whole hour starts a bucket, and time.Date picks one of a fall-back's
	// two 02:00s regardless of which one now is in.
	thisHour := time.Unix(now.Unix()-now.Unix()%3600, 0).In(loc)
	firstHour := thisHour.Add(-time.Duration(hours-1) * time.Hour)
	since := min(firstDay.Unix(), firstHour.Unix())
	since -= since % 3600

	rows, err := windowRows(tx, since, math.MaxInt64, nil)
	if err != nil {
		return Rollup{}, err
	}
	for i := range rows {
		rows[i].project = ps.of(rows[i].project)
	}
	out := Rollup{
		GeneratedAt: now.Format(time.RFC3339), Timezone: ZoneName(loc),
		Days: []Day{}, Hours: []Hour{}, Projects: []Project{}, Models: []Model{},
	}
	dayKey := func(unix int64) string { return time.Unix(unix, 0).In(loc).Format(time.DateOnly) }
	// Buckets start on whole UTC hours; formatting the instant keeps the offset,
	// so the repeated 02:00 of a DST fall-back stays two distinct hours.
	hourKey := func(unix int64) string { return time.Unix(unix, 0).In(loc).Format(time.RFC3339) }
	unpriced := map[string]bool{}
	cost := func(model string, c Counts) (float64, bool) {
		v, ok := prices.Cost(model, c)
		if !ok && c.Total() > 0 {
			unpriced[model] = true
		}
		return v, ok
	}

	// Days.
	type dayAgg struct {
		models   map[string]*ModelSlice
		mUSD     map[string]*money
		projects map[int64]*struct {
			tokens int64
			usd    money
		}
	}
	dayAggs := map[string]*dayAgg{}
	firstDayKey := firstDay.Format(time.DateOnly)
	// Hours.
	type hourAgg struct {
		models   map[string]*HourModel
		projects map[int64]int64
	}
	hourAggs := map[string]*hourAgg{}
	firstHourUnix := firstHour.Unix()
	// Projects in the day window.
	type projAgg struct {
		c      Counts
		usd    money
		models map[string]int64
		days   map[string]bool
	}
	projAggs := map[int64]*projAgg{}

	for _, r := range rows {
		cst, ok := cost(r.model, r.c)
		total := r.c.Total()
		if dk := dayKey(r.hour); dk >= firstDayKey {
			d := dayAggs[dk]
			if d == nil {
				d = &dayAgg{models: map[string]*ModelSlice{}, mUSD: map[string]*money{}, projects: map[int64]*struct {
					tokens int64
					usd    money
				}{}}
				dayAggs[dk] = d
			}
			ms := d.models[r.model]
			if ms == nil {
				ms = &ModelSlice{Model: r.model, Lane: r.lane}
				d.models[r.model], d.mUSD[r.model] = ms, &money{}
			}
			addTokens(&ms.Tokens, r.c)
			ms.Responses += r.c.Responses
			d.mUSD[r.model].add(cst, ok)
			p := d.projects[r.project]
			if p == nil {
				p = &struct {
					tokens int64
					usd    money
				}{}
				d.projects[r.project] = p
			}
			p.tokens += total
			p.usd.add(cst, ok)

			pa := projAggs[r.project]
			if pa == nil {
				pa = &projAgg{models: map[string]int64{}, days: map[string]bool{}}
				projAggs[r.project] = pa
			}
			pa.c.add(r.c)
			pa.usd.add(cst, ok)
			pa.models[r.model] += total
			if total > 0 {
				pa.days[dk] = true
			}
		}
		if r.hour+3600 > firstHourUnix {
			hk := hourKey(r.hour)
			h := hourAggs[hk]
			if h == nil {
				h = &hourAgg{models: map[string]*HourModel{}, projects: map[int64]int64{}}
				hourAggs[hk] = h
			}
			hm := h.models[r.model]
			if hm == nil {
				hm = &HourModel{Model: r.model, Lane: r.lane}
				h.models[r.model] = hm
			}
			hm.Tokens += total
			h.projects[r.project] += total
		}
	}

	sessDays, sessProjects, err := sessionDays(tx, firstDay.Unix()-firstDay.Unix()%3600, dayKey, firstDayKey, ps.of)
	if err != nil {
		return Rollup{}, err
	}

	// Counted from today, never stepped from the previous day: a skipped
	// midnight resolves into the day before, so that step would never advance.
	for i := range days {
		dk := dayStart(now.Year(), now.Month(), now.Day()-(days-1)+i, loc).Format(time.DateOnly)
		day := Day{Day: dk, Models: []ModelSlice{}, Projects: []DayProject{}}
		if sd := sessDays[dk]; sd != nil {
			day.Sessions, day.LongestSessionMinutes = sd.sessions, sd.longestHours*60
		}
		if a := dayAggs[dk]; a != nil {
			for model, ms := range a.models {
				ms.USD = a.mUSD[model].ptr()
				day.Models = append(day.Models, *ms)
			}
			sort.Slice(day.Models, func(i, j int) bool {
				return sumTokens(day.Models[i].Tokens) > sumTokens(day.Models[j].Tokens) ||
					sumTokens(day.Models[i].Tokens) == sumTokens(day.Models[j].Tokens) && day.Models[i].Model < day.Models[j].Model
			})
			for id, p := range a.projects {
				day.Projects = append(day.Projects, DayProject{Name: names[id], Root: roots[id], Tokens: p.tokens, USD: p.usd.ptr(), Sessions: sessProjects[dk][id]})
			}
			sort.Slice(day.Projects, func(i, j int) bool {
				return day.Projects[i].Tokens > day.Projects[j].Tokens ||
					day.Projects[i].Tokens == day.Projects[j].Tokens && day.Projects[i].Name < day.Projects[j].Name
			})
		}
		out.Days = append(out.Days, day)
	}

	for t, i := firstHour, 0; i < hours; t, i = t.Add(time.Hour), i+1 {
		hk := hourKey(t.Unix())
		hour := Hour{Hour: hk, Models: []HourModel{}, Projects: []HourProject{}}
		if a := hourAggs[hk]; a != nil {
			for _, hm := range a.models {
				hour.Models = append(hour.Models, *hm)
			}
			sort.Slice(hour.Models, func(i, j int) bool {
				return hour.Models[i].Tokens > hour.Models[j].Tokens ||
					hour.Models[i].Tokens == hour.Models[j].Tokens && hour.Models[i].Model < hour.Models[j].Model
			})
			for id, tokens := range a.projects {
				hour.Projects = append(hour.Projects, HourProject{Name: names[id], Root: roots[id], Tokens: tokens})
			}
			sort.Slice(hour.Projects, func(i, j int) bool {
				return hour.Projects[i].Tokens > hour.Projects[j].Tokens ||
					hour.Projects[i].Tokens == hour.Projects[j].Tokens && hour.Projects[i].Name < hour.Projects[j].Name
			})
		}
		out.Hours = append(out.Hours, hour)
	}

	spans, err := projectSpans(tx, ps.of, nil)
	if err != nil {
		return Rollup{}, err
	}
	// Unrounded: buckets start on whole UTC hours, so hour >= local midnight
	// keeps exactly the hours of firstDay onwards, even at a +05:30 offset.
	projSessions, err := projectSessions(tx, firstDay.Unix(), ps.of)
	if err != nil {
		return Rollup{}, err
	}
	for id, pa := range projAggs {
		if pa.c.Total() == 0 {
			continue
		}
		p := Project{Name: names[id], Root: roots[id], ActiveDays: len(pa.days), Sessions: projSessions[id],
			Tokens: tokensOf(pa.c), USD: pa.usd.ptr(), Models: []ProjectModel{}}
		if sp, ok := spans[id]; ok {
			p.FirstDay, p.LastDay = dayKey(sp[0]), dayKey(sp[1])
		}
		for model, tokens := range pa.models {
			p.Models = append(p.Models, ProjectModel{Model: model, Tokens: tokens})
		}
		sort.Slice(p.Models, func(i, j int) bool {
			return p.Models[i].Tokens > p.Models[j].Tokens || p.Models[i].Tokens == p.Models[j].Tokens && p.Models[i].Model < p.Models[j].Model
		})
		out.Projects = append(out.Projects, p)
	}
	sort.Slice(out.Projects, func(i, j int) bool {
		a, b := sumTokens(out.Projects[i].Tokens), sumTokens(out.Projects[j].Tokens)
		return a > b || a == b && out.Projects[i].Name < out.Projects[j].Name
	})

	if err := lifetime(tx, &out, cost, dayKey); err != nil {
		return Rollup{}, err
	}
	pending, _ := meta(tx, "pending_bytes")
	indexedAt, _ := meta(tx, "last_index_at")
	out.Coverage.PendingBytes, _ = strconv.ParseInt(pending, 10, 64)
	out.Coverage.Complete = indexedAt != "" && out.Coverage.PendingBytes == 0
	for model := range unpriced {
		out.Unpriced = append(out.Unpriced, model)
	}
	sort.Strings(out.Unpriced)
	return out, nil
}

func addTokens(t *Tokens, c Counts) {
	t.Input += c.Input
	t.Output += c.Output
	t.CacheWrite += c.CacheWrite5m + c.CacheWrite1h
	t.CacheRead += c.CacheRead
}

func sumTokens(t Tokens) int64 { return t.Input + t.Output + t.CacheWrite + t.CacheRead }

// windowRows reads every bucket starting in [since, until) — of the stored
// project ids in members only, or of every project when members is nil.
func windowRows(q querier, since, until int64, members []int64) ([]row, error) {
	query, args := `SELECT hour, project, model, lane, input, output, cache_write_5m, cache_write_1h, cache_read, responses
		FROM buckets WHERE hour >= ? AND hour < ?`, []any{since, until}
	if members != nil {
		in, ids := inList(members)
		query += " AND project IN (" + in + ")"
		args = append(args, ids...)
	}
	rs, err := q.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var rows []row
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.hour, &r.project, &r.model, &r.lane, &r.c.Input, &r.c.Output, &r.c.CacheWrite5m, &r.c.CacheWrite1h, &r.c.CacheRead, &r.c.Responses); err != nil {
			return nil, err
		}
		rows = append(rows, r)
	}
	return rows, rs.Err()
}

// inList is the "?,?,…" placeholder list and the arguments binding ids.
func inList(ids []int64) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

type sessDay struct {
	sessions     int
	longestHours int64
}

// sessionDays counts distinct sessions active per local day and per day and
// project, and the longest session per day: the longest run of CONSECUTIVE
// active hours of any one session (an hour counts when the session had a
// response in it; an idle hour breaks the run), attributed to the local day
// the run ended. An overnight ten-hour run counts as ten hours, once, on its
// last day; a session resumed the next day is two runs, never the gap.
func sessionDays(q querier, since int64, dayKey func(int64) string, firstDayKey string, of func(int64) int64) (map[string]*sessDay, map[string]map[int64]int, error) {
	rs, err := q.Query("SELECT session, hour, project, first, last FROM session_hours WHERE hour >= ?", since)
	if err != nil {
		return nil, nil, err
	}
	defer rs.Close()
	type key struct {
		day     string
		session int64
	}
	active := map[key]bool{}
	perProject := map[string]map[int64]map[int64]bool{}
	for rs.Next() {
		var session, hour, project, first, last int64
		if err := rs.Scan(&session, &hour, &project, &first, &last); err != nil {
			return nil, nil, err
		}
		project = of(project)
		dk := dayKey(hour)
		if dk < firstDayKey {
			continue
		}
		active[key{dk, session}] = true
		if perProject[dk] == nil {
			perProject[dk] = map[int64]map[int64]bool{}
		}
		if perProject[dk][project] == nil {
			perProject[dk][project] = map[int64]bool{}
		}
		perProject[dk][project][session] = true
	}
	if err := rs.Err(); err != nil {
		return nil, nil, err
	}
	days := map[string]*sessDay{}
	day := func(dk string) *sessDay {
		if days[dk] == nil {
			days[dk] = &sessDay{}
		}
		return days[dk]
	}
	for k := range active {
		day(k.day).sessions++
	}
	// Every active hour of each session that touched the window, in order: a run
	// ending in the window may have started before it. session_hours holds one
	// row per session, hour and project, so DISTINCT folds the projects.
	err = eachRun(q, `SELECT DISTINCT session, hour FROM session_hours
		WHERE session IN (SELECT session FROM session_hours WHERE hour >= ?) ORDER BY session, hour`, []any{since}, func(start, end int64) {
		if dk := dayKey(end); dk >= firstDayKey {
			d := day(dk)
			d.longestHours = max(d.longestHours, (end-start)/3600+1)
		}
	})
	if err != nil {
		return nil, nil, err
	}
	counts := map[string]map[int64]int{}
	for dk, projects := range perProject {
		counts[dk] = map[int64]int{}
		for id, sessions := range projects {
			counts[dk][id] = len(sessions)
		}
	}
	return days, counts, nil
}

// eachRun calls fn with every run of consecutive active hours of one session
// — an hour with a response extends the run, an idle hour ends it. The query
// must yield DISTINCT (session, hour) pairs ordered by session, then hour.
func eachRun(q querier, query string, args []any, fn func(start, end int64)) error {
	rs, err := q.Query(query, args...)
	if err != nil {
		return err
	}
	defer rs.Close()
	var current, runStart, runEnd int64 = -1, 0, 0
	for rs.Next() {
		var session, hour int64
		if err := rs.Scan(&session, &hour); err != nil {
			return err
		}
		if session == current && hour == runEnd+3600 {
			runEnd = hour
			continue
		}
		if current >= 0 {
			fn(runStart, runEnd)
		}
		current, runStart, runEnd = session, hour, hour
	}
	if err := rs.Err(); err != nil {
		return err
	}
	if current >= 0 {
		fn(runStart, runEnd)
	}
	return nil
}

func projectSessions(q querier, since int64, of func(int64) int64) (map[int64]int, error) {
	rs, err := q.Query("SELECT DISTINCT project, session FROM session_hours WHERE hour >= ?", since)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	// Counted per canonical project: a folded pair must not count a session twice.
	sessions := map[int64]map[int64]bool{}
	for rs.Next() {
		var id, session int64
		if err := rs.Scan(&id, &session); err != nil {
			return nil, err
		}
		id = of(id)
		if sessions[id] == nil {
			sessions[id] = map[int64]bool{}
		}
		sessions[id][session] = true
	}
	out := map[int64]int{}
	for id, set := range sessions {
		out[id] = len(set)
	}
	return out, rs.Err()
}

// projectSpans is each canonical project's first and last bucket hour — of
// the stored project ids in members only, or of every project when members
// is nil.
func projectSpans(q querier, of func(int64) int64, members []int64) (map[int64][2]int64, error) {
	query, args := "SELECT project, MIN(hour), MAX(hour) FROM buckets", []any(nil)
	if members != nil {
		var in string
		in, args = inList(members)
		query += " WHERE project IN (" + in + ")"
	}
	rs, err := q.Query(query+" GROUP BY project", args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := map[int64][2]int64{}
	for rs.Next() {
		var id, first, last int64
		if err := rs.Scan(&id, &first, &last); err != nil {
			return nil, err
		}
		id = of(id)
		if prev, ok := out[id]; ok {
			first, last = min(first, prev[0]), max(last, prev[1])
		}
		out[id] = [2]int64{first, last}
	}
	return out, rs.Err()
}

func lifetime(q querier, out *Rollup, cost func(string, Counts) (float64, bool), dayKey func(int64) string) error {
	rs, err := q.Query(`SELECT model, MAX(lane), SUM(input), SUM(output), SUM(cache_write_5m), SUM(cache_write_1h), SUM(cache_read), SUM(responses), MIN(hour), MAX(hour)
		FROM buckets GROUP BY model`)
	if err != nil {
		return err
	}
	defer rs.Close()
	var all money
	var total Counts
	oldest := int64(-1)
	for rs.Next() {
		var m Model
		var c Counts
		var first, last int64
		if err := rs.Scan(&m.Model, &m.Lane, &c.Input, &c.Output, &c.CacheWrite5m, &c.CacheWrite1h, &c.CacheRead, &c.Responses, &first, &last); err != nil {
			return err
		}
		var mm money
		mm.add(cost(m.Model, c))
		all.add(mm.usd, mm.priced)
		m.Tokens, m.USD, m.FirstDay, m.LastDay = tokensOf(c), mm.ptr(), dayKey(first), dayKey(last)
		out.Models = append(out.Models, m)
		total.add(c)
		if oldest < 0 || first < oldest {
			oldest = first
		}
	}
	if err := rs.Err(); err != nil {
		return err
	}
	sort.Slice(out.Models, func(i, j int) bool {
		a, b := sumTokens(out.Models[i].Tokens), sumTokens(out.Models[j].Tokens)
		return a > b || a == b && out.Models[i].Model < out.Models[j].Model
	})
	out.Lifetime = Lifetime{Tokens: tokensOf(total), USD: all.ptr(), Responses: total.Responses}
	if err := q.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&out.Lifetime.Sessions); err != nil {
		return err
	}
	if oldest >= 0 {
		from := dayKey(oldest)
		out.Coverage.From = &from
	}
	return nil
}

// projectSet is how stored project ids roll up: a moved checkout's old root
// folds into its new one, and every canonical project has a display name.
type projectSet struct {
	canon map[int64]int64  // every stored id → the id it rolls up under
	names map[int64]string // canonical id → unique display name
	roots map[int64]string // canonical id → absolute root
}

func (p projectSet) of(id int64) int64 {
	if c, ok := p.canon[id]; ok {
		return c
	}
	return id
}

// members lists, ascending, every stored id that rolls up under canon.
func (p projectSet) members(canon int64) []int64 {
	var ids []int64
	for id, c := range p.canon {
		if c == canon {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// projects reads every root ever indexed. Folding happens here, at rollup
// time — stored buckets keep the root they were recorded under, so a wrong
// fold is undone by the next rollup, never baked in.
func projects(q querier) (projectSet, error) {
	rs, err := q.Query(`SELECT p.id, p.root, COALESCE(SUM(b.input + b.output + b.cache_write_5m + b.cache_write_1h + b.cache_read), 0)
		FROM projects p LEFT JOIN buckets b ON b.project = p.id GROUP BY p.id`)
	if err != nil {
		return projectSet{}, err
	}
	defer rs.Close()
	var candidates []nameCandidate
	for rs.Next() {
		var c nameCandidate
		if err := rs.Scan(&c.id, &c.root, &c.tokens); err != nil {
			return projectSet{}, err
		}
		c.live = onDisk(c.root)
		candidates = append(candidates, c)
	}
	if err := rs.Err(); err != nil {
		return projectSet{}, err
	}
	return foldProjects(candidates), nil
}

// onDisk is a root's liveness: it still exists. "" (no cwd) never does.
func onDisk(root string) bool {
	_, err := os.Stat(root)
	return root != "" && err == nil
}

// foldProjects folds a DEAD root (gone from disk) into the one LIVE root that
// shares its basename: checkouts move (~/Documents/Work/FixIt →
// ~/Work/Projects/Org/FixIt) and transcripts keep the old path. With no live
// namesake, or with two or more, the dead root stays its own project — a
// basename alone never picks between candidates. Names are assigned after
// folding, so a folded root never takes a name of its own.
func foldProjects(candidates []nameCandidate) projectSet {
	ps := projectSet{canon: map[int64]int64{}, roots: map[int64]string{}}
	base := func(root string) string { return filepath.Base(filepath.Clean(root)) }
	liveByBase := map[string][]int64{}
	for _, c := range candidates {
		if c.live {
			liveByBase[base(c.root)] = append(liveByBase[base(c.root)], c.id)
		}
	}
	var kept []nameCandidate
	for _, c := range candidates {
		target := c.id
		if live := liveByBase[base(c.root)]; !c.live && c.root != "" && len(live) == 1 {
			target = live[0]
		}
		ps.canon[c.id] = target
		if target == c.id {
			kept = append(kept, c)
			ps.roots[c.id] = c.root
		}
	}
	ps.names = uniqueNames(kept)
	return ps
}

type nameCandidate struct {
	id     int64
	root   string
	tokens int64
	live   bool
}

// uniqueNames: among roots sharing a basename, the one still on disk — then
// the one with the most tokens — keeps the bare basename ("FixIt"); the others
// are qualified by as many parent directories as it takes ("Work/FixIt"), the
// full path as a last resort. A moved checkout therefore never pushes the
// live repository off its short name.
func uniqueNames(candidates []nameCandidate) map[int64]string {
	short := func(root string, depth int) string {
		parts := strings.Split(filepath.ToSlash(filepath.Clean(root)), "/")
		return strings.Join(parts[max(0, len(parts)-depth):], "/")
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.live != b.live {
			return a.live
		}
		if a.tokens != b.tokens {
			return a.tokens > b.tokens
		}
		return a.root < b.root
	})
	names := map[int64]string{}
	taken := map[string]bool{}
	for _, c := range candidates {
		name := c.root
		if c.root == "" {
			name = "unknown"
		} else {
			parts := len(strings.Split(filepath.ToSlash(filepath.Clean(c.root)), "/"))
			for depth := 1; depth < parts; depth++ {
				if candidate := short(c.root, depth); !taken[candidate] {
					name = candidate
					break
				}
			}
		}
		taken[name] = true
		names[c.id] = name
	}
	return names
}

// nameKey is root's last element: what every name uniqueNames can give it
// ends in ("unknown" for ""), and equal keys are equal basenames, all a fold
// matches on. Roots with different keys never touch each other's fold or name.
func nameKey(root string) string {
	if root == "" {
		return "unknown"
	}
	parts := strings.Split(filepath.ToSlash(filepath.Clean(root)), "/")
	return parts[len(parts)-1]
}

// dayStart is the first instant of the local calendar day y-m-d; an
// overflowing m or d rolls over as in time.Date. Where a DST jump skips
// midnight (America/Santiago, America/Havana) time.Date resolves the missing
// 00:00 to 23:00 of the day before — that day then starts at the jump.
func dayStart(y int, m time.Month, d int, loc *time.Location) time.Time {
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	t := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	if t.Day() != day.Day() {
		_, t = t.ZoneBounds()
	}
	return t
}

// ZoneName is the IANA name of loc. time.Local reports "Local", so the
// system zone is recovered from $TZ or the /etc/localtime link.
func ZoneName(loc *time.Location) string {
	if loc != time.Local {
		return loc.String()
	}
	if tz := os.Getenv("TZ"); tz != "" {
		return strings.TrimPrefix(tz, ":")
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok {
			return name
		}
	}
	name, _ := time.Now().Zone()
	return name
}
