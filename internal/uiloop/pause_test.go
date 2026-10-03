package uiloop

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pausedLoop is nine shot screens in one published area: state routes a
// review of nine batches of one.
func pausedLoop(t *testing.T) *Tool {
	t.Helper()
	cfg := testConfig()
	cfg.Apps["portal"] = App{BaseURL: "http://127.0.0.1:1", Viewports: []string{"phone"}, Themes: []string{"light"}}
	tool := newTool(t, cfg)
	var shots []shot
	for i := range 9 {
		shots = append(shots, shot{order: i, id: fmt.Sprintf("s%d", i), area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1})
	}
	writePass(t, tool, 1, shots)
	writeFile(t, filepath.Join(tool.passAbs(1), "publish", "index.json"), `{"pass":1,"sets":[{"key":"ui-polish-tasks","area":"tasks","status":"pushed","url":"u1"}]}`)
	return tool
}

func locksOf(t *testing.T, tool *Tool) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(tool.passAbs(1), "locks", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// A pause routes every stage to paused and refuses the verbs that start
// work, while the rest keep working and nothing on disk moves; resume frees
// the drained run's claims, so state routes where the loop stopped.
func TestAPauseDrainsTheLoopAndResumeRoutesWhereItStopped(t *testing.T) {
	tool := pausedLoop(t)
	tool.Now = func() time.Time { return time.Date(2026, 10, 3, 21, 0, 0, 0, time.UTC) }
	state := func() (StateData, []string) {
		t.Helper()
		res, err := tool.State(StateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return res.Data.(StateData), res.Next
	}
	if _, err := tool.Batches(BatchesOptions{Size: 1}); err != nil {
		t.Fatal(err)
	}
	before, _ := state()
	if _, err := tool.Batches(BatchesOptions{Claim: 4, Owner: "run-a"}); err != nil {
		t.Fatal(err)
	}
	claims := locksOf(t, tool)
	res, err := tool.Pause(PauseOptions{Reason: "fixing state speed", By: "lukas"})
	if err != nil || !res.Data.(PauseData).Changed {
		t.Fatalf("pause: %+v %v", res, err)
	}
	if again, err := tool.Pause(PauseOptions{Reason: "another"}); err != nil || again.Data.(PauseData).Changed || again.Data.(PauseData).Paused.Reason != "fixing state speed" {
		t.Fatalf("a second pause replaced the first: %+v %v", again, err)
	}
	if !slices.Equal(locksOf(t, tool), claims) {
		t.Fatalf("pause touched the claims: %v, was %v", locksOf(t, tool), claims)
	}
	s, next := state()
	want := NextStage{Stage: "paused", Reason: "paused by lukas at 2026-10-03T21:00:00Z: fixing state speed"}
	if !reflect.DeepEqual(s.Next, want) || s.Paused == nil || s.Paused.Pass != 1 || !slices.Equal(next, []string{resumeCommand}) {
		t.Fatalf("paused state: next %+v, paused %+v, commands %v", s.Next, s.Paused, next)
	}
	for verb, start := range map[string]func() error{
		"run":             func() error { _, err := tool.Run(context.Background(), RunOptions{}); return err },
		"batches --claim": func() error { _, err := tool.Batches(BatchesOptions{Claim: 1, Owner: "run-b"}); return err },
		"lanes":           func() error { _, err := tool.Lanes(LanesOptions{}); return err },
	} {
		if err := start(); diagCode(err) != DiagPaused {
			t.Errorf("%s while paused: %v", verb, err)
		}
	}
	if _, err := tool.Batches(BatchesOptions{}); err != nil {
		t.Errorf("batches without --claim while paused: %v", err)
	}
	if _, err := tool.Checkpoints(CheckpointsOptions{}); err != nil {
		t.Errorf("checkpoints while paused: %v", err)
	}
	doc, err := tool.Doctor(context.Background(), DoctorOptions{For: "review"})
	if err != nil {
		t.Fatal(err)
	}
	if i := slices.IndexFunc(doc.Data.(DoctorData).Checks, func(c DoctorCheck) bool { return c.ID == "paused" }); i < 0 || doc.Data.(DoctorData).Checks[i].Status != DoctorWarn {
		t.Errorf("doctor's paused row: %+v", doc.Data.(DoctorData).Checks)
	}

	resumed, err := tool.Resume()
	if err != nil || len(resumed.Data.(PauseData).Released) != 4 {
		t.Fatalf("resume released %+v: %v", resumed.Data, err)
	}
	if after, _ := state(); after.Paused != nil || !reflect.DeepEqual(after.Next, before.Next) {
		t.Fatalf("resumed next %+v, before the pause %+v", after.Next, before.Next)
	}
	if again, err := tool.Resume(); err != nil || again.Data.(PauseData).Changed {
		t.Fatalf("a resume without a pause: %+v %v", again, err)
	}
}

// resume frees only owner leases, and only out of a pause: a live run's
// claims and every process lease stay.
func TestResumeFreesOwnerLeasesOnlyOutOfAPause(t *testing.T) {
	tool := pausedLoop(t)
	writeFile(t, filepath.Join(tool.passAbs(1), "review", "raw", "tasks-1.json"), `{"batch":"tasks-1","area":"tasks","findings":[]}`)
	if _, err := tool.MergeReview(MergeReviewOptions{Owner: "run-a"}); err != nil {
		t.Fatal(err)
	}
	host, err := leaseHost()
	if err != nil {
		t.Fatal(err)
	}
	publish := Lease{Owner: "ui-loop publish", Host: host, PID: os.Getpid(), StartedAt: time.Now().UTC().Format(time.RFC3339), TTL: "1h"}
	if err := createLease(tool.leaseFile(1, leasePublish), publish); err != nil {
		t.Fatal(err)
	}
	held := locksOf(t, tool)
	if res, err := tool.Resume(); err != nil || res.Data.(PauseData).Changed || !slices.Equal(locksOf(t, tool), held) {
		t.Fatalf("resume without a pause freed %v: %+v %v", held, res, err)
	}
	if _, err := tool.Pause(PauseOptions{}); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Resume()
	if err != nil || !slices.Equal(res.Data.(PauseData).Released, []string{".ui-loop/pass-1/locks/synth.json"}) {
		t.Fatalf("resume released %+v: %v", res.Data, err)
	}
	if left := locksOf(t, tool); len(left) != 1 || filepath.Base(left[0]) != "publish.json" {
		t.Fatalf("leases after resume: %v", left)
	}
}

// A pause holds where the loop is fragile: of concurrent pauses one writes,
// a split under a pause still splits (only its claim is refused), and a pass
// state cannot read still routes paused.
func TestAPauseHoldsUnderRacesSplitsAndUnreadablePasses(t *testing.T) {
	tool := pausedLoop(t)
	if _, err := tool.Batches(BatchesOptions{Size: 2}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var wrote atomic.Int32
	for i := range 8 {
		wg.Go(func() {
			res, err := tool.Pause(PauseOptions{Reason: fmt.Sprintf("pause %d", i)})
			if err != nil {
				t.Error(err)
			} else if res.Data.(PauseData).Changed {
				wrote.Add(1)
			}
		})
	}
	wg.Wait()
	if wrote.Load() != 1 {
		t.Fatalf("%d of 8 concurrent pauses wrote paused.json", wrote.Load())
	}
	res, err := tool.Batches(BatchesOptions{Split: "tasks-1", Claim: 2, Owner: "run-a"})
	if diagCode(err) != DiagPaused || len(res.Data.(SplitData).Parts) != 2 {
		t.Fatalf("split under a pause: %+v %v", res.Data, err)
	}
	if again, err := tool.Batches(BatchesOptions{}); err != nil || slices.ContainsFunc(again.Data.(BatchesData).Batches, func(b Batch) bool { return b.ID == "tasks-1" }) {
		t.Fatalf("the split did not stand: %v", err)
	}
	writeFile(t, filepath.Join(tool.passAbs(1), "capture.json"), `{}`)
	writeFile(t, filepath.Join(tool.passAbs(1), "review", "raw", "tasks-2.json"), `{"batch":"tasks-2","findi`)
	state, err := tool.State(StateOptions{})
	if err != nil || state.Data.(StateData).Next.Stage != "paused" || state.Data.(StateData).Paused == nil || len(state.Diagnostics) != 1 {
		t.Fatalf("an unreadable pass hid the pause: %+v %v", state, err)
	}
}
