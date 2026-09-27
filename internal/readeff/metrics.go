package readeff

import (
	"math"
	"math/bits"
	"sort"
	"strings"
)

// Config holds the report's two judgement calls.
type Config struct {
	BigFile int // a whole-file read above this many lines counts as big
	Window  int // calls after a search in which a read of a hit counts
}

// DefaultConfig: 400 lines is where a guarded Read starts asking for a range;
// three calls is "the next thing the agent did".
var DefaultConfig = Config{BigFile: 400, Window: 3}

// Totals are one scope's counts. Lines are what results put into context.
type Totals struct {
	Sessions    int `json:"sessions"`
	Calls       int `json:"calls"`
	ReadCalls   int `json:"read_calls"`
	SearchCalls int `json:"search_calls"`
	EditCalls   int `json:"edit_calls"`
	ReadLines   int `json:"read_lines"`
	SearchLines int `json:"search_lines"`
	OtherLines  int `json:"other_lines"`
	MixedLines  int `json:"mixed_lines"` // one result, several outputs: not attributable
	Changed     int `json:"lines_changed"`
	// Reads with a known range, and those overlapping an earlier read of the
	// same file with no edit of it (and no compaction) in between.
	RangedReads int `json:"ranged_reads"`
	Rereads     int `json:"rereads"`
	RereadLines int `json:"reread_lines"`
	// Whole-file reads of files above Config.BigFile lines.
	BigWhole      int `json:"big_whole_reads"`
	BigWholeLines int `json:"big_whole_lines"`
	// Searches judged for emptiness, the empty ones, and empty ones the agent
	// re-ran within the window and found something.
	EmptySearches int `json:"empty_searches"`
	RetriedEmpty  int `json:"retried_empty"`
	// Searches that printed files, and those followed by a read of one.
	SearchesFound int            `json:"searches_found"`
	SearchHits    int            `json:"search_hits"`
	Stale         int            `json:"stale"`
	Compactions   int            `json:"compactions"`
	Blocked       map[string]int `json:"blocked"`
}

// LinesPerChange is navigation lines (read + search + mixed) per line changed; 0
// when nothing changed.
func (t Totals) LinesPerChange() float64 {
	if t.Changed == 0 {
		return 0
	}
	return round1(float64(t.ReadLines+t.SearchLines+t.MixedLines) / float64(t.Changed))
}

// RereadRate is the share of ranged reads that re-read, in percent.
func (t Totals) RereadRate() float64 { return pct(t.Rereads, t.RangedReads) }

// HitRate is the share of file-printing searches followed by a read of a
// hit, in percent.
func (t Totals) HitRate() float64 { return pct(t.SearchHits, t.SearchesFound) }

func (t *Totals) add(o Totals) {
	t.Sessions += o.Sessions
	t.Calls += o.Calls
	t.ReadCalls += o.ReadCalls
	t.SearchCalls += o.SearchCalls
	t.EditCalls += o.EditCalls
	t.ReadLines += o.ReadLines
	t.SearchLines += o.SearchLines
	t.OtherLines += o.OtherLines
	t.MixedLines += o.MixedLines
	t.Changed += o.Changed
	t.RangedReads += o.RangedReads
	t.Rereads += o.Rereads
	t.RereadLines += o.RereadLines
	t.BigWhole += o.BigWhole
	t.BigWholeLines += o.BigWholeLines
	t.EmptySearches += o.EmptySearches
	t.RetriedEmpty += o.RetriedEmpty
	t.SearchesFound += o.SearchesFound
	t.SearchHits += o.SearchHits
	t.Stale += o.Stale
	t.Compactions += o.Compactions
	for k, v := range o.Blocked {
		if t.Blocked == nil {
			t.Blocked = map[string]int{}
		}
		t.Blocked[k] += v
	}
}

// FileStat is one file's read traffic across sessions.
type FileStat struct {
	Path     string `json:"path"`
	Lines    int    `json:"lines"`
	Reads    int    `json:"reads"`
	Rereads  int    `json:"rereads"`
	Sessions int    `json:"sessions"`
}

// fileAcc accumulates a FileStat; sessions dedupe by id.
type fileAcc struct {
	FileStat
	seen map[string]bool
}

// Step is one call of a session trace: what it was and what it counted as.
type Step struct {
	Index int      `json:"index"`
	Tool  string   `json:"tool"`
	Class Class    `json:"class"`
	Lines int      `json:"lines"`
	Files []string `json:"files,omitempty"`
	Flags []string `json:"flags,omitempty"`
}

// analyze folds one session into its totals and the per-file accumulators.
// steps, when not nil, receives the per-call trace.
func analyze(s Session, cfg Config, files map[string]*fileAcc, steps *[]Step) Totals {
	t := Totals{Sessions: 1}
	held := map[string][]Span{} // ranges read since the file last changed
	flag := func(f string) {
		if steps != nil {
			st := &(*steps)[len(*steps)-1]
			st.Flags = append(st.Flags, f)
		}
	}
	for i, c := range s.Calls {
		if steps != nil {
			*steps = append(*steps, Step{Index: i + 1, Tool: c.Tool, Class: c.Class, Lines: c.Lines, Files: append(spanPaths(c.Spans), c.Edited...)})
		}
		if c.Tool == compactTool {
			t.Compactions++
			clear(held)
			flag("compaction")
			continue
		}
		t.Calls++
		if c.Stale {
			t.Stale++
			flag("stale")
		}
		if c.Blocked != "" {
			if t.Blocked == nil {
				t.Blocked = map[string]int{}
			}
			t.Blocked[c.Blocked]++
			flag("blocked:" + c.Blocked)
			t.OtherLines += c.Lines
			continue
		}
		// Output lines belong to a kind only when every piece of output in the
		// result is of that kind; `cat a.go; rg foo` is counted as mixed.
		switch {
		case bits.OnesCount8(c.outputs) > 1:
			t.MixedLines += c.Lines
			flag("mixed")
		case c.Read:
			t.ReadLines += c.Lines
		case c.Search:
			t.SearchLines += c.Lines
		default:
			t.OtherLines += c.Lines
		}
		// Reads count before edits: `cat a.go; echo x > a.go` read the old
		// file, then changed it.
		if c.Read {
			t.ReadCalls++
			for _, sp := range c.Spans {
				n := sp.N
				if len(c.Spans) == 1 && n == 0 && c.Visible == 1 {
					n = c.Lines
				}
				key := foldWorktree(sp.Path, s.Repo)
				f := files[key]
				if f == nil {
					f = &fileAcc{FileStat: FileStat{Path: key}, seen: map[string]bool{}}
					files[key] = f
				}
				f.Reads++
				f.Lines += n
				if !f.seen[s.ID] {
					f.seen[s.ID] = true
					f.Sessions++
				}
				if size := max(sp.Total, sp.N); sp.Whole && size > cfg.BigFile {
					t.BigWhole++
					t.BigWholeLines += n
					flag("big-whole")
				}
				if sp.Start == 0 {
					continue
				}
				t.RangedReads++
				if overlapsAny(sp, held[sp.Path]) {
					t.Rereads++
					t.RereadLines += n
					f.Rereads++
					flag("reread")
				}
				held[sp.Path] = append(held[sp.Path], sp)
			}
		}
		if c.Search {
			t.SearchCalls++
			if c.Empty {
				t.EmptySearches++
				flag("empty")
				if retried(s.Calls, i, cfg.Window) {
					t.RetriedEmpty++
					flag("retried")
				}
			} else if len(c.Found) > 0 {
				t.SearchesFound++
				if readsHit(s.Calls, i, cfg.Window) {
					t.SearchHits++
					flag("hit")
				} else {
					flag("miss")
				}
			}
		}
		if c.Edit {
			t.EditCalls++
			t.Changed += c.Changed
			for _, p := range c.Edited {
				delete(held, p)
			}
		}
	}
	return t
}

// foldWorktree ranks a worktree's copy of a file as the repository's file:
// <repo>/.worktrees/<name>/x and <repo>/.claude/worktrees/<name>/x are <repo>/x.
func foldWorktree(p, repo string) string {
	if repo == "" {
		return p
	}
	for _, dir := range []string{"/.worktrees/", "/.claude/worktrees/"} {
		prefix := repo + dir
		if rest, ok := strings.CutPrefix(p, prefix); ok {
			if _, file, ok := strings.Cut(rest, "/"); ok {
				return repo + "/" + file
			}
		}
	}
	return p
}

func spanPaths(spans []Span) []string {
	var out []string
	for _, sp := range spans {
		out = append(out, sp.Path)
	}
	return out
}

// overlapsAny reports whether sp shares a line with any held range. A whole
// read spans the file; a held range of unknown length covers its start line.
func overlapsAny(sp Span, held []Span) bool {
	for _, h := range held {
		if sp.Start <= end(h) && h.Start <= end(sp) {
			return true
		}
	}
	return false
}

func end(s Span) int {
	if s.Whole && s.N == 0 {
		return math.MaxInt
	}
	return s.Start + max(s.N, 1) - 1
}

// retried reports an empty search re-run (same segment) within the window
// that found something — the first try ran from the wrong place.
func retried(calls []Call, i, window int) bool {
	q := calls[i].Query
	for _, c := range next(calls, i, window) {
		if q != "" && c.Search && c.Query == q && !c.Empty {
			return true
		}
	}
	return false
}

// readsHit reports a read of a file the search at i printed, within the window.
func readsHit(calls []Call, i, window int) bool {
	found := map[string]bool{}
	for _, p := range calls[i].Found {
		found[p] = true
	}
	for _, c := range next(calls, i, window) {
		for _, sp := range c.Spans {
			if found[sp.Path] {
				return true
			}
		}
	}
	return false
}

// next is the window of calls after i, compaction markers skipped.
func next(calls []Call, i, window int) []Call {
	var out []Call
	for _, c := range calls[i+1:] {
		if len(out) == window {
			break
		}
		if c.Tool != compactTool {
			out = append(out, c)
		}
	}
	return out
}

// topFiles ranks files by lines read, then by re-reads.
func topFiles(files map[string]*fileAcc, n int) []FileStat {
	out := make([]FileStat, 0, len(files))
	for _, f := range files {
		out = append(out, f.FileStat)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Lines != out[j].Lines {
			return out[i].Lines > out[j].Lines
		}
		if out[i].Rereads != out[j].Rereads {
			return out[i].Rereads > out[j].Rereads
		}
		return out[i].Path < out[j].Path
	})
	if n >= 0 && len(out) > n {
		out = out[:n]
	}
	return out
}

func pct(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return round1(100 * float64(a) / float64(b))
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
