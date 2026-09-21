package claudeguards

import "testing"

func TestSelectReapVictims(t *testing.T) {
	got := map[int]string{}
	for _, v := range selectReapVictims(fakeTable) {
		got[v.pid] = reapKind(v)
	}
	// 400 (orphan xcodebuild, 2 h), 401 (orphan appium, 19 h), 402 (WDA
	// runner under the orphan) are reaped; 201 has a live codex owner and is
	// 5 min old, 501 has a live claude owner.
	want := map[int]string{400: "xcodebuild", 401: "appium", 402: "WebDriverAgent"}
	if len(got) != len(want) {
		t.Fatalf("victims %v want %v", got, want)
	}
	for pid, kind := range want {
		if got[pid] != kind {
			t.Errorf("pid %d: %q want %q", pid, got[pid], kind)
		}
	}
	// The same orphan under ten minutes is left alone: a fresh probe may
	// still be starting up.
	young := []machineProc{{pid: 9, ppid: 1, etime: "05:00", args: "xcodebuild test-without-building"}}
	if v := selectReapVictims(young); len(v) != 0 {
		t.Errorf("young orphan reaped: %v", v)
	}
}
