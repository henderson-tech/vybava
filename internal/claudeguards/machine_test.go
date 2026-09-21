package claudeguards

import (
	"strings"
	"testing"
)

// fakeTable is the 2026-09-19 shape in miniature: a claude session owning a
// turbo dev that spawned next dev, a stale codex session, two booted sims, an
// orphaned Appium probe and an old xcodebuild with a live owner.
var fakeTable = []machineProc{
	{pid: 100, ppid: 1, etime: "12:30:00", tty: "ttys005", args: "claude"},
	{pid: 101, ppid: 100, etime: "01:00:00", tty: "ttys005", args: "node /w/node_modules/.bin/turbo dev --filter=@fixit/web"},
	{pid: 102, ppid: 101, etime: "00:59:00", tty: "ttys005", args: "next-server (v16.3.3)"},
	{pid: 103, ppid: 100, etime: "00:30:00", tty: "ttys005", args: "node /w/node_modules/expo/bin/cli start --dev-client"},
	{pid: 200, ppid: 1, etime: "2-03:00:00", tty: "ttys044", args: "codex"},
	{pid: 201, ppid: 200, etime: "00:05:00", tty: "ttys044", args: "xcodebuild test-without-building -project WebDriverAgent.xcodeproj"},
	{pid: 300, ppid: 1, etime: "04:00:00", tty: "??", args: "/Library/Developer/CoreSimulator/Volumes/iOS_23A/launchd_sim"},
	{pid: 301, ppid: 1, etime: "03:00:00", tty: "??", args: "/Library/Developer/CoreSimulator/Volumes/iOS_23A/launchd_sim"},
	{pid: 400, ppid: 1, etime: "02:10:00", tty: "??", args: "xcodebuild test-without-building -xctestrun WDA.xctestrun"},
	{pid: 401, ppid: 1, etime: "19:00:00", tty: "??", args: "node /w/node_modules/.bin/appium --port 14006"},
	{pid: 402, ppid: 400, etime: "02:09:00", tty: "??", args: "/Users/x/Library/Developer/Xcode/DerivedData/WebDriverAgentRunner-Runner"},
	{pid: 500, ppid: 1, etime: "00:20:00", tty: "ttys009", args: "bun --watch src/main.ts"},
	{pid: 501, ppid: 100, etime: "00:03:00", tty: "ttys005", args: "node /w/node_modules/.bin/appium --port 4723"},
}

func TestEtimeSeconds(t *testing.T) {
	for in, want := range map[string]int{"00:05": 5, "12:30:00": 45000, "2-03:00:00": 183600, "01:00": 60} {
		if got, ok := etimeSeconds(in); !ok || got != want {
			t.Errorf("%s: got %d ok=%v want %d", in, got, ok, want)
		}
	}
	if _, ok := etimeSeconds("x"); ok {
		t.Error("garbage parsed")
	}
}

func TestCountMachine(t *testing.T) {
	c := countMachine(fakeTable)
	if c.Sims != 2 || c.Emulators != 0 {
		t.Errorf("sims %d emulators %d", c.Sims, c.Emulators)
	}
	// turbo dev counts once, at its next-server leaf.
	if c.Next != 1 || c.Metro != 1 || c.API != 1 || c.devServers() != 3 {
		t.Errorf("next %d metro %d api %d", c.Next, c.Metro, c.API)
	}
	if c.Claude != 1 || c.Codex != 1 {
		t.Errorf("claude %d codex %d", c.Claude, c.Codex)
	}
	if len(c.Stale) != 2 {
		t.Errorf("stale %v", c.Stale)
	}
}

func TestWeatherLines(t *testing.T) {
	stats := machineStats{Cores: 14, Load1: 12.3, TotalGB: 96, FreeGB: 0.4, CompressorGB: 38}
	c := countMachine(fakeTable)
	line := weatherLine(stats, c)
	want := "🌡️ Mac: load 12.3 on 14 cores · free 0.4 GB, compressor 38 GB of 96 · 1 claude + 1 codex sessions · 2 sims · 1 metro · 1 next · 1 api\n"
	if line != want {
		t.Errorf("got %q", line)
	}
	if p := weatherPressure(stats, c, defaultGuardConfig()); !strings.Contains(p, "2 Claude sessions older than 10 h") {
		t.Errorf("pressure: %q", p)
	}
	calm := machineStats{Cores: 14, Load1: 3, TotalGB: 96, FreeGB: 20, CompressorGB: 5}
	if p := weatherPressure(calm, machineCounts{}, defaultGuardConfig()); p != "" {
		t.Errorf("calm machine warned: %q", p)
	}
}

func TestParseVMStat(t *testing.T) {
	out := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:                               10000.\nPages speculative:                         5000.\nPages occupied by compressor:            2000000.\n"
	free, comp, err := parseVMStat(out)
	if err != nil || free < 0.22 || free > 0.24 || comp < 30 || comp > 31 {
		t.Errorf("free %.3f comp %.2f err %v", free, comp, err)
	}
}
