package analysis

// Metric is one comparable number of the closed metric vocabulary.
type Metric string

const (
	MetricHitchRatio         Metric = "hitchRatioMsPerS"
	MetricWorstAppHitch      Metric = "worstAppHitchMs"
	MetricHitchCount         Metric = "hitchCount"
	MetricAnimatingFpsP10    Metric = "animatingFpsP10"
	MetricAnimatingFpsMedian Metric = "animatingFpsMedian"
	MetricJankyPct           Metric = "jankyPct"
	MetricFpsP10             Metric = "fpsP10"
	MetricRestFrames         Metric = "restFrames"
	MetricRestRunMs          Metric = "restRunMs"
	MetricRTDrawAvgMs        Metric = "rtDrawAvgMs"
	MetricTwoVsyncGaps       Metric = "twoVsyncGaps"
	MetricDrops              Metric = "drops"
	MetricSlope              Metric = "slope"
	MetricIOSRestMainMsPerS  Metric = "iosRestMainMsPerS"
	// MetricRTDrawMatchedMs is compare's RenderThread draw per frame over
	// the CPU clocks every compared run drew at (matchedClockDraw): never a
	// per-run scalar, it exists only between runs.
	MetricRTDrawMatchedMs Metric = "rtDrawMatchedClockMs"
)

// Direction says which way a metric gets better.
type Direction string

const (
	LowerIsBetter  Direction = "lower"
	HigherIsBetter Direction = "higher"
)

// AllMetrics lists the vocabulary in report order; MetricDirections must
// name every one (held by a test).
var AllMetrics = []Metric{
	MetricHitchRatio, MetricWorstAppHitch, MetricHitchCount,
	MetricAnimatingFpsP10, MetricAnimatingFpsMedian, MetricJankyPct, MetricFpsP10,
	MetricRestFrames, MetricRestRunMs, MetricRTDrawAvgMs, MetricRTDrawMatchedMs, MetricTwoVsyncGaps, MetricDrops,
	MetricSlope, MetricIOSRestMainMsPerS,
}

// MetricDirections is the exhaustive direction table.
var MetricDirections = map[Metric]Direction{
	MetricHitchRatio:         LowerIsBetter,
	MetricWorstAppHitch:      LowerIsBetter,
	MetricHitchCount:         LowerIsBetter,
	MetricAnimatingFpsP10:    HigherIsBetter,
	MetricAnimatingFpsMedian: HigherIsBetter,
	MetricJankyPct:           LowerIsBetter,
	MetricFpsP10:             HigherIsBetter,
	MetricRestFrames:         LowerIsBetter,
	MetricRestRunMs:          LowerIsBetter,
	MetricRTDrawAvgMs:        LowerIsBetter,
	MetricTwoVsyncGaps:       LowerIsBetter,
	MetricDrops:              LowerIsBetter,
	MetricSlope:              LowerIsBetter,
	MetricIOSRestMainMsPerS:  LowerIsBetter,
	MetricRTDrawMatchedMs:    LowerIsBetter,
}

// Scalars flattens the metrics into the comparable vocabulary. A metric
// the evidence cannot speak to is absent, never zero.
func (m Metrics) Scalars() map[Metric]float64 {
	out := map[Metric]float64{}
	if m.IOS != nil {
		out[MetricHitchRatio] = m.IOS.HitchRatioMsPerS
		out[MetricWorstAppHitch] = m.IOS.WorstAppHitchMs
		out[MetricHitchCount] = float64(m.IOS.HitchCount)
	}
	if d := m.Display; d != nil && d.Frames > 0 {
		out[MetricFpsP10] = d.FpsP10
		// The animating readings and the janky share come from the animating
		// bins only: without one they are unread, not zero.
		if d.AnimatingBins > 0 {
			out[MetricAnimatingFpsP10] = d.AnimatingFpsP10
			out[MetricAnimatingFpsMedian] = d.AnimatingFpsMedian
			out[MetricJankyPct] = d.JankyPct
		}
	}
	if p := m.Present; p != nil {
		// Present readings without FrameTimeline are unread, never zero samples.
		if p.FrameTimeline {
			out[MetricRestFrames] = float64(p.RestFrames)
			out[MetricRestRunMs] = p.RestRunMs
			out[MetricTwoVsyncGaps] = float64(p.PresentGaps.TwoVsync)
			out[MetricDrops] = float64(p.Drops.Total)
		}
		if p.RTDrawMs.Avg != nil {
			out[MetricRTDrawAvgMs] = *p.RTDrawMs.Avg
		}
	}
	if m.Slope != nil {
		out[MetricSlope] = *m.Slope
	}
	if m.IOSRestMainMsPerS != nil {
		out[MetricIOSRestMainMsPerS] = *m.IOSRestMainMsPerS
	}
	return out
}
