package claudeguards

import (
	"strings"
	"testing"
)

// killTree is this Mac's shape on 2026-10-01: cmux → login → zsh → switcheroo
// → claude (the calling session, 32503) with its Bash shell, own Codex
// sidekick and own Appium; another session (60209, re-parented to launchd,
// its group leader gone) with its own sidekick and Appium; a daemon.
var killTree = []procRow{
	{machineProc{pid: 1, ppid: 0, args: "/sbin/launchd"}, 1},
	{machineProc{pid: 49134, ppid: 1, args: "/Applications/cmux.app/Contents/MacOS/cmux"}, 49134},
	{machineProc{pid: 30493, ppid: 49134, args: "/usr/bin/login -flp x /bin/bash --noprofile --norc -c exec -l /bin/zsh"}, 30493},
	{machineProc{pid: 30521, ppid: 30493, args: "-/bin/zsh"}, 30521},
	{machineProc{pid: 32255, ppid: 30521, args: "bun --no-env-file /Users/x/.local/bin/switcheroo start --model opus"}, 32255},
	{machineProc{pid: 32503, ppid: 32255, args: "/Users/x/.bun/bin/claude --settings /tmp/cmux-claude-settings.Q1"}, 32255},
	{machineProc{pid: 74357, ppid: 32503, args: "/bin/zsh -c source /Users/x/.claude/shell-snapshots/snapshot-zsh-1.sh"}, 74357},
	{machineProc{pid: 74400, ppid: 32503, args: "/Users/x/.local/bin/claude-guards bash"}, 74400},
	{machineProc{pid: 40813, ppid: 74357, args: "bun --no-env-file /Users/x/.local/bin/switcheroo codex run --cwd /w"}, 74357},
	{machineProc{pid: 40841, ppid: 40813, args: "/Users/x/.local/bin/codex -c model=gpt exec"}, 74357},
	{machineProc{pid: 50000, ppid: 74357, args: "node /w/node_modules/.bin/appium --port 4723"}, 74357},
	{machineProc{pid: 60209, ppid: 1, args: "/Users/x/.bun/bin/claude --session-id da19 --settings /tmp/s"}, 60000},
	{machineProc{pid: 60300, ppid: 60209, args: "/bin/zsh -c source /Users/x/.claude/shell-snapshots/snapshot-zsh-2.sh"}, 60300},
	{machineProc{pid: 60400, ppid: 60300, args: "node /v/node_modules/.bin/appium --port 4724"}, 60300},
	{machineProc{pid: 61000, ppid: 60300, args: "bun --no-env-file /Users/x/.local/bin/switcheroo codex run --cwd /v"}, 60300},
	{machineProc{pid: 777, ppid: 1, args: "/usr/sbin/some-daemon"}, 777},
}

func withKillTree(t *testing.T) {
	t.Helper()
	saveTable, savePID := killProcTable, hookPID
	killProcTable = func() []procRow { return killTree }
	hookPID = func() int { return 74400 }
	t.Cleanup(func() { killProcTable, hookPID = saveTable, savePID })
}

const incidentKill = `pids=$(ps -axo pid,command | grep -E 'switcheroo|codex exec|codex .*calendar-perf|marketplace-ui-vt-4229/node_modules/.bin/appium|wdio run .*calendar|xctrace record' | grep -v grep | awk '{print $1}'); kill -INT ${=pids}`

func TestProcessKill(t *testing.T) {
	withKillTree(t)
	for cmd, want := range map[string]string{
		// The incident and its relatives: targets picked by text.
		incidentKill:                                               "process:pattern-kill",
		"pkill -f appium":                                          "process:pattern-kill",
		"killall node":                                             "process:pattern-kill",
		"sudo pkill -9 -f 'codex .*'":                              "process:pattern-kill",
		"lsof -ti:3000 | xargs kill":                               "process:pattern-kill",
		"lsof -ti:3000 | xargs -r kill -9":                         "process:pattern-kill",
		"kill -9 $(lsof -ti:3000)":                                 "process:pattern-kill",
		"kill 777 $(pgrep -f appium)":                              "process:pattern-kill",
		"pgrep -f appium | xargs kill":                             "process:pattern-kill",
		"for p in $(pgrep -f wdio); do kill $p; done":              "process:pattern-kill",
		"while read p; do kill $p; done < <(pgrep -f wdio)":        "process:pattern-kill",
		`ps aux | grep '[a]ppium' | awk '{print $2}' | xargs kill`: "process:pattern-kill",
		"fuser -k 3000/tcp":                                        "process:pattern-kill",
		`bash -c "pkill -f switcheroo"`:                            "process:pattern-kill",
		// Literal PIDs: only an agent session, its launcher or an
		// ancestor of one outside the calling session's tree.
		"kill -INT 60209":    "process:session-kill",
		"kill 61000":         "process:session-kill",
		"kill 32503":         "process:session-kill",
		"kill -TERM 32255":   "process:session-kill",
		"kill 30521":         "process:session-kill",
		"kill 49134":         "process:session-kill",
		"kill 777 60209":     "process:session-kill",
		"kill -- -32255":     "process:session-kill",
		"kill -9 -60000":     "process:session-kill",
		"kill -9 -1":         "process:session-kill",
		"sudo kill -9 60209": "process:session-kill",
		// Allowed: own work, probes, unknown or ordinary PIDs, read-only listings.
		"kill $!":                     "",
		`kill "$!" 2>/dev/null`:       "",
		"kill %1":                     "",
		"kill 12345":                  "",
		"kill 777":                    "",
		"kill -INT 40813 40841 50000": "",
		"kill -- -74357":              "",
		"kill -0 123":                 "",
		"kill -0 60209":               "",
		"kill -l":                     "",
		"kill $PID":                   "",
		"kill $(cat /tmp/app.pid)":    "",
		"ps aux | grep foo":           "",
		"pgrep -fl appium":            "",
		"lsof -ti:3000":               "",
		"ps -p 777 && kill 777":       "",
		"kill $!; pgrep -fl appium":   "",
		"pkill -0 -f appium":          "",
		"killall -l":                  "",
		"command -v pkill":            "",
		"docker ps -q | xargs docker kill; ps aux":         "",
		"ssh devops 'pkill -f node'":                       "",
		`git commit -m "stop pkill -f appium"`:             "",
		`rg -n 'pkill -f|killall' docs`:                    "",
		"while kill -0 $pid 2>/dev/null; do sleep 1; done": "",
	} {
		in := &HookInput{}
		in.ToolInput.Command = cmd
		got := ""
		if d := guardProcessKill(in); d != nil {
			got = d.Rule
		}
		if got != want {
			t.Errorf("%s: rule %q, want %q", cmd, got, want)
		}
	}
}

// The block names the sanctioned way out: TaskStop, literal PIDs, the user.
func TestProcessKillMessage(t *testing.T) {
	withKillTree(t)
	for _, cmd := range []string{incidentKill, "kill 60209"} {
		in := &HookInput{}
		in.ToolInput.Command = cmd
		d := Bash(in)
		if d == nil {
			t.Fatalf("%s: not blocked", cmd)
		}
		for _, want := range []string{"TaskStop", "kill $!", "pgrep -fl", "ask the user"} {
			if !strings.Contains(d.Text(), want) {
				t.Errorf("%s: message lacks %q:\n%s", cmd, want, d.Text())
			}
		}
	}
	in := &HookInput{}
	in.ToolInput.Command = "kill 30521"
	if d := Codex(in); d == nil || !strings.Contains(d.Message, "an ancestor of the agent session") {
		t.Errorf("Codex must refuse a session's ancestor, got %v", d)
	}
}

// No process table means no verdict on a literal PID; a pattern kill needs none.
func TestProcessKillFailsOpenWithoutTable(t *testing.T) {
	save := killProcTable
	killProcTable = func() []procRow { return nil }
	t.Cleanup(func() { killProcTable = save })
	in := &HookInput{}
	in.ToolInput.Command = "kill 60209"
	if d := guardProcessKill(in); d != nil {
		t.Errorf("literal kill without a table blocked: %s", d.Rule)
	}
	in.ToolInput.Command = "pkill -f appium"
	if d := guardProcessKill(in); d == nil {
		t.Error("pattern kill allowed without a table")
	}
}

func TestSessionKind(t *testing.T) {
	for args, want := range map[string]bool{
		"/Users/x/.bun/bin/claude --resume":                                      true,
		"bun --no-env-file /Users/x/.local/bin/switcheroo start --model opus":    true,
		"/Users/x/.local/bin/codex exec --json":                                  true,
		"node /usr/lib/node_modules/@anthropic-ai/claude-code/cli.js":            true,
		"/Applications/cmux.app/Contents/Resources/bin/cmux-claude-wrapper":      true,
		"bash /Applications/cmux.app/Contents/Resources/bin/cmux-codex-wrapper":  true,
		"tail -f /tmp/claude.log":                                                false,
		"/Users/x/.local/bin/claude-guards bash":                                 false,
		"/bin/zsh -c source /Users/x/.claude/shell-snapshots/s.sh && switcheroo": false,
		"node /w/node_modules/.bin/appium":                                       false,
		"/x/codex-code-mode-host":                                                false,
	} {
		if got := sessionKind(machineProc{args: args}) != ""; got != want {
			t.Errorf("%s: session=%v, want %v", args, got, want)
		}
	}
}
