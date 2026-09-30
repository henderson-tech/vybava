// Package framestats reads Android frame evidence: `dumpsys gfxinfo <pkg>
// framestats` dumps (Parse) and Perfetto protobuf traces (ReadTrace). Both
// answer in milliseconds rounded to 0.01; a metric with no samples is nil
// (JSON null), never a sentinel number, so a budget comparison on an empty
// run cannot pass by accident. Contract: docs/framestats.md.
package framestats

import (
	"math"
	"sort"
)

// Ms is a rounded millisecond value; nil means "no samples".
type Ms = *float64

// percentile is the linear-interpolation percentile p (0..100) of the
// non-NaN values, NaN when there are none.
func percentile(in []float64, p float64) float64 {
	values := make([]float64, 0, len(in))
	for _, v := range in {
		if !math.IsNaN(v) {
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return math.NaN()
	}
	sort.Float64s(values)
	idx := p / 100 * float64(len(values)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return values[lo]
	}
	return values[lo] + (values[hi]-values[lo])*(idx-float64(lo))
}

// ms rounds v to 0.01 and turns NaN into nil.
func ms(v float64) Ms {
	if math.IsNaN(v) {
		return nil
	}
	r := math.Round(v*100) / 100
	return &r
}

// pctMs is percentile then ms.
func pctMs(in []float64, p float64) Ms { return ms(percentile(in, p)) }

// nsToMs converts a nanosecond delta to float milliseconds.
func nsToMs(ns int64) float64 { return float64(ns) / 1e6 }
