package analysis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/runx"
)

// Budget is one scenario's limits in the closed budget vocabulary (design
// section 8.3); an adapter scenario row carries it under these keys.
type Budget struct {
	// IOSHitchRatioMsPerSMax: hitch ms per recorded second (Apple: 5 good,
	// 5-10 warning, above 10 critical).
	IOSHitchRatioMsPerSMax *float64 `json:"iosHitchRatioMsPerSMax,omitempty"`
	// IOSHitchRatio120MsPerSMax replaces it on a 120 Hz device.
	IOSHitchRatio120MsPerSMax *float64 `json:"iosHitchRatio120MsPerSMax,omitempty"`
	// IOSWorstHitchMsMax: the worst hitch of the app (system-attributed
	// hitches excluded).
	IOSWorstHitchMsMax *float64 `json:"iosWorstHitchMsMax,omitempty"`
	// AndroidAnimatingFpsP10MinShare: animating worst decile over the
	// refresh rate (0.75 = 90 FPS at 120 Hz).
	AndroidAnimatingFpsP10MinShare *float64 `json:"androidAnimatingFpsP10MinShare,omitempty"`
	// AndroidJankyPctMax: animating intervals over 1.5 periods, percent.
	AndroidJankyPctMax *float64 `json:"androidJankyPctMax,omitempty"`
	// AndroidRestFramesMax / AndroidRestRunMsMax: frames after the opening
	// run at rest, and the longest ambient run.
	AndroidRestFramesMax *float64 `json:"androidRestFramesMax,omitempty"`
	AndroidRestRunMsMax  *float64 `json:"androidRestRunMsMax,omitempty"`
	// AndroidDragRtDrawMsMax: RenderThread draw per frame (average).
	AndroidDragRtDrawMsMax *float64 `json:"androidDragRtDrawMsMax,omitempty"`
	// AndroidFlingTwoVsyncGapsMax: two-vsync present gaps per fling script.
	AndroidFlingTwoVsyncGapsMax *float64 `json:"androidFlingTwoVsyncGapsMax,omitempty"`
	// SlopeMax: last round's cost over the first's.
	SlopeMax *float64 `json:"slopeMax,omitempty"`
	// AndroidFpsP10Min is the 60-capped worst decile, kept for old
	// scenario rows: reported, and never gating a display above 60 Hz.
	AndroidFpsP10Min *float64 `json:"androidFpsP10Min,omitempty"`
}

// ParseBudget decodes one scenario budget, refusing keys outside the
// vocabulary (CONFIG_INVALID names them).
func ParseBudget(scenario string, raw json.RawMessage) (Budget, error) {
	var b Budget
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return b, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&b); err != nil {
		return b, diag(DiagConfigInvalid, fmt.Sprintf("scenario %s budget: %v", scenario, err),
			"perflab adapter check --json (budget keys: docs/perflab.md, the closed budget vocabulary)")
	}
	return b, nil
}

// Check is one budget key applied to one measured value.
type Check struct {
	Key    string  `json:"key"`
	Value  float64 `json:"value"`
	Limit  float64 `json:"limit"`
	Pass   bool    `json:"pass"`
	Gating bool    `json:"gating"`
	Note   string  `json:"note,omitempty"`
}

// RowVerdict is a report row's outcome.
type RowVerdict string

const (
	RowPass       RowVerdict = "pass"
	RowFail       RowVerdict = "fail"
	RowUnbudgeted RowVerdict = "unbudgeted"
	RowFailed     RowVerdict = "not-measured"
)

// ReportRow is the newest record of one scenario x device.
type ReportRow struct {
	Scenario   string     `json:"scenario"`
	Device     string     `json:"device"`
	Variant    string     `json:"variant"`
	RunDir     string     `json:"runDir"`
	RecordedAt string     `json:"recordedAt"`
	RefreshHz  float64    `json:"refreshHz,omitempty"`
	Checks     []Check    `json:"checks"`
	Verdict    RowVerdict `json:"verdict"`
	Error      string     `json:"error,omitempty"`
	metrics    *Metrics
}

// BoardRow is the vitrinka board table row (4740's shape) for
// compose_board.
type BoardRow struct {
	Screen     string   `json:"screen"`
	RestFrames *int     `json:"restFrames"`
	DragDrawMs *float64 `json:"dragDrawMs"`
	FlingGaps  *int     `json:"flingGaps"`
	Verdict    string   `json:"verdict"`
}

// ReportData is `perflab report`'s data.
type ReportData struct {
	Rows      []ReportRow `json:"rows"`
	BoardRows []BoardRow  `json:"boardRows"`
}

// ReportOptions tunes Report.
type ReportOptions struct {
	// Budgets maps a scenario to its budget.
	Budgets map[string]Budget
	// Gate fails (exit 2) on any failing row (OVER_BUDGET) or when nothing
	// was measured (NOTHING_MEASURED).
	Gate bool
}

// Report takes the newest record per scenario x device across the run
// analyses and checks it against its scenario budget.
func Report(runs []RunAnalysis, opts ReportOptions) (ReportData, []runx.Diagnostic, error) {
	type key struct{ scenario, device string }
	newest := map[key]ReportRow{}
	newestAt := map[key]int64{}
	for _, ra := range runs {
		for _, rec := range ra.Records {
			k := key{rec.Scenario, ra.Provenance.Device}
			at := rec.RecordedAt.UnixNano()
			if prev, ok := newestAt[k]; ok && prev >= at {
				continue
			}
			newestAt[k] = at
			row := ReportRow{Scenario: rec.Scenario, Device: ra.Provenance.Device, Variant: rec.Variant, RunDir: ra.RunDir,
				RecordedAt: rec.RecordedAt.UTC().Format("2006-01-02T15:04:05Z"), RefreshHz: ra.Provenance.RefreshHz, Checks: []Check{}, metrics: rec.Metrics}
			switch {
			case rec.Metrics == nil:
				row.Verdict = RowFailed
				if rec.Error != nil {
					row.Error = rec.Error.Code + ": " + rec.Error.Detail
				}
			default:
				refresh := row.RefreshHz
				if rec.Metrics.Display != nil && rec.Metrics.Display.RefreshHz > 0 {
					refresh = rec.Metrics.Display.RefreshHz
				} else if rec.Metrics.Present != nil && rec.Metrics.Present.RefreshHz > 0 {
					refresh = rec.Metrics.Present.RefreshHz
				}
				row.RefreshHz = refresh
				row.Checks = checkBudget(opts.Budgets[rec.Scenario], *rec.Metrics, refresh)
				row.Verdict = verdictOf(row.Checks)
			}
			newest[k] = row
		}
	}
	data := ReportData{Rows: []ReportRow{}, BoardRows: []BoardRow{}}
	for _, r := range newest {
		data.Rows = append(data.Rows, r)
	}
	sort.Slice(data.Rows, func(i, j int) bool {
		if data.Rows[i].Scenario != data.Rows[j].Scenario {
			return data.Rows[i].Scenario < data.Rows[j].Scenario
		}
		return data.Rows[i].Device < data.Rows[j].Device
	})
	measured := 0
	var failing []string
	for _, r := range data.Rows {
		data.BoardRows = append(data.BoardRows, boardRow(r))
		if r.Verdict != RowFailed {
			measured++
		}
		if r.Verdict == RowFail {
			var over []string
			for _, c := range r.Checks {
				if c.Gating && !c.Pass {
					over = append(over, fmt.Sprintf("%s %g > %g", c.Key, c.Value, c.Limit))
				}
			}
			failing = append(failing, fmt.Sprintf("%s on %s (%s)", r.Scenario, r.Device, strings.Join(over, ", ")))
		}
	}
	if !opts.Gate {
		return data, nil, nil
	}
	if measured == 0 {
		return data, nil, diag(DiagNothingMeasured, fmt.Sprintf("%d run dir(s) hold no measured record", len(runs)),
			"perflab run <scenario> --device <id> --lease <token>")
	}
	if len(failing) > 0 {
		return data, nil, diag(DiagOverBudget, strings.Join(failing, "; "), "perflab compare <before runDir> <after runDir> --json to see what moved")
	}
	return data, nil, nil
}

// checkBudget applies every set key the metrics can speak to.
func checkBudget(b Budget, m Metrics, refreshHz float64) []Check {
	var out []Check
	add := func(key string, value, limit float64, pass, gating bool, note string) {
		out = append(out, Check{Key: key, Value: round2(value), Limit: limit, Pass: pass, Gating: gating, Note: note})
	}
	atMost := func(key string, limit *float64, value float64, ok bool) {
		if limit != nil && ok {
			add(key, value, *limit, value <= *limit, true, "")
		}
	}
	if ios := m.IOS; ios != nil {
		switch {
		case refreshHz >= 100 && b.IOSHitchRatio120MsPerSMax != nil:
			atMost("iosHitchRatio120MsPerSMax", b.IOSHitchRatio120MsPerSMax, ios.HitchRatioMsPerS, true)
		default:
			atMost("iosHitchRatioMsPerSMax", b.IOSHitchRatioMsPerSMax, ios.HitchRatioMsPerS, true)
		}
		atMost("iosWorstHitchMsMax", b.IOSWorstHitchMsMax, ios.WorstAppHitchMs, true)
	}
	if d := m.Display; d != nil && d.Frames > 0 {
		if b.AndroidAnimatingFpsP10MinShare != nil && d.RefreshHz > 0 {
			limit := *b.AndroidAnimatingFpsP10MinShare
			share := d.AnimatingFpsP10 / d.RefreshHz
			add("androidAnimatingFpsP10MinShare", share, limit, share >= limit, true,
				fmt.Sprintf("%.1f of %.0f Hz", d.AnimatingFpsP10, d.RefreshHz))
		}
		atMost("androidJankyPctMax", b.AndroidJankyPctMax, d.JankyPct, true)
		if b.AndroidFpsP10Min != nil {
			gating := d.RefreshHz <= 60.5
			note := ""
			if !gating {
				note = "60-capped; reported only on a display above 60 Hz"
			}
			add("androidFpsP10Min", d.FpsP10, *b.AndroidFpsP10Min, d.FpsP10 >= *b.AndroidFpsP10Min, gating, note)
		}
	}
	if p := m.Present; p != nil {
		atMost("androidRestFramesMax", b.AndroidRestFramesMax, float64(p.RestFrames), true)
		atMost("androidRestRunMsMax", b.AndroidRestRunMsMax, p.RestRunMs, true)
		if p.RTDrawMs.Avg != nil {
			atMost("androidDragRtDrawMsMax", b.AndroidDragRtDrawMsMax, *p.RTDrawMs.Avg, true)
		}
		atMost("androidFlingTwoVsyncGapsMax", b.AndroidFlingTwoVsyncGapsMax, float64(p.PresentGaps.TwoVsync), true)
	}
	if m.Slope != nil {
		atMost("slopeMax", b.SlopeMax, *m.Slope, true)
	}
	return out
}

func verdictOf(checks []Check) RowVerdict {
	gating := 0
	for _, c := range checks {
		if !c.Gating {
			continue
		}
		gating++
		if !c.Pass {
			return RowFail
		}
	}
	if gating == 0 {
		return RowUnbudgeted
	}
	return RowPass
}

func boardRow(r ReportRow) BoardRow {
	br := BoardRow{Screen: r.Scenario + " on " + r.Device, Verdict: string(r.Verdict)}
	if m := r.metrics; m != nil && m.Present != nil {
		rest, gaps := m.Present.RestFrames, m.Present.PresentGaps.TwoVsync
		br.RestFrames, br.FlingGaps = &rest, &gaps
		if m.Present.RTDrawMs.Avg != nil {
			v := *m.Present.RTDrawMs.Avg
			br.DragDrawMs = &v
		}
	}
	return br
}

// Markdown renders the report as one table plus the failing checks.
func (d ReportData) Markdown() string {
	var b strings.Builder
	b.WriteString("| Scenario | Device | Variant | Recorded | Verdict | Checks |\n|---|---|---|---|---|---|\n")
	for _, r := range d.Rows {
		var checks []string
		for _, c := range r.Checks {
			mark := "ok"
			switch {
			case !c.Gating:
				mark = "info"
			case !c.Pass:
				mark = "OVER"
			}
			checks = append(checks, fmt.Sprintf("%s %g / %g %s", c.Key, c.Value, c.Limit, mark))
		}
		if r.Error != "" {
			checks = append(checks, r.Error)
		}
		cell := strings.Join(checks, "; ")
		if cell == "" {
			cell = "-"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s |\n", r.Scenario, r.Device, r.Variant, r.RecordedAt, r.Verdict, strings.ReplaceAll(cell, "|", "/"))
	}
	return b.String()
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
