package framestats

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// DiagCappedFpsSource: the sidecar carries a profiler's (Flashlight's) FPS
// on a display above 60 Hz. That FPS tops out at 60, so its numbers are
// reported for reference and never enter a verdict; the display metrics
// stand in. Warning.
const DiagCappedFpsSource = "CAPPED_FPS_SOURCE"

// DiagStepsUnmapped: taps were given but none landed inside the polled
// window, so no step carries a reading. Warning.
const DiagStepsUnmapped = "STEPS_UNMAPPED"

// ProfilerSample is one of the profiler's own 500 ms readings, on the host
// clock.
type ProfilerSample struct {
	AtMs float64  `json:"atMs"`
	Fps  *float64 `json:"fps,omitempty"`
}

// Sidecar is the poll sidecar a capture writes as `frames/<stamp>.json`:
// the framestats dumps polled through the window, the display's vsync
// period from `dumpsys SurfaceFlinger --latency`, and the profiler's own
// readings beside them.
type Sidecar struct {
	VsyncPeriodNs   *int64           `json:"vsyncPeriodNs"`
	Dumps           []Dump           `json:"dumps"`
	ProfilerSamples []ProfilerSample `json:"profilerSamples,omitempty"`
}

// ReadSidecar reads a poll sidecar (gzip accepted).
func ReadSidecar(path string) (Sidecar, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Sidecar{}, diag(DiagFileUnreadable, fmt.Sprintf("cannot read %s: %v", path, err), "pass the frames/<stamp>.json the capture wrote")
	}
	if bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err == nil {
			raw, err = io.ReadAll(zr)
		}
		if err != nil {
			return Sidecar{}, diag(DiagFileUnreadable, fmt.Sprintf("%s: %v", path, err), "pass the frames/<stamp>.json the capture wrote")
		}
	}
	var sc Sidecar
	if err := json.Unmarshal(raw, &sc); err != nil || len(sc.Dumps) == 0 {
		detail := fmt.Sprintf("%s holds no {vsyncPeriodNs, dumps:[{uptimeMs, frames}]}", path)
		if err != nil {
			detail += ": " + err.Error()
		}
		return Sidecar{}, diag(DiagNotFramestats, detail, "pass the frames/<stamp>.json the capture wrote")
	}
	return sc, nil
}

// PeriodNs is the sidecar's vsync period, 0 when the capture had none.
func (s Sidecar) PeriodNs() int64 {
	if s.VsyncPeriodNs == nil {
		return 0
	}
	return *s.VsyncPeriodNs
}

// MaxAdbMs is the slowest adb call whose dump still anchors the clock.
const MaxAdbMs = 400.0

// FallbackDumpLagMs is how long after a result's recordedAt the first dump
// lands when the sidecar carries no host timestamps.
const FallbackDumpLagMs = 150.0

// ClockOffset is `uptimeMs - host ms`: the median over dumps that carry a
// host timestamp (and an adb call under MaxAdbMs when timed), else the first
// dump against recordedAtMs + FallbackDumpLagMs. source names which.
func ClockOffset(s Sidecar, recordedAtMs float64) (offsetMs float64, source string) {
	var offsets []float64
	for _, d := range s.Dumps {
		if d.AtMs == nil || (d.AdbMs != nil && *d.AdbMs >= MaxAdbMs) {
			continue
		}
		offsets = append(offsets, d.UptimeMs-*d.AtMs)
	}
	if len(offsets) > 0 {
		return quantileOr0(offsets, 0.5), "dumps"
	}
	return s.Dumps[0].UptimeMs - (recordedAtMs + FallbackDumpLagMs), "recordedAt"
}

var stampRe = regexp.MustCompile(`(\d{4}-\d\d-\d\dT\d\d)-(\d\d)-(\d\d)-(\d{3})Z`)

// RecordedAtFromName reads the `2026-10-02T10-33-10-748Z` stamp of a
// sidecar or result file name as epoch ms.
func RecordedAtFromName(name string) (float64, bool) {
	m := stampRe.FindStringSubmatch(name)
	if m == nil {
		return 0, false
	}
	t, err := time.Parse(time.RFC3339Nano, fmt.Sprintf("%s:%s:%s.%sZ", m[1], m[2], m[3], m[4]))
	if err != nil {
		return 0, false
	}
	return float64(t.UnixMilli()), true
}

// WorstGapWindowMs is how long after a tap the worst present gap is read.
const WorstGapWindowMs = 600.0

// StepRate is one step kind at the display rate.
type StepRate struct {
	Label string `json:"label"`
	Taps  int    `json:"taps"`
	// Bins: 500 ms bins started at each tap that animated; their median and
	// worst decile (steps-120c).
	Bins      int     `json:"bins"`
	FpsMedian float64 `json:"fpsMedian"`
	FpsP10    float64 `json:"fpsP10"`
	// Intervals and InBurstFps: the present intervals at most RestGapMs
	// long between a tap and the next, and the rate while drawing.
	Intervals  int     `json:"intervals"`
	InBurstFps float64 `json:"inBurstFps"`
	JankyPct   float64 `json:"jankyPct"`
	// WorstGapAfterTapMs is the longest present gap inside the
	// WorstGapWindowMs after any of the step's taps.
	WorstGapAfterTapMs float64 `json:"worstGapAfterTapMs"`
}

// StepOptions tunes SidecarSteps.
type StepOptions struct {
	// TapsMs are the taps on the host clock (epoch ms), in order.
	TapsMs []float64
	// Cycle names the taps of one replayed round, one name per tap.
	Cycle []string
	// Groups aggregate step kinds ("modes": multi, team, list, day).
	Groups map[string][]string
	// RecordedAtMs anchors a sidecar without host timestamps.
	RecordedAtMs float64
}

// PollResult is the display-rate reading of a sidecar.
type PollResult struct {
	Display      DisplayMetrics `json:"display"`
	ClockOffset  float64        `json:"clockOffsetMs"`
	ClockSource  string         `json:"clockSource"`
	Steps        []StepRate     `json:"steps,omitempty"`
	Groups       []StepRate     `json:"groups,omitempty"`
	ProfilerFps  *ProfilerFps   `json:"profilerFps,omitempty"`
	TapsInWindow int            `json:"tapsInWindow"`
}

// ProfilerFps is the profiler's own reading, kept for reference only when
// it is capped (Capped never enters a verdict).
type ProfilerFps struct {
	Samples int     `json:"samples"`
	Median  float64 `json:"median"`
	P10     float64 `json:"p10"`
	Capped  bool    `json:"capped"`
}

type stepAcc struct {
	taps         int
	bins         []float64
	gaps         []float64
	worstGap     float64
	intervalsAll int
}

// AnalyzeSidecar reads the display metrics and, given taps and a cycle,
// the per-step rates of one poll sidecar.
func AnalyzeSidecar(sc Sidecar, opts StepOptions) (PollResult, []runx.Diagnostic) {
	res := PollResult{Display: SummarizeDisplay(sc.Dumps, sc.PeriodNs())}
	var diags []runx.Diagnostic
	if pf := profilerFps(sc.ProfilerSamples); pf != nil {
		pf.Capped = res.Display.RefreshHz > 60.5
		res.ProfilerFps = pf
		if pf.Capped {
			diags = append(diags, warn(DiagCappedFpsSource,
				fmt.Sprintf("the profiler's FPS tops out at 60 on this %.0f Hz display (median %.1f); it is reported for reference and never judged", res.Display.RefreshHz, pf.Median),
				"judge the display metrics (animatingFpsP10, jankyPct) instead"))
		}
	}
	if len(opts.TapsMs) == 0 || len(opts.Cycle) == 0 {
		return res, diags
	}
	res.ClockOffset, res.ClockSource = ClockOffset(sc, opts.RecordedAtMs)
	start, end := sc.Dumps[0].UptimeMs, sc.Dumps[len(sc.Dumps)-1].UptimeMs
	presents, frames := uniquePresents(sc.Dumps, start, end)
	period := periodMs(sc.PeriodNs(), frames)
	refresh := 1000 / period
	var taps []float64
	for _, t := range opts.TapsMs {
		if u := t + res.ClockOffset; u >= start && u <= end {
			taps = append(taps, u)
		}
	}
	res.TapsInWindow = len(taps)
	if len(taps) == 0 {
		diags = append(diags, warn(DiagStepsUnmapped, fmt.Sprintf("none of the %d taps lands inside the polled window (clock from %s)", len(opts.TapsMs), res.ClockSource),
			"check the runner log belongs to this window"))
		return res, diags
	}
	accs := map[string]*stepAcc{}
	var order []string
	for i, t := range taps {
		next := end
		if i+1 < len(taps) {
			next = taps[i+1]
		}
		name := opts.Cycle[i%len(opts.Cycle)]
		acc := accs[name]
		if acc == nil {
			acc = &stepAcc{}
			accs[name] = acc
			order = append(order, name)
		}
		acc.taps++
		lo := sort.SearchFloat64s(presents, t)
		hi := sort.SearchFloat64s(presents, next)
		ps := presents[lo:hi]
		// Bins started at the tap (steps-120c).
		for b := t; b < next; b += BinMs {
			stop := math.Min(b+BinMs, next)
			var bb bin
			var prev float64
			first := true
			for _, p := range ps {
				if p < b || p >= stop {
					continue
				}
				if !first {
					bb.addGap(p-prev, period)
				}
				prev, first = p, false
			}
			if bb.intervals >= AnimatingMinIntervals {
				acc.bins = append(acc.bins, bb.fps(refresh, period, BinMs))
			}
		}
		// In-burst intervals (steps-120b) and the worst gap after the tap.
		for j := 1; j < len(ps); j++ {
			g := ps[j] - ps[j-1]
			if g <= RestGapMs {
				acc.gaps = append(acc.gaps, g)
			}
			if ps[j] < t+WorstGapWindowMs {
				acc.worstGap = math.Max(acc.worstGap, g)
			}
		}
	}
	for _, name := range order {
		res.Steps = append(res.Steps, stepRate(name, accs[name], period))
	}
	groupNames := make([]string, 0, len(opts.Groups))
	for g := range opts.Groups {
		groupNames = append(groupNames, g)
	}
	sort.Strings(groupNames)
	for _, g := range groupNames {
		merged := &stepAcc{}
		for _, k := range opts.Groups[g] {
			if a := accs[k]; a != nil {
				merged.taps += a.taps
				merged.bins = append(merged.bins, a.bins...)
				merged.gaps = append(merged.gaps, a.gaps...)
				merged.worstGap = math.Max(merged.worstGap, a.worstGap)
			}
		}
		res.Groups = append(res.Groups, stepRate(g, merged, period))
	}
	return res, diags
}

func stepRate(label string, a *stepAcc, period float64) StepRate {
	r := StepRate{Label: label, Taps: a.taps, Bins: len(a.bins), Intervals: len(a.gaps),
		FpsMedian: round1(quantileOr0(a.bins, 0.5)), FpsP10: round1(quantileOr0(a.bins, 0.1)),
		WorstGapAfterTapMs: round1(a.worstGap)}
	if len(a.gaps) > 0 {
		span, janky := 0.0, 0
		for _, g := range a.gaps {
			span += g
			if g > JankyPeriods*period {
				janky++
			}
		}
		r.InBurstFps = round1(1000 * float64(len(a.gaps)) / span)
		r.JankyPct = round1(100 * float64(janky) / float64(len(a.gaps)))
	}
	return r
}

func profilerFps(samples []ProfilerSample) *ProfilerFps {
	var fps []float64
	for _, s := range samples {
		if s.Fps != nil {
			fps = append(fps, *s.Fps)
		}
	}
	if len(fps) == 0 {
		return nil
	}
	return &ProfilerFps{Samples: len(fps), Median: round1(quantileOr0(fps, 0.5)), P10: round1(quantileOr0(fps, 0.1))}
}

// ReadTapsMs lists the `COMMAND performActions` timestamps of a
// WebdriverIO runner log as epoch ms.
func ReadTapsMs(r io.Reader) ([]float64, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var out []float64
	for _, line := range strings.Split(string(raw), "\n") {
		m := tapLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, m[1])
		if err == nil {
			out = append(out, float64(t.UnixNano())/1e6)
		}
	}
	return out, nil
}

var tapLineRe = regexp.MustCompile(`(\d{4}-\d\d-\d\dT[\d:.]+Z) INFO webdriver: COMMAND performActions`)
