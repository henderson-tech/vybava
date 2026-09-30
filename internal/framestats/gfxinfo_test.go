package framestats

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/runx"
)

const s20Drag = "testdata/s20-drag.framestats.txt"

func val(t *testing.T, name string, got Ms) float64 {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = null, want a value", name)
	}
	return *got
}

// The fixture is the first 40 rows of a hand-driven S20 drag (2026-09-30);
// the expected numbers are the original fsparse tool's output on it, so the
// port keeps its metric definitions.
func TestParseMatchesTheFsparseDefinitions(t *testing.T) {
	s, err := ParseFiles([]string{s20Drag}, ParseOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Frames != 40 || s.InputFrames != 19 || s.Over8ms != 40 || s.Over16ms != 40 || s.DeadlineMissed != 40 {
		t.Fatalf("counts = frames %d input %d over8 %d over16 %d missed %d", s.Frames, s.InputFrames, s.Over8ms, s.Over16ms, s.DeadlineMissed)
	}
	want := map[string]struct {
		got  Ms
		want float64
	}{
		"cadenceMedianMs":      {s.CadenceMedianMs, 16.6},
		"cadenceP90Ms":         {s.CadenceP90Ms, 33.21},
		"inputCadenceMedianMs": {s.InputCadenceMedianMs, 24.9},
		"frameP50Ms":           {s.FrameP50Ms, 43.7},
		"frameP90Ms":           {s.FrameP90Ms, 57.04},
		"frameP99Ms":           {s.FrameP99Ms, 99.77},
		"frameMaxMs":           {s.FrameMaxMs, 103.77},
		"gpu median":           {s.StageMediansMs["gpu"], 7.07},
		"issueCommands p90":    {s.StageP90Ms["issueCommands"], 17.87},
		"first input frame":    {s.FirstInputFramesTotalMs[0], 103.77},
	}
	for name, c := range want {
		if got := val(t, name, c.got); got != c.want {
			t.Errorf("%s = %v, want %v", name, got, c.want)
		}
	}
}

const header = "Flags,IntendedVsync,InputEventId,SyncQueued,SyncStart,SwapBuffers,FrameCompleted,GpuCompleted,DisplayPresentTime,"

// row builds a frame starting at startMs that takes totalMs, presented one
// fixed latency after its vsync (so present intervals equal vsync deltas).
func row(flags int, startMs float64, input bool, totalMs float64) string {
	iv := int64(startMs * 1e6)
	end := iv + int64(totalMs*1e6)
	in := "0"
	if input {
		in = "7"
	}
	return strings.Join([]string{
		itoa(int64(flags)), itoa(iv), in, itoa(iv + 1e6), itoa(iv + 2e6), itoa(iv + 3e6), itoa(end), itoa(end), itoa(iv + 50e6),
	}, ",") + ","
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func dump(rows ...string) string {
	return "---PROFILEDATA---\n" + header + "\n" + strings.Join(rows, "\n") + "\n---PROFILEDATA---\n"
}

func parse(t *testing.T, opts ParseOptions, dumps ...string) Summary {
	t.Helper()
	inputs := make([]Input, len(dumps))
	for i, d := range dumps {
		inputs[i] = Input{Name: "dump", Reader: strings.NewReader(d)}
	}
	s, err := Parse(inputs, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestParseDedupesAcrossDumpsAndSkipsFlaggedRows(t *testing.T) {
	a := dump(row(0, 0, false, 5), row(0, 8.33, false, 5))
	b := dump(row(0, 8.33, false, 5), row(1, 16.67, false, 40), row(0, 25, false, 5))
	s := parse(t, ParseOptions{}, a, b)
	if s.RowsTotal != 4 || s.RowsFlagged != 1 || s.Frames != 3 {
		t.Fatalf("rows = total %d flagged %d frames %d, want 4 1 3", s.RowsTotal, s.RowsFlagged, s.Frames)
	}
	if s.Over16ms != 0 {
		t.Errorf("the flagged 40 ms frame was measured: over16ms = %d", s.Over16ms)
	}
}

func TestParseSplitsGesturesAndMeasuresTheReleaseWindow(t *testing.T) {
	s := parse(t, ParseOptions{AfterReleaseMs: 250},
		dump(
			// gesture 1: first input frame 12 ms, release at 16.66
			row(0, 0, true, 12), row(0, 8.33, true, 6), row(0, 16.66, true, 6),
			// its release window: one 20 ms frame, one 6 ms frame, then idle
			row(0, 24.99, false, 20), row(0, 33.32, false, 6), row(0, 400, false, 30),
			// gesture 2, 600 ms later: a tap, then a clean window
			row(0, 1000, true, 18), row(0, 1008.33, false, 6),
		))
	if len(s.Gestures) != 2 {
		t.Fatalf("gestures = %d, want 2", len(s.Gestures))
	}
	g1, g2 := s.Gestures[0], s.Gestures[1]
	if g1.InputFrames != 3 || val(t, "g1 first", g1.FirstInputFrameMs) != 12 {
		t.Errorf("gesture 1 = %+v", g1)
	}
	if g1.ReleaseWindow.Frames != 2 || g1.ReleaseWindow.Over16ms != 1 || val(t, "g1 max", g1.ReleaseWindow.MaxFrameMs) != 20 {
		t.Errorf("gesture 1 release window = %+v (the 400 ms frame is outside 250 ms)", g1.ReleaseWindow)
	}
	if g2.ReleaseWindow.Frames != 1 || g2.ReleaseWindow.Over16ms != 0 {
		t.Errorf("gesture 2 release window = %+v", g2.ReleaseWindow)
	}
	if s.ReleaseOver16ms != 1 || val(t, "first input max", s.FirstInputFrameMaxMs) != 18 {
		t.Errorf("aggregates = releaseOver16ms %d firstInputFrameMaxMs %v", s.ReleaseOver16ms, *s.FirstInputFrameMaxMs)
	}
	if val(t, "input present p50", s.InputPresentIntervalP50Ms) != 8.33 {
		t.Errorf("inputPresentIntervalP50Ms = %v, want 8.33", *s.InputPresentIntervalP50Ms)
	}
}

func TestParseReportsMetricsWithoutSamplesAsNull(t *testing.T) {
	s := parse(t, ParseOptions{}, dump(row(1, 0, true, 5), row(2, 8.33, true, 5)))
	if s.Frames != 0 || s.RowsFlagged != 2 {
		t.Fatalf("frames %d flagged %d, want 0 2", s.Frames, s.RowsFlagged)
	}
	if s.CadenceMedianMs != nil || s.FrameP50Ms != nil || s.InputPresentIntervalP50Ms != nil || s.FirstInputFrameMaxMs != nil {
		t.Error("an empty run must report null metrics, never a number a budget could pass on")
	}
}

func TestParseRefusesInputWithoutProfileData(t *testing.T) {
	_, err := Parse([]Input{{Name: "gfxinfo.txt", Reader: strings.NewReader("Total frames rendered: 12\n")}}, ParseOptions{})
	var d runx.DiagError
	if !errors.As(err, &d) || d.Diag.Code != DiagNotFramestats {
		t.Fatalf("err = %v, want %s", err, DiagNotFramestats)
	}
}
