package macwatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const fixtureTop = `Processes: 2032 total, 9 running, 2023 sleeping, 15656 threads
2026/09/20 13:22:00
Load Avg: 11.29, 12.81, 19.66
PhysMem: 93G used (13G wired, 42G compressor), 2613M unused.
`

// pid ppid %cpu rss etime tty command
const fixturePS = `    1     0   0.1   12000 03-04:23:36 ??       /sbin/launchd
  100     1   0.5   20000    03:50:33 ttys004  /Users/me/.bun/bin/claude --model opus
  200   100   1.0   30000       23:11 ttys004  /bin/zsh -c bun test
  300   200  97.8 1851552       00:23 ??       /Users/me/.nvm/versions/node/v24/bin/node /Users/me/.bun/install/cache/jest-worker/processChild.js
  400     1  55.0  535920 03-04:23:36 ??       /System/Library/PrivateFrameworks/SkyLight.framework/Resources/WindowServer -daemon
  500     1   2.0 4221984 03-04:23:16 ??       /Applications/OrbStack.app/Contents/MacOS/OrbStack Helper vmgr
  600     1   3.0   40000       00:01 ??       /usr/bin/java -jar gradle.jar
  700     1   0.0    5000       00:01 ??       /usr/bin/xcodebuild test
  800     1   0.0    5000       00:01 ??       node /x/tsc --watch
`

const fixtureLsof = `p300
fcwd
n/Users/me/Work/Projects/FixIt-Technologies/FixIt/.worktrees/macwatch/apps/api
p100
fcwd
n/Users/me/Work/Projects/FixIt-Technologies/FixIt
p400
fcwd
n/
`

func fixtureRunner(t *testing.T) Runner {
	return func(_ context.Context, name string, args ...string) ([]byte, error) {
		switch name {
		case "sysctl":
			return []byte("{ 11.29 12.81 19.66 }\n"), nil
		case "top":
			return []byte(fixtureTop), nil
		case "ps":
			return []byte(fixturePS), nil
		case "xcrun":
			return []byte("    iPhone 16 (A) (Booted)\n    iPhone 16 (B) (Booted)\n"), nil
		case "lsof":
			if strings.Join(args, " ") != "-a -d cwd -Fpn -p 100,300,400,500,600" {
				t.Errorf("lsof called for the wrong pids: %v", args)
			}
			return []byte(fixtureLsof), errors.New("exit status 1")
		}
		t.Fatalf("unexpected command %s %v", name, args)
		return nil, nil
	}
}

func TestSampleAttributesOwnerProjectAndCounts(t *testing.T) {
	sampler := Sampler{Run: fixtureRunner(t), Home: "/Users/me", Now: func() time.Time { return time.Date(2026, 9, 20, 13, 22, 0, 0, time.Local) }}
	s, err := sampler.Sample(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sys := s.System
	if sys.Load1 != 11.29 || sys.Procs != 2032 || sys.Running != 9 || sys.Threads != 15656 {
		t.Fatalf("load/processes: %+v", sys)
	}
	if sys.UsedGB != 93 || sys.FreeGB != 2.55 || sys.CompressorGB != 42 {
		t.Fatalf("memory: %+v", sys)
	}
	if sys.Claude != 1 || sys.Codex != 0 || sys.Sims != 2 || sys.Xcodebuild != 1 || sys.Tsc != 1 || sys.Java != 1 {
		t.Fatalf("suspect counts: %+v", sys)
	}

	// top by CPU >= 3 % is 300, 400, 600; top 4 by RSS adds 500 (300 and 400 repeat).
	var pids []int
	byPID := map[int]ProcessRow{}
	for _, p := range s.Processes {
		pids = append(pids, p.PID)
		byPID[p.PID] = p
	}
	if len(pids) != 4 || pids[0] != 300 || pids[1] != 400 || pids[2] != 500 || pids[3] != 600 {
		t.Fatalf("selected pids %v", pids)
	}
	jest := byPID[300]
	if jest.OwnerKind != "claude" || jest.OwnerPID != 100 || jest.OwnerTTY != "ttys004" {
		t.Fatalf("owner walk: %+v", jest)
	}
	if jest.Project != "FixIt-Technologies/FixIt:macwatch" || jest.RSSMB != 1808 || jest.Cmd != "node processChild.js" {
		t.Fatalf("project/rss/cmd: %+v", jest)
	}
	ws := byPID[400]
	if ws.OwnerKind != "-" || ws.Project != "-" || ws.Cwd != "/" || ws.Cmd != "WindowServer -daemon" {
		t.Fatalf("unowned system process: %+v", ws)
	}
	if byPID[500].Cwd != "-" || byPID[500].Cmd != "OrbStack.app/Contents/MacOS/OrbStack Helper vmgr" {
		t.Fatalf("missing cwd / app path: %+v", byPID[500])
	}
	if !strings.HasPrefix(jest.TSV(), "P\t2026-09-20T13:22:00\t300\t200\t97.8\t1808\t00:23\t??\tclaude\t100\tttys004\tFixIt-Technologies/FixIt:macwatch\t") {
		t.Fatalf("row shape: %s", jest.TSV())
	}
}

func TestProjectLabel(t *testing.T) {
	root := "/Users/me/Work/Projects"
	cases := map[string]string{
		"/Users/me/Work/Projects/FixIt-Technologies/FixIt/apps/api":                       "FixIt-Technologies/FixIt",
		"/Users/me/Work/Projects/FixIt-Technologies/FixIt/.worktrees/macwatch/apps":       "FixIt-Technologies/FixIt:macwatch",
		"/Users/me/Work/Projects/FixIt-Technologies/FixIt.worktrees/company-tab/apps/web": "FixIt-Technologies/FixIt:company-tab",
		"/Users/me/Work/Projects/FixIt-Technologies":                                      "-",
		"/Users/me/Downloads": "-",
		"":                    "-",
	}
	for cwd, want := range cases {
		if got := ProjectLabel(root, cwd); got != want {
			t.Errorf("%q: got %q want %q", cwd, got, want)
		}
	}
}

func TestOwnerWalkStopsAtHopLimit(t *testing.T) {
	byPID := map[int]proc{1: {pid: 1, ppid: 0, command: "/sbin/launchd"}, 2: {pid: 2, ppid: 1, command: "/Users/me/.bun/bin/codex", tty: "ttys001"}}
	pid := 2
	for i := 3; i <= 3+ownerHops; i++ { // ownerHops+1 links between the leaf and codex
		byPID[i] = proc{pid: i, ppid: pid, command: "/bin/sh"}
		pid = i
	}
	if got := ownerOf(byPID, pid); got.kind != "" {
		t.Fatalf("leaf %d hops away must be unowned, got %+v", ownerHops+1, got)
	}
	if got := ownerOf(byPID, pid-1); got.kind != "codex" || got.pid != 2 || got.tty != "ttys001" {
		t.Fatalf("leaf %d hops away must find codex, got %+v", ownerHops, got)
	}
}
