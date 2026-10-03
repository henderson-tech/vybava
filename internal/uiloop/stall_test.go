package uiloop

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

// stallPass is one published area of five screens, s0…s4: one batch, tasks-1.
func stallPass(t *testing.T, tool *Tool) {
	t.Helper()
	var shots []shot
	for i := range 5 {
		shots = append(shots, shot{order: i, id: fmt.Sprintf("s%d", i), area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: i + 1})
	}
	writePass(t, tool, 1, shots)
	writeFile(t, filepath.Join(tool.passAbs(1), "publish", "index.json"), `{"pass":1,"sets":[{"key":"ui-polish-tasks","area":"tasks","status":"pushed","url":"u1"}]}`)
}

func batchesOf(t *testing.T, tool *Tool, o BatchesOptions) BatchesData {
	t.Helper()
	res, err := tool.Batches(o)
	if err != nil {
		t.Fatal(err)
	}
	return res.Data.(BatchesData)
}

func splitOf(t *testing.T, tool *Tool, o BatchesOptions) SplitData {
	t.Helper()
	res, err := tool.Batches(o)
	if err != nil {
		t.Fatal(err)
	}
	return res.Data.(SplitData)
}

func batchIDs(bs []Batch) []string {
	out := []string{}
	for _, b := range bs {
		out = append(out, b.ID)
	}
	return out
}

// A split halves a batch in batch order, each part with its own screens'
// digests; splitting it again answers the same parts and changes nothing, a
// part splits again but one screen never does, and a re-plan keeps every
// part while the batches and left list the parts, never a split batch.
func TestASplitHalvesTheBatchOnceAndARePlanKeepsIt(t *testing.T) {
	tool := newTool(t, testConfig())
	stallPass(t, tool)
	d := batchesOf(t, tool, BatchesOptions{}).Batches[0].Digests
	want := []Batch{
		{ID: "tasks-1.1", Area: "tasks", Screens: []string{"s0", "s1", "s2"}, Digests: map[string]string{"s0": d["s0"], "s1": d["s1"], "s2": d["s2"]}},
		{ID: "tasks-1.2", Area: "tasks", Screens: []string{"s3", "s4"}, Digests: map[string]string{"s3": d["s3"], "s4": d["s4"]}},
	}
	if s := splitOf(t, tool, BatchesOptions{Split: "tasks-1"}); s.Batch != "tasks-1" || !reflect.DeepEqual(s.Parts, want) || s.Claimed != nil {
		t.Fatalf("split: %+v", s)
	}
	file := filepath.Join(tool.reviewDir(1), "batches.json")
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if s := splitOf(t, tool, BatchesOptions{Split: "tasks-1"}); !reflect.DeepEqual(s.Parts, want) {
		t.Errorf("split again: %+v", s.Parts)
	}
	if after, err := os.ReadFile(file); err != nil || string(after) != string(before) {
		t.Errorf("a second split rewrote batches.json: %v", err)
	}
	if s := splitOf(t, tool, BatchesOptions{Split: "tasks-1.2"}); !slices.Equal(batchIDs(s.Parts), []string{"tasks-1.2.1", "tasks-1.2.2"}) || !slices.Equal(s.Parts[1].Screens, []string{"s4"}) {
		t.Errorf("a part split again: %+v", s.Parts)
	}
	if _, err := tool.Batches(BatchesOptions{Split: "tasks-1.2.2"}); diagCode(err) != DiagSelectionInvalid {
		t.Errorf("one screen split: %v", err)
	}
	b := batchesOf(t, tool, BatchesOptions{})
	leaves := []string{"tasks-1.1", "tasks-1.2.1", "tasks-1.2.2"}
	if !slices.Equal(batchIDs(b.Batches), leaves) || !slices.Equal(b.Left, leaves) || b.Screens != 5 {
		t.Errorf("re-planned batches: %+v", b)
	}
	var f BatchesFile
	if _, err := readJSON(file, &f); err != nil || !slices.Equal(batchIDs(f.Batches), []string{"tasks-1", "tasks-1.1", "tasks-1.2", "tasks-1.2.1", "tasks-1.2.2"}) || !slices.Equal(f.Batches[2].Parts, []string{"tasks-1.2.1", "tasks-1.2.2"}) {
		t.Errorf("batches.json after the re-plan: %+v %v", f, err)
	}
	res, err := tool.State(StateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Data.(StateData).Review; r.Planned != 3 || !slices.Equal(r.Left, leaves) {
		t.Errorf("state.review: %+v", r)
	}
}

// A split batch is done when its parts are, each part judged by its raw's
// per-screen digests like any batch, and merge-review folds the parts' raws.
func TestASplitBatchIsDoneWhenAllItsPartsAre(t *testing.T) {
	tool := newTool(t, testConfig())
	evidenceRepo(t, tool)
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	stallPass(t, tool)
	batchesOf(t, tool, BatchesOptions{})
	parts := splitOf(t, tool, BatchesOptions{Split: "tasks-1"}).Parts
	v2Raw(t, tool, 1, "tasks-1.1", "tasks-1.1", parts[0].Digests, parts[0].Screens...)
	if done, err := tool.rawBatchIDs(1); err != nil || !done["tasks-1.1"] || done["tasks-1.2"] || done["tasks-1"] {
		t.Fatalf("one part reviewed: %v %v", done, err)
	}
	if b := batchesOf(t, tool, BatchesOptions{}); !slices.Equal(b.Done, []string{"tasks-1.1"}) || !slices.Equal(b.Left, []string{"tasks-1.2"}) {
		t.Errorf("one part reviewed: done %v, left %v", b.Done, b.Left)
	}
	v2Raw(t, tool, 1, "tasks-1.2", "tasks-1.2", parts[1].Digests, parts[1].Screens...)
	if done, err := tool.rawBatchIDs(1); err != nil || !done["tasks-1"] {
		t.Fatalf("both parts reviewed: %v %v", done, err)
	}
	merged, err := tool.MergeReview(MergeReviewOptions{Pass: 1})
	if err != nil {
		t.Fatal(err)
	}
	if m := merged.Data.(MergeReviewData); m.Raw != 2 || m.Reviewed != 5 || m.Findings != 5 || len(m.Left) != 0 || len(merged.Diagnostics) != 0 {
		t.Errorf("merge of the parts: %+v %+v", m, merged.Diagnostics)
	}
}

// A split releases the batch's claim, whoever holds it, and its parts are
// claimed like any batch: by --split's --claim, then by a plain --claim.
func TestAPartIsClaimableAndASplitReleasesTheBatchClaim(t *testing.T) {
	tool := newTool(t, testConfig())
	stallPass(t, tool)
	if c := batchesOf(t, tool, BatchesOptions{Claim: 1, Owner: "run-a"}).Claimed; !slices.Equal(batchIDs(*c), []string{"tasks-1"}) {
		t.Fatalf("run-a claims: %+v", *c)
	}
	s := splitOf(t, tool, BatchesOptions{Split: "tasks-1", Claim: 1, Owner: "run-b"})
	if h, err := tool.readLease(1, batchLease("tasks-1")); err != nil || h != nil {
		t.Errorf("the split batch is still claimed: %+v %v", h, err)
	}
	if s.Claimed == nil || !slices.Equal(batchIDs(*s.Claimed), []string{"tasks-1.1"}) {
		t.Errorf("run-b claims a part: %+v", s.Claimed)
	}
	if c := batchesOf(t, tool, BatchesOptions{Claim: 2, Owner: "run-c"}).Claimed; !slices.Equal(batchIDs(*c), []string{"tasks-1.2"}) {
		t.Errorf("run-c claims the other part: %+v", *c)
	}
}

// A blocked part is neither done nor left, so the area is reviewed and the
// review goes on to synthesize: merge-review lists the part's unread screens
// as unreviewed with the reason and warns nothing incomplete. Only a part is
// blocked, never a split batch, and never without a reason.
func TestABlockedPartsScreensAreUnreviewedAndTheReviewSynthesizes(t *testing.T) {
	tool := newTool(t, testConfig())
	stallPass(t, tool)
	batchesOf(t, tool, BatchesOptions{})
	splitOf(t, tool, BatchesOptions{Split: "tasks-1", Claim: 2, Owner: "run-a"})
	writeFile(t, filepath.Join(tool.reviewDir(1), "raw", "tasks-1.1.json"), `{"batch":"tasks-1.1","area":"tasks","findings":[]}`)
	for _, o := range []BatchesOptions{{Block: "tasks-1.2"}, {Block: "tasks-1", Reason: "x"}} {
		if _, err := tool.Batches(o); diagCode(err) != DiagSelectionInvalid {
			t.Errorf("block %+v: %v", o, err)
		}
	}
	res, err := tool.Batches(BatchesOptions{Block: "tasks-1.2", Reason: "reviewer stalled twice"})
	if err != nil || res.Data != (BlockedBatch{Batch: "tasks-1.2", Reason: "reviewer stalled twice"}) {
		t.Fatalf("block: %+v %v", res.Data, err)
	}
	if h, err := tool.readLease(1, batchLease("tasks-1.2")); err != nil || h != nil {
		t.Errorf("the blocked part is still claimed: %+v %v", h, err)
	}
	if b := batchesOf(t, tool, BatchesOptions{Claim: 1, Owner: "run-b"}); len(b.Left) != 0 || !b.Batches[1].Blocked || len(*b.Claimed) != 0 {
		t.Errorf("batches with a blocked part: %+v", b)
	}
	st, err := tool.State(StateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := st.Data.(StateData)
	if r := s.Review; len(r.Left) != 0 || !slices.Equal(r.Done, []string{"tasks-1.1"}) || !slices.Equal(r.ReviewedAreas, []string{"tasks"}) ||
		!slices.Equal(r.Blocked, []BlockedBatch{{Batch: "tasks-1.2", Reason: "reviewer stalled twice"}}) || r.Stalls == nil {
		t.Errorf("state.review: %+v", r)
	}
	if s.Next.Stage != "review" || s.Next.Reason != "no backlog yet" || s.Next.Parallel != 1 {
		t.Errorf("next: %+v", s.Next)
	}
	merged, err := tool.MergeReview(MergeReviewOptions{})
	if err != nil {
		t.Fatal(err)
	}
	m := merged.Data.(MergeReviewData)
	if len(m.Left) != 0 || len(merged.Diagnostics) != 0 || m.Reviewed != 3 ||
		!slices.Equal(m.Unreviewed, []string{"s3 (stalled: reviewer stalled twice)", "s4 (stalled: reviewer stalled twice)"}) {
		t.Errorf("merge with a blocked part: %+v %+v", m, merged.Diagnostics)
	}
}

// --stall counts a batch's stalls in review/attempts/<id>.json, with when,
// and state shows the count; an unknown batch or two actions are refused.
func TestAStallIsCountedAndShownInState(t *testing.T) {
	tool := newTool(t, testConfig())
	stallPass(t, tool)
	batchesOf(t, tool, BatchesOptions{})
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	tool.Now = func() time.Time { return now }
	for want := 1; want <= 2; want++ {
		res, err := tool.Batches(BatchesOptions{Stall: "tasks-1"})
		if err != nil || res.Data != (StallData{Batch: "tasks-1", Stalls: want}) {
			t.Fatalf("stall %d: %+v %v", want, res.Data, err)
		}
		now = now.Add(time.Hour)
	}
	var a Attempts
	if _, err := readJSON(filepath.Join(tool.reviewDir(1), "attempts", "tasks-1.json"), &a); err != nil ||
		!reflect.DeepEqual(a, Attempts{Stalls: 2, At: []string{"2026-10-03T09:00:00Z", "2026-10-03T10:00:00Z"}}) {
		t.Errorf("attempts: %+v %v", a, err)
	}
	res, err := tool.State(StateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if r := res.Data.(StateData).Review; !reflect.DeepEqual(r.Stalls, map[string]int{"tasks-1": 2}) {
		t.Errorf("state.review.stalls: %v", r.Stalls)
	}
	for _, o := range []BatchesOptions{{Stall: "tasks-9"}, {Stall: "tasks-1", Split: "tasks-1"}, {Stall: "tasks-1", Claim: 1, Owner: "run-a"}} {
		if _, err := tool.Batches(o); diagCode(err) != DiagSelectionInvalid {
			t.Errorf("stall %+v: %v", o, err)
		}
	}
}

// A blocked batch ends its pass's review as well as a complete one does: its
// screens are unreviewed there, so they never carry, and the rest carry on.
func TestABlockedBatchLeavesTheCarryOn(t *testing.T) {
	tool := carryPasses(t)
	writePass(t, tool, 1, []shot{{order: 3, id: "users", area: "admin", vp: "phone", theme: "light", status: "ok", bytes: 1}})
	batchesOf(t, tool, BatchesOptions{Pass: 1})
	if _, err := tool.Batches(BatchesOptions{Pass: 1, Block: "admin-1", Reason: "reviewer stalled twice"}); err != nil {
		t.Fatal(err)
	}
	basis, err := tool.reviewBasis(1)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tool.reviewDir(1), "basis.json"), `{"basis":"`+basis+`"}`)
	if b := batchesOf(t, tool, BatchesOptions{Pass: 2}); !slices.Equal(b.Carried, []Carried{{Screen: "tasks", From: 1}}) {
		t.Errorf("pass 2 after a blocked pass-1 batch: %+v", b)
	}
}
