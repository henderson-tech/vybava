package tokentime

import (
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"time"
)

// beatsRangeCap bounds a beats range: a quarter of minute runs is already a
// large answer, and a timesheet asks for one month.
const beatsRangeCap = 92

// BeatRun is [start, minutes]: a run of consecutive active minutes, start in
// unix minutes (unix seconds / 60).
type BeatRun [2]int64

// BeatProject is one project's active minutes across the range.
type BeatProject struct {
	Name  string    `json:"name"`
	Root  string    `json:"root"`
	Human []BeatRun `json:"human"`
	AI    []BeatRun `json:"ai"`
}

// Beats is the `tokentime beats --json` payload, a contract with
// claude-switcheroo's timesheet (src/timesheet/contract.ts, TokentimeBeats).
type Beats struct {
	From     string        `json:"from"`
	To       string        `json:"to"`
	Timezone string        `json:"timezone"`
	AIBridge int           `json:"aiBridge,omitempty"` // BeatsOptions.Bridge; absent: ai is the minutes as recorded
	Coverage Coverage      `json:"coverage"`
	Projects []BeatProject `json:"projects"`
}

// BeatsBridgeCap is the longest bridge, in minutes.
const BeatsBridgeCap = 240

// BeatsOptions select an inclusive range of local days.
type BeatsOptions struct {
	Range    Range // from ParseBeatsRange
	Location *time.Location
	// Bridge, 1..BeatsBridgeCap minutes, fills each session's gaps up to it
	// (see bridgeAI); 0 reports the AI minutes as recorded.
	Bridge int
}

// ParseBeatsRange reads an inclusive range of local days, at most
// beatsRangeCap long.
func ParseBeatsRange(from, to string) (Range, error) {
	r, err := ParseRange(from, to, BucketDay)
	if err != nil {
		return Range{}, err
	}
	if n := int(r.to.Sub(r.from)/(24*time.Hour)) + 1; n > beatsRangeCap {
		return Range{}, fmt.Errorf("%w: beats cover at most %d days; %s..%s is %d", ErrBadRange, beatsRangeCap, from, to, n)
	}
	return r, nil
}

// Beats reads every project's active minutes across a range without
// indexing: never the lock, never a write. Moved checkouts fold into their
// live project, as in the rollup.
func (s *Store) Beats(opts BeatsOptions) (Beats, error) {
	r := opts.Range
	if r.bucket == "" {
		return Beats{}, fmt.Errorf("%w: build the range with ParseBeatsRange", ErrBadRange)
	}
	if opts.Bridge < 0 || opts.Bridge > BeatsBridgeCap {
		return Beats{}, fmt.Errorf("%w: a bridge is 1..%d minutes, not %d", ErrBadRange, BeatsBridgeCap, opts.Bridge)
	}
	loc := opts.Location
	if loc == nil {
		loc = time.Local
	}
	if s.db != nil && s.version > 0 && s.version < beatsSchema {
		return Beats{}, fmt.Errorf("%w: schema %d records no beats, this binary reads %d or later", ErrStaleSchema, s.version, beatsSchema)
	}
	tx, err := s.begin()
	if err != nil {
		return Beats{}, err
	}
	defer tx.Rollback()
	ps, err := projects(tx)
	if err != nil {
		return Beats{}, err
	}
	since := dayStart(r.from.Year(), r.from.Month(), r.from.Day(), loc).Unix() / 60
	until := dayStart(r.to.Year(), r.to.Month(), r.to.Day()+1, loc).Unix() / 60
	// A gap across the range's edge is bridged too: AI minutes up to a
	// bridge beyond it on either side are read, and the runs clipped.
	margin := int64(opts.Bridge)

	rows, err := tx.Query("SELECT minute, project, kind FROM beats WHERE minute >= ? AND minute < ?", since-margin, until+margin)
	if err != nil {
		return Beats{}, err
	}
	defer rows.Close()
	minutes := map[int64]*[2][]int64{} // canonical project → human, ai minutes
	for rows.Next() {
		var minute, project int64
		var kind int
		if err := rows.Scan(&minute, &project, &kind); err != nil {
			return Beats{}, err
		}
		if kind == beatHuman && (minute < since || minute >= until) {
			continue
		}
		canon := ps.of(project)
		m := minutes[canon]
		if m == nil {
			m = &[2][]int64{}
			minutes[canon] = m
		}
		if kind == beatHuman || kind == beatAI {
			m[kind] = append(m[kind], minute)
		}
	}
	if err := rows.Err(); err != nil {
		return Beats{}, err
	}

	out := Beats{From: r.from.Format(time.DateOnly), To: r.to.Format(time.DateOnly), Timezone: ZoneName(loc), AIBridge: opts.Bridge, Projects: []BeatProject{}}
	ai := map[int64][]BeatRun{}
	if opts.Bridge > 0 {
		if ai, err = bridgeAI(tx, s.version >= fileBeatsSchema, ps, minutes, since, until, margin); err != nil {
			return Beats{}, err
		}
	} else {
		for canon, m := range minutes {
			ai[canon] = runs(m[beatAI])
		}
	}
	for canon, m := range minutes {
		p := BeatProject{Name: ps.names[canon], Root: ps.roots[canon], Human: runs(m[beatHuman]), AI: ai[canon]}
		if p.AI == nil {
			p.AI = []BeatRun{}
		}
		if len(p.Human) == 0 && len(p.AI) == 0 {
			continue // only AI minutes a bridge beyond the range
		}
		out.Projects = append(out.Projects, p)
	}
	sort.Slice(out.Projects, func(i, j int) bool { return out.Projects[i].Root < out.Projects[j].Root })

	if out.Coverage, err = beatsCoverage(tx, loc); err != nil {
		return Beats{}, err
	}
	return out, nil
}

// runs merges minutes — unsorted, repeated where folded roots overlap — into
// runs of consecutive minutes, oldest first.
func runs(minutes []int64) []BeatRun {
	out := []BeatRun{}
	sort.Slice(minutes, func(i, j int) bool { return minutes[i] < minutes[j] })
	for _, m := range minutes {
		n := len(out)
		switch {
		case n > 0 && m < out[n-1][0]+out[n-1][1]: // a repeat
		case n > 0 && m == out[n-1][0]+out[n-1][1]:
			out[n-1][1]++
		default:
			out = append(out, BeatRun{m, 1})
		}
	}
	return out
}

// minuteSpan is a half-open run of minutes, [start, end).
type minuteSpan = [2]int64

// rootMinute is an AI minute under one canonical project.
type rootMinute struct{ minute, canon int64 }

// bridgeAI bridges each session on one timeline, like your own attention:
// a file's AI minutes in time order, a gap of up to n minutes to its next one
// is filled and credited to the root of the earlier minute, and a session
// holds one root a minute (see bridgeSession). A session waiting on agents it
// spawned is where they are (see bridgeFamilies). Files' runs are then
// unioned per project: parallel sessions add up, two in one project count
// once. An AI minute of beats no file claims (a day before file_beats, or one
// the focus re-read could not swap: filed by cwd) is bridged per project
// instead, gaps of up to n merged. minutes holds the AI minutes from since-n
// to until+n; the runs are clipped to [since, until).
func bridgeAI(q querier, perFile bool, ps projectSet, minutes map[int64]*[2][]int64, since, until, n int64) (map[int64][]BeatRun, error) {
	claimed := map[rootMinute]bool{} // every AI minute of beats; true once a file answered in it
	for canon, m := range minutes {
		for _, minute := range m[beatAI] {
			claimed[rootMinute{minute, canon}] = false
		}
	}
	spans := map[int64][]minuteSpan{} // canonical project → spans, unsorted
	if perFile {
		rows, err := q.Query(`SELECT b.file, b.minute, b.project, s.session, s.parent FROM file_beats b
			LEFT JOIN file_sessions s ON s.file = b.file WHERE b.minute >= ? AND b.minute < ?`, since-n, until+n)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		files := map[int64][]rootMinute{}
		fams := map[int64]fileSession{}
		for rows.Next() {
			var file, minute, project int64
			var session, parent sql.NullInt64
			if err := rows.Scan(&file, &minute, &project, &session, &parent); err != nil {
				return nil, err
			}
			k := rootMinute{minute, ps.of(project)}
			if _, ok := claimed[k]; ok {
				claimed[k] = true
				files[file] = append(files[file], k)
			}
			if session.Valid {
				fams[file] = fileSession{session: session.Int64, parent: parent.Int64}
			} else {
				fams[file] = fileSession{session: file} // a Claude main transcript
			}
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		bridgeFamilies(files, fams, n, ps, spans)
	}
	legacy := map[int64][]int64{}
	for k, ok := range claimed {
		if !ok {
			legacy[k.canon] = append(legacy[k.canon], k.minute)
		}
	}
	for canon, ms := range legacy {
		sort.Slice(ms, func(i, j int) bool { return ms[i] < ms[j] })
		for i, a := range ms {
			end := a + 1
			if i+1 < len(ms) && ms[i+1]-a <= n {
				end = ms[i+1]
			}
			spans[canon] = append(spans[canon], minuteSpan{a, end})
		}
	}
	out := make(map[int64][]BeatRun, len(spans))
	for canon, sp := range spans {
		out[canon] = spanRuns(sp, since, until)
	}
	return out, nil
}

// sessionSpan is one AI minute of a session, start, and the silence after it
// filled up to end, under one canonical project.
type sessionSpan struct{ canon, start, end int64 }

// agentCover is an agent's span over one minute of the session that spawned it.
type agentCover struct{ canon, from int64 }

// famMinute is one minute of a session, by its session key.
type famMinute struct{ session, minute int64 }

// bridgeFamilies adds every file's spans (bridgeSession). A session waiting
// on agents it spawned is where they are: a minute of its filled silence
// gives way wherever one of them (file_sessions: a Claude session's
// subagents and workflow agents, a Codex thread's spawned threads) covers
// that minute under another root, answering or silent, from an answer no
// older than the session's last — that agent's root already counts it. Its
// own answers never give way, and agents never give way to the session that
// spawned them, so parallel agents still add up. A thread spawned by a
// spawned thread is its parent's agent alone, one level at a time.
func bridgeFamilies(files map[int64][]rootMinute, fams map[int64]fileSession, n int64, ps projectSet, spans map[int64][]minuteSpan) {
	bridged := make(map[int64][]sessionSpan, len(files))
	below := map[famMinute][]agentCover{} // a session's minute → its agents' spans over it
	for file, f := range files {
		bridged[file] = bridgeSession(f, n, ps)
		if parent := fams[file].parent; parent != 0 {
			for _, sp := range bridged[file] {
				for m := sp.start; m < sp.end; m++ {
					below[famMinute{parent, m}] = append(below[famMinute{parent, m}], agentCover{sp.canon, sp.start})
				}
			}
		}
	}
	for file, ss := range bridged {
		key := fams[file].session
		for _, sp := range ss {
			lo := sp.start
			for m := sp.start + 1; m < sp.end; m++ {
				if slices.ContainsFunc(below[famMinute{key, m}], func(c agentCover) bool { return c.canon != sp.canon && c.from >= sp.start }) {
					if lo < m {
						spans[sp.canon] = append(spans[sp.canon], minuteSpan{lo, m})
					}
					lo = m + 1
				}
			}
			if lo < sp.end {
				spans[sp.canon] = append(spans[sp.canon], minuteSpan{lo, sp.end})
			}
		}
	}
}

// bridgeSession is one file's spans: each AI minute, with the gap to its
// next minute when that is at most n minutes later, under one root. A minute
// the session answered in under several goes to the one its next minute
// shares (where it went on), else the one the minute before went to, else
// the first by root — a minute more than n away counts as neither, so the
// answer never depends on how far the range reaches.
func bridgeSession(f []rootMinute, n int64, ps projectSet) []sessionSpan {
	sort.Slice(f, func(i, j int) bool {
		return f[i].minute < f[j].minute || f[i].minute == f[j].minute && ps.roots[f[i].canon] < ps.roots[f[j].canon]
	})
	var out []sessionSpan
	var last, lastAt int64 // the root the minute before went to (ids start at 1), and that minute
	for i := 0; i < len(f); {
		a, j := f[i].minute, i
		for j < len(f) && f[j].minute == a {
			j++
		}
		k := j
		for k < len(f) && f[k].minute == f[j].minute {
			k++
		}
		here, next := f[i:j], f[j:k]
		if len(next) > 0 && next[0].minute-a > n {
			next = nil
		}
		to := f[i].canon
		if s := slices.IndexFunc(here, func(h rootMinute) bool {
			return slices.ContainsFunc(next, func(x rootMinute) bool { return x.canon == h.canon })
		}); s >= 0 {
			to = here[s].canon
		} else if a-lastAt <= n && slices.Contains(here, rootMinute{a, last}) {
			to = last
		}
		end := a + 1
		if len(next) > 0 {
			end = next[0].minute
		}
		out = append(out, sessionSpan{to, a, end})
		last, lastAt, i = to, a, j
	}
	return out
}

// spanRuns unions spans, clipped to [since, until), into runs, oldest first.
func spanRuns(spans []minuteSpan, since, until int64) []BeatRun {
	sort.Slice(spans, func(i, j int) bool { return spans[i][0] < spans[j][0] })
	out := []BeatRun{}
	for _, s := range spans {
		lo, hi := max(s[0], since), min(s[1], until)
		if lo >= hi {
			continue
		}
		if n := len(out); n > 0 && lo <= out[n-1][0]+out[n-1][1] {
			out[n-1][1] = max(out[n-1][1], hi-out[n-1][0])
		} else {
			out = append(out, BeatRun{lo, hi - lo})
		}
	}
	return out
}

// beatsCoverage: from is the first local day whose beats are complete — the
// day after beats_since, never before the first beat. beats_since starts at
// the oldest Claude transcript still on disk when beats began (the ones
// before it were deleted unread) and moves to the last write of any file that
// vanished before its backlog was paid. A store without beats has none. Beats
// are pending while tokens or the backlog are.
func beatsCoverage(q querier, loc *time.Location) (Coverage, error) {
	var c Coverage
	var first sql.NullInt64
	if err := q.QueryRow("SELECT MIN(minute) FROM beats").Scan(&first); err != nil {
		return c, err
	}
	sinceRaw, err := meta(q, "beats_since")
	if err != nil {
		return c, err
	}
	if first.Valid {
		day := time.Unix(first.Int64*60, 0).In(loc).Format(time.DateOnly)
		if since, _ := strconv.ParseInt(sinceRaw, 10, 64); since > 0 {
			t := time.Unix(since, 0).In(loc)
			if next := dayStart(t.Year(), t.Month(), t.Day()+1, loc).Format(time.DateOnly); next > day {
				day = next
			}
		}
		c.From = &day
	}
	pending, _ := meta(q, "pending_bytes")
	backlog, _ := meta(q, "beats_pending_bytes")
	indexedAt, _ := meta(q, "last_index_at")
	p, _ := strconv.ParseInt(pending, 10, 64)
	b, _ := strconv.ParseInt(backlog, 10, 64)
	c.PendingBytes = p + b
	c.Complete = indexedAt != "" && c.PendingBytes == 0
	return c, nil
}
