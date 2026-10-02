package devlab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Two processes acquire one device: exactly one wins, the other gets
// DEVICE_LEASED naming the winner's session. The kernel lock is the only
// arbiter, so this runs two real processes, not goroutines.
func TestAcquireTwoProcessesOneWins(t *testing.T) {
	if os.Getenv("DEVLAB_HELPER_STATE") != "" {
		t.Skip("helper mode")
	}
	tl := newTestLab(t)
	tl.seed(t, map[string]*Device{"s20": s20Row()})
	type outcome struct {
		OK      bool   `json:"ok"`
		Code    string `json:"code"`
		Detail  string `json:"detail"`
		Session string `json:"session"`
	}
	var procs []*exec.Cmd
	var outs []*bytes.Buffer
	for _, session := range []string{"aaaaaaaa-1111", "bbbbbbbb-2222"} {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperAcquire$")
		cmd.Env = append(os.Environ(), "DEVLAB_HELPER_STATE="+tl.StateDir, "DEVLAB_HELPER_SESSION="+session)
		out := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = out, os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		procs, outs = append(procs, cmd), append(outs, out)
	}
	var results []outcome
	for i, p := range procs {
		if err := p.Wait(); err != nil {
			t.Fatalf("helper %d: %v\n%s", i, err, outs[i])
		}
		line := outs[i].String()
		start := strings.Index(line, "{")
		var o outcome
		if start < 0 || json.Unmarshal([]byte(strings.SplitN(line[start:], "\n", 2)[0]), &o) != nil {
			t.Fatalf("helper %d printed no outcome: %q", i, line)
		}
		results = append(results, o)
	}
	winners := 0
	for _, r := range results {
		if r.OK {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("want exactly one winner, got %+v", results)
	}
	for i, r := range results {
		if r.OK {
			continue
		}
		winner := results[1-i].Session
		if r.Code != DiagDeviceLeased || !strings.Contains(r.Detail, "session "+winner[:8]) {
			t.Fatalf("loser must get DEVICE_LEASED naming %s, got %+v", winner[:8], r)
		}
	}
}

// TestHelperAcquire is the child of TestAcquireTwoProcessesOneWins.
func TestHelperAcquire(t *testing.T) {
	dir := os.Getenv("DEVLAB_HELPER_STATE")
	if dir == "" {
		t.Skip("only runs as a helper process")
	}
	session := os.Getenv("DEVLAB_HELPER_SESSION")
	l := &Lab{
		StateDir:  dir,
		Exec:      func(context.Context, Cmd) (CmdOut, error) { return CmdOut{Code: 1}, nil },
		LookPath:  func(string) (string, error) { return "", nil },
		Now:       time.Now,
		Getenv:    func(k string) string { return map[string]string{"CLAUDE_CODE_SESSION_ID": session}[k] },
		ProcStart: func(int) (time.Time, bool, error) { return time.Time{}, false, nil },
		StopGroup: func(int) error { return nil },
		TempDir:   os.TempDir,
		LockWait:  10 * time.Second,
		Pid:       os.Getpid(),
	}
	_, err := l.Acquire(context.Background(), "s20", AcquireOptions{})
	o := map[string]any{"ok": err == nil, "session": session}
	if err != nil {
		d := diagErr(t, err)
		o["code"], o["detail"] = d.Code, d.Detail
	}
	b, _ := json.Marshal(o)
	fmt.Println(string(b))
}

func TestStaleness(t *testing.T) {
	cases := []struct {
		name      string
		holderPID int
		recycled  bool
		age       time.Duration // time since acquire, TTL 1h
		want      string        // acquire outcome by another session
	}{
		{name: "dead holder, expired: reclaimed", holderPID: deadClaudePID, age: 2 * time.Hour, want: DiagLeaseStaleReclaimed},
		{name: "dead holder within TTL: not stale", holderPID: deadClaudePID, age: 30 * time.Minute, want: DiagDeviceLeased},
		{name: "live holder, expired: not stale", holderPID: claudePID, age: 2 * time.Hour, want: DiagDeviceLeased},
		{name: "recycled pid, expired: reclaimed", holderPID: claudePID, recycled: true, age: 2 * time.Hour, want: DiagLeaseStaleReclaimed},
		{name: "unknown holder, expired: reclaimed", holderPID: 0, age: 2 * time.Hour, want: DiagLeaseStaleReclaimed},
		{name: "unknown holder within TTL: not stale", holderPID: 0, age: 30 * time.Minute, want: DiagDeviceLeased},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := newTestLab(t)
			tl.seed(t, map[string]*Device{"s20": s20Row()})
			tl.procs[deadClaudePID] = tl.clock.Add(-time.Hour)
			if tc.holderPID != 0 {
				tl.as("session-holder", tc.holderPID)
			}
			tl.acquire(t, "s20", time.Hour)
			delete(tl.procs, deadClaudePID)
			if tc.recycled {
				tl.procs[claudePID] = tl.clock.Add(time.Minute)
			}
			tl.clock = tl.clock.Add(tc.age)
			tl.as("session-next", 0)
			res, err := tl.Acquire(context.Background(), "s20", AcquireOptions{})
			switch tc.want {
			case DiagDeviceLeased:
				if code(err) != DiagDeviceLeased {
					t.Fatalf("want DEVICE_LEASED, got %v", err)
				}
			case DiagLeaseStaleReclaimed:
				if err != nil || !hasCode(res.Diagnostics, "info", DiagLeaseStaleReclaimed) {
					t.Fatalf("want a reclaim, got %v %v", err, codes(res.Diagnostics))
				}
				if g := res.Data.(AcquireData).Lease.Generation; g != 2 {
					t.Fatalf("generation %d, want 2", g)
				}
			}
		})
	}
}

func TestLeaseTokenLifecycle(t *testing.T) {
	tl := newTestLab(t)
	tl.seed(t, map[string]*Device{"s20": s20Row()})
	tl.as("session-one", claudePID)
	if _, err := tl.ReleaseLease("s20", "plt_whatever"); code(err) != DiagLeaseInvalid {
		t.Fatalf("release of a free device: %v", err)
	}
	token := tl.acquire(t, "s20", time.Hour)
	if !strings.HasPrefix(token, "plt_") {
		t.Fatalf("token %q", token)
	}
	raw, _ := os.ReadFile(tl.leasePath("s20"))
	if strings.Contains(string(raw), token) {
		t.Fatal("the lease file must store only the token's hash")
	}
	if _, _, _, err := tl.Verify("s20", ""); code(err) != DiagLeaseRequired {
		t.Fatalf("no token: %v", err)
	}
	if _, _, _, err := tl.Verify("s20", "plt_wrong"); code(err) != DiagLeaseInvalid {
		t.Fatalf("wrong token: %v", err)
	}
	if _, _, _, err := tl.Verify(s20Serial, token); err != nil {
		t.Fatalf("verify by serial alias: %v", err)
	}
	h, err := tl.Hold("s20", token, "install")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.RecordInstall(Installed{VariantID: "pf1-abc", Package: "app.fixit.client.dev", ArtifactSHA256: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	h.Done()

	tl.clock = tl.clock.Add(2 * time.Hour)
	_, _, _, err = tl.Verify("s20", token)
	if d := diagErr(t, err); d.Code != DiagLeaseInvalid || !strings.Contains(d.Fix, "lease renew s20 --lease "+token) {
		t.Fatalf("expired lease: %+v", d)
	}
	if _, err := tl.Renew("s20", token, RenewOptions{For: time.Hour}); err != nil {
		t.Fatalf("renewing an expired lease the token still holds: %v", err)
	}
	if _, _, _, err := tl.Verify("s20", token); err != nil {
		t.Fatalf("after renew: %v", err)
	}
	if _, err := tl.Renew("s20", token, RenewOptions{For: 9 * time.Hour}); code(err) != DiagUsage {
		t.Fatalf("renew past 8h: %v", err)
	}
	if _, err := tl.ReleaseLease("s20", token); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := tl.Verify("s20", token); code(err) != DiagLeaseInvalid {
		t.Fatalf("token after release: %v", err)
	}
	res, err := tl.Acquire(context.Background(), "s20", AcquireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v := res.Data.(AcquireData).Lease
	if v.Generation != 2 || v.LastInstalled == nil || v.LastInstalled.VariantID != "pf1-abc" || v.LastInstalled.ByGeneration != 1 {
		t.Fatalf("the fence must survive a release: %+v", v)
	}
}

func TestBreak(t *testing.T) {
	cases := []struct {
		name    string
		pid     int
		age     time.Duration
		reason  string
		wantErr string
	}{
		{name: "live holder within TTL", pid: claudePID, age: time.Minute, reason: "stuck", wantErr: DiagDeviceLeased},
		{name: "dead holder within TTL", pid: deadClaudePID, age: time.Minute, reason: "crashed"},
		{name: "live holder, expired", pid: claudePID, age: 3 * time.Hour, reason: "forgot"},
		{name: "no reason", pid: deadClaudePID, age: time.Minute, wantErr: DiagUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := newTestLab(t)
			tl.seed(t, map[string]*Device{"s20": s20Row()})
			tl.procs[deadClaudePID] = tl.clock
			tl.as("session-holder", tc.pid)
			tl.acquire(t, "s20", time.Hour)
			delete(tl.procs, deadClaudePID)
			tl.clock = tl.clock.Add(tc.age)
			res, err := tl.Break("s20", BreakOptions{Reason: tc.reason})
			if code(err) != tc.wantErr {
				t.Fatalf("break: %v, want %q", err, tc.wantErr)
			}
			if tc.wantErr == "" && res.Data.(LeaseView).Held {
				t.Fatal("the lease must be gone")
			}
		})
	}
}

func TestReapReleasesOnlyStale(t *testing.T) {
	tl := newTestLab(t)
	tl.seed(t, map[string]*Device{"s20": s20Row(), "iphone11": iphone11Row()})
	tl.procs[deadClaudePID] = tl.clock
	tl.as("session-dead", deadClaudePID)
	tl.acquire(t, "s20", time.Hour)
	tl.as("session-live", claudePID)
	tl.acquire(t, "iphone11", time.Hour)
	delete(tl.procs, deadClaudePID)
	tl.clock = tl.clock.Add(2 * time.Hour)

	dry, err := tl.Reap(true)
	if err != nil {
		t.Fatal(err)
	}
	if got := dry.Data.(ReapData).Released; len(got) != 1 || got[0].Device != "s20" {
		t.Fatalf("dry run: %+v", got)
	}
	if held, _ := tl.HeldLeases(); len(held) != 1 || held[0].DeviceID != "iphone11" {
		t.Fatalf("a dry run releases nothing; held: %+v", held)
	}
	if ids := tl.ReapStale(&bytes.Buffer{}); len(ids) != 1 || ids[0] != "s20" {
		t.Fatalf("reap: %v", ids)
	}
	st, _ := tl.Status("")
	for _, v := range st.Data.(StatusData).Leases {
		if v.Device == "s20" && (v.Held || v.Released == nil || !strings.Contains(v.Released.Reason, "reaped")) {
			t.Fatalf("s20 after reap: %+v", v)
		}
		if v.Device == "iphone11" && !v.Held {
			t.Fatal("a live holder's lease is never reaped, expired or not")
		}
	}
}

func TestFence(t *testing.T) {
	sha := strings.Repeat("a", 64)
	cases := []struct {
		name     string
		last     *Installed
		observed Installed
		wantErr  bool
	}{
		{name: "same apk", last: &Installed{VariantID: "v1", ArtifactSHA256: sha}, observed: Installed{ArtifactSHA256: sha}},
		{name: "apk changed outside perflab", last: &Installed{VariantID: "v1", ArtifactSHA256: sha}, observed: Installed{ArtifactSHA256: strings.Repeat("b", 64)}, wantErr: true},
		{name: "same ios stamp", last: &Installed{VariantID: "v1", BundleVersion: "412.3c9a1f"}, observed: Installed{BundleVersion: "412.3c9a1f"}},
		{name: "ios stamp changed", last: &Installed{VariantID: "v1", BundleVersion: "412.3c9a1f"}, observed: Installed{BundleVersion: "412"}, wantErr: true},
		{name: "another package", last: &Installed{VariantID: "v1", Package: "a.dev", ArtifactSHA256: sha}, observed: Installed{Package: "a", ArtifactSHA256: sha}, wantErr: true},
		{name: "nothing recorded", observed: Installed{ArtifactSHA256: sha}, wantErr: true},
		{name: "no comparable fingerprint", last: &Installed{VariantID: "v1", ArtifactSHA256: sha}, observed: Installed{BundleVersion: "1"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkFence("s20", "plt_x", tc.last, tc.observed)
			if (err != nil) != tc.wantErr {
				t.Fatalf("fence: %v", err)
			}
			if err != nil && code(err) != DiagDeviceStateChanged {
				t.Fatalf("code %s", code(err))
			}
		})
	}
}

func TestReleaseSessionOnlyThatSession(t *testing.T) {
	tl := newTestLab(t)
	tl.seed(t, map[string]*Device{"s20": s20Row(), "iphone11": iphone11Row()})
	tl.as("session-ending", claudePID)
	token := tl.acquire(t, "s20", time.Hour)
	h, err := tl.Hold("s20", token, "net forward")
	if err != nil {
		t.Fatal(err)
	}
	tl.procs[51000] = tl.clock
	tl.procs[51001] = tl.clock
	for _, pid := range []int{51000, 51001} {
		if err := h.RecordChild(Child{PID: pid, What: "forwarder"}); err != nil {
			t.Fatal(err)
		}
	}
	h.Done()
	tl.procs[51001] = tl.clock.Add(time.Hour) // the pid was recycled
	tl.as("session-other", claudePID)
	tl.acquire(t, "iphone11", time.Hour)

	var log bytes.Buffer
	released := tl.ReleaseSession("session-ending", &log)
	if len(released) != 1 || released[0] != "s20" {
		t.Fatalf("released %v", released)
	}
	if len(tl.stopped) != 1 || tl.stopped[0] != 51000 {
		t.Fatalf("stopped %v: only the recorded group whose leader is still the same process", tl.stopped)
	}
	held, _ := tl.HeldLeases()
	if len(held) != 1 || held[0].DeviceID != "iphone11" {
		t.Fatalf("the other session's lease must stay: %+v", held)
	}
	if !strings.Contains(log.String(), "s20") {
		t.Fatalf("log %q", log.String())
	}
	if got := tl.ReleaseSession("", &log); got != nil {
		t.Fatal("an empty session id releases nothing")
	}
}

// A child inside another process's group (net forward under the invoking
// shell's group) is stopped by its pid alone: the group may hold the
// shell's caller too. A child leading its own group (a runner) gets the
// group signal.
func TestReleaseStopsAForeignGroupChildByPid(t *testing.T) {
	tl := newTestLab(t)
	tl.seed(t, map[string]*Device{"s20": s20Row()})
	tl.as("session-a", claudePID)
	token := tl.acquire(t, "s20", time.Hour)
	h, err := tl.Hold("s20", token, "net forward")
	if err != nil {
		t.Fatal(err)
	}
	tl.procs[52000], tl.procs[53000] = tl.clock, tl.clock
	for _, c := range []Child{{PID: 52000, PGID: 51900, What: "perflab net forward :23936"}, {PID: 53000, PGID: 53000, What: "perflab run"}} {
		if err := h.RecordChild(c); err != nil {
			t.Fatal(err)
		}
	}
	h.Done()
	if _, err := tl.ReleaseLease("s20", token); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(tl.signalled) != "[52000]" || fmt.Sprint(tl.stopped) != "[53000]" {
		t.Fatalf("signalled pids %v, stopped groups %v: the forwarder's shell group 51900 must never be signalled", tl.signalled, tl.stopped)
	}
}

// A long verb's heartbeat keeps its lease at least ActiveGrace ahead, so a
// run started near the end of the TTL is never cut by it; a lease with
// more time left keeps its expiry.
func TestHeartbeatKeepsAnActiveLeaseAlive(t *testing.T) {
	for _, c := range []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{"near the end of the TTL", 2 * time.Minute, ActiveGrace},
		{"plenty left", time.Hour, time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			tl := newTestLab(t)
			tl.seed(t, map[string]*Device{"s20": s20Row()})
			tl.as("session-a", claudePID)
			token := tl.acquire(t, "s20", c.ttl)
			h, err := tl.Hold("s20", token, "run")
			if err != nil {
				t.Fatal(err)
			}
			defer h.Done()
			acquired := tl.clock
			tl.clock = tl.clock.Add(time.Second) // the beat is seen by its heartbeatAt
			stop, errs := h.StartHeartbeat(context.Background(), time.Hour)
			defer stop()
			want := acquired.Add(c.want)
			if c.want == ActiveGrace {
				want = tl.clock.Add(ActiveGrace)
			}
			for deadline := time.Now().Add(5 * time.Second); ; {
				ls, err := tl.readLease("s20")
				if err != nil {
					t.Fatal(err)
				}
				if ls.ExpiresAt != nil && ls.ExpiresAt.Equal(want) && ls.HeartbeatAt != nil && ls.HeartbeatAt.Equal(tl.clock) {
					break
				}
				select {
				case err := <-errs:
					t.Fatalf("heartbeat: %v", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatalf("expiresAt %v, want %v", ls.ExpiresAt, want)
				}
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}

func TestDeviceBusyAndLedgerLocked(t *testing.T) {
	tl := newTestLab(t)
	tl.seed(t, map[string]*Device{"s20": s20Row()})
	tl.as("session-one", claudePID)
	token := tl.acquire(t, "s20", time.Hour)
	h, err := tl.Hold("s20", token, "run")
	if err != nil {
		t.Fatal(err)
	}
	_, err = tl.Hold("s20", token, "install")
	if d := diagErr(t, err); d.Code != DiagDeviceBusy || !strings.Contains(d.Detail, "perflab run") || !strings.Contains(d.Detail, "pid 4242") {
		t.Fatalf("second hold: %+v", d)
	}
	h.Done()
	if h2, err := tl.Hold("s20", token, "install"); err != nil {
		t.Fatalf("after Done: %v", err)
	} else {
		h2.Done()
	}

	held, err := tl.lock("ledger", "device add", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	err = tl.updateLedger("device probe", func(*Ledger) error { return nil })
	if d := diagErr(t, err); d.Code != DiagLedgerLocked || !strings.Contains(d.Detail, "pid 4242, device add") {
		t.Fatalf("ledger lock: %+v", d)
	}
}

func TestHeldLeasesByAlias(t *testing.T) {
	tl := newTestLab(t)
	if held, err := tl.HeldLeases(); err != nil || held != nil {
		t.Fatalf("a machine that never used the lab: %v %v", held, err)
	}
	tl.seed(t, map[string]*Device{"s20": s20Row(), "iphone11": iphone11Row()})
	tl.as("session-one", claudePID)
	tl.acquire(t, "iphone11", time.Hour)
	for _, handle := range []string{"iphone11", strings.ToLower(iphone11UDID), iphone11Core} {
		if h, ok, err := tl.LeaseFor(handle); err != nil || !ok || h.DeviceID != "iphone11" || h.Platform != PlatformIOS {
			t.Fatalf("LeaseFor(%s) = %+v %v %v", handle, h, ok, err)
		}
	}
	if _, ok, _ := tl.LeaseFor(s20Serial); ok {
		t.Fatal("s20 is not leased")
	}
}
