package analysis

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/framestats"
	"github.com/henderson-tech/vybava/internal/runx"
)

// Verdict is one scenario x device x metric judgement.
type Verdict string

const (
	VerdictImproved    Verdict = "improved"
	VerdictRegressed   Verdict = "regressed"
	VerdictWithinNoise Verdict = "within-noise"
	VerdictNoisy       Verdict = "noisy"
	VerdictTooFewRuns  Verdict = "too-few-runs"
)

// ConfoundField is a field that changes the numbers by itself.
type ConfoundField string

const (
	ConfoundNativeKey     ConfoundField = "nativeKey"
	ConfoundPublicEnv     ConfoundField = "publicEnvHash"
	ConfoundDevice        ConfoundField = "device"
	ConfoundInputSource   ConfoundField = "inputSource"
	ConfoundProductionEqv ConfoundField = "productionEquivalent"
)

// ConfoundFields lists the closed set --allow-confound accepts.
var ConfoundFields = []ConfoundField{ConfoundNativeKey, ConfoundPublicEnv, ConfoundDevice, ConfoundInputSource, ConfoundProductionEqv}

// ParseConfoundField validates an --allow-confound value.
func ParseConfoundField(s string) (ConfoundField, error) {
	for _, f := range ConfoundFields {
		if string(f) == s {
			return f, nil
		}
	}
	names := make([]string, len(ConfoundFields))
	for i, f := range ConfoundFields {
		names[i] = string(f)
	}
	return "", diag(DiagUsage, fmt.Sprintf("--allow-confound %q is not a confounder", s),
		"perflab compare <a> <b> --allow-confound <"+strings.Join(names, "|")+">")
}

// ThermalWarnStatus is the thermal status a record warns at (Android
// THERMAL_STATUS_MODERATE, iOS serious).
const ThermalWarnStatus = 2

// SwapWarnMb is the swap in use a record warns above.
const SwapWarnMb = 1024.0

// Side is one compare side: records of one or more run dirs, optionally of
// one variant only.
type Side struct {
	Label   string        `json:"label"`
	Runs    []RunAnalysis `json:"-"`
	Variant string        `json:"variant,omitempty"`
}

// CompareOptions tunes Compare.
type CompareOptions struct {
	Scenario      string
	Threshold     float64
	MinRuns       int
	Noise         NoiseRule
	AllowConfound []ConfoundField
}

// Confounder is one field the sides disagree on.
type Confounder struct {
	Field   ConfoundField `json:"field"`
	A       []string      `json:"a"`
	B       []string      `json:"b"`
	Allowed bool          `json:"allowed"`
}

// CompareRow is one scenario x device x metric.
type CompareRow struct {
	Scenario  string    `json:"scenario"`
	Device    string    `json:"device"`
	Metric    Metric    `json:"metric"`
	Direction Direction `json:"direction"`
	A         Stats     `json:"a"`
	B         Stats     `json:"b"`
	// Delta is (B - A) / A on the medians; nil when A's median is 0.
	Delta   *float64 `json:"delta"`
	Verdict Verdict  `json:"verdict"`
	Reasons []string `json:"reasons,omitempty"`
}

// StepCompare is one step kind's headline number on both sides (median of
// the per-run values): hitch ms on iOS, the bins' worst decile on Android.
type StepCompare struct {
	Scenario string  `json:"scenario"`
	Step     string  `json:"step"`
	Measure  string  `json:"measure"`
	A        float64 `json:"a"`
	B        float64 `json:"b"`
}

// Comparison is `perflab compare`'s data.
type Comparison struct {
	A           Side          `json:"a"`
	B           Side          `json:"b"`
	Threshold   float64       `json:"threshold"`
	Rows        []CompareRow  `json:"rows"`
	Steps       []StepCompare `json:"steps,omitempty"`
	Confounders []Confounder  `json:"confounders"`
}

type sample struct {
	scenario, device string
	metrics          Metrics
	build            Build
	before, after    *DeviceState
	provenance       Provenance
	recordedAt       int64
}

func (s Side) samples(scenario string) []sample {
	var out []sample
	for _, ra := range s.Runs {
		for _, rec := range ra.Records {
			if rec.Metrics == nil || (s.Variant != "" && rec.Variant != s.Variant) || (scenario != "" && rec.Scenario != scenario) {
				continue
			}
			out = append(out, sample{scenario: rec.Scenario, device: ra.Provenance.Device, metrics: *rec.Metrics, build: rec.Build,
				before: rec.Before, after: rec.After, provenance: ra.Provenance, recordedAt: rec.RecordedAt.UnixNano()})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].recordedAt < out[j].recordedAt })
	return out
}

// Compare judges side B against side A per scenario x device x metric.
// Confounders are an error (CONFOUNDED, exit 2) unless allowed; high
// thermal status or swap in a record is a warning.
func Compare(a, b Side, opts CompareOptions) (Comparison, []runx.Diagnostic, error) {
	if opts.Threshold <= 0 {
		opts.Threshold = DefaultRegressionThreshold
	}
	if opts.Noise == (NoiseRule{}) {
		opts.Noise = DeviceNoiseRule
	}
	if opts.MinRuns > 0 {
		opts.Noise.MinimumSamples = opts.MinRuns
	}
	c := Comparison{A: a, B: b, Threshold: opts.Threshold, Rows: []CompareRow{}, Confounders: []Confounder{}}
	sa, sb := a.samples(opts.Scenario), b.samples(opts.Scenario)
	if len(sa) == 0 || len(sb) == 0 {
		return c, nil, diag(DiagNothingMeasured, fmt.Sprintf("side %s has %d measured records, side %s %d", a.Label, len(sa), b.Label, len(sb)),
			"perflab run <scenario> --device <id> --lease <token> --variant <a> --variant <b> --alternate --repeat 2")
	}
	var diags []runx.Diagnostic
	allowed := map[ConfoundField]bool{}
	for _, f := range opts.AllowConfound {
		allowed[f] = true
	}
	var blocking []string
	for _, f := range ConfoundFields {
		va, vb := confoundValues(sa, f), confoundValues(sb, f)
		if strings.Join(va, ",") == strings.Join(vb, ",") && len(va) == 1 {
			continue
		}
		cf := Confounder{Field: f, A: va, B: vb, Allowed: allowed[f]}
		c.Confounders = append(c.Confounders, cf)
		detail := fmt.Sprintf("%s differs: %s on %s, %s on %s", f, strings.Join(va, "|"), a.Label, strings.Join(vb, "|"), b.Label)
		if cf.Allowed {
			diags = append(diags, warn(DiagConfounded, detail+" (allowed)", ""))
		} else {
			blocking = append(blocking, detail)
		}
	}
	diags = append(diags, stateWarnings(append(append([]sample{}, sa...), sb...))...)

	type key struct{ scenario, device string }
	groupsA, groupsB := map[key][]sample{}, map[key][]sample{}
	var keys []key
	for _, s := range sa {
		k := key{s.scenario, s.device}
		if _, ok := groupsA[k]; !ok {
			keys = append(keys, k)
		}
		groupsA[k] = append(groupsA[k], s)
	}
	for _, s := range sb {
		k := key{s.scenario, s.device}
		if _, ok := groupsA[k]; !ok {
			if _, seen := groupsB[k]; !seen {
				keys = append(keys, k)
			}
		}
		groupsB[k] = append(groupsB[k], s)
	}
	tooFew, noisy := 0, 0
	for _, k := range keys {
		ga, gb := groupsA[k], groupsB[k]
		for _, m := range AllMetrics {
			va, vb := metricValues(ga, m), metricValues(gb, m)
			if len(va) == 0 && len(vb) == 0 {
				continue
			}
			row := judge(k.scenario, k.device, m, va, vb, opts)
			switch row.Verdict {
			case VerdictTooFewRuns:
				tooFew++
			case VerdictNoisy:
				noisy++
			}
			c.Rows = append(c.Rows, row)
		}
		if va, vb, clocks := matchedClockDraw(ga, gb); len(clocks) > 0 {
			row := judge(k.scenario, k.device, MetricRTDrawMatchedMs, va, vb, opts)
			row.Reasons = append(row.Reasons, "clocks: "+strings.Join(clocks, ", "))
			c.Rows = append(c.Rows, row)
		}
		c.Steps = append(c.Steps, compareSteps(k.scenario, ga, gb)...)
	}
	if tooFew > 0 {
		diags = append(diags, warn(DiagTooFewRuns, fmt.Sprintf("%d row(s) have fewer than %d runs a side", tooFew, opts.Noise.MinimumSamples),
			"perflab run <scenario> --device <id> --lease <token> --variant <a> --variant <b> --alternate --repeat 2"))
	}
	if noisy > 0 {
		diags = append(diags, warn(DiagNoisy, fmt.Sprintf("%d row(s) fail the noise rule (relative MAD, p90/p50 or half drift)", noisy),
			"re-run alternating the variants at a quiet moment: perflab run <scenario> ... --alternate --repeat 2"))
	}
	if len(blocking) > 0 {
		return c, diags, diag(DiagConfounded, strings.Join(blocking, "; "),
			"compare like with like, or accept the difference: perflab compare <a> <b> --allow-confound <field>")
	}
	return c, diags, nil
}

func judge(scenario, device string, m Metric, va, vb []float64, opts CompareOptions) CompareRow {
	row := CompareRow{Scenario: scenario, Device: device, Metric: m, Direction: MetricDirections[m], A: Summarize(va), B: Summarize(vb)}
	na, nb := AnalyzeNoise(va, opts.Noise), AnalyzeNoise(vb, opts.Noise)
	if row.A.N > 0 && row.A.Median != 0 {
		d := math.Round((row.B.Median-row.A.Median)/math.Abs(row.A.Median)*1000) / 1000
		row.Delta = &d
	}
	switch {
	case na.TooFew || nb.TooFew:
		row.Verdict = VerdictTooFewRuns
		row.Reasons = append(prefixed("a", na.Reasons), prefixed("b", nb.Reasons)...)
		return row
	case na.Noisy || nb.Noisy:
		row.Verdict = VerdictNoisy
		row.Reasons = append(prefixed("a", na.Reasons), prefixed("b", nb.Reasons)...)
		return row
	}
	var change float64
	switch {
	case row.Delta != nil:
		change = *row.Delta
	case row.B.Median == row.A.Median:
		change = 0
	default:
		// A is 0 and B is not: any rise of a lower-is-better count is a
		// full regression.
		change = math.Inf(1)
	}
	if row.Direction == HigherIsBetter {
		change = -change
	}
	switch {
	case change > opts.Threshold:
		row.Verdict = VerdictRegressed
	case change < -opts.Threshold:
		row.Verdict = VerdictImproved
	default:
		row.Verdict = VerdictWithinNoise
	}
	return row
}

func prefixed(side string, reasons []string) []string {
	out := make([]string, len(reasons))
	for i, r := range reasons {
		out[i] = side + ": " + r
	}
	return out
}

// matchedClockMinFrames is the frames a CPU clock must hold in every run to
// enter the clock-matched draw (fewer is one stray frame's noise).
const matchedClockMinFrames = 20

// matchedClockDraw is each run's RenderThread draw per frame over only the
// (CPU, clock) points every run of both sides drew at, each point weighted
// by its frames pooled over all runs: an injected drag skips the touch
// boost, so the governor moved the raw average 4.12 -> 3.90 ms between two
// runs of one build while the mid cores at 1690 MHz read 3.23-3.49 ms. Nil
// when the sides share no such point.
func matchedClockDraw(ga, gb []sample) (va, vb []float64, clocks []string) {
	type point struct {
		cpu int
		mhz int64
	}
	all := append(append([]sample{}, ga...), gb...)
	perRun := make([]map[point]framestats.RTCpu, len(all))
	pooled := map[point]int{}
	for i, s := range all {
		perRun[i] = map[point]framestats.RTCpu{}
		if s.metrics.Present == nil {
			return nil, nil, nil
		}
		for _, c := range s.metrics.Present.RTCpu {
			if c.MHz <= 0 {
				continue
			}
			p := point{c.CPU, c.MHz}
			perRun[i][p] = c
			pooled[p] += c.Frames
		}
	}
	var shared []point
	for p := range pooled {
		inAll := true
		for _, run := range perRun {
			if run[p].Frames < matchedClockMinFrames {
				inAll = false
				break
			}
		}
		if inAll {
			shared = append(shared, p)
		}
	}
	if len(shared) == 0 {
		return nil, nil, nil
	}
	sort.Slice(shared, func(i, j int) bool {
		if shared[i].cpu != shared[j].cpu {
			return shared[i].cpu < shared[j].cpu
		}
		return shared[i].mhz < shared[j].mhz
	})
	weigh := func(run map[point]framestats.RTCpu) float64 {
		sum, weight := 0.0, 0.0
		for _, p := range shared {
			w := float64(pooled[p])
			sum += w * run[p].AvgDrawMs
			weight += w
		}
		return math.Round(sum/weight*100) / 100
	}
	for i := range all {
		if i < len(ga) {
			va = append(va, weigh(perRun[i]))
		} else {
			vb = append(vb, weigh(perRun[i]))
		}
	}
	for _, p := range shared {
		clocks = append(clocks, fmt.Sprintf("cpu%d@%dMHz", p.cpu, p.mhz))
	}
	return va, vb, clocks
}

func metricValues(ss []sample, m Metric) []float64 {
	var out []float64
	for _, s := range ss {
		if v, ok := s.metrics.Scalars()[m]; ok {
			out = append(out, v)
		}
	}
	return out
}

func confoundValues(ss []sample, f ConfoundField) []string {
	set := map[string]bool{}
	for _, s := range ss {
		var v string
		switch f {
		case ConfoundNativeKey:
			v = s.build.NativeKey
		case ConfoundPublicEnv:
			v = s.build.PublicEnvHash
		case ConfoundDevice:
			v = s.provenance.Device
		case ConfoundInputSource:
			v = string(s.provenance.InputSource)
		case ConfoundProductionEqv:
			v = fmt.Sprint(s.build.ProductionEquivalent)
		}
		set[v] = true
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

func stateWarnings(ss []sample) []runx.Diagnostic {
	var diags []runx.Diagnostic
	for _, s := range ss {
		for _, st := range []*DeviceState{s.before, s.after} {
			if st == nil {
				continue
			}
			if st.ThermalStatus != nil && *st.ThermalStatus >= ThermalWarnStatus {
				diags = append(diags, warn(DiagThermalHot, fmt.Sprintf("%s on %s ran at thermal status %d", s.scenario, s.device, *st.ThermalStatus),
					"let the device cool (thermal status 0), then re-run"))
			}
			if st.SwapUsedMb != nil && *st.SwapUsedMb > SwapWarnMb {
				diags = append(diags, warn(DiagMemoryPressure, fmt.Sprintf("%s on %s ran with %.0f MB swap in use", s.scenario, s.device, *st.SwapUsedMb),
					"perflab device shell "+s.device+" --lease <token> --json -- shell am kill-all, then re-run"))
			}
		}
	}
	return dedupeDiags(diags)
}

// compareSteps lines up each step kind's median per side.
func compareSteps(scenario string, ga, gb []sample) []StepCompare {
	type acc struct {
		measure string
		a, b    []float64
	}
	byStep := map[string]*acc{}
	var order []string
	add := func(step, measure string, v float64, side int) {
		x := byStep[step]
		if x == nil {
			x = &acc{measure: measure}
			byStep[step] = x
			order = append(order, step)
		}
		if side == 0 {
			x.a = append(x.a, v)
		} else {
			x.b = append(x.b, v)
		}
	}
	for side, g := range [][]sample{ga, gb} {
		for _, s := range g {
			for _, st := range s.metrics.IOSSteps {
				add(st.Label, "hitchMs", st.HitchMs, side)
			}
			for _, st := range append(append([]framestats.StepRate{}, s.metrics.AndroidGroups...), s.metrics.AndroidSteps...) {
				// A step with no measured bin (a group no tap matched) has
				// no fps: its 0 is unread, never a sample.
				if st.Bins > 0 {
					add(st.Label, "fpsP10", st.FpsP10, side)
				}
			}
		}
	}
	var out []StepCompare
	for _, step := range order {
		x := byStep[step]
		if len(x.a) == 0 || len(x.b) == 0 {
			continue
		}
		out = append(out, StepCompare{Scenario: scenario, Step: step, Measure: x.measure, A: Summarize(x.a).Median, B: Summarize(x.b).Median})
	}
	return out
}
