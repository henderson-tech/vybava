package framestats

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Frame-time thresholds: one 120 Hz and one 60 Hz vsync period.
const (
	over8Threshold  = 8.33
	over16Threshold = 16.67
	// cadenceGapMs: an IntendedVsync or present delta above this is an idle
	// gap, not a frame interval, and stays out of the cadence percentiles.
	cadenceGapMs = 100
	// gestureGapMs: input frames further apart than this belong to separate
	// gestures (a drag delivers input every vsync).
	gestureGapMs = 100
	// DefaultAfterReleaseMs is the window after each gesture's last input
	// frame that the release metrics cover.
	DefaultAfterReleaseMs = 250
)

// Input is one framestats dump to parse.
type Input struct {
	Name   string
	Reader io.Reader
}

// ParseOptions tunes Parse.
type ParseOptions struct {
	Label string
	// AfterReleaseMs is the release window (default DefaultAfterReleaseMs).
	AfterReleaseMs float64
	// Rows adds one Row per kept frame to the summary.
	Rows bool
}

// Window counts the frames that START inside a window after a release.
type Window struct {
	Frames     int `json:"frames"`
	Over16ms   int `json:"over16ms"`
	MaxFrameMs Ms  `json:"maxFrameMs"`
}

// Gesture is one run of input frames (touch down to release).
type Gesture struct {
	// StartMs is the first input frame's IntendedVsync, from the first kept row.
	StartMs           float64 `json:"startMs"`
	InputFrames       int     `json:"inputFrames"`
	FirstInputFrameMs Ms      `json:"firstInputFrameMs"`
	ReleaseWindow     Window  `json:"releaseWindow"`
}

// Row is one kept frame, for --rows.
type Row struct {
	TMs       float64 `json:"tMs"`
	VsyncID   int64   `json:"vsyncId"`
	Input     bool    `json:"input"`
	TotalMs   Ms      `json:"totalMs"`
	UIMs      Ms      `json:"uiMs"`
	RTMs      Ms      `json:"renderThreadMs"`
	GPUMs     Ms      `json:"gpuMs"`
	PresentMs Ms      `json:"presentMs"`
}

// Summary is `framestats parse`'s data.
type Summary struct {
	Label       string   `json:"label,omitempty"`
	Files       []string `json:"files"`
	RowsTotal   int      `json:"rowsTotal"`
	RowsFlagged int      `json:"rowsFlagged"`
	Frames      int      `json:"frames"`
	InputFrames int      `json:"inputFrames"`
	SpanMs      Ms       `json:"spanMs"`

	CadenceMedianMs       Ms  `json:"cadenceMedianMs"`
	CadenceP90Ms          Ms  `json:"cadenceP90Ms"`
	InputCadenceMedianMs  Ms  `json:"inputCadenceMedianMs"`
	SettleCadenceMedianMs Ms  `json:"settleCadenceMedianMs"`
	DeltasLe10ms          int `json:"deltasLe10ms"`
	Deltas10to20ms        int `json:"deltas10to20ms"`
	Deltas20to100ms       int `json:"deltas20to100ms"`
	GapsOver100ms         int `json:"gapsOver100ms"`

	PresentIntervalP50Ms      Ms `json:"presentIntervalP50Ms"`
	PresentIntervalP90Ms      Ms `json:"presentIntervalP90Ms"`
	InputPresentIntervalP50Ms Ms `json:"inputPresentIntervalP50Ms"`
	InputPresentIntervalP90Ms Ms `json:"inputPresentIntervalP90Ms"`

	FrameP50Ms     Ms  `json:"frameP50Ms"`
	FrameP90Ms     Ms  `json:"frameP90Ms"`
	FrameP99Ms     Ms  `json:"frameP99Ms"`
	FrameMaxMs     Ms  `json:"frameMaxMs"`
	Over8ms        int `json:"over8ms"`
	Over16ms       int `json:"over16ms"`
	DeadlineMissed int `json:"deadlineMissed"`

	StageMediansMs map[string]Ms `json:"stageMediansMs"`
	StageP90Ms     map[string]Ms `json:"stageP90Ms"`

	FirstInputFramesTotalMs []Ms     `json:"firstInputFramesTotalMs"`
	FirstInputFramesStages  []string `json:"firstInputFramesStages"`
	SteadyInputFrameP50Ms   Ms       `json:"steadyInputFrameP50Ms"`
	SteadyInputFrameP90Ms   Ms       `json:"steadyInputFrameP90Ms"`
	SettleFrameP50Ms        Ms       `json:"settleFrameP50Ms"`
	SettleFrameP90Ms        Ms       `json:"settleFrameP90Ms"`

	ReleaseWindowMs float64   `json:"releaseWindowMs"`
	Gestures        []Gesture `json:"gestures"`
	// FirstInputFrameMaxMs is the slowest first input frame over all gestures.
	FirstInputFrameMaxMs Ms `json:"firstInputFrameMaxMs"`
	// ReleaseOver16ms sums every gesture's release-window frames over 16.67 ms.
	ReleaseOver16ms   int   `json:"releaseOver16ms"`
	ReleaseMaxFrameMs Ms    `json:"releaseMaxFrameMs"`
	Rows              []Row `json:"rows,omitempty"`
}

type stage struct{ name, from, to string }

// stages are the per-frame phases, in pipeline order; the first ten are
// the ones listed per first input frame.
var stages = []stage{
	{"wake(vsync->input)", "IntendedVsync", "HandleInputStart"},
	{"input", "HandleInputStart", "AnimationStart"},
	{"animation", "AnimationStart", "PerformTraversalsStart"},
	{"traversal", "PerformTraversalsStart", "DrawStart"},
	{"draw(record)", "DrawStart", "SyncQueued"},
	{"syncQueueWait", "SyncQueued", "SyncStart"},
	{"sync", "SyncStart", "IssueDrawCommandsStart"},
	{"issueCommands", "IssueDrawCommandsStart", "SwapBuffers"},
	{"swap", "SwapBuffers", "FrameCompleted"},
	{"gpu", "SwapBuffers", "GpuCompleted"},
	{"uiThread(vsync->syncQueued)", "IntendedVsync", "SyncQueued"},
	{"renderThread(syncStart->completed)", "SyncStart", "FrameCompleted"},
}

type frameRow map[string]int64

// ParseFiles opens every path and parses them as one run.
func ParseFiles(paths []string, opts ParseOptions) (Summary, error) {
	inputs := make([]Input, 0, len(paths))
	for _, path := range paths {
		fh, err := os.Open(path)
		if err != nil {
			return Summary{}, diag(DiagFileUnreadable, fmt.Sprintf("cannot read %s: %v", path, err),
				"framestats parse <framestats.txt>... --json")
		}
		defer fh.Close()
		inputs = append(inputs, Input{Name: path, Reader: fh})
	}
	return Parse(inputs, opts)
}

// Parse reads every ---PROFILEDATA--- block of every input, dedupes rows by
// IntendedVsync (a run is often dumped several times, each dump holding the
// last ~120 frames), drops rows with Flags != 0 and summarises the rest.
func Parse(inputs []Input, opts ParseOptions) (Summary, error) {
	if opts.AfterReleaseMs <= 0 {
		opts.AfterReleaseMs = DefaultAfterReleaseMs
	}
	s := Summary{
		Label: opts.Label, Files: []string{}, ReleaseWindowMs: opts.AfterReleaseMs,
		StageMediansMs: map[string]Ms{}, StageP90Ms: map[string]Ms{},
		FirstInputFramesTotalMs: []Ms{}, FirstInputFramesStages: []string{}, Gestures: []Gesture{},
	}
	seen := map[int64]frameRow{}
	blocks := 0
	for _, in := range inputs {
		s.Files = append(s.Files, in.Name)
		n, err := readBlocks(in.Reader, func(r frameRow) {
			iv := r["IntendedVsync"]
			if _, dup := seen[iv]; dup {
				return
			}
			seen[iv] = r
			s.RowsTotal++
			if r["Flags"] != 0 {
				s.RowsFlagged++
			}
		})
		if err != nil {
			return s, diag(DiagFileUnreadable, fmt.Sprintf("cannot read %s: %v", in.Name, err),
				"framestats parse <framestats.txt>... --json")
		}
		blocks += n
	}
	if blocks == 0 {
		return s, diag(DiagNotFramestats,
			"no ---PROFILEDATA--- block in the input; a plain `dumpsys gfxinfo <pkg>` carries none",
			"adb shell dumpsys gfxinfo <pkg> framestats > framestats.txt")
	}
	rows := make([]frameRow, 0, len(seen))
	for _, r := range seen {
		if r["Flags"] == 0 {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i]["IntendedVsync"] < rows[j]["IntendedVsync"] })
	s.Frames = len(rows)
	if len(rows) == 0 {
		return s, nil
	}
	summarize(&s, rows, opts)
	return s, nil
}

// readBlocks streams the rows of every PROFILEDATA block and returns how
// many blocks it saw.
func readBlocks(r io.Reader, emit func(frameRow)) (int, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	in := false
	blocks := 0
	var header []string
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "---PROFILEDATA---" {
			in = !in
			if in {
				blocks++
			}
			header = nil
			continue
		}
		if !in || line == "" {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(line, ","), ",")
		if parts[0] == "Flags" {
			header = parts
			continue
		}
		if header == nil {
			continue
		}
		row := frameRow{}
		for i, h := range header {
			if i < len(parts) {
				if v, err := strconv.ParseInt(parts[i], 10, 64); err == nil {
					row[h] = v
				}
			}
		}
		emit(row)
	}
	return blocks, sc.Err()
}

// frameTime is FrameCompleted (or a later GpuCompleted) minus IntendedVsync;
// NaN for a row whose completion precedes its swap (an aborted frame).
func frameTime(r frameRow) float64 {
	if r["FrameCompleted"] < r["SwapBuffers"] {
		return math.NaN()
	}
	end := r["FrameCompleted"]
	if g := r["GpuCompleted"]; g > end {
		end = g
	}
	return nsToMs(end - r["IntendedVsync"])
}

func isInput(r frameRow) bool { return r["InputEventId"] != 0 }

func summarize(s *Summary, rows []frameRow, opts ParseOptions) {
	t0 := rows[0]["IntendedVsync"]
	s.SpanMs = ms(nsToMs(rows[len(rows)-1]["IntendedVsync"] - t0))
	stageValues := map[string][]float64{}
	frames := make([]float64, len(rows))
	var inputFrames, settleFrames []float64
	var deltas, inputDeltas, settleDeltas []float64
	var presents, inputPresents []float64
	firstInput := -1
	for i, r := range rows {
		ft := frameTime(r)
		frames[i] = ft
		if !math.IsNaN(ft) {
			if ft > over8Threshold {
				s.Over8ms++
			}
			if ft > over16Threshold {
				s.Over16ms++
			}
			end := r["FrameCompleted"]
			if r["GpuCompleted"] > end {
				end = r["GpuCompleted"]
			}
			if dl := r["FrameDeadline"]; dl > 0 && end > dl {
				s.DeadlineMissed++
			}
			for _, st := range stages {
				a, b := r[st.from], r[st.to]
				if a > 0 && b > 0 && b >= a {
					stageValues[st.name] = append(stageValues[st.name], nsToMs(b-a))
				}
			}
		}
		input := isInput(r)
		if input {
			s.InputFrames++
			if firstInput < 0 {
				firstInput = i
			}
			inputFrames = append(inputFrames, ft)
		} else {
			settleFrames = append(settleFrames, ft)
		}
		if i == 0 {
			continue
		}
		prev := rows[i-1]
		d := nsToMs(r["IntendedVsync"] - prev["IntendedVsync"])
		switch {
		case d <= 10:
			s.DeltasLe10ms++
		case d <= 20:
			s.Deltas10to20ms++
		case d <= cadenceGapMs:
			s.Deltas20to100ms++
		default:
			s.GapsOver100ms++
		}
		if d <= cadenceGapMs {
			deltas = append(deltas, d)
			if input && isInput(prev) {
				inputDeltas = append(inputDeltas, d)
			} else if !input && !isInput(prev) {
				settleDeltas = append(settleDeltas, d)
			}
		}
		if p, q := r["DisplayPresentTime"], prev["DisplayPresentTime"]; p > 0 && q > 0 && p > q {
			if pd := nsToMs(p - q); pd <= cadenceGapMs {
				presents = append(presents, pd)
				if input && isInput(prev) {
					inputPresents = append(inputPresents, pd)
				}
			}
		}
	}
	s.CadenceMedianMs, s.CadenceP90Ms = pctMs(deltas, 50), pctMs(deltas, 90)
	s.InputCadenceMedianMs, s.SettleCadenceMedianMs = pctMs(inputDeltas, 50), pctMs(settleDeltas, 50)
	s.PresentIntervalP50Ms, s.PresentIntervalP90Ms = pctMs(presents, 50), pctMs(presents, 90)
	s.InputPresentIntervalP50Ms, s.InputPresentIntervalP90Ms = pctMs(inputPresents, 50), pctMs(inputPresents, 90)
	s.FrameP50Ms, s.FrameP90Ms = pctMs(frames, 50), pctMs(frames, 90)
	s.FrameP99Ms, s.FrameMaxMs = pctMs(frames, 99), pctMs(frames, 100)
	for _, st := range stages {
		s.StageMediansMs[st.name] = pctMs(stageValues[st.name], 50)
		s.StageP90Ms[st.name] = pctMs(stageValues[st.name], 90)
	}
	if firstInput >= 0 {
		for i := firstInput; i < len(rows) && i < firstInput+5; i++ {
			s.FirstInputFramesTotalMs = append(s.FirstInputFramesTotalMs, ms(frames[i]))
			var b strings.Builder
			for _, st := range stages[:10] {
				a, c := rows[i][st.from], rows[i][st.to]
				if a > 0 && c >= a {
					fmt.Fprintf(&b, "%s=%.2f ", st.name, nsToMs(c-a))
				}
			}
			s.FirstInputFramesStages = append(s.FirstInputFramesStages, strings.TrimSpace(b.String()))
		}
	}
	if len(inputFrames) > 5 {
		s.SteadyInputFrameP50Ms = pctMs(inputFrames[5:], 50)
		s.SteadyInputFrameP90Ms = pctMs(inputFrames[5:], 90)
	}
	s.SettleFrameP50Ms, s.SettleFrameP90Ms = pctMs(settleFrames, 50), pctMs(settleFrames, 90)
	summarizeGestures(s, rows, frames, opts.AfterReleaseMs)
	if opts.Rows {
		s.Rows = make([]Row, len(rows))
		for i, r := range rows {
			row := Row{
				TMs: math.Round(nsToMs(r["IntendedVsync"]-t0)*100) / 100, VsyncID: r["FrameTimelineVsyncId"],
				Input: isInput(r), TotalMs: ms(frames[i]),
				UIMs:  spanMs(r, "IntendedVsync", "SyncQueued"),
				RTMs:  spanMs(r, "SyncStart", "FrameCompleted"),
				GPUMs: spanMs(r, "SwapBuffers", "GpuCompleted"),
			}
			if p := r["DisplayPresentTime"]; p > 0 {
				row.PresentMs = ms(nsToMs(p - t0))
			}
			s.Rows[i] = row
		}
	}
}

// spanMs is to-from in ms, nil when either stamp is missing or out of order.
func spanMs(r frameRow, from, to string) Ms {
	a, b := r[from], r[to]
	if a <= 0 || b <= 0 || b < a {
		return nil
	}
	return ms(nsToMs(b - a))
}

// summarizeGestures splits the input frames into gestures and measures the
// frames that start within afterMs of each gesture's last input frame (the
// release: a snap-back, a fly-off, a dock tap's commit).
func summarizeGestures(s *Summary, rows []frameRow, frames []float64, afterMs float64) {
	t0 := rows[0]["IntendedVsync"]
	var firsts, releaseFrames []float64
	var current *Gesture
	lastInput := -1
	closeGesture := func() {
		if current == nil {
			return
		}
		release := rows[lastInput]["IntendedVsync"]
		limit := release + int64(afterMs*1e6)
		var window []float64
		for j := lastInput + 1; j < len(rows) && rows[j]["IntendedVsync"] <= limit; j++ {
			ft := frames[j]
			if math.IsNaN(ft) {
				continue
			}
			window = append(window, ft)
			current.ReleaseWindow.Frames++
			if ft > over16Threshold {
				current.ReleaseWindow.Over16ms++
			}
		}
		current.ReleaseWindow.MaxFrameMs = pctMs(window, 100)
		releaseFrames = append(releaseFrames, window...)
		s.ReleaseOver16ms += current.ReleaseWindow.Over16ms
		s.Gestures = append(s.Gestures, *current)
		current = nil
	}
	for i, r := range rows {
		if !isInput(r) {
			continue
		}
		if current != nil && nsToMs(r["IntendedVsync"]-rows[lastInput]["IntendedVsync"]) > gestureGapMs {
			closeGesture()
		}
		if current == nil {
			current = &Gesture{
				StartMs:           math.Round(nsToMs(r["IntendedVsync"]-t0)*100) / 100,
				FirstInputFrameMs: ms(frames[i]),
			}
			if !math.IsNaN(frames[i]) {
				firsts = append(firsts, frames[i])
			}
		}
		current.InputFrames++
		lastInput = i
	}
	closeGesture()
	s.FirstInputFrameMaxMs = pctMs(firsts, 100)
	s.ReleaseMaxFrameMs = pctMs(releaseFrames, 100)
}
