package xctrace

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// The hitch tables, by device generation. Their stage siblings
// (hitches-frame-lifetimes, -framewait, -updates, -renders, -gpu and the
// *-interval tables) break EVERY frame into stages: summed, an iPhone 11 read
// 13.92 ms/s for 2.77. They are never summed; only one of these is read.
const (
	// TableHitches lists the hitches themselves on iOS 26 devices.
	TableHitches = "hitches"
	// TableHitchesSummary is the same list as an iOS 18 device records it
	// under Xcode 26.
	TableHitchesSummary = "hitches-summary"
	// TableRenders holds one row per render pass with its offscreen passes.
	TableRenders = "hitches-renders"
	// TableUpdates holds one row per app/system update (commit) span.
	TableUpdates = "hitches-updates"
	// TableTimeProfile is the Time Profiler's sample table.
	TableTimeProfile = "time-profile"
)

// PickHitchTable chooses the one table the hitch metrics read, or fails
// closed with HITCH_TABLE_MISSING naming the schemas the run has.
func PickHitchTable(run Run) (string, error) {
	switch {
	case run.Has(TableHitches):
		return TableHitches, nil
	case run.Has(TableHitchesSummary):
		return TableHitchesSummary, nil
	}
	var hitchish []string
	for _, s := range run.Schemas() {
		if strings.Contains(s, "hitch") {
			hitchish = append(hitchish, s)
		}
	}
	found := "none"
	if len(hitchish) > 0 {
		found = strings.Join(hitchish, ", ")
	}
	return "", diag(DiagHitchTableMissing,
		fmt.Sprintf("the trace lists neither %q nor %q (hitch-like tables found: %s); their stage siblings are never summed in their place", TableHitches, TableHitchesSummary, found),
		reRecordFix)
}

// HitchKind splits a hitch by where its frame ran late.
type HitchKind string

const (
	// HitchCommit: the app's commit (a "Potentially expensive app update",
	// iOS 18's "Pre-Commit(s) latency" and "Expensive Commit(s)").
	HitchCommit HitchKind = "commit"
	// HitchRender: the render server ("expensive render, N offscreen
	// passes", "GPU work", iOS 18's "Commit to Render latency").
	HitchRender HitchKind = "render"
	// HitchUnknown: Instruments named no cause.
	HitchUnknown HitchKind = "unknown"
)

func classifyHitch(narrative string) HitchKind {
	n := strings.ToLower(narrative)
	switch {
	case strings.Contains(n, "app update") || strings.Contains(n, "pre-commit") || strings.Contains(n, "expensive commit"):
		return HitchCommit
	case strings.Contains(n, "render") || strings.Contains(n, "gpu") || strings.Contains(n, "offscreen"):
		return HitchRender
	}
	return HitchUnknown
}

// Hitch is one row of the hitch table.
type Hitch struct {
	StartMs    float64   `json:"startMs"`
	DurationMs float64   `json:"durationMs"`
	Process    string    `json:"process,omitempty"`
	System     bool      `json:"system"`
	Narrative  string    `json:"narrative,omitempty"`
	Kind       HitchKind `json:"kind"`
	// Step and StepOffsetMs place the hitch on the interaction (marks).
	Step         string   `json:"step,omitempty"`
	StepOffsetMs *float64 `json:"stepOffsetMs,omitempty"`
}

// ReadHitches reads every row of a hitch table export, deduped by
// start:duration (the same hitch listed per containment level).
func ReadHitches(r io.Reader) ([]Hitch, string, error) {
	var hitches []Hitch
	seen := map[[2]int64]bool{}
	schema, err := ReadTable(r, func(row Row) error {
		start, ok1 := row.Cell("start").Int()
		dur, ok2 := row.Cell("duration").Int()
		if !ok1 || !ok2 {
			return nil
		}
		key := [2]int64{start, dur}
		if seen[key] {
			return nil
		}
		seen[key] = true
		h := Hitch{StartMs: float64(start) / 1e6, DurationMs: float64(dur) / 1e6}
		if p := row.Cell("process"); p != nil {
			h.Process = p.Label()
		}
		if s, ok := row.Cell("is-system").Int(); ok {
			h.System = s != 0
		}
		for _, col := range []string{"narrative-description", "type-label", "narrative"} {
			if c := row.Cell(col); c != nil && c.Label() != "" {
				h.Narrative = c.Label()
				break
			}
		}
		h.Kind = classifyHitch(h.Narrative)
		hitches = append(hitches, h)
		return nil
	})
	if err != nil {
		return nil, schema.Name, diag(DiagTraceUnreadable, fmt.Sprintf("hitch table: %v", err), reRecordFix)
	}
	sort.SliceStable(hitches, func(i, j int) bool { return hitches[i].StartMs < hitches[j].StartMs })
	return hitches, schema.Name, nil
}

// HitchMetrics is the iOS reading of one recording.
type HitchMetrics struct {
	Device           Device  `json:"device"`
	Table            string  `json:"table"`
	RecordingMs      float64 `json:"recordingMs"`
	HitchCount       int     `json:"hitchCount"`
	HitchTimeMs      float64 `json:"hitchTimeMs"`
	HitchRatioMsPerS float64 `json:"hitchRatioMsPerS"`
	WorstHitchMs     float64 `json:"worstHitchMs"`
	// WorstAppHitchMs leaves out hitches Instruments attributes to the
	// system or to another process (the iosWorstHitchMsMax budget).
	WorstAppHitchMs float64 `json:"worstAppHitchMs"`
	CommitHitches   int     `json:"commitHitches"`
	RenderHitches   int     `json:"renderHitches"`
	Top             []Hitch `json:"top"`
}

// TopHitches is how many of the longest hitches Summarize lists.
const TopHitches = 10

// Summarize measures the hitches of one run. app is the target process's
// name (TOC); empty keeps every non-system row as the app's.
func Summarize(run Run, table string, hitches []Hitch) HitchMetrics {
	m := HitchMetrics{Device: run.Device, Table: table, RecordingMs: round2(run.RecordingMs()), Top: []Hitch{}}
	var total float64
	for _, h := range hitches {
		m.HitchCount++
		total += h.DurationMs
		m.WorstHitchMs = math.Max(m.WorstHitchMs, h.DurationMs)
		if !h.System && (h.Process == "" || run.Process.Name == "" || strings.HasPrefix(h.Process, run.Process.Name+" ")) {
			m.WorstAppHitchMs = math.Max(m.WorstAppHitchMs, h.DurationMs)
		}
		switch h.Kind {
		case HitchCommit:
			m.CommitHitches++
		case HitchRender:
			m.RenderHitches++
		}
	}
	m.HitchTimeMs = round2(total)
	if run.RecordingMs() > 0 {
		m.HitchRatioMsPerS = round2(total / (run.RecordingMs() / 1000))
	}
	m.WorstHitchMs, m.WorstAppHitchMs = round2(m.WorstHitchMs), round2(m.WorstAppHitchMs)
	top := append([]Hitch(nil), hitches...)
	sort.SliceStable(top, func(i, j int) bool { return top[i].DurationMs > top[j].DurationMs })
	if len(top) > TopHitches {
		top = top[:TopHitches]
	}
	for _, h := range top {
		h.StartMs, h.DurationMs = round2(h.StartMs), round2(h.DurationMs)
		m.Top = append(m.Top, h)
	}
	return m
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }

// quantile is the linearly interpolated q-quantile (0..1) of values.
func quantile(values []float64, q float64) float64 {
	if len(values) == 0 {
		return math.NaN()
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	pos := float64(len(s)-1) * q
	lo, hi := int(math.Floor(pos)), int(math.Ceil(pos))
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}
