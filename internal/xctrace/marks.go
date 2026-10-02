package xctrace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DefaultTapLag is the delay between a W3C action leaving the host and the
// touch landing on the device; marks and taps are shifted by it.
const DefaultTapLag = 250 * time.Millisecond

// Step is one labelled interaction span on the trace's own clock (seconds
// from the recording's start).
type Step struct {
	Label string  `json:"label"`
	From  float64 `json:"from"`
	To    float64 `json:"to"`
}

// Kind is the step's label without its round prefix ("r2 multi" -> "multi").
func (s Step) Kind() string { return StepKind(s.Label) }

var roundPrefix = regexp.MustCompile(`^r\d+ `)

// StepKind strips a "rN " round prefix.
func StepKind(label string) string { return roundPrefix.ReplaceAllString(label, "") }

// Mark is one entry of a marks file. A wall-clock mark (`atMs`, epoch ms)
// opens a step that runs until the next mark; a trace-relative entry
// (`from`/`to`, seconds, the `<trace>.steps.json` shape) is a step already.
type Mark struct {
	Label string   `json:"label"`
	AtMs  *float64 `json:"atMs,omitempty"`
	From  *float64 `json:"from,omitempty"`
	To    *float64 `json:"to,omitempty"`
}

// LoadMarks reads a marks file: a JSON array of Mark.
func LoadMarks(path string) ([]Mark, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, diag(DiagFileUnreadable, fmt.Sprintf("cannot read marks %s: %v", path, err), "pass the marks file the scenario wrote beside its trace")
	}
	var marks []Mark
	if err := json.Unmarshal(raw, &marks); err != nil {
		// A runner that appends one mark per step writes JSONL instead.
		marks = nil
		for n, line := range strings.Split(string(raw), "\n") {
			if line = strings.TrimSpace(line); line == "" {
				continue
			}
			var m Mark
			if lerr := json.Unmarshal([]byte(line), &m); lerr != nil {
				return nil, diag(DiagFileUnreadable, fmt.Sprintf("marks %s is neither a JSON array nor JSONL of {label, atMs} or {label, from, to} (line %d: %v)", path, n+1, lerr), "pass the marks file the scenario wrote beside its trace")
			}
			marks = append(marks, m)
		}
	}
	for i, m := range marks {
		if m.Label == "" || (m.AtMs == nil && (m.From == nil || m.To == nil)) {
			return nil, diag(DiagFileUnreadable, fmt.Sprintf("marks %s entry %d needs a label and atMs, or from and to", path, i), "pass the marks file the scenario wrote beside its trace")
		}
	}
	return marks, nil
}

// StepsFromMarks maps marks onto the recording: a wall-clock mark lands at
// `atMs - start + tapLag`; it runs until the next mark or the recording's
// end. Steps entirely outside [0, recordingS] are dropped and counted.
func StepsFromMarks(marks []Mark, start time.Time, tapLag time.Duration, recordingS float64) ([]Step, int) {
	type opened struct {
		label string
		at    float64
	}
	var steps []Step
	var walls []opened
	for _, m := range marks {
		if m.AtMs != nil {
			at := (*m.AtMs-float64(start.UnixNano())/1e6)/1000 + tapLag.Seconds()
			walls = append(walls, opened{m.Label, at})
			continue
		}
		steps = append(steps, Step{Label: m.Label, From: *m.From, To: *m.To})
	}
	sort.SliceStable(walls, func(i, j int) bool { return walls[i].at < walls[j].at })
	for i, w := range walls {
		end := recordingS
		if i+1 < len(walls) {
			end = walls[i+1].at
		}
		steps = append(steps, Step{Label: w.label, From: w.at, To: end})
	}
	return clipSteps(steps, recordingS)
}

func clipSteps(steps []Step, recordingS float64) ([]Step, int) {
	var kept []Step
	dropped := 0
	for _, s := range steps {
		if s.To <= 0 || s.From >= recordingS {
			dropped++
			continue
		}
		kept = append(kept, s)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].From < kept[j].From })
	return kept, dropped
}

// Tap is one W3C action the runner sent, read from its log.
type Tap struct {
	At time.Time
	// Recording numbers the xctrace recording the tap sits in (1 for the
	// first "Starting recording" ... "Recording completed" block); 0 is a
	// tap outside every recording, or a log without recording markers.
	Recording int
}

var tapRe = regexp.MustCompile(`(\d{4}-\d\d-\d\dT[\d:.]+Z) INFO webdriver: COMMAND performActions`)

// ReadWdioTaps lists every `COMMAND performActions` of a WebdriverIO log
// with its timestamp and the recording block it belongs to.
func ReadWdioTaps(r io.Reader) ([]Tap, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var taps []Tap
	block, current := 0, 0
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.Contains(line, "Starting recording"):
			block++
			current = block
			continue
		case strings.Contains(line, "Recording completed"):
			current = 0
			continue
		}
		m := tapRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, m[1])
		if err != nil {
			continue
		}
		taps = append(taps, Tap{At: at, Recording: current})
	}
	return taps, sc.Err()
}

// CycleLabel names the i-th tap of a replayed round: "r<round> <name>".
func CycleLabel(cycle []string, i int) string {
	return fmt.Sprintf("r%d %s", i/len(cycle)+1, cycle[i%len(cycle)])
}

// StepsFromTaps labels the taps inside the recording by cycling through
// the step names (one name per tap); each step runs to the next tap, the
// last to the recording's end. When the log carries recording markers the
// block with the most taps inside this trace is the trace's own, and only
// its taps that miss the trace are reported as dropped; a log without
// markers is windowed by the trace alone.
func StepsFromTaps(taps []Tap, cycle []string, start time.Time, tapLag time.Duration, recordingS float64) ([]Step, int) {
	if len(cycle) == 0 {
		return nil, 0
	}
	offset := func(t Tap) float64 { return t.At.Sub(start).Seconds() + tapLag.Seconds() }
	inside := func(s float64) bool { return s >= 0 && s <= recordingS }
	hits := map[int]int{}
	for _, t := range taps {
		if t.Recording > 0 && inside(offset(t)) {
			hits[t.Recording]++
		}
	}
	block := 0
	for b, n := range hits {
		if n > hits[block] || (n == hits[block] && b < block) {
			block = b
		}
	}
	var at []float64
	dropped := 0
	for _, t := range taps {
		if block > 0 && t.Recording != block {
			continue
		}
		s := offset(t)
		if !inside(s) {
			if block > 0 {
				dropped++
			}
			continue
		}
		at = append(at, s)
	}
	steps := make([]Step, len(at))
	for i, s := range at {
		end := recordingS
		if i+1 < len(at) {
			end = at[i+1]
		}
		steps[i] = Step{Label: CycleLabel(cycle, i), From: s, To: end}
	}
	return steps, dropped
}

// stepAt is the step whose span holds t (seconds), or nil.
func stepAt(steps []Step, t float64) *Step {
	var found *Step
	for i := range steps {
		if steps[i].From <= t && t < steps[i].To {
			found = &steps[i]
		}
	}
	return found
}

// LabelHitches places each hitch on the step holding its start.
func LabelHitches(hitches []Hitch, steps []Step) {
	for i := range hitches {
		t := hitches[i].StartMs / 1000
		if s := stepAt(steps, t); s != nil {
			off := round2((t - s.From) * 1000)
			hitches[i].Step, hitches[i].StepOffsetMs = s.Label, &off
		}
	}
}

// RenderPass is one level-0 row of hitches-renders.
type RenderPass struct {
	StartMs    float64
	DurationMs float64
	Offscreen  int64
}

// ReadRenders reads hitches-renders, keeping containment level 0 (deeper
// levels repeat the same pass).
func ReadRenders(r io.Reader) ([]RenderPass, error) {
	var out []RenderPass
	_, err := ReadTable(r, func(row Row) error {
		if lvl, ok := row.Cell("containment-level").Int(); ok && lvl != 0 {
			return nil
		}
		start, ok1 := row.Cell("start").Int()
		dur, ok2 := row.Cell("duration").Int()
		off, ok3 := row.Cell("offscreen-passes").Int()
		if ok1 && ok2 && ok3 {
			out = append(out, RenderPass{StartMs: float64(start) / 1e6, DurationMs: float64(dur) / 1e6, Offscreen: off})
		}
		return nil
	})
	if err != nil {
		return nil, diag(DiagTraceUnreadable, fmt.Sprintf("%s: %v", TableRenders, err), reRecordFix)
	}
	return out, nil
}

// LongUpdateMs is where an app update (commit) counts as long.
const LongUpdateMs = 6.0

// Update is one level-0 row of hitches-updates.
type Update struct {
	StartMs    float64
	DurationMs float64
	Process    string
}

// ReadUpdates reads hitches-updates at containment level 0.
func ReadUpdates(r io.Reader) ([]Update, error) {
	var out []Update
	_, err := ReadTable(r, func(row Row) error {
		if lvl, ok := row.Cell("containment-level").Int(); ok && lvl != 0 {
			return nil
		}
		start, ok1 := row.Cell("start").Int()
		dur, ok2 := row.Cell("duration").Int()
		if ok1 && ok2 {
			out = append(out, Update{StartMs: float64(start) / 1e6, DurationMs: float64(dur) / 1e6, Process: row.Cell("process").Label()})
		}
		return nil
	})
	if err != nil {
		return nil, diag(DiagTraceUnreadable, fmt.Sprintf("%s: %v", TableUpdates, err), reRecordFix)
	}
	return out, nil
}

// StepSummary is one step kind over every round.
type StepSummary struct {
	Label         string   `json:"label"`
	Occurrences   int      `json:"occurrences"`
	Hitches       int      `json:"hitches"`
	HitchMs       float64  `json:"hitchMs"`
	WorstMs       float64  `json:"worstMs"`
	CommitHitches int      `json:"commitHitches"`
	RenderHitches int      `json:"renderHitches"`
	OffscreenP50  *float64 `json:"offscreenP50"`
	OffscreenMax  *int64   `json:"offscreenMax"`
	// LongUpdates counts the app's level-0 updates of at least LongUpdateMs.
	LongUpdates     int     `json:"longUpdates"`
	LongUpdateMaxMs float64 `json:"longUpdateMaxMs"`
}

// SummarizeSteps aggregates hitches, render passes and the app's long
// updates per step kind, in first-seen order. renders and updates may be
// nil (the trace lacks the table).
func SummarizeSteps(steps []Step, hitches []Hitch, renders []RenderPass, updates []Update, app string) []StepSummary {
	byKind := map[string]*StepSummary{}
	passes := map[string][]float64{}
	var order []string
	get := func(kind string) *StepSummary {
		s, ok := byKind[kind]
		if !ok {
			s = &StepSummary{Label: kind}
			byKind[kind] = s
			order = append(order, kind)
		}
		return s
	}
	for _, st := range steps {
		get(st.Kind()).Occurrences++
	}
	for _, h := range hitches {
		st := stepAt(steps, h.StartMs/1000)
		if st == nil {
			continue
		}
		s := get(st.Kind())
		s.Hitches++
		s.HitchMs += h.DurationMs
		s.WorstMs = math.Max(s.WorstMs, h.DurationMs)
		switch h.Kind {
		case HitchCommit:
			s.CommitHitches++
		case HitchRender:
			s.RenderHitches++
		}
	}
	for _, rp := range renders {
		if st := stepAt(steps, rp.StartMs/1000); st != nil {
			k := st.Kind()
			passes[k] = append(passes[k], float64(rp.Offscreen))
		}
	}
	for _, u := range updates {
		if u.DurationMs < LongUpdateMs || (app != "" && !strings.HasPrefix(u.Process, app+" ")) {
			continue
		}
		if st := stepAt(steps, u.StartMs/1000); st != nil {
			s := get(st.Kind())
			s.LongUpdates++
			s.LongUpdateMaxMs = math.Max(s.LongUpdateMaxMs, round2(u.DurationMs))
		}
	}
	out := make([]StepSummary, 0, len(order))
	for _, k := range order {
		s := *byKind[k]
		s.HitchMs, s.WorstMs = round2(s.HitchMs), round2(s.WorstMs)
		if p := passes[k]; len(p) > 0 {
			p50 := round2(quantile(p, 0.5))
			mx := int64(quantile(p, 1))
			s.OffscreenP50, s.OffscreenMax = &p50, &mx
		}
		out = append(out, s)
	}
	return out
}
