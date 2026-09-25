package claudeguards

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	{pid: 400, ppid: 1, etime: "02:10:00", tty: "??", args: "xcodebuild test-without-building -project /w/WebDriverAgent.xcodeproj -scheme WebDriverAgentRunner"},
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
}

func TestWeatherLines(t *testing.T) {
	stats := machineStats{Cores: 14, Load1: 12.3, TotalGB: 96, FreeGB: 0.4, CompressorGB: 38}
	c := countMachine(fakeTable)
	line := weatherLine(stats, c)
	want := "🌡️ Mac: load 12.3 on 14 cores · free 0.4 GB, compressor 38 GB of 96 · 1 claude + 1 codex sessions · 2 sims · 1 metro · 1 next · 1 api\n"
	if line != want {
		t.Errorf("got %q", line)
	}
	idle := idleReport{Idle: []idleSession{
		{pid: 1, project: "FixIt", idleFor: 19 * time.Hour, rssKB: 900 << 10},
		{pid: 2, project: "vybava/bun-store-safety", idleFor: 50 * time.Hour, rssKB: 300 << 10},
	}}
	p := weatherPressure(stats, c, defaultGuardConfig(), idle)
	if !strings.Contains(p, "2 Claude sessions idle for 10 h+ with no background task, teammate or cron hold 1.2 GB") ||
		!strings.Contains(p, "FixIt 900 MB idle 19h, vybava/bun-store-safety 300 MB idle 2d") {
		t.Errorf("pressure: %q", p)
	}
	if p := weatherPressure(stats, c, defaultGuardConfig(), idleReport{}); strings.Contains(p, "idle for") || !strings.HasSuffix(p, "(/wk:pause).\n") {
		t.Errorf("no idle session must name none: %q", p)
	}
	calm := machineStats{Cores: 14, Load1: 3, TotalGB: 96, FreeGB: 20, CompressorGB: 5}
	if p := weatherPressure(calm, machineCounts{}, defaultGuardConfig(), idle); p != "" {
		t.Errorf("calm machine warned: %q", p)
	}
}

// Idleness is the session's own status, never process age, and a session
// with a live background task, a teammate or a cron is never named, however
// long it has been idle. Undecidable is held.
func TestIdleSessionsClassification(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.Local)
	started := func(etime string) string {
		sec, _ := etimeSeconds(etime)
		// Claude Code writes procStart in UTC.
		return now.Add(-time.Duration(sec) * time.Second).UTC().Format("Mon Jan _2 15:04:05 2006")
	}
	const age = "2-00:00:00"
	claude := func(pid int, args string) machineProc {
		return machineProc{pid: pid, ppid: 1, rssKB: 300 << 10, etime: age, tty: "ttys001", args: args}
	}
	table := []machineProc{
		claude(100, "claude"), // idle 12 h, helpers only: named
		{pid: 101, ppid: 100, rssKB: 50 << 10, etime: age, args: "/Users/x/.local/share/onyx-bridge/bin/mcp-lazy --manifest m.json"},
		{pid: 102, ppid: 100, rssKB: 1 << 10, etime: "00:10", args: "caffeinate -i -t 300"},
		{pid: 103, ppid: 101, rssKB: 20 << 10, etime: age, args: "node /x/server.js"},
		claude(110, "claude"), // a run_in_background shell
		{pid: 111, ppid: 110, etime: "05:00:00", args: "/bin/zsh -c source /Users/x/.claude/shell-snapshots/snapshot-zsh-1.sh 2>/dev/null && vitrinka work watch --board b"},
		claude(120, "claude"), // leads a live teammate
		claude(121, "/Users/x/.bun/bin/claude --agent-id w@session-s120 --parent-session-id s120 --agent-type general-purpose"),
		claude(130, "claude"), // durable cron in its project
		claude(140, "claude"), // CronCreate in its transcript
		claude(150, "claude"), // transcript not read to the end yet
		claude(160, "claude"), // busy for 30 h: a long turn is not idleness
		claude(170, "claude"), // idle only 2 h
		claude(180, "claude"), // no session file
		claude(190, "claude"), // session file of an older process with this pid
		claude(210, "claude"), // a Monitor whose command mentions mcp is still a task
		{pid: 211, ppid: 210, etime: "01:00:00", args: "/bin/zsh -c source /Users/x/.claude/shell-snapshots/s.sh && npx @playwright/mcp --port 1"},
		claude(220, "claude"), // an in-process background agent still writing
		claude(230, "claude"), // idle, but no statusUpdatedAt to date it by
		{pid: 300, ppid: 1, rssKB: 900 << 10, etime: "9-00:00:00", args: "codex"},
	}
	idleFile := func(id string, idleFor time.Duration) claudeSessionFile {
		return claudeSessionFile{SessionID: id, Cwd: "/w/" + id, Status: "idle", StatusUpdatedAt: now.Add(-idleFor).UnixMilli(), ProcStart: started(age)}
	}
	files := map[int]claudeSessionFile{
		100: {SessionID: "s100", Cwd: "/w/FixIt/.worktrees/perf/apps/api", Status: "idle", StatusUpdatedAt: now.Add(-12 * time.Hour).UnixMilli(), ProcStart: started(age)},
		110: idleFile("s110", 30*time.Hour),
		120: idleFile("s120", 20*time.Hour),
		130: idleFile("s130", 15*time.Hour),
		140: idleFile("s140", 15*time.Hour),
		150: idleFile("s150", 15*time.Hour),
		160: {SessionID: "s160", Status: "busy", StatusUpdatedAt: now.Add(-30 * time.Hour).UnixMilli(), ProcStart: started(age)},
		170: idleFile("s170", 2*time.Hour),
		190: {SessionID: "s190", Status: "idle", StatusUpdatedAt: now.Add(-40 * time.Hour).UnixMilli(), ProcStart: started("9-00:00:00")},
		210: idleFile("s210", 11*time.Hour),
		220: idleFile("s220", 20*time.Hour),
		230: {SessionID: "s230", Status: "idle", ProcStart: started(age)},
	}
	facts := sessionFacts{
		session:       func(pid int) (claudeSessionFile, bool) { s, ok := files[pid]; return s, ok },
		durableCron:   func(cwd string) bool { return cwd == "/w/s130" },
		agentActivity: func(s claudeSessionFile) bool { return s.SessionID == "s220" },
		sessionCron: func(s claudeSessionFile) (bool, bool) {
			switch s.SessionID {
			case "s140":
				return true, true
			case "s150":
				return false, false
			}
			return false, true
		},
	}
	r := idleSessions(table, now, facts)
	if len(r.Idle) != 1 || r.Idle[0].pid != 100 {
		t.Fatalf("idle = %+v", r.Idle)
	}
	if s := r.Idle[0]; s.rssKB != 371<<10 || s.project != "FixIt/perf" || s.idleFor != 12*time.Hour {
		t.Errorf("session 100: rss %d project %q idle %s", s.rssKB, s.project, s.idleFor)
	}
	want := map[sessionHold]int{holdTask: 3, holdTeammates: 1, holdCron: 2, holdUndecided: 4}
	for hold, n := range want {
		if r.Held[hold] != n {
			t.Errorf("held %s = %d, want %d (all: %v)", hold, r.Held[hold], n, r.Held)
		}
	}
	if len(r.Held) != len(want) {
		t.Errorf("unexpected holds: %v", r.Held)
	}
}

// A session-only cron is visible only in the transcript. The read is
// incremental under a budget, undecided until it reaches the end, and matches
// a CronCreate tool_use, never the name in tool output or a tool listing.
func TestSessionCronScan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	lines := `{"type":"user","message":{"content":[{"type":"tool_result","content":"grep '\"name\":\"CronCreate\"' x"}]}}` + "\n" +
		`{"type":"attachment","attachment":{"content":"deferred tools: CronCreate, CronDelete"}}` + "\n"
	if err := os.WriteFile(path, []byte(lines), 0o600); err != nil {
		t.Fatal(err)
	}
	cache := &cronCache{entries: map[string]cronScan{}, used: map[string]bool{}}
	budget := int64(1)
	if cron, decided := cache.scan("s", path, &budget); cron || decided {
		t.Fatalf("a budget-cut read must stay undecided: cron=%v decided=%v", cron, decided)
	}
	budget = 1 << 20
	if cron, decided := cache.scan("s", path, &budget); cron || !decided {
		t.Fatalf("read to the end with no CronCreate call: cron=%v decided=%v", cron, decided)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"CronCreate","input":{"cron":"0 9 * * *"}}]}}` + "\n")
	_ = f.Close()
	if cron, decided := cache.scan("s", path, &budget); !cron || !decided {
		t.Fatalf("an appended CronCreate call must be found: cron=%v decided=%v", cron, decided)
	}
}

// An in-process background agent shows only as writes under the session's
// directory after the lead went idle; no directory means none ever ran.
func TestWrittenAfter(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	idleSince := time.Now().Add(-12 * time.Hour)
	if writtenAfter(dir, idleSince) {
		t.Error("a session that never spawned an agent has no directory and holds nothing")
	}
	agent := filepath.Join(dir, "subagents", "workflows", "wf_1", "agent-a.jsonl")
	if err := os.MkdirAll(filepath.Dir(agent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agent, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := idleSince.Add(-time.Hour)
	for _, p := range []string{agent, filepath.Dir(agent), filepath.Join(dir, "subagents", "workflows"), filepath.Join(dir, "subagents"), dir} {
		if err := os.Chtimes(p, before, before); err != nil {
			t.Fatal(err)
		}
	}
	if writtenAfter(dir, idleSince) {
		t.Error("an agent that finished before the lead went idle holds nothing")
	}
	now := time.Now()
	if err := os.Chtimes(agent, now, now); err != nil {
		t.Fatal(err)
	}
	if !writtenAfter(dir, idleSince) {
		t.Error("an agent still writing after the lead went idle must hold the session")
	}
}

// A durable cron file holds tasks unless it provably lists none; what cannot
// be parsed holds them too.
func TestHasScheduledTasks(t *testing.T) {
	dir := t.TempDir()
	if hasScheduledTasks(filepath.Join(dir, "missing.json")) {
		t.Error("a missing file holds nothing")
	}
	for content, want := range map[string]bool{
		`[]`: false, `{}`: false, `{"tasks":[]}`: false, `{"tasks":null}`: false,
		`[{"cron":"0 9 * * *"}]`: true, `{"tasks":[{"cron":"*/5 * * * *"}]}`: true,
		`{"tasks":{"a":{}}}`: true, `{not json`: true,
	} {
		path := filepath.Join(dir, "scheduled_tasks.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := hasScheduledTasks(path); got != want {
			t.Errorf("%s: got %v, want %v", content, got, want)
		}
	}
}

// `weather --reap` is SessionStart's one `ps`: the sweep reuses the table the
// line was counted from. The table holds no orphan, so nothing is signalled.
func TestWeatherReapReadsTheTableOnce(t *testing.T) {
	origTable, origStats := machineProcTable, readMachineStats
	t.Cleanup(func() { machineProcTable, readMachineStats = origTable, origStats })
	reads := 0
	machineProcTable = func() []machineProc {
		reads++
		return fakeTable[:2]
	}
	readMachineStats = func() (machineStats, error) {
		return machineStats{Cores: 14, Load1: 3, TotalGB: 96, FreeGB: 20, CompressorGB: 5}, nil
	}
	var out, errOut bytes.Buffer
	if err := Weather(false, true, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Errorf("process table read %d times, want 1", reads)
	}
	if !strings.HasPrefix(out.String(), "🌡️ Mac: load 3.0 on 14 cores") || errOut.Len() != 0 {
		t.Errorf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
}

func TestParseVMStat(t *testing.T) {
	out := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:                               10000.\nPages speculative:                         5000.\nPages occupied by compressor:            2000000.\n"
	free, comp, err := parseVMStat(out)
	if err != nil || free < 0.22 || free > 0.24 || comp < 30 || comp > 31 {
		t.Errorf("free %.3f comp %.2f err %v", free, comp, err)
	}
}
