package tokentime

import (
	"database/sql"
	"fmt"
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
	Coverage Coverage      `json:"coverage"`
	Projects []BeatProject `json:"projects"`
}

// BeatsOptions select an inclusive range of local days.
type BeatsOptions struct {
	Range    Range // from ParseBeatsRange
	Location *time.Location
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

	rows, err := tx.Query("SELECT minute, project, kind FROM beats WHERE minute >= ? AND minute < ?", since, until)
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

	out := Beats{From: r.from.Format(time.DateOnly), To: r.to.Format(time.DateOnly), Timezone: ZoneName(loc), Projects: []BeatProject{}}
	for canon, m := range minutes {
		out.Projects = append(out.Projects, BeatProject{Name: ps.names[canon], Root: ps.roots[canon], Human: runs(m[beatHuman]), AI: runs(m[beatAI])})
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
