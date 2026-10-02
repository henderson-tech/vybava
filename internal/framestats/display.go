package framestats

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/henderson-tech/vybava/internal/runx"
)

// The display-rate reading (vt-4229's android-framestats.ts, consolidated
// here): a profiler's FPS counts a frame under 16.7 ms as 16.7 ms and tops
// out at 60, so a 120 Hz phone reads 60 whatever the app does. These numbers
// judge the presented frames against the display's own vsync period:
//
//   - every BinMs bin is the display rate minus the vsyncs its frames dropped
//     (a present interval of n periods drops n - 1); a bin at rest counts the
//     full rate, as the profiler credits idle time;
//   - an interval longer than RestGapMs is the app at rest, not a drop;
//   - "animating" bins hold at least AnimatingMinIntervals intervals;
//   - janky = the share of animating intervals over JankyPeriods periods.
const (
	BinMs                 = 500.0
	RestGapMs             = 60.0
	AnimatingMinIntervals = 6
	JankyPeriods          = 1.5
	// maxPeriodNs: a plausible vsync period; anything larger is a timestamp
	// in the wrong column (Android 13's swapped FrameInterval/FrameStartTime).
	maxPeriodNs = 1e9
	// skipFlags drops WindowLayoutChanged (1) and SkippedFrame (8) rows; an
	// RTAnimation (2) or SurfaceCanvas (4) frame still reached the glass.
	skipFlags = 1 | 8
)

// DiagColumnOrderSwapped: Android 13 prints FrameInterval before
// FrameStartTime in the header but writes the values the other way round;
// the reader took the column holding a plausible period. Info, no fix.
const DiagColumnOrderSwapped = "COLUMN_ORDER_SWAPPED"

// DisplayFrame is one frame of a framestats dump, as the poll sidecar
// (`frames/<stamp>.json`) stores it.
type DisplayFrame struct {
	IntendedVsyncNs  int64 `json:"intendedVsyncNs"`
	FrameCompletedNs int64 `json:"frameCompletedNs"`
	// FrameIntervalNs is the vsync period the frame was scheduled on; 0 when
	// the row has none.
	FrameIntervalNs int64 `json:"frameIntervalNs"`
	// DisplayPresentNs is when the frame reached the glass; 0 when the
	// platform does not say.
	DisplayPresentNs int64 `json:"displayPresentNs"`
}

// presentMs is where the frame met the glass, on the uptime clock in ms: its
// DisplayPresentTime, else its completion when the platform has no present
// time or reports a stale one (seconds before the frame's own vsync, seen on
// 8 to 12 frames of every S20 window; the TypeScript reader placed them there).
func (f DisplayFrame) presentMs() float64 {
	if f.DisplayPresentNs > 0 && f.DisplayPresentNs >= f.IntendedVsyncNs {
		return float64(f.DisplayPresentNs) / 1e6
	}
	return float64(f.FrameCompletedNs) / 1e6
}

// Dump is one `dumpsys gfxinfo <pkg> framestats` reading.
type Dump struct {
	// UptimeMs is SystemClock.uptimeMillis() at the dump: framestats' own
	// clock (CLOCK_MONOTONIC).
	UptimeMs float64 `json:"uptimeMs"`
	// AtMs is the host's wall clock at the dump (the midpoint of the adb
	// call), when the poller recorded it.
	AtMs *float64 `json:"atMs,omitempty"`
	// AdbMs is how long the adb call took, when recorded; a slow call
	// places AtMs badly and is left out of the clock offset.
	AdbMs  *float64       `json:"adbMs,omitempty"`
	Frames []DisplayFrame `json:"frames"`
}

var uptimeRe = regexp.MustCompile(`(?m)^Uptime: (\d+)`)

// ParseDump reads the frames of appID's windows from one framestats dump
// (a popup's own window is left out), dropping skipped and layout-change
// rows. A dump without an `Uptime:` line cannot be placed on the clock.
func ParseDump(name string, r io.Reader, appID string) (Dump, []runx.Diagnostic, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return Dump{}, nil, diag(DiagFileUnreadable, fmt.Sprintf("cannot read %s: %v", name, err), "adb shell dumpsys gfxinfo <pkg> framestats > framestats.txt")
	}
	m := uptimeRe.FindSubmatch(raw)
	if m == nil {
		return Dump{}, nil, diag(DiagNotFramestats, name+": no `Uptime:` line, so the dump cannot be placed on the device clock",
			"adb shell dumpsys gfxinfo <pkg> framestats > framestats.txt")
	}
	up, _ := strconv.ParseFloat(string(m[1]), 64)
	d := Dump{UptimeMs: up, Frames: []DisplayFrame{}}
	swapped := false
	blocks, _, err := readBlocks(name, strings.NewReader(string(raw)), appID, func(row frameRow) {
		if row["Flags"]&skipFlags != 0 {
			return
		}
		interval := int64(0)
		for _, col := range []string{"FrameInterval", "FrameStartTime"} {
			if v := row[col]; v > 0 && v < maxPeriodNs {
				interval = v
				swapped = swapped || col == "FrameStartTime"
				break
			}
		}
		d.Frames = append(d.Frames, DisplayFrame{
			IntendedVsyncNs:  row["IntendedVsync"],
			FrameCompletedNs: row["FrameCompleted"],
			FrameIntervalNs:  interval,
			DisplayPresentNs: max(0, row["DisplayPresentTime"]),
		})
	})
	if err != nil {
		return d, nil, err
	}
	if blocks == 0 {
		return d, nil, diag(DiagNotFramestats, fmt.Sprintf("%s: no ---PROFILEDATA--- block for window %s/", name, appID),
			"adb shell dumpsys gfxinfo <pkg> framestats > framestats.txt")
	}
	var diags []runx.Diagnostic
	if swapped {
		diags = append(diags, runx.Diagnostic{Code: DiagColumnOrderSwapped, Severity: "info",
			Detail: name + ": FrameInterval and FrameStartTime hold each other's values (Android 13); the period was read from FrameStartTime"})
	}
	return d, diags, nil
}

// ParseVsyncPeriodNs reads the first line of `dumpsys SurfaceFlinger
// --latency`: the display's vsync period, ns.
func ParseVsyncPeriodNs(latency string) (int64, bool) {
	first, _, _ := strings.Cut(strings.TrimSpace(latency), "\n")
	v, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil || v <= 0 || v >= maxPeriodNs {
		return 0, false
	}
	return v, true
}

// DisplayMetrics is the window judged at the display's own rate.
type DisplayMetrics struct {
	RefreshHz          float64 `json:"refreshHz"`
	Frames             int     `json:"frames"`
	FpsP10             float64 `json:"fpsP10"`
	FpsMedian          float64 `json:"fpsMedian"`
	AnimatingBins      int     `json:"animatingBins"`
	AnimatingFpsP10    float64 `json:"animatingFpsP10"`
	AnimatingFpsMedian float64 `json:"animatingFpsMedian"`
	JankyPct           float64 `json:"jankyPct"`
}

// uniquePresents lists each frame once (the last dump that listed it wins:
// an early dump can hold a frame not yet presented) as its present time,
// kept inside [fromMs, toMs) and sorted.
func uniquePresents(dumps []Dump, fromMs, toMs float64) ([]float64, []DisplayFrame) {
	byVsync := map[int64]DisplayFrame{}
	for _, d := range dumps {
		for _, f := range d.Frames {
			byVsync[f.IntendedVsyncNs] = f
		}
	}
	frames := make([]DisplayFrame, 0, len(byVsync))
	var presents []float64
	for _, f := range byVsync {
		frames = append(frames, f)
		if t := f.presentMs(); t >= fromMs && t < toMs {
			presents = append(presents, t)
		}
	}
	sort.Float64s(presents)
	return presents, frames
}

// periodMs is the display's period: the SurfaceFlinger reading, else the
// frames' own median interval, else 60 Hz.
func periodMs(vsyncPeriodNs int64, frames []DisplayFrame) float64 {
	if vsyncPeriodNs > 0 {
		return float64(vsyncPeriodNs) / 1e6
	}
	var intervals []float64
	for _, f := range frames {
		if f.FrameIntervalNs > 0 {
			intervals = append(intervals, float64(f.FrameIntervalNs))
		}
	}
	if p := quantileOr0(intervals, 0.5); p > 0 {
		return p / 1e6
	}
	return 1000.0 / 60
}

type bin struct{ intervals, dropped, janky int }

func (b bin) fps(refreshHz, periodMs, binMs float64) float64 {
	return math.Max(0, refreshHz*(1-float64(b.dropped)*periodMs/binMs))
}

// addGap counts one present interval into a bin; rest gaps are skipped.
func (b *bin) addGap(gap, periodMs float64) {
	if gap > RestGapMs {
		return
	}
	b.intervals++
	b.dropped += max(0, int(math.Round(gap/periodMs))-1)
	if gap > JankyPeriods*periodMs {
		b.janky++
	}
}

// SummarizeDisplay judges the frames polled through a window: the window
// runs from the first dump's uptime to the last one's, each frame counts
// once, and the period is vsyncPeriodNs (SurfaceFlinger), else the frames'
// own median interval, else 60 Hz.
func SummarizeDisplay(dumps []Dump, vsyncPeriodNs int64) DisplayMetrics {
	if len(dumps) == 0 {
		return DisplayMetrics{}
	}
	start, end := dumps[0].UptimeMs, dumps[len(dumps)-1].UptimeMs
	presents, frames := uniquePresents(dumps, start, end)
	period := periodMs(vsyncPeriodNs, frames)
	refresh := 1000 / period
	bins := make([]bin, max(0, int(math.Floor((end-start)/BinMs))))
	for i := 1; i < len(presents); i++ {
		idx := int(math.Floor((presents[i] - start) / BinMs))
		if idx < 0 || idx >= len(bins) {
			continue
		}
		bins[idx].addGap(presents[i]-presents[i-1], period)
	}
	var all, animating []float64
	intervals, janky := 0, 0
	for _, b := range bins {
		f := b.fps(refresh, period, BinMs)
		all = append(all, f)
		if b.intervals >= AnimatingMinIntervals {
			animating = append(animating, f)
			intervals += b.intervals
			janky += b.janky
		}
	}
	m := DisplayMetrics{
		RefreshHz: round1(refresh), Frames: len(presents),
		FpsP10: round1(quantileOr0(all, 0.1)), FpsMedian: round1(quantileOr0(all, 0.5)),
		AnimatingBins:   len(animating),
		AnimatingFpsP10: round1(quantileOr0(animating, 0.1)), AnimatingFpsMedian: round1(quantileOr0(animating, 0.5)),
	}
	if intervals > 0 {
		m.JankyPct = round1(100 * float64(janky) / float64(intervals))
	}
	return m
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// quantileOr0 is the linearly interpolated q-quantile (0..1), 0 for none
// (the TypeScript reader's convention the published numbers carry).
func quantileOr0(values []float64, q float64) float64 {
	if len(values) == 0 {
		return 0
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	pos := float64(len(s)-1) * q
	lo, hi := int(math.Floor(pos)), int(math.Ceil(pos))
	return s[lo]*(1-(pos-float64(lo))) + s[hi]*(pos-float64(lo))
}
