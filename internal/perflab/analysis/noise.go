package analysis

import (
	"fmt"
	"math"
	"sort"
)

// NoiseRule is when a set of runs is too scattered to judge (ported from
// FixIt's appium/perf/stats.ts).
type NoiseRule struct {
	MinimumSamples   int     `json:"minimumSamples"`
	MaxRelativeMad   float64 `json:"maxRelativeMad"`
	MaxP90ToP50Ratio float64 `json:"maxP90ToP50Ratio"`
	MaxHalfDrift     float64 `json:"maxHalfDrift"`
}

// DeviceNoiseRule is the device-run default: two alternated runs a side.
var DeviceNoiseRule = NoiseRule{MinimumSamples: 2, MaxRelativeMad: 0.25, MaxP90ToP50Ratio: 1.5, MaxHalfDrift: 0.25}

// LatencyNoiseRule is the latency-sample default (five samples).
var LatencyNoiseRule = NoiseRule{MinimumSamples: 5, MaxRelativeMad: 0.25, MaxP90ToP50Ratio: 1.5, MaxHalfDrift: 0.25}

// DefaultRegressionThreshold is the relative change a verdict needs.
const DefaultRegressionThreshold = 0.15

// Stats summarize one side's samples of one metric.
type Stats struct {
	N      int       `json:"n"`
	Values []float64 `json:"values"`
	Min    float64   `json:"min"`
	Median float64   `json:"median"`
	P90    float64   `json:"p90"`
	Max    float64   `json:"max"`
}

// Summarize is the interpolated summary of samples (in run order).
func Summarize(samples []float64) Stats {
	s := Stats{N: len(samples), Values: append([]float64(nil), samples...)}
	if len(samples) == 0 {
		return s
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	s.Min, s.Max = sorted[0], sorted[len(sorted)-1]
	s.Median, s.P90 = interp(sorted, 0.5), interp(sorted, 0.9)
	return s
}

func interp(sorted []float64, q float64) float64 {
	pos := float64(len(sorted)-1) * q
	lo, hi := int(math.Floor(pos)), int(math.Ceil(pos))
	return sorted[lo] + (sorted[hi]-sorted[lo])*(pos-float64(lo))
}

// Noise is a side judged against the rule.
type Noise struct {
	Noisy         bool     `json:"noisy"`
	TooFew        bool     `json:"tooFew"`
	RelativeMad   float64  `json:"relativeMad"`
	P90ToP50Ratio float64  `json:"p90ToP50Ratio"`
	HalfDrift     float64  `json:"halfDrift"`
	Reasons       []string `json:"reasons"`
}

// AnalyzeNoise applies the rule to samples in run order. A side whose
// median is 0 (a screen at rest drawing nothing) is noisy only when its
// samples differ.
func AnalyzeNoise(samples []float64, rule NoiseRule) Noise {
	n := Noise{Reasons: []string{}}
	if len(samples) < rule.MinimumSamples {
		n.TooFew = true
		n.Reasons = append(n.Reasons, fmt.Sprintf("requires at least %d samples, received %d", rule.MinimumSamples, len(samples)))
	}
	if len(samples) == 0 {
		n.Noisy = true
		return n
	}
	sum := Summarize(samples)
	dev := make([]float64, len(samples))
	for i, v := range samples {
		dev[i] = math.Abs(v - sum.Median)
	}
	madSum := Summarize(dev)
	half := len(samples) / 2
	if sum.Median == 0 {
		if sum.Max != sum.Min {
			n.Reasons = append(n.Reasons, fmt.Sprintf("median 0 with samples up to %g", sum.Max))
		}
	} else {
		n.RelativeMad = madSum.Median / math.Abs(sum.Median)
		n.P90ToP50Ratio = sum.P90 / sum.Median
		// One sample has no halves (the stats.ts port read that as infinite
		// drift; with --min-runs 1 asked for, it is no drift).
		if half > 0 {
			n.HalfDrift = math.Abs(Summarize(samples[:half]).Median-Summarize(samples[half:]).Median) / math.Abs(sum.Median)
		}
		if n.RelativeMad > rule.MaxRelativeMad {
			n.Reasons = append(n.Reasons, fmt.Sprintf("relative MAD %.3f exceeds %.3f", n.RelativeMad, rule.MaxRelativeMad))
		}
		if n.P90ToP50Ratio > rule.MaxP90ToP50Ratio {
			n.Reasons = append(n.Reasons, fmt.Sprintf("p90/p50 %.3f exceeds %.3f", n.P90ToP50Ratio, rule.MaxP90ToP50Ratio))
		}
		if n.HalfDrift > rule.MaxHalfDrift {
			n.Reasons = append(n.Reasons, fmt.Sprintf("half-run drift %.3f exceeds %.3f", n.HalfDrift, rule.MaxHalfDrift))
		}
	}
	n.Noisy = len(n.Reasons) > 0
	return n
}
