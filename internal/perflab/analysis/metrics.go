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
	MetricRestFrames, MetricRestRunMs, MetricRTDrawAvgMs, MetricTwoVsyncGaps, MetricDrops,
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
	if m.Display != nil && m.Display.Frames > 0 {
		out[MetricAnimatingFpsP10] = m.Display.AnimatingFpsP10
		out[MetricAnimatingFpsMedian] = m.Display.AnimatingFpsMedian
		out[MetricJankyPct] = m.Display.JankyPct
		out[MetricFpsP10] = m.Display.FpsP10
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
