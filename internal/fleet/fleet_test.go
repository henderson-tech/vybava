package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/plugingc"
	"github.com/henderson-tech/vybava/internal/runx"
)

var now = time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC)

const ctime = "Mon Jan _2 15:04:05 2006"

// fixture is a home with a registry, a ledger dir and two repositories, one
// with a linked worktree.
type fixture struct {
	home, registry, ledgers string
	fixit, vybava, worktree string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	home := t.TempDir()
	f := fixture{
		home:     home,
		registry: filepath.Join(home, ".claude", "sessions"),
		ledgers:  filepath.Join(home, ".local", "state", "vybava", "fleet", "ledger"),
		fixit:    filepath.Join(home, "Work", "FixIt"),
		vybava:   filepath.Join(home, "Work", "vybava"),
	}
	f.worktree = filepath.Join(f.fixit, ".worktrees", "feat", "x-vt-1")
	for _, dir := range []string{f.registry, filepath.Join(f.fixit, ".git", "worktrees", "x"), filepath.Join(f.vybava, ".git"), f.worktree} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitdir := filepath.Join(f.fixit, ".git", "worktrees", "x")
	mustWrite(t, filepath.Join(f.worktree, ".git"), "gitdir: "+gitdir+"\n")
	mustWrite(t, filepath.Join(gitdir, "commondir"), "../..\n")
	return f
}

func (f fixture) env(processes plugingc.ProcessLister) Env {
	return Env{Home: f.home, Now: now, Processes: processes, GOOS: "darwin"}
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// register writes a registry record the way Claude Code does.
func (f fixture) register(t *testing.T, pid int, sessionID, status, cwd string, since time.Duration, extra map[string]any) {
	t.Helper()
	rec := map[string]any{
		"pid": pid, "pidDomain": "darwin", "procStart": started(pid).Format(ctime),
		"sessionId": sessionID, "status": status, "cwd": cwd, "name": "s-" + sessionID,
		"startedAt": started(pid).UnixMilli(), "statusUpdatedAt": now.Add(-since).UnixMilli(), "updatedAt": now.UnixMilli(),
		"kind": "interactive", "entrypoint": "cli", "version": "2.1.287",
	}
	for key, value := range extra {
		if value == nil {
			delete(rec, key)
			continue
		}
		rec[key] = value
	}
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(f.registry, strconv.Itoa(pid)+".json"), string(body))
}

// started is each fixture process's own start time.
func started(pid int) time.Time { return now.Add(-time.Duration(pid) * time.Minute) }

// table is a process lister over fixed processes.
func table(processes ...plugingc.Process) plugingc.ProcessLister {
	return func(context.Context) (plugingc.ProcessTable, error) {
		out := plugingc.ProcessTable{}
		for _, p := range processes {
			out[p.PID] = p
		}
		return out, nil
	}
}

func claude(pid int) plugingc.Process {
	return plugingc.Process{PID: pid, Started: started(pid), Command: "/Users/me/.local/bin/claude"}
}

func TestReadOrdersWaitingFirstAndGroupsByRepository(t *testing.T) {
	f := newFixture(t)
	f.register(t, 11, "a-waiting", "waiting", f.fixit, 2*time.Minute, map[string]any{"waitingFor": "input needed"})
	f.register(t, 12, "b-busy", "busy", f.worktree, time.Minute, nil)
	f.register(t, 13, "c-idle", "idle", f.vybava, time.Hour, nil)
	f.register(t, 14, "d-waiting", "waiting", f.vybava, 9*time.Minute, nil)
	mustWrite(t, filepath.Join(f.registry, "14.deadbeef.key"), "never read")

	snap, diags, err := Read(context.Background(), f.env(table(claude(11), claude(12), claude(13), claude(14))))
	if err != nil {
		t.Fatal(err)
	}
	if len(diags) != 0 {
		t.Fatalf("diagnostics = %+v", diags)
	}
	var order []string
	for _, s := range snap.Sessions {
		order = append(order, s.SessionID)
	}
	if got, want := strings.Join(order, ","), "d-waiting,a-waiting,b-busy,c-idle"; got != want {
		t.Fatalf("order = %s, want %s", got, want)
	}
	busy := snap.Sessions[2]
	if busy.Project != "FixIt" || busy.Worktree != "feat/x-vt-1" || busy.Root != f.fixit {
		t.Fatalf("worktree session = %+v", busy)
	}
	if snap.Sessions[1].WaitingFor != "input needed" || snap.Sessions[1].AgeSeconds != 120 {
		t.Fatalf("waiting session = %+v", snap.Sessions[1])
	}
	if got := snap.Projects[0]; got.Project != "FixIt" || got.Counts.Waiting != 1 || got.Counts.Busy != 1 {
		t.Fatalf("first project = %+v, want FixIt (1 waiting, 1 busy)", got)
	}
	if snap.Counts != (Counts{Total: 4, Waiting: 2, Busy: 1, Idle: 1}) {
		t.Fatalf("counts = %+v", snap.Counts)
	}
	if !strings.Contains(snap.Sessions[0].Resume, "claude --resume d-waiting") {
		t.Fatalf("resume = %q", snap.Sessions[0].Resume)
	}
}

func TestReadProvesDeathAndNeverGuessesIt(t *testing.T) {
	f := newFixture(t)
	f.register(t, 21, "gone-busy", "busy", f.vybava, time.Minute, nil)
	f.register(t, 22, "foreign-waiting", "waiting", f.vybava, time.Minute, nil)
	f.register(t, 23, "recycled-busy", "busy", f.vybava, time.Minute, nil)
	f.register(t, 24, "gone-idle", "idle", f.vybava, time.Minute, nil)
	f.register(t, 25, "alive-busy", "busy", f.vybava, time.Minute, nil)
	f.register(t, 26, "other-domain", "busy", f.vybava, time.Minute, map[string]any{"pidDomain": "linux"})

	recycled := plugingc.Process{PID: 23, Started: started(23).Add(5 * time.Minute), Command: "claude"}
	foreign := plugingc.Process{PID: 22, Started: started(22), Command: "/usr/bin/vim"}
	snap, _, err := Read(context.Background(), f.env(table(foreign, recycled, claude(25))))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"gone-busy":       {"dead", "gone"},
		"foreign-waiting": {"dead", "foreign"},
		"recycled-busy":   {"dead", "recycled"},
		"gone-idle":       {"ended", "gone"},
		"alive-busy":      {"busy", "alive"},
		"other-domain":    {"busy", "unknown"},
	}
	for _, s := range snap.Sessions {
		if got := [2]string{string(s.State), string(s.Liveness)}; got != want[s.SessionID] {
			t.Errorf("%s = %v, want %v", s.SessionID, got, want[s.SessionID])
		}
	}

	failing := func(context.Context) (plugingc.ProcessTable, error) { return nil, errors.New("ps: exit 1") }
	snap, diags, err := Read(context.Background(), f.env(failing))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Dead != 0 || snap.Counts.Ended != 0 {
		t.Fatalf("an unreadable process table must judge nobody dead: %+v", snap.Counts)
	}
	if len(diags) != 1 || diags[0].Code != DiagLivenessUnavailable {
		t.Fatalf("diagnostics = %+v", diags)
	}
}

// Claude Code writes procStart in UTC while ps reports lstart in local time:
// east of UTC a live session's process seems to start after its own record.
// The zone must only ever keep a session alive, never make it dead.
func TestLivenessIgnoresTheProcStartZone(t *testing.T) {
	cest := time.FixedZone("CEST", 2*60*60)
	start := time.Date(2026, 10, 2, 6, 53, 54, 0, time.UTC)
	view := processView{known: true, table: plugingc.ProcessTable{
		1: {PID: 1, Started: start.In(cest), Command: "/Users/me/.bun/bin/claude"},
		2: {PID: 2, Started: start.Add(3 * time.Hour).In(cest), Command: "claude"},
	}}
	utcStamp := start.Format(ctime)            // what 2.1.28x writes
	localStamp := start.In(cest).Format(ctime) // what a local-time writer would
	cases := []struct {
		pid   int
		stamp string
		want  Liveness
	}{
		{1, utcStamp, LivenessAlive},
		{1, localStamp, LivenessAlive},
		{2, utcStamp, LivenessRecycled},
		{2, localStamp, LivenessRecycled},
		{1, "not a time", LivenessAlive},
	}
	for _, c := range cases {
		if got := view.classify(c.pid, c.stamp, "darwin", "darwin", cest); got != c.want {
			t.Errorf("pid %d procStart %q = %s, want %s", c.pid, c.stamp, got, c.want)
		}
	}
}

func TestReadSkipsOneBadFileButFailsOnAnUnknownShape(t *testing.T) {
	f := newFixture(t)
	f.register(t, 31, "good", "busy", f.vybava, time.Minute, nil)
	f.register(t, 32, "no-cwd", "busy", f.vybava, time.Minute, map[string]any{"cwd": nil})
	snap, diags, err := Read(context.Background(), f.env(table(claude(31))))
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Total != 1 || len(diags) != 1 || diags[0].Code != DiagRegistryFileSkipped || !strings.Contains(diags[0].Detail, `32.json: field "cwd" missing`) {
		t.Fatalf("snapshot %+v diagnostics %+v", snap.Counts, diags)
	}

	f = newFixture(t)
	f.register(t, 41, "x", "busy", f.vybava, time.Minute, map[string]any{"sessionId": nil, "session_id": "x"})
	f.register(t, 42, "y", "busy", f.vybava, time.Minute, map[string]any{"sessionId": nil, "session_id": "y"})
	_, _, err = Read(context.Background(), f.env(table()))
	var derr runx.DiagError
	if !errors.As(err, &derr) || derr.Diag.Code != DiagRegistryShapeUnknown || !strings.Contains(derr.Diag.Detail, `field "sessionId" missing in 2`) {
		t.Fatalf("err = %v", err)
	}
	if derr.ExitCode() != 2 {
		t.Fatalf("exit = %d", derr.ExitCode())
	}
}

func TestReadWithoutRegistryIsAnEmptyFleet(t *testing.T) {
	home := t.TempDir()
	snap, diags, err := Read(context.Background(), Env{Home: home, Now: now, Processes: table()})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Total != 0 || len(diags) != 1 || diags[0].Code != DiagRegistryMissing {
		t.Fatalf("snapshot %+v diagnostics %+v", snap, diags)
	}
}

func TestReadCodexRowsDegradeToAWarning(t *testing.T) {
	f := newFixture(t)
	f.register(t, 51, "s", "busy", f.vybava, time.Minute, nil)
	env := f.env(table(claude(51)))

	env.Codex = func(context.Context, time.Time) ([]CodexRow, []string, error) {
		return nil, nil, errors.New("ps: operation not permitted")
	}
	snap, diags, err := Read(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Counts.Total != 1 || len(snap.Codex) != 0 || len(diags) != 1 || diags[0].Code != DiagCodexUnavailable {
		t.Fatalf("codex %+v diagnostics %+v", snap.Codex, diags)
	}

	env.Codex = func(context.Context, time.Time) ([]CodexRow, []string, error) {
		return []CodexRow{{PID: 900, ThreadID: "t1", CWD: f.worktree}}, []string{"open rollouts unavailable: lsof"}, nil
	}
	snap, diags, err = Read(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Codex) != 1 || snap.Codex[0].Project != "FixIt" || len(diags) != 1 || diags[0].Code != DiagCodexPartial {
		t.Fatalf("codex %+v diagnostics %+v", snap.Codex, diags)
	}
}

func TestReviveOffersOnlyProvenDeaths(t *testing.T) {
	f := newFixture(t)
	env := f.env(table(claude(62)))
	f.register(t, 61, "dead-busy", "busy", f.vybava, 3*time.Minute, nil)
	f.register(t, 62, "alive", "busy", f.vybava, time.Minute, nil)
	f.register(t, 63, "dead-idle", "idle", f.vybava, time.Minute, nil)
	// A session that exited in order: its registry record is gone, its ledger
	// still names the owner process and an open workflow.
	f.register(t, 64, "exited", "busy", f.fixit, time.Minute, nil)
	if _, err := Record(env, "exited", Event{Kind: JobWorkflow, ID: "wf_1", Status: JobStarted, RunID: "wf_1", ScriptPath: "/s.js"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.registry, "64.json")); err != nil {
		t.Fatal(err)
	}
	// An unstamped ledger proves nothing.
	if _, err := Record(env, "never-registered", Event{Kind: JobShell, ID: "b1", Status: JobStarted}); err != nil {
		t.Fatal(err)
	}

	revive, diags, err := ReadRevive(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range revive.Sessions {
		got = append(got, s.SessionID+":"+string(s.Source))
	}
	if strings.Join(got, ",") != "exited:ledger,dead-busy:registry" {
		t.Fatalf("revive = %v", got)
	}
	exited := revive.Sessions[0]
	if exited.CWD != f.fixit || len(exited.OpenJobs) != 1 || exited.OpenJobs[0].RunID != "wf_1" || !strings.HasPrefix(exited.Resume, "cd ") {
		t.Fatalf("ledger revive = %+v", exited)
	}
	if len(diags) != 1 || diags[0].Code != DiagLedgerUnproven {
		t.Fatalf("diagnostics = %+v", diags)
	}
}
