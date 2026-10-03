package uiloop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A lease has one holder: a second taker is held and told who holds it,
// the holder takes it back (renewed, since kept), and only the holder's
// release removes it; an owner lease matches on its owner alone.
func TestALeaseHasOneHolderAndOnlyItReleasesIt(t *testing.T) {
	tool := newTool(t, testConfig())
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	tool.Now = func() time.Time { return now }
	a := leaseReq{owner: "run-a", pid: os.Getpid(), ttl: time.Hour}
	b := leaseReq{owner: "run-b", pid: os.Getpid(), ttl: time.Hour}
	got, held, err := tool.acquireLease(1, leaseSynth, a)
	if err != nil || held != nil || got.StartedAt != "2026-10-03T09:00:00Z" {
		t.Fatalf("first taker: %+v %+v %v", got, held, err)
	}
	if _, held, err := tool.acquireLease(1, leaseSynth, b); err != nil || held == nil || held.Owner != "run-a" || held.Stale != "" {
		t.Fatalf("a second taker is held: %+v %v", held, err)
	}
	now = now.Add(50 * time.Minute)
	renewed, held, err := tool.acquireLease(1, leaseSynth, a)
	if err != nil || held != nil || renewed.StartedAt != got.StartedAt || renewed.TTL != "1h50m0s" {
		t.Fatalf("the holder renews: %+v %+v %v", renewed, held, err)
	}
	if err := tool.releaseLease(1, leaseSynth, Lease{Owner: "run-b", Host: got.Host, PID: os.Getpid(), StartedAt: got.StartedAt}); err != nil {
		t.Fatal(err)
	}
	if h, err := tool.readLease(1, leaseSynth); err != nil || h == nil {
		t.Fatalf("another owner's release leaves it: %+v %v", h, err)
	}
	if err := tool.releaseLease(1, leaseSynth, renewed); err != nil {
		t.Fatal(err)
	}
	if _, held, err := tool.acquireLease(1, leaseSynth, b); err != nil || held != nil {
		t.Errorf("a released lease is free: %+v %v", held, err)
	}
	// An owner lease is its owner's from any host: macOS renames the host per network.
	claim := Lease{Owner: "run-c", Host: "renamed.local", StartedAt: "2026-10-03T09:50:00Z", TTL: "1h0m0s"}
	if err := writeJSON(tool.leaseFile(1, batchLease("tasks-1")), claim); err != nil {
		t.Fatal(err)
	}
	if _, held, err := tool.acquireLease(1, batchLease("tasks-1"), leaseReq{owner: "run-c", ttl: time.Hour}); err != nil || held != nil {
		t.Errorf("run-c takes its claim back under another host name: %+v %v", held, err)
	}
}

// A capture's or a publish's lease is renewed while the verb runs, so it
// holds past its ttl; release drops it.
func TestAHeldLeaseOutlivesItsTTLWhileItsVerbRuns(t *testing.T) {
	tool := newTool(t, testConfig())
	release, held, err := tool.holdLease(1, leasePublish, leaseReq{owner: "ui-loop publish", pid: os.Getpid(), ttl: time.Second})
	if err != nil || held != nil {
		t.Fatal(held, err)
	}
	time.Sleep(1500 * time.Millisecond)
	if h, err := tool.readLease(1, leasePublish); err != nil || h == nil || h.Stale != "" {
		t.Errorf("renewed past its ttl: %+v %v", h, err)
	}
	release(&err)
	if h, rerr := tool.readLease(1, leasePublish); err != nil || rerr != nil || h != nil {
		t.Errorf("released: %+v %v %v", h, err, rerr)
	}
}

// A lease goes stale when its pid died on this host, its ttl ran out or its
// run wrote done.json; the next taker replaces it, and doctor reports a
// process lease left behind.
func TestAStaleLeaseIsReplaced(t *testing.T) {
	tool := newTool(t, testConfig())
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	tool.Now = func() time.Time { return now }
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	const run = "2026-10-03T09:00:00Z"
	for _, c := range []struct {
		name   string
		req    leaseReq
		expire func()
		why    string
	}{
		{"dead pid", leaseReq{owner: "crashed", pid: exited.ProcessState.Pid(), ttl: time.Hour}, func() {}, "is gone"},
		{"ttl", leaseReq{owner: "claim", ttl: time.Hour}, func() { now = now.Add(time.Hour) }, "ttl ran out"},
		{"done", leaseReq{owner: "ui-loop run", pid: os.Getpid(), ttl: time.Hour, run: run},
			func() { writeFile(t, filepath.Join(tool.passAbs(1), "done.json"), `{"v":1,"pass":1,"run":"`+run+`"}`) }, "run finished"},
	} {
		if _, held, err := tool.acquireLease(1, leaseCapture, c.req); err != nil || held != nil {
			t.Fatalf("%s: take: %+v %v", c.name, held, err)
		}
		if h, err := tool.readLease(1, leaseCapture); err != nil || c.name != "dead pid" && h.Stale != "" {
			t.Fatalf("%s: holds before it expires: %+v %v", c.name, h, err)
		}
		c.expire()
		if h, err := tool.readLease(1, leaseCapture); err != nil || !strings.Contains(h.Stale, c.why) {
			t.Fatalf("%s: stale: %+v %v", c.name, h, err)
		}
		if c.name == "dead pid" {
			row, err := tool.doctorLeases(DoctorCheck{ID: "pass", Status: DoctorOK})
			if err != nil || row.Status != DoctorWarn || len(row.diags) != 1 || row.diags[0].Code != DiagLeaseStale {
				t.Errorf("doctor reports a crashed holder's lease: %+v %v", row, err)
			}
		}
		got, held, err := tool.acquireLease(1, leaseCapture, leaseReq{owner: "next " + c.name, pid: os.Getpid(), ttl: time.Hour})
		if err != nil || held != nil || got.Owner != "next "+c.name {
			t.Fatalf("%s: a stale lease is replaced: %+v %v", c.name, held, err)
		}
		if err := tool.releaseLease(1, leaseCapture, got); err != nil {
			t.Fatal(err)
		}
	}
}

// Two claims at once never share a batch, a complete batch is never
// claimed, and an owner claiming again gets its own claims back.
func TestTwoClaimsTakeDisjointBatches(t *testing.T) {
	tool := newTool(t, testConfig())
	dir := stagePass(t, tool)
	writeFile(t, filepath.Join(dir, "review", "raw", "tasks-1.json"), `{"batch":"tasks-1","area":"tasks"}`)
	claim := func(owner string, n int) BatchesData {
		res, err := tool.Batches(BatchesOptions{Size: 1, Claim: n, Owner: owner})
		if err != nil {
			t.Error(err)
			return BatchesData{}
		}
		return res.Data.(BatchesData)
	}
	ids := func(b BatchesData) []string {
		var out []string
		for _, c := range *b.Claimed {
			out = append(out, c.ID)
		}
		return out
	}
	var a, b BatchesData
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a = claim("run-a", 2) }()
	go func() { defer wg.Done(); b = claim("run-b", 2) }()
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	both := append(ids(a), ids(b)...)
	slices.Sort(both)
	if !slices.Equal(both, []string{"admin-1", "tasks-2"}) || len(a.Batches) != 3 || !slices.Equal(a.Left, []string{"tasks-2", "admin-1"}) {
		t.Fatalf("disjoint claims of the left batches: a %v, b %v, batches %d, left %v", ids(a), ids(b), len(a.Batches), a.Left)
	}
	if again := claim("run-a", 2); !slices.Equal(ids(again), ids(a)) {
		t.Errorf("run-a claims again: %v, held %v", ids(again), ids(a))
	}
	if res, err := tool.Batches(BatchesOptions{}); err != nil || res.Data.(BatchesData).Claimed != nil {
		t.Errorf("without --claim nothing is claimed: %v", err)
	}
	if _, err := tool.Batches(BatchesOptions{Claim: 2}); diagCode(err) != DiagSelectionInvalid {
		t.Errorf("--claim needs --owner: %v", err)
	}
}

// Of N identical review runs one synthesizes: the synth lease an owner's
// merge-review takes outlives the command, so another owner (or a
// merge-review without one) is refused until it runs out.
func TestMergeReviewRefusesWhileAnotherOwnerHoldsSynth(t *testing.T) {
	tool := newTool(t, testConfig())
	writePass(t, tool, 1, []shot{{order: 0, id: "tasks", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1}})
	writeFile(t, filepath.Join(tool.passAbs(1), "review", "raw", "tasks-1.json"), `{"batch":"tasks-1","area":"tasks","findings":[]}`)
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	tool.Now = func() time.Time { return now }
	if _, err := tool.MergeReview(MergeReviewOptions{Owner: "run-a"}); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{"run-b", ""} {
		_, err := tool.MergeReview(MergeReviewOptions{Owner: owner})
		if diagCode(err) != DiagLeaseHeld || !strings.Contains(err.Error(), "held by run-a since 2026-10-03T09:00:00Z") {
			t.Errorf("owner %q while run-a synthesizes: %v", owner, err)
		}
	}
	if _, err := tool.MergeReview(MergeReviewOptions{Owner: "run-a"}); err != nil {
		t.Errorf("run-a merges again: %v", err)
	}
	now = now.Add(2 * time.Hour)
	if _, err := tool.MergeReview(MergeReviewOptions{Owner: "run-b"}); err != nil {
		t.Errorf("run-a's synth ran out: %v", err)
	}
}

// state's next.parallel counts only the left batches no live claim holds:
// the runs reviewing the rest need no company.
func TestParallelCountsOnlyUnclaimedBatches(t *testing.T) {
	tool := newTool(t, testConfig())
	var shots []shot
	for i := range 9 {
		shots = append(shots, shot{order: i, id: fmt.Sprintf("s%d", i), area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1})
	}
	writePass(t, tool, 1, shots)
	writeFile(t, filepath.Join(tool.passAbs(1), "publish", "index.json"), `{"pass":1,"sets":[{"key":"ui-polish-tasks","area":"tasks","status":"pushed","url":"u1"}]}`)
	for _, c := range []struct {
		owner       string
		claim, want int
	}{{"", 0, 3}, {"run-a", 4, 2}, {"run-b", 4, 1}} {
		if _, err := tool.Batches(BatchesOptions{Size: 1, Claim: c.claim, Owner: c.owner}); err != nil {
			t.Fatal(err)
		}
		res, err := tool.State(StateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if next := res.Data.(StateData).Next; next.Stage != "review" || next.Parallel != c.want {
			t.Errorf("after %q claimed %d of 9: %+v, want parallel %d", c.owner, c.claim, next, c.want)
		}
	}
}
