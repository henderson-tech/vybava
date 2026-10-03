package analysis

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
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
	// AndroidRestTicksMax: a screen showing a live value (a 1 Hz countdown)
	// may present one short burst per change. With it set, the rest bursts
	// that read as ticks (framestats.TickBurstMaxMs) are counted against it
	// and androidRestFramesMax judges only the rest frames outside them, so
	// a loop or a leak beside the value still fails.
	AndroidRestTicksMax *float64 `json:"androidRestTicksMax,omitempty"`
	// AndroidDragRtDrawMsMax: RenderThread draw per frame (average).
	AndroidDragRtDrawMsMax *float64 `json:"androidDragRtDrawMsMax,omitempty"`
	// AndroidFlingTwoVsyncGapsMax: two-vsync present gaps per fling script.
	AndroidFlingTwoVsyncGapsMax *float64 `json:"androidFlingTwoVsyncGapsMax,omitempty"`
	// SlopeMax: last round's cost over the first's.
	SlopeMax *float64 `json:"slopeMax,omitempty"`
	// IOSRestMainMsPerSMax: an iOS rest probe's main-thread running ms per
	// recorded second (Time Profiler): a screen at rest leaves the main
	// thread idle, so a smooth loop fails here though it never hitches.
	IOSRestMainMsPerSMax *float64 `json:"iosRestMainMsPerSMax,omitempty"`
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

// budgetKeys is the closed budget vocabulary: Budget's JSON keys.
var budgetKeys = func() map[string]bool {
	keys := map[string]bool{}
	ty := reflect.TypeOf(Budget{})
	for i := 0; i < ty.NumField(); i++ {
		keys[strings.Split(ty.Field(i).Tag.Get("json"), ",")[0]] = true
	}
	return keys
}()

// Exemption waives budget keys of one scenario for a named, deliberate
// cost the product keeps (a loop on purpose, such as a live-search radar):
// the checks are still measured and shown, marked exempt with the name and
// reason, and never fail the row or the gate. An adapter scenario row
// carries it under "exempt".
type Exemption struct {
	Name   string   `json:"name"`
	Reason string   `json:"reason"`
	Keys   []string `json:"keys"`
}

// ValidateExemptions refuses an exemption without a name, a reason or keys,
// or naming a key outside the budget vocabulary (CONFIG_INVALID).
func ValidateExemptions(scenario string, exemptions []Exemption) error {
	fix := "perflab adapter check --json (an exemption is {name, reason, keys: [budget keys]})"
	for i, e := range exemptions {
		if strings.TrimSpace(e.Name) == "" || strings.TrimSpace(e.Reason) == "" || len(e.Keys) == 0 {
			return diag(DiagConfigInvalid, fmt.Sprintf("scenario %s exemption %d needs a name, a reason and keys", scenario, i), fix)
		}
		for _, k := range e.Keys {
			if !budgetKeys[k] {
				return diag(DiagConfigInvalid, fmt.Sprintf("scenario %s exemption %s names %q, no budget key", scenario, e.Name, k), fix)
			}
		}
	}
	return nil
}

// Check is one budget key applied to one measured value. Exempt names the
// exemption ("<name>: <reason>") that took the check out of the verdict.
type Check struct {
	Key    string  `json:"key"`
	Value  float64 `json:"value"`
	Limit  float64 `json:"limit"`
	Pass   bool    `json:"pass"`
	Gating bool    `json:"gating"`
	Note   string  `json:"note,omitempty"`
	Exempt string  `json:"exempt,omitempty"`
}

// RowVerdict is a report row's outcome.
type RowVerdict string

const (
	RowPass RowVerdict = "pass"
	RowFail RowVerdict = "fail"
	// RowExempt: only exempt checks are over budget (Exemption).
	RowExempt     RowVerdict = "exempt"
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
// compose_board: one row per screen and device. A probed screen's columns
// come each from its own probe kind (rest frames from the rest probe, the
// drag columns from the drag probe, the fling ones from the fling probe);
// another scenario fills the columns its budget names. The verdict is the
// worst of the screen's rows.
type BoardRow struct {
	Screen     string `json:"screen"`
	RestFrames *int   `json:"restFrames"`
	// RestTicks: the rest bursts read as a live value's ticks, set when the
	// rest budget allows them (androidRestTicksMax).
	RestTicks   *int     `json:"restTicks,omitempty"`
	DragDrawMs  *float64 `json:"dragDrawMs"`
	DragFpsP10  *float64 `json:"dragFpsP10"`
	FlingGaps   *int     `json:"flingGaps"`
	FlingFpsP10 *float64 `json:"flingFpsP10"`
	Verdict     string   `json:"verdict"`
}

// ReportData is `perflab report`'s data.
type ReportData struct {
	Rows      []ReportRow `json:"rows"`
	BoardRows []BoardRow  `json:"boardRows"`
}

// ReportOptions tunes Report.
type ReportOptions struct {
	// Budgets maps a scenario to its budget. A probe scenario's row
	// overlays the probe default (ProbeBudget): the keys it sets win.
	Budgets map[string]Budget
	// Exemptions maps a scenario to its named exemptions.
	Exemptions map[string][]Exemption
	// Gate fails (exit 2) on any failing row (OVER_BUDGET) or when nothing
	// was measured (NOTHING_MEASURED).
	Gate bool
}

// ProbeBudget is a probe's default verdict per kind (docs/perflab.md
// "Budgets"), nil for a kind with none: a screen at rest stops drawing, a
// drag stays inside the RenderThread budget, and a drag or fling keeps its
// animating bins at 0.75 x refresh. A fling's two-vsync count has no default
// (the baseline's count per script).
func ProbeBudget(kind string, platform Platform) json.RawMessage {
	if platform == PlatformIOS {
		if kind == "rest" {
			// 50 ms/s is 5% of the main thread: FixIt's iPhone 11 rested at
			// 21 ms/s and read 164 with a looping comet.
			return json.RawMessage(`{"iosHitchRatioMsPerSMax":5,"iosRestMainMsPerSMax":50}`)
		}
		return json.RawMessage(`{"iosHitchRatioMsPerSMax":5}`)
	}
	switch kind {
	case "rest":
		return json.RawMessage(`{"androidRestFramesMax":0,"androidRestRunMsMax":6000}`)
	case "drag":
		return json.RawMessage(`{"androidDragRtDrawMsMax":4,"androidAnimatingFpsP10MinShare":0.75}`)
	case "fling":
		return json.RawMessage(`{"androidAnimatingFpsP10MinShare":0.75}`)
	}
	return nil
}

// probeScenarioRe names a probe's records: probe-<kind>[-<screen label>].
var probeScenarioRe = regexp.MustCompile(`^probe-(rest|drag|fling|custom)(?:-(.+))?$`)

// probeScreen splits a probe scenario into its kind and screen; ok is false
// for any other scenario.
func probeScreen(scenario string) (kind, screen string, ok bool) {
	m := probeScenarioRe.FindStringSubmatch(scenario)
	if m == nil {
		return "", "", false
	}
	screen = m[2]
	if screen == "" {
		screen = "probe"
	}
	return m[1], screen, true
}

// budgetFor is the adapter's scenario budget; a probe scenario's budget is
// its kind's default (ProbeBudget) with the adapter row's keys over it, so
// a row adding androidRestTicksMax keeps the rest frame limit.
func budgetFor(scenario string, platform Platform, budgets map[string]Budget) Budget {
	row, hasRow := budgets[scenario]
	kind, _, probe := probeScreen(scenario)
	if !probe {
		return row
	}
	var b Budget
	if raw := ProbeBudget(kind, platform); raw != nil {
		b, _ = ParseBudget(scenario, raw)
	}
	if hasRow {
		overlayBudget(&b, row)
	}
	return b
}

// overlayBudget sets every key over sets onto b.
func overlayBudget(b *Budget, over Budget) {
	dst, src := reflect.ValueOf(b).Elem(), reflect.ValueOf(over)
	for i := 0; i < src.NumField(); i++ {
		if f := src.Field(i); !f.IsNil() {
			dst.Field(i).Set(f)
		}
	}
}

// Report takes the newest record per scenario x device across the run
// analyses and checks it against its scenario budget (a probe without an
// adapter row: ProbeBudget).
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
				row.Checks = exempt(checkBudget(budgetFor(rec.Scenario, ra.Provenance.Platform, opts.Budgets), *rec.Metrics, refresh),
					opts.Exemptions[rec.Scenario])
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
	data.BoardRows = boardRows(data.Rows)
	measured := 0
	var failing []string
	for _, r := range data.Rows {
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
	if m.IOSRestMainMsPerS != nil {
		atMost("iosRestMainMsPerSMax", b.IOSRestMainMsPerSMax, *m.IOSRestMainMsPerS, true)
	}
	if d := m.Display; d != nil && d.Frames > 0 {
		// The animating share and the janky share read the animating bins
		// only: without one there is no reading to check (Metrics.Scalars).
		if b.AndroidAnimatingFpsP10MinShare != nil && d.RefreshHz > 0 && d.AnimatingBins > 0 {
			limit := *b.AndroidAnimatingFpsP10MinShare
			share := d.AnimatingFpsP10 / d.RefreshHz
			add("androidAnimatingFpsP10MinShare", share, limit, share >= limit, true,
				fmt.Sprintf("%.1f of %.0f Hz", d.AnimatingFpsP10, d.RefreshHz))
		}
		atMost("androidJankyPctMax", b.AndroidJankyPctMax, d.JankyPct, d.AnimatingBins > 0)
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
		// Without FrameTimeline the present readings are unread, not zero: no
		// check, so a trace missing the evidence never passes these budgets.
		if p.FrameTimeline {
			if b.AndroidRestTicksMax != nil {
				// A live value's ticks are allowed; the frames outside them are not.
				if b.AndroidRestFramesMax != nil {
					outside := float64(p.RestFrames - p.RestTickFrames)
					add("androidRestFramesMax", outside, *b.AndroidRestFramesMax, outside <= *b.AndroidRestFramesMax, true,
						fmt.Sprintf("outside ticks; %d of %d rest frames in %d ticks", p.RestTickFrames, p.RestFrames, p.RestTicks))
				}
				add("androidRestTicksMax", float64(p.RestTicks), *b.AndroidRestTicksMax, float64(p.RestTicks) <= *b.AndroidRestTicksMax, true,
					fmt.Sprintf("one every %.0f ms", p.RestTickIntervalMs))
			} else {
				atMost("androidRestFramesMax", b.AndroidRestFramesMax, float64(p.RestFrames), true)
			}
			atMost("androidRestRunMsMax", b.AndroidRestRunMsMax, p.RestRunMs, true)
			atMost("androidFlingTwoVsyncGapsMax", b.AndroidFlingTwoVsyncGapsMax, float64(p.PresentGaps.TwoVsync), true)
		}
		if p.RTDrawMs.Avg != nil {
			atMost("androidDragRtDrawMsMax", b.AndroidDragRtDrawMsMax, *p.RTDrawMs.Avg, true)
		}
	}
	if m.Slope != nil {
		atMost("slopeMax", b.SlopeMax, *m.Slope, true)
	}
	return out
}

// exempt takes the checks an exemption names out of the verdict: still
// measured and shown, never gating.
func exempt(checks []Check, exemptions []Exemption) []Check {
	for i := range checks {
		for _, e := range exemptions {
			for _, k := range e.Keys {
				if checks[i].Key == k && checks[i].Gating {
					checks[i].Gating = false
					checks[i].Exempt = e.Name + ": " + e.Reason
				}
			}
		}
	}
	return checks
}

func verdictOf(checks []Check) RowVerdict {
	gating, exempted, exemptOver := 0, false, false
	for _, c := range checks {
		if c.Exempt != "" {
			exempted = true
			exemptOver = exemptOver || !c.Pass
		}
		if !c.Gating {
			continue
		}
		gating++
		if !c.Pass {
			return RowFail
		}
	}
	switch {
	case exemptOver:
		return RowExempt
	case gating == 0 && !exempted:
		return RowUnbudgeted
	}
	return RowPass
}

// verdictRank orders verdicts worst first for a screen's merged row.
var verdictRank = map[RowVerdict]int{RowFail: 0, RowExempt: 1, RowPass: 2, RowUnbudgeted: 3, RowFailed: 4}

// boardRows merges the report rows into one board row per screen x device
// (BoardRow), in the rows' order.
func boardRows(rows []ReportRow) []BoardRow {
	type key struct{ screen, device string }
	out := []BoardRow{}
	index := map[key]int{}
	for _, r := range rows {
		kind, screen, probe := probeScreen(r.Scenario)
		if !probe {
			screen = r.Scenario
		}
		k := key{screen, r.Device}
		i, seen := index[k]
		if !seen {
			i = len(out)
			index[k] = i
			out = append(out, BoardRow{Screen: screen + " on " + r.Device, Verdict: string(r.Verdict)})
		} else if verdictRank[r.Verdict] < verdictRank[RowVerdict(out[i].Verdict)] {
			out[i].Verdict = string(r.Verdict)
		}
		fillBoardRow(&out[i], r, kind)
	}
	return out
}

// fillBoardRow writes the columns a row speaks to: a probe's by its kind,
// any other scenario's by the budget keys it was checked against.
func fillBoardRow(br *BoardRow, r ReportRow, kind string) {
	m := r.metrics
	if m == nil {
		return
	}
	if kind == "" {
		for _, c := range r.Checks {
			switch c.Key {
			case "androidRestFramesMax":
				kind = "rest"
			case "androidDragRtDrawMsMax":
				kind = "drag"
			case "androidFlingTwoVsyncGapsMax":
				kind = "fling"
			}
		}
	}
	p, d := m.Present, m.Display
	var fps *float64
	if d != nil && d.AnimatingBins > 0 {
		v := d.AnimatingFpsP10
		fps = &v
	}
	switch kind {
	case "rest":
		if p != nil && p.FrameTimeline {
			v := p.RestFrames
			br.RestFrames = &v
			for _, c := range r.Checks {
				if c.Key == "androidRestTicksMax" {
					ticks := p.RestTicks
					br.RestTicks = &ticks
				}
			}
		}
	case "drag":
		if p != nil && p.RTDrawMs.Avg != nil {
			v := *p.RTDrawMs.Avg
			br.DragDrawMs = &v
		}
		br.DragFpsP10 = fps
	case "fling":
		if p != nil && p.FrameTimeline {
			v := p.PresentGaps.TwoVsync
			br.FlingGaps = &v
		}
		br.FlingFpsP10 = fps
	}
}

// Markdown renders the report as one table, then every exemption in force,
// so an exempt row is never a silent pass.
func (d ReportData) Markdown() string {
	var b strings.Builder
	var exemptions []string
	b.WriteString("| Scenario | Device | Variant | Recorded | Verdict | Checks |\n|---|---|---|---|---|---|\n")
	for _, r := range d.Rows {
		var checks []string
		for _, c := range r.Checks {
			mark := "ok"
			switch {
			case c.Exempt != "" && !c.Pass:
				mark = "EXEMPT"
			case c.Exempt != "":
				mark = "ok (exempt)"
			case !c.Gating:
				mark = "info"
			case !c.Pass:
				mark = "OVER"
			}
			if c.Exempt != "" {
				exemptions = append(exemptions, fmt.Sprintf("- %s on %s: %s %g / %g, %s", r.Scenario, r.Device, c.Key, c.Value, c.Limit, c.Exempt))
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
	if len(exemptions) > 0 {
		b.WriteString("\nExemptions (measured, never gating):\n\n" + strings.Join(exemptions, "\n") + "\n")
	}
	return b.String()
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
