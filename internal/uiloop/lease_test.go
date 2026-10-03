package uiloop

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A lease has one holder: a second taker is held and told who holds it,
// the holder takes it back (renewed, since kept), and only the holder's
// release removes it.
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
