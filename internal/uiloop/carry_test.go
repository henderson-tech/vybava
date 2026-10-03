package uiloop

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// writePNG writes a w×h grey PNG whose first `changed` pixels are white.
func writePNG(t *testing.T, file string, w, h, changed int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := range w * h {
		c := color.RGBA{R: 128, G: 128, B: 128, A: 255}
		if i < changed {
			c = color.RGBA{R: 255, G: 255, B: 255, A: 255}
		}
		img.Set(i%w, i/w, c)
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	writeFile(t, file, b.String())
}

// carryShots captures pass with provenance: tasks and task-detail, each one
// shot whose PNG is the same 10×10 grey.
func carryShots(t *testing.T, tool *Tool, pass int) {
	t.Helper()
	if err := tool.captureProvenance(pass, false); err != nil {
		t.Fatal(err)
	}
	writePass(t, tool, pass, []shot{
		{order: 1, id: "tasks", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1},
		{order: 2, id: "task-detail", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1},
	})
	for _, id := range []string{"tasks", "task-detail"} {
		writePNG(t, filepath.Join(tool.passAbs(pass), "shots", id, "phone.light.png"), 10, 10, 0)
	}
}

// carryPasses is a reviewed pass 1 — tasks with a met item, task-detail with
// an open one, both judged — and an unreviewed pass 2 with the same shots
// (carryShots).
func carryPasses(t *testing.T) *Tool {
	t.Helper()
	tool := newTool(t, testConfig())
	evidenceRepo(t, tool)
	carryShots(t, tool, 1)
	carryShots(t, tool, 2)
	res, err := tool.Batches(BatchesOptions{Pass: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range res.Data.(BatchesData).Batches {
		v2Raw(t, tool, 1, b.ID, b.ID, b.Digests, b.Screens...)
	}
	writeFile(t, filepath.Join(tool.reviewDir(1), "backlog.json"), `{"v":1,"pass":1,"reviewed":["task-detail","tasks"],"findings":[
		{"key":"tasks-met","screen":"tasks","area":"tasks","severity":"polish","status":"met","title":"M","files":[],"acceptance":"x"},
		{"key":"detail-open","screen":"task-detail","area":"tasks","severity":"broken","status":"open","title":"O","files":["d.ts"],"acceptance":"x"}]}`)
	basis, err := tool.reviewBasis(1)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tool.reviewDir(1), "basis.json"), `{"basis":"`+basis+`"}`)
	return tool
}

// A screen whose pixels did not move and that no open item names carries the
// previous review: it leaves the batches, state counts it, and merge-review
// copies its items with carriedFrom and counts it reviewed. A screen with an
// open item is batched however still its pixels are.
func TestAnUnmovedScreenWithoutOpenItemsCarriesItsReview(t *testing.T) {
	tool := carryPasses(t)
	res, err := tool.Batches(BatchesOptions{Pass: 2})
	if err != nil {
		t.Fatal(err)
	}
	b := res.Data.(BatchesData)
	if !slices.Equal(b.Carried, []Carried{{Screen: "tasks", From: 1}}) || len(b.Batches) != 1 || !slices.Equal(b.Batches[0].Screens, []string{"task-detail"}) || b.Screens != 1 {
		t.Fatalf("batches of pass 2: %+v", b)
	}
	v2Raw(t, tool, 2, "tasks-1", "tasks-1", b.Batches[0].Digests, "task-detail")
	state, err := tool.State(StateOptions{Pass: 2})
	if err != nil {
		t.Fatal(err)
	}
	if r := state.Data.(StateData).Review; r.Carried != 1 || r.CarriedFrom == nil || *r.CarriedFrom != 1 || !slices.Equal(r.Done, []string{"tasks-1"}) {
		t.Errorf("state.review: %+v", r)
	}
	if _, err := tool.MergeReview(MergeReviewOptions{Pass: 2}); err != nil {
		t.Fatal(err)
	}
	draft, err := LoadBacklog(filepath.Join(tool.reviewDir(2), "backlog.draft.json"))
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]Finding{}
	for _, f := range draft.Findings {
		byKey[f.Key] = f
	}
	if m := byKey["tasks-met"]; m.Status != "met" || m.CarriedFrom != 1 || byKey["detail-open"].CarriedFrom != 0 || !slices.Contains(draft.Reviewed, "tasks") {
		t.Errorf("draft: %+v", draft)
	}
}

// A pass whose every screen carried has no batch and no raw file to wait
// for: merge-review drafts its backlog from the carried review alone, and
// counts the carried item apart from this pass's verdicts.
func TestAPassWhoseEveryScreenCarriedMergesWithoutARaw(t *testing.T) {
	tool := carryPasses(t)
	writeFile(t, filepath.Join(tool.reviewDir(1), "backlog.json"), `{"v":1,"pass":1,"reviewed":["task-detail","tasks"],"findings":[
		{"key":"detail-met","screen":"task-detail","area":"tasks","severity":"broken","status":"met","title":"O","files":["d.ts"],"acceptance":"x"}]}`)
	res, err := tool.Batches(BatchesOptions{Pass: 2})
	if err != nil {
		t.Fatal(err)
	}
	if b := res.Data.(BatchesData); len(b.Batches) != 0 || len(b.Carried) != 2 {
		t.Fatalf("batches of pass 2: %+v", b)
	}
	merged, err := tool.MergeReview(MergeReviewOptions{Pass: 2})
	if err != nil {
		t.Fatal(err)
	}
	if m := merged.Data.(MergeReviewData); m.Reviewed != 2 || m.Findings != 1 || m.Carried != 1 || len(m.ByStatus) != 0 || m.Open != 0 || len(m.Left) != 0 || len(merged.Diagnostics) != 0 {
		t.Errorf("merge of a carried pass: %+v %+v", m, merged.Diagnostics)
	}
}

// A shot whose PNG changed carries only while the share of differing pixels
// stays within uiLoop.review.carryTolerance; a size change always moves it.
func TestCarryWeighsChangedPixelsAgainstTheTolerance(t *testing.T) {
	tool := carryPasses(t)
	shot := filepath.Join(tool.passAbs(2), "shots", "tasks", "phone.light.png")
	for _, c := range []struct {
		name      string
		h, differ int
		tolerance float64
		carried   bool
	}{
		{"1 of 100 pixels within 5%", 10, 1, 0.05, true},
		{"1 of 100 pixels above the 0.1% default", 10, 1, -1, false},
		{"a taller shot", 11, 0, 0.5, false},
	} {
		writePNG(t, shot, 10, c.h, c.differ)
		tool.Config.Review.CarryTolerance = nil
		if c.tolerance >= 0 {
			tool.Config.Review.CarryTolerance = &c.tolerance
		}
		res, err := tool.Batches(BatchesOptions{Pass: 2})
		if err != nil {
			t.Fatal(err)
		}
		var file BatchesFile
		if _, err := readJSON(filepath.Join(tool.reviewDir(2), "batches.json"), &file); err != nil {
			t.Fatal(err)
		}
		if got := len(res.Data.(BatchesData).Carried) == 1; got != c.carried || file.V != 2 || len(file.Carried) == 1 != c.carried {
			t.Errorf("%s: carried %+v, file %+v", c.name, res.Data.(BatchesData).Carried, file)
		}
	}
}

// Only pass N-1's review of the same evidence carries: a pass captured after
// an unreviewed one, or a screen whose manifest entry changed since (a known
// issue added), is batched however still its pixels are.
func TestOnlyThePassBeforeCarriesTheSameEntry(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(t *testing.T, tool *Tool) int
	}{
		{"a pass after an unreviewed one", func(t *testing.T, tool *Tool) int {
			carryShots(t, tool, 3)
			return 3
		}},
		{"a known issue added since", func(t *testing.T, tool *Tool) int {
			file := filepath.Join(tool.passAbs(2), "shots", "tasks", "phone.light.json")
			var rec map[string]any
			if _, err := readJSON(file, &rec); err != nil {
				t.Fatal(err)
			}
			rec["knownIssues"] = []string{"the header wraps at 390 px"}
			body, err := json.Marshal(rec)
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, file, string(body))
			return 2
		}},
	} {
		tool := carryPasses(t)
		res, err := tool.Batches(BatchesOptions{Pass: c.setup(t, tool)})
		if err != nil {
			t.Fatal(err)
		}
		if b := res.Data.(BatchesData); len(b.Carried) != 0 || b.Screens != 2 {
			t.Errorf("%s: %+v", c.name, b)
		}
	}
}

// A carried screen retaken after batches planned the carry reopens without
// another batches run: state stops counting it and leaves its area
// unreviewed, merge-review copies nothing onto the new pixels, and the next
// batches run puts it back in a batch.
func TestARetakenCarriedScreenReopens(t *testing.T) {
	tool := carryPasses(t)
	res, err := tool.Batches(BatchesOptions{Pass: 2})
	if err != nil {
		t.Fatal(err)
	}
	v2Raw(t, tool, 2, "tasks-1", "tasks-1", res.Data.(BatchesData).Batches[0].Digests, "task-detail")
	// A --resume retake rewrites the carried screen's PNG.
	writePNG(t, filepath.Join(tool.passAbs(2), "shots", "tasks", "phone.light.png"), 10, 10, 50)
	state, err := tool.State(StateOptions{Pass: 2})
	if err != nil {
		t.Fatal(err)
	}
	if r := state.Data.(StateData).Review; r.Carried != 0 || r.CarriedFrom != nil || len(r.ReviewedAreas) != 0 {
		t.Errorf("state.review after the retake: %+v", r)
	}
	merged, err := tool.MergeReview(MergeReviewOptions{Pass: 2})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := LoadBacklog(filepath.Join(tool.reviewDir(2), "backlog.draft.json"))
	if err != nil {
		t.Fatal(err)
	}
	if m := merged.Data.(MergeReviewData); m.Carried != 0 || !slices.Equal(m.Unreviewed, []string{"tasks"}) || !slices.Equal(draft.Reviewed, []string{"task-detail"}) {
		t.Errorf("merge after the retake: %+v, draft %+v", m, draft)
	}
	if res, err = tool.Batches(BatchesOptions{Pass: 2}); err != nil {
		t.Fatal(err)
	}
	if b := res.Data.(BatchesData); len(b.Carried) != 0 || !slices.Equal(b.Batches[0].Screens, []string{"task-detail", "tasks"}) || !slices.Equal(b.Left, []string{"tasks-1"}) {
		t.Errorf("batches after the retake: %+v", b)
	}
}

// A review that started without v2 batches (a raw exists; batches.json is v1
// or missing) keeps v1 batches and carries nothing: a carry would re-chunk
// the batches its v1 raws were judged against.
func TestAReviewStartedOnV1BatchesKeepsThem(t *testing.T) {
	tool := carryPasses(t)
	records, err := LoadRecords(tool.passAbs(2))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(tool.reviewDir(2), "batches.json")
	if err := writeJSON(file, BatchesFile{V: 1, Pass: 2, Size: DefaultBatchSize, Batches: ComputeBatches(passScreens(records), tool.Config.Areas, DefaultBatchSize)}); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tool.reviewDir(2), "raw", "tasks-1.json"), `{"batch":"tasks-1","basis":"stale","screensRead":["task-detail"]}`)
	for _, name := range []string{"a v1 batches.json", "no batches.json"} {
		if name == "no batches.json" {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}
		res, err := tool.Batches(BatchesOptions{Pass: 2})
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) || len(res.Data.(BatchesData).Carried) != 0 {
			t.Errorf("%s: wrote %s, carried %+v", name, got, res.Data.(BatchesData).Carried)
		}
	}
}
